package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/materialize"
)

type mcpConfigDoctorCheck struct {
	cityPath string
	cfg      *config.City
	lookPath config.LookPathFunc
}

type mcpSharedTargetDoctorCheck struct {
	cityPath string
	cfg      *config.City
	lookPath config.LookPathFunc
}

type mcpTargetConflict struct {
	Provider string
	Target   string
	Agents   []string
}

func newMCPConfigDoctorCheck(cityPath string, cfg *config.City, lookPath config.LookPathFunc) *mcpConfigDoctorCheck {
	return &mcpConfigDoctorCheck{cityPath: cityPath, cfg: cfg, lookPath: lookPath}
}

func newMCPSharedTargetDoctorCheck(cityPath string, cfg *config.City, lookPath config.LookPathFunc) *mcpSharedTargetDoctorCheck {
	return &mcpSharedTargetDoctorCheck{cityPath: cityPath, cfg: cfg, lookPath: lookPath}
}

func (*mcpConfigDoctorCheck) Name() string                     { return "mcp-config" }
func (*mcpConfigDoctorCheck) CanFix() bool                     { return false }
func (*mcpConfigDoctorCheck) Fix(_ *doctor.CheckContext) error { return nil }

func (c *mcpConfigDoctorCheck) Run(_ *doctor.CheckContext) *doctor.CheckResult {
	issues, _ := inspectMCPProjectionHealth(c.cityPath, c.cfg, c.lookPath)
	if len(issues) == 0 {
		return &doctor.CheckResult{Name: c.Name(), Status: doctor.StatusOK, Message: "MCP definitions and delivery paths are valid"}
	}
	return &doctor.CheckResult{
		Name:    c.Name(),
		Status:  doctor.StatusError,
		Message: summarizeMCPIssues(issues),
		Details: issues,
		FixHint: `fix the reported MCP definitions or provider/runtime settings, then rerun "gc doctor"`,
	}
}

func (*mcpSharedTargetDoctorCheck) Name() string                     { return "mcp-shared-target" }
func (*mcpSharedTargetDoctorCheck) CanFix() bool                     { return false }
func (*mcpSharedTargetDoctorCheck) Fix(_ *doctor.CheckContext) error { return nil }

func (c *mcpSharedTargetDoctorCheck) Run(_ *doctor.CheckContext) *doctor.CheckResult {
	_, conflicts := inspectMCPProjectionHealth(c.cityPath, c.cfg, c.lookPath)
	if len(conflicts) == 0 {
		return &doctor.CheckResult{Name: c.Name(), Status: doctor.StatusOK, Message: "no projected MCP target conflicts"}
	}
	details := make([]string, 0, len(conflicts))
	for _, conflict := range conflicts {
		details = append(details, fmt.Sprintf(
			"%s (%s): %s",
			conflict.Target,
			conflict.Provider,
			strings.Join(conflict.Agents, ", "),
		))
	}
	return &doctor.CheckResult{
		Name:    c.Name(),
		Status:  doctor.StatusError,
		Message: summarizeMCPIssues(details),
		Details: details,
		FixHint: `make the effective MCP payload identical for every agent that shares a provider-native target, or split them onto different targets`,
	}
}

func inspectMCPProjectionHealth(cityPath string, cfg *config.City, lookPath config.LookPathFunc) ([]string, []mcpTargetConflict) {
	if cfg == nil || len(cfg.Agents) == 0 {
		return nil, nil
	}

	type targetState struct {
		hashes map[string][]string
	}

	targets := make(map[string]*targetState)
	var issues []string

	for i := range cfg.Agents {
		agent := &cfg.Agents[i]
		view, err := resolveConfiguredAgentMCPProjection(cityPath, cfg, agent, lookPath)
		if err != nil {
			issues = append(issues, fmt.Sprintf("agent %q: %v", agent.QualifiedName(), err))
			continue
		}
		if len(view.Catalog.Servers) == 0 || strings.TrimSpace(view.Projection.Provider) == "" || strings.TrimSpace(view.Projection.Target) == "" {
			continue
		}
		key := view.Projection.Provider + "|" + filepath.Clean(view.Projection.Target)
		state := targets[key]
		if state == nil {
			state = &targetState{hashes: make(map[string][]string)}
			targets[key] = state
		}
		hash := view.Projection.Hash()
		state.hashes[hash] = append(state.hashes[hash], agent.QualifiedName())
	}

	sort.Strings(issues)

	conflicts := make([]mcpTargetConflict, 0, len(targets))
	for key, state := range targets {
		if len(state.hashes) < 2 {
			continue
		}
		parts := strings.SplitN(key, "|", 2)
		agents := make([]string, 0, 4)
		for _, members := range state.hashes {
			sort.Strings(members)
			agents = append(agents, members...)
		}
		sort.Strings(agents)
		conflicts = append(conflicts, mcpTargetConflict{
			Provider: parts[0],
			Target:   parts[1],
			Agents:   agents,
		})
	}
	sort.Slice(conflicts, func(i, j int) bool {
		if conflicts[i].Target != conflicts[j].Target {
			return conflicts[i].Target < conflicts[j].Target
		}
		return conflicts[i].Provider < conflicts[j].Provider
	})
	return issues, conflicts
}

func summarizeMCPIssues(items []string) string {
	switch len(items) {
	case 0:
		return ""
	case 1:
		return items[0]
	default:
		return fmt.Sprintf("%s (and %d more)", items[0], len(items)-1)
	}
}

// claudeDisabledMCPServersKey is the Claude Code setting that records a
// project-scoped (.mcp.json) MCP server the user rejected. It has the highest
// priority of the approval settings: it overrides enableAllProjectMcpServers
// and enabledMcpjsonServers, and Claude never re-prompts for a rejected
// server, so a gc-projected server listed here silently never loads.
const claudeDisabledMCPServersKey = "disabledMcpjsonServers"

// mcpRejectedServersDoctorCheck warns when an MCP server gc projects into a
// Claude agent's .mcp.json is rejected in any Claude settings source that
// applies to that project.
type mcpRejectedServersDoctorCheck struct {
	cityPath string
	cfg      *config.City
	lookPath config.LookPathFunc
}

// mcpApprovalSource is one file Claude may read disabledMcpjsonServers
// from. projectKey is set for the per-project entry of the Claude state file.
// Only local project settings are machine-written approval state, so only
// those are fixable.
type mcpApprovalSource struct {
	path       string
	projectKey string
	fixable    bool
}

type mcpServerRejection struct {
	agent  string
	server string
	target string
	source mcpApprovalSource
}

func newMCPRejectedServersDoctorCheck(cityPath string, cfg *config.City, lookPath config.LookPathFunc) *mcpRejectedServersDoctorCheck {
	return &mcpRejectedServersDoctorCheck{cityPath: cityPath, cfg: cfg, lookPath: lookPath}
}

func (*mcpRejectedServersDoctorCheck) Name() string { return "mcp-rejected-servers" }
func (*mcpRejectedServersDoctorCheck) CanFix() bool { return true }

func (c *mcpRejectedServersDoctorCheck) Run(_ *doctor.CheckContext) *doctor.CheckResult {
	rejections, readErrs := c.inspect()
	if len(rejections) == 0 && len(readErrs) == 0 {
		return &doctor.CheckResult{Name: c.Name(), Status: doctor.StatusOK, Message: "no gc-projected Claude MCP servers are rejected in Claude settings"}
	}
	details := make([]string, 0, len(rejections)+len(readErrs))
	var manual []string
	for _, r := range rejections {
		where := r.source.path
		if r.source.projectKey != "" {
			where = fmt.Sprintf("%s (projects[%q])", r.source.path, r.source.projectKey)
		}
		details = append(details, fmt.Sprintf("%s projected for %s (%s) but rejected in %s", r.server, r.agent, r.target, where))
		if !r.source.fixable {
			manual = append(manual, where)
		}
	}
	details = append(details, readErrs...)
	message := summarizeMCPIssues(details)
	if len(rejections) > 0 {
		message = fmt.Sprintf("%d gc-projected MCP server rejection(s) in Claude settings: %s", len(rejections), summarizeMCPIssues(details))
	}
	hint := `run "gc doctor --fix" to remove the rejections from .claude/settings.local.json`
	if len(manual) > 0 {
		hint += "; remove them by hand from " + strings.Join(uniqueSortedMCPStrings(manual), ", ")
	}
	return &doctor.CheckResult{
		Name:    c.Name(),
		Status:  doctor.StatusWarning,
		Message: message,
		Details: details,
		FixHint: hint,
	}
}

// Fix removes rejected gc-projected server names from local project settings
// (.claude/settings.local.json) only. Committed project settings, user settings
// and the Claude state file are reported but never rewritten.
func (c *mcpRejectedServersDoctorCheck) Fix(_ *doctor.CheckContext) error {
	rejections, _ := c.inspect()
	byFile := make(map[string]map[string]bool)
	for _, r := range rejections {
		if !r.source.fixable {
			continue
		}
		if byFile[r.source.path] == nil {
			byFile[r.source.path] = make(map[string]bool)
		}
		byFile[r.source.path][r.server] = true
	}
	paths := make([]string, 0, len(byFile))
	for path := range byFile {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	var errs []error
	for _, path := range paths {
		if err := removeClaudeDisabledMCPServers(path, byFile[path]); err != nil {
			errs = append(errs, fmt.Errorf("removing MCP rejections from %s: %w", path, err))
		}
	}
	return errors.Join(errs...)
}

func (c *mcpRejectedServersDoctorCheck) inspect() ([]mcpServerRejection, []string) {
	if c.cfg == nil {
		return nil, nil
	}
	cache := make(map[mcpApprovalSource]claudeDisabledRead)
	var rejections []mcpServerRejection
	readErrSet := make(map[string]bool)
	for i := range c.cfg.Agents {
		agent := &c.cfg.Agents[i]
		view, err := resolveConfiguredAgentMCPProjection(c.cityPath, c.cfg, agent, c.lookPath)
		// Projection errors are the mcp-config check's to report.
		if err != nil || view.Projection.Provider != materialize.MCPProviderClaude || len(view.Projection.Servers) == 0 {
			continue
		}
		projected := make(map[string]bool, len(view.Projection.Servers))
		for _, server := range view.Projection.Servers {
			projected[server.Name] = true
		}
		configDir, explicitConfigDir := c.claudeConfigDir(agent)
		for _, source := range mcpApprovalSources(filepath.Dir(view.Projection.Target), configDir, explicitConfigDir) {
			read, ok := cache[source]
			if !ok {
				read = readClaudeDisabledMCPServers(source)
				cache[source] = read
			}
			if read.err != nil {
				readErrSet[fmt.Sprintf("cannot read %s: %v", source.path, read.err)] = true
				continue
			}
			for _, name := range read.names {
				if projected[name] {
					rejections = append(rejections, mcpServerRejection{
						agent:  agent.QualifiedName(),
						server: name,
						target: view.Projection.Target,
						source: source,
					})
				}
			}
		}
	}
	sort.Slice(rejections, func(i, j int) bool {
		a, b := rejections[i], rejections[j]
		if a.agent != b.agent {
			return a.agent < b.agent
		}
		if a.server != b.server {
			return a.server < b.server
		}
		return a.source.path < b.source.path
	})
	readErrs := make([]string, 0, len(readErrSet))
	for msg := range readErrSet {
		readErrs = append(readErrs, msg)
	}
	sort.Strings(readErrs)
	return rejections, readErrs
}

// claudeConfigDir resolves the Claude user config directory an agent's session
// runs with: the provider's CLAUDE_CONFIG_DIR override, then the inherited
// process value, then Claude's default ~/.claude. explicit reports whether
// CLAUDE_CONFIG_DIR set it, which moves the state file inside the directory.
// An empty dir means no home directory could be determined; user-scope sources
// are then skipped.
func (c *mcpRejectedServersDoctorCheck) claudeConfigDir(agent *config.Agent) (dir string, explicit bool) {
	if resolved, err := config.ResolveProvider(agent, &c.cfg.Workspace, c.cfg.Providers, c.lookPath); err == nil && resolved != nil {
		if dir := strings.TrimSpace(resolved.Env["CLAUDE_CONFIG_DIR"]); dir != "" {
			return dir, true
		}
	}
	if dir := strings.TrimSpace(os.Getenv("CLAUDE_CONFIG_DIR")); dir != "" {
		return dir, true
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", false
	}
	return filepath.Join(home, ".claude"), false
}

// mcpApprovalSources lists every file that can carry disabledMcpjsonServers
// for a project rooted at targetDir. Claude resolves project settings through
// the git root, so the target directory, its git root, and — for a linked git
// worktree — the main checkout that owns the shared .git are all consulted.
// With an explicit CLAUDE_CONFIG_DIR the state file lives inside the config
// directory; by default it lives beside ~/.claude as ~/.claude.json.
func mcpApprovalSources(targetDir, configDir string, explicitConfigDir bool) []mcpApprovalSource {
	projectDirs := []string{filepath.Clean(targetDir)}
	for _, root := range gitProjectRootsFor(targetDir) {
		if !containsString(projectDirs, root) {
			projectDirs = append(projectDirs, root)
		}
	}
	var sources []mcpApprovalSource
	for _, dir := range projectDirs {
		sources = append(sources,
			mcpApprovalSource{path: filepath.Join(dir, ".claude", "settings.json")},
			mcpApprovalSource{path: filepath.Join(dir, ".claude", "settings.local.json"), fixable: true},
		)
	}
	if configDir == "" {
		return sources
	}
	sources = append(sources, mcpApprovalSource{path: filepath.Join(configDir, "settings.json")})
	statePath := filepath.Join(configDir, ".claude.json")
	if !explicitConfigDir {
		statePath = filepath.Join(filepath.Dir(configDir), ".claude.json")
	}
	for _, dir := range projectDirs {
		for _, key := range projectKeyVariants(dir) {
			sources = append(sources, mcpApprovalSource{path: statePath, projectKey: key})
		}
	}
	return sources
}

// projectKeyVariants returns the spellings Claude may have recorded a project
// under: the cleaned path and, when different, its symlink-resolved form
// (e.g. /var vs /private/var on macOS).
func projectKeyVariants(dir string) []string {
	keys := []string{filepath.Clean(dir)}
	if resolved, err := filepath.EvalSymlinks(dir); err == nil && resolved != keys[0] {
		keys = append(keys, resolved)
	}
	return keys
}

// gitProjectRootsFor returns the checkout root containing dir (the nearest
// ancestor with a .git entry) and, when that checkout is a linked worktree, the
// main checkout owning the shared git directory. It returns nil outside a git
// checkout.
func gitProjectRootsFor(dir string) []string {
	root := gitRootFor(dir)
	if root == "" {
		return nil
	}
	roots := []string{root}
	if main := linkedWorktreeMainCheckout(root); main != "" && main != root {
		roots = append(roots, main)
	}
	return roots
}

// gitRootFor walks up from dir to the nearest directory containing a .git
// entry (a directory, or a file in a linked worktree). It returns "" when dir
// is not inside a git checkout.
func gitRootFor(dir string) string {
	dir = filepath.Clean(dir)
	for {
		if _, err := os.Lstat(filepath.Join(dir, ".git")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}

// linkedWorktreeMainCheckout resolves a linked worktree's main checkout: its
// .git file names the per-worktree git dir ("gitdir: <path>"), whose commondir
// points at the shared .git directory, whose parent is the main checkout. It
// returns "" for a regular checkout or any layout it cannot follow.
func linkedWorktreeMainCheckout(root string) string {
	data, err := os.ReadFile(filepath.Join(root, ".git"))
	if err != nil {
		return "" // a .git directory (regular checkout) or unreadable
	}
	gitDir, ok := strings.CutPrefix(strings.TrimSpace(string(data)), "gitdir:")
	if !ok {
		return ""
	}
	gitDir = strings.TrimSpace(gitDir)
	if !filepath.IsAbs(gitDir) {
		gitDir = filepath.Join(root, gitDir)
	}
	common, err := os.ReadFile(filepath.Join(gitDir, "commondir"))
	if err != nil {
		return ""
	}
	commonDir := strings.TrimSpace(string(common))
	if !filepath.IsAbs(commonDir) {
		commonDir = filepath.Join(gitDir, commonDir)
	}
	commonDir = filepath.Clean(commonDir)
	if filepath.Base(commonDir) != ".git" {
		return "" // bare repository: no main checkout
	}
	return filepath.Dir(commonDir)
}

type claudeDisabledRead struct {
	names []string
	err   error
}

// readClaudeDisabledMCPServers reads disabledMcpjsonServers from one source. A
// missing file (or missing project entry) is not an error.
func readClaudeDisabledMCPServers(source mcpApprovalSource) claudeDisabledRead {
	data, err := os.ReadFile(source.path)
	if errors.Is(err, fs.ErrNotExist) {
		return claudeDisabledRead{}
	}
	if err != nil {
		return claudeDisabledRead{err: err}
	}
	type approvalSettings struct {
		Disabled []string `json:"disabledMcpjsonServers"`
	}
	if source.projectKey == "" {
		var settings approvalSettings
		if err := json.Unmarshal(data, &settings); err != nil {
			return claudeDisabledRead{err: err}
		}
		return claudeDisabledRead{names: settings.Disabled}
	}
	var state struct {
		Projects map[string]approvalSettings `json:"projects"`
	}
	if err := json.Unmarshal(data, &state); err != nil {
		return claudeDisabledRead{err: err}
	}
	return claudeDisabledRead{names: state.Projects[source.projectKey].Disabled}
}

// removeClaudeDisabledMCPServers rewrites one local settings file without the
// given server names, preserving every other top-level key verbatim and
// dropping the key when the list empties. The write is atomic.
func removeClaudeDisabledMCPServers(path string, remove map[string]bool) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var settings map[string]json.RawMessage
	if err := json.Unmarshal(data, &settings); err != nil {
		return err
	}
	var disabled []string
	if raw, ok := settings[claudeDisabledMCPServersKey]; ok {
		if err := json.Unmarshal(raw, &disabled); err != nil {
			return fmt.Errorf("decoding %s: %w", claudeDisabledMCPServersKey, err)
		}
	}
	kept := make([]string, 0, len(disabled))
	for _, name := range disabled {
		if !remove[name] {
			kept = append(kept, name)
		}
	}
	if len(kept) == len(disabled) {
		return nil
	}
	if len(kept) == 0 {
		delete(settings, claudeDisabledMCPServersKey)
	} else {
		encoded, err := json.Marshal(kept)
		if err != nil {
			return err
		}
		settings[claudeDisabledMCPServersKey] = encoded
	}
	out, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return err
	}
	return fsys.WriteFileAtomic(fsys.OSFS{}, path, append(out, '\n'), info.Mode().Perm())
}

func uniqueSortedMCPStrings(values []string) []string {
	seen := make(map[string]bool, len(values))
	out := make([]string, 0, len(values))
	for _, v := range values {
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	sort.Strings(out)
	return out
}
