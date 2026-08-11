package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/codexhooks"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/hooks"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/suspensionstate"
	workdirutil "github.com/gastownhall/gascity/internal/workdir"
)

type codexHooksDriftCheck struct {
	cityPath             string
	dirs                 []string
	userHooksPath        string
	resolveProjectRoot   func(string) (string, error)
	preflightReplacement func(string) error
	writeFileAtomic      func(string, []byte, os.FileMode) error
	ownership            codexHookOwnership
	activeConsumers      func() ([]codexHookConsumer, error)
}

// codexHookConsumer is one runtime-active consumer of Codex's additive hook
// sources. Multiple sessions may share a project root and are intentionally
// checked as one consumer while retaining their identities for diagnostics.
type codexHookConsumer struct {
	workDir     string
	sessionName string
}

type codexHookOwnership struct {
	fileSourcesActive  bool
	sessionFlagsActive bool
	filesystemOwned    bool
}

func (o codexHookOwnership) migrationEligible() bool {
	return o.sessionFlagsActive && !o.filesystemOwned
}

func newCodexHooksDriftCheck(cityPath string, dirs []string, configs ...*config.City) *codexHooksDriftCheck {
	cityPath = strings.TrimSpace(cityPath)
	if cityPath != "" {
		cityPath = filepath.Clean(cityPath)
	}
	ownership := codexHookOwnership{fileSourcesActive: true, sessionFlagsActive: true}
	if len(configs) > 0 {
		ownership = codexHookOwnershipForCity(cityPath, configs[0])
	}
	userHooksPath := codexUserHooksPath()
	check := &codexHooksDriftCheck{
		cityPath:           cityPath,
		dirs:               cleanCodexHookDirs(dirs),
		userHooksPath:      userHooksPath,
		resolveProjectRoot: resolveCodexProjectRoot,
		preflightReplacement: func(cityPath string) error {
			_, err := hooks.ManagedCodexSessionFlags(cityPath)
			return err
		},
		writeFileAtomic: func(path string, data []byte, mode os.FileMode) error {
			return codexhooks.WriteFileAtomicNoFollow(fsys.OSFS{}, path, data, mode)
		},
		ownership: ownership,
	}
	// Tests and the legacy no-config construction retain the supplied dirs as
	// consumers. A real configured city must instead derive consumers from the
	// open, live session inventory; configured scale-to-zero slots are dormant.
	check.activeConsumers = func() ([]codexHookConsumer, error) {
		consumers := make([]codexHookConsumer, 0, len(check.dirs))
		for _, dir := range check.dirs {
			consumers = append(consumers, codexHookConsumer{workDir: dir})
		}
		return consumers, nil
	}
	if len(configs) > 0 && configs[0] != nil {
		check.activeConsumers = codexHookRuntimeConsumers(cityPath, configs[0])
	}
	return check
}

// codexHookRuntimeConsumers returns only sessions that are both persisted as
// active and presently live in the runtime. Filesystem hooks are consumed at
// execution time, so configured-but-unprovisioned pool slots cannot be owners.
func codexHookRuntimeConsumers(cityPath string, cfg *config.City) func() ([]codexHookConsumer, error) {
	return func() ([]codexHookConsumer, error) {
		store, err := openSessionProviderStore(cityPath)
		if err != nil {
			return nil, fmt.Errorf("opening session inventory: %w", err)
		}
		infos, err := session.NewStore(beads.SessionStore{Store: cliSessionStore(store, cfg, cityPath)}).ListLabeledSessionInfosUnfiltered()
		if err != nil {
			return nil, fmt.Errorf("listing open sessions: %w", err)
		}
		sp, err := newStatusSessionProviderForCity(cfg, cityPath)
		if err != nil {
			return nil, fmt.Errorf("constructing runtime liveness observer: %w", err)
		}
		var consumers []codexHookConsumer
		for _, info := range infos {
			if info.Closed || info.State != session.StateActive || strings.TrimSpace(info.WorkDir) == "" {
				continue
			}
			agent := findAgentByTemplate(cfg, info.Template)
			if !codexHookProviderName(info.Provider, cfg.Providers) && !agentUsesCodexHookSurface(cfg, agent) {
				continue
			}
			processNames := []string(nil)
			if agent != nil {
				processNames = config.AgentProcessNames(cfg, *agent, exec.LookPath)
			}
			if !runtime.ObserveLiveness(sp, info.SessionName, processNames).Running {
				continue
			}
			consumers = append(consumers, codexHookConsumer{workDir: info.WorkDir, sessionName: info.SessionName})
		}
		return consumers, nil
	}
}

func codexUserHooksPath() string {
	if codexHome := strings.TrimSpace(os.Getenv("CODEX_HOME")); codexHome != "" {
		return filepath.Join(filepath.Clean(codexHome), "hooks.json")
	}
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".codex", "hooks.json")
	}
	return ""
}

func codexHookOwnershipForCity(cityPath string, cfg *config.City) codexHookOwnership {
	var ownership codexHookOwnership
	if cfg == nil {
		return ownership
	}
	cityState, _ := loadSuspensionState(fsys.OSFS{}, cityPath)
	if effectiveCitySuspended(cfg, cityState) {
		return ownership
	}
	suspendedRigPaths := codexHookSuspendedRigPaths(cityPath, cfg)
	for i := range cfg.Agents {
		agent := &cfg.Agents[i]
		if agent.Suspended || agentInSuspendedRig(cityPath, agent, cfg.Rigs, suspendedRigPaths) {
			continue
		}
		launchesCodex := agent.StartCommand == "" && codexHookProviderName(codexHookEffectiveAgentProvider(cfg, agent), cfg.Providers)
		usesSessionFlags := launchesCodex && sessionProviderUsesT3Bridge(effectiveSessionProvider(agent.Session, cfg.Session.Provider))
		if launchesCodex {
			ownership.fileSourcesActive = true
			if usesSessionFlags {
				ownership.sessionFlagsActive = true
			} else {
				ownership.filesystemOwned = true
			}
		}
		for _, provider := range config.ResolveInstallHooks(agent, &cfg.Workspace) {
			if codexHookProviderName(provider, cfg.Providers) && !usesSessionFlags {
				ownership.fileSourcesActive = true
				ownership.filesystemOwned = true
			}
		}
	}
	return ownership
}

func resolveCodexProjectRoot(dir string) (string, error) {
	dir = filepath.Clean(dir)
	gitPath := filepath.Join(dir, ".git")
	info, err := os.Lstat(gitPath)
	if errors.Is(err, os.ErrNotExist) {
		return dir, nil
	}
	if err != nil {
		return "", fmt.Errorf("inspecting %s: %w", gitPath, err)
	}
	if info.IsDir() {
		return dir, nil
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("inspecting %s: expected a regular gitdir pointer", gitPath)
	}
	data, err := os.ReadFile(gitPath)
	if err != nil {
		return "", fmt.Errorf("reading %s: %w", gitPath, err)
	}
	const prefix = "gitdir: "
	line := strings.TrimSpace(string(data))
	if !strings.HasPrefix(line, prefix) || strings.TrimSpace(strings.TrimPrefix(line, prefix)) == "" {
		return "", fmt.Errorf("parsing %s: malformed gitdir pointer", gitPath)
	}
	adminDir := strings.TrimSpace(strings.TrimPrefix(line, prefix))
	if !filepath.IsAbs(adminDir) {
		adminDir = filepath.Join(dir, adminDir)
	}
	adminDir = filepath.Clean(adminDir)
	commonData, err := os.ReadFile(filepath.Join(adminDir, "commondir"))
	if errors.Is(err, os.ErrNotExist) {
		if _, gitdirErr := os.Lstat(filepath.Join(adminDir, "gitdir")); errors.Is(gitdirErr, os.ErrNotExist) {
			return dir, nil
		} else if gitdirErr != nil {
			return "", fmt.Errorf("inspecting linked-worktree marker for %s: %w", dir, gitdirErr)
		}
	}
	if err != nil {
		return "", fmt.Errorf("reading linked-worktree common dir for %s: %w", dir, err)
	}
	commonDir := strings.TrimSpace(string(commonData))
	if commonDir == "" {
		return "", fmt.Errorf("parsing linked-worktree common dir for %s: empty path", dir)
	}
	if !filepath.IsAbs(commonDir) {
		commonDir = filepath.Join(adminDir, commonDir)
	}
	return filepath.Dir(filepath.Clean(commonDir)), nil
}

func codexHookWorkDirs(cityPath string, cfg *config.City) []string {
	var dirs []string
	addCodexHookDir(&dirs, cityPath)
	if cfg == nil {
		return dirs
	}
	suspendedRigPaths := codexHookSuspendedRigPaths(cityPath, cfg)
	for i := range cfg.Rigs {
		rig := &cfg.Rigs[i]
		suspended := suspendedRigPaths[filepath.Clean(rig.Path)]
		if suspended || strings.TrimSpace(rig.Path) == "" {
			continue
		}
		addCodexHookDir(&dirs, rig.Path)
	}
	for i := range cfg.Agents {
		agent := &cfg.Agents[i]
		if agent.Suspended || agentInSuspendedRig(cityPath, agent, cfg.Rigs, suspendedRigPaths) {
			continue
		}
		if !agentUsesCodexHookSurface(cfg, agent) {
			continue
		}
		addCodexHookAgentWorkDirs(&dirs, cityPath, cfg, agent)
	}
	return dirs
}

func codexHookSuspendedRigPaths(cityPath string, cfg *config.City) map[string]bool {
	paths := map[string]bool{}
	if cfg == nil {
		return paths
	}
	suspState, _ := loadSuspensionState(fsys.OSFS{}, cityPath)
	for i := range cfg.Rigs {
		rig := &cfg.Rigs[i]
		if strings.TrimSpace(rig.Path) != "" && suspensionstate.EffectiveRigSuspended(suspState, rig.Name, rig.EffectiveSuspendedOnStart()) {
			paths[filepath.Clean(rig.Path)] = true
		}
	}
	return paths
}

func cleanCodexHookDirs(dirs []string) []string {
	var cleaned []string
	for _, dir := range dirs {
		addCodexHookDir(&cleaned, dir)
	}
	sort.Strings(cleaned)
	return cleaned
}

func addCodexHookDir(dirs *[]string, dir string) {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return
	}
	dir = filepath.Clean(dir)
	for _, existing := range *dirs {
		if existing == dir {
			return
		}
	}
	*dirs = append(*dirs, dir)
}

func agentUsesCodexHookSurface(cfg *config.City, agent *config.Agent) bool {
	if cfg == nil || agent == nil {
		return false
	}
	if codexHookProviderName(codexHookEffectiveAgentProvider(cfg, agent), cfg.Providers) {
		return true
	}
	for _, provider := range config.ResolveInstallHooks(agent, &cfg.Workspace) {
		if codexHookProviderName(provider, cfg.Providers) {
			return true
		}
	}
	return false
}

func codexHookEffectiveAgentProvider(cfg *config.City, agent *config.Agent) string {
	if agent == nil {
		return ""
	}
	if provider := strings.TrimSpace(agent.Provider); provider != "" {
		return provider
	}
	if cfg != nil {
		return strings.TrimSpace(cfg.Workspace.Provider)
	}
	return ""
}

func codexHookProviderName(name string, providers map[string]config.ProviderSpec) bool {
	name = strings.TrimSpace(name)
	if name == "" {
		return false
	}
	return name == "codex" || config.BuiltinFamily(name, providers) == "codex"
}

func addCodexHookAgentWorkDirs(dirs *[]string, cityPath string, cfg *config.City, agent *config.Agent) {
	addCodexHookAgentWorkDir(dirs, cityPath, cfg, agent, agent.QualifiedName())
	for _, slot := range codexHookPoolSlots(agent) {
		instanceAgent, qualifiedInstance, _ := poolDesiredRequestIdentity(agent, slot)
		if qualifiedInstance == agent.QualifiedName() {
			continue
		}
		addCodexHookAgentWorkDir(dirs, cityPath, cfg, instanceAgent, qualifiedInstance)
	}
}

func addCodexHookAgentWorkDir(dirs *[]string, cityPath string, cfg *config.City, agent *config.Agent, qualifiedName string) {
	workDir, err := resolveCodexHookAgentWorkDir(cityPath, cfg, agent, qualifiedName)
	if err != nil {
		return
	}
	addCodexHookDir(dirs, workDir)
}

func resolveCodexHookAgentWorkDir(cityPath string, cfg *config.City, agent *config.Agent, qualifiedName string) (string, error) {
	if agent == nil {
		return "", nil
	}
	cityName := loadedCityName(cfg, cityPath)
	var rigs []config.Rig
	if cfg != nil {
		rigs = cfg.Rigs
	}
	if strings.TrimSpace(qualifiedName) == "" {
		qualifiedName = agent.QualifiedName()
	}
	workDir, err := workdirutil.ResolveWorkDirPathStrict(cityPath, cityName, qualifiedName, *agent, rigs)
	if err != nil {
		return "", err
	}
	if err := workdirutil.ValidateAncestorWorktreesNotStale(workDir); err != nil {
		return "", err
	}
	return workDir, nil
}

func codexHookPoolSlots(agent *config.Agent) []int {
	if agent == nil || !agent.SupportsInstanceExpansion() {
		return nil
	}
	limit := 1
	if len(agent.NamepoolNames) > 0 {
		limit = len(agent.NamepoolNames)
	} else if maxSessions := agent.EffectiveMaxActiveSessions(); maxSessions != nil {
		if *maxSessions <= 1 {
			return nil
		}
		limit = *maxSessions
	} else if minSessions := agent.EffectiveMinActiveSessions(); minSessions > 1 {
		limit = minSessions
	}
	slots := make([]int, 0, limit)
	for slot := 1; slot <= limit; slot++ {
		slots = append(slots, slot)
	}
	return slots
}

func (c *codexHooksDriftCheck) Name() string { return "codex-hooks-drift" }

func (c *codexHooksDriftCheck) CanFix() bool { return c.ownership.migrationEligible() }

func (c *codexHooksDriftCheck) Fix(_ *doctor.CheckContext) error {
	if !c.ownership.migrationEligible() {
		return errors.New("codex hook migration is unavailable while filesystem hooks remain authoritative")
	}
	if err := c.preflightReplacement(c.cityPath); err != nil {
		return fmt.Errorf("preflighting replacement Codex session flags: %w", err)
	}
	type migration struct {
		source     codexHookSource
		original   []byte
		migrated   []byte
		mode       os.FileMode
		backupPath string
	}
	var migrations []migration
	for _, source := range c.codexHookSources() {
		if source.err != nil {
			return fmt.Errorf("resolving Codex project hook source for %s: %w", source.path, source.err)
		}
		data, info, err := readRegularCodexHookFile(source.path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return fmt.Errorf("reading Codex hook source %s: %w", source.path, err)
		}
		migrated, changed, err := hooks.RemoveManagedCodexHooksForCity(data, c.cityPath)
		if err != nil {
			return fmt.Errorf("migrating Codex hook source %s: %w", source.path, err)
		}
		remaining, err := hooks.AuditCodexHooks(migrated)
		if err != nil {
			return fmt.Errorf("auditing migrated Codex hook source %s: %w", source.path, err)
		}
		if len(remaining.ManagedBehaviorCounts) > 0 {
			return fmt.Errorf("refusing to migrate Codex hook source %s: managed-looking handlers without proven current-city ownership remain (%s)", source.path, formatCodexHookCounts(remaining.ManagedBehaviorCounts, []string{"mail", "nudge", "pre-compact", "session-start"}))
		}
		if !changed {
			continue
		}
		digest := fmt.Sprintf("%x", sha256.Sum256(data))
		migrations = append(migrations, migration{
			source:     source,
			original:   data,
			migrated:   migrated,
			mode:       info.Mode(),
			backupPath: source.path + ".gc-backup-" + digest,
		})
	}
	filesystem := fsys.OSFS{}
	sort.Slice(migrations, func(i, j int) bool { return migrations[i].source.path < migrations[j].source.path })
	var migrateLocked func(int) error
	migrateLocked = func(index int) error {
		if index < len(migrations) {
			return codexhooks.WithPathLock(filesystem, migrations[index].source.path, func() error {
				return migrateLocked(index + 1)
			})
		}
		for _, migration := range migrations {
			current, currentInfo, err := readRegularCodexHookFile(migration.source.path)
			if err != nil {
				return fmt.Errorf("re-reading Codex hook source %s before migration: %w", migration.source.path, err)
			}
			if !bytes.Equal(current, migration.original) || fsys.ComparableMode(currentInfo.Mode()) != fsys.ComparableMode(migration.mode) {
				return fmt.Errorf("codex hook source %s changed during migration", migration.source.path)
			}
		}
		for _, migration := range migrations {
			if err := writeCodexHookBackup(migration.backupPath, migration.original, migration.mode); err != nil {
				return err
			}
		}
		rollback := func(count int, cause error) error {
			errs := []error{cause}
			for index := count - 1; index >= 0; index-- {
				migration := migrations[index]
				if err := c.writeFileAtomic(migration.source.path, migration.original, fsys.ComparableMode(migration.mode)); err != nil {
					errs = append(errs, fmt.Errorf("rolling back Codex hook source %s: %w", migration.source.path, err))
					continue
				}
				restored, restoredInfo, err := readRegularCodexHookFile(migration.source.path)
				if err != nil || !bytes.Equal(restored, migration.original) || fsys.ComparableMode(restoredInfo.Mode()) != fsys.ComparableMode(migration.mode) {
					if err == nil {
						err = errors.New("content or mode mismatch")
					}
					errs = append(errs, fmt.Errorf("verifying rollback of Codex hook source %s: %w", migration.source.path, err))
				}
			}
			return errors.Join(errs...)
		}
		for index, migration := range migrations {
			if err := c.writeFileAtomic(migration.source.path, migration.migrated, fsys.ComparableMode(migration.mode)); err != nil {
				return rollback(index+1, fmt.Errorf("writing migrated Codex hook source %s (backup %s): %w", migration.source.path, migration.backupPath, err))
			}
			written, writtenInfo, err := readRegularCodexHookFile(migration.source.path)
			if err != nil {
				return rollback(index+1, fmt.Errorf("verifying migrated Codex hook source %s (backup %s): %w", migration.source.path, migration.backupPath, err))
			}
			if !bytes.Equal(written, migration.migrated) || fsys.ComparableMode(writtenInfo.Mode()) != fsys.ComparableMode(migration.mode) {
				return rollback(index+1, fmt.Errorf("verifying migrated Codex hook source %s: content or mode mismatch; original bytes remain in %s", migration.source.path, migration.backupPath))
			}
		}
		return nil
	}
	return migrateLocked(0)
}

func readRegularCodexHookFile(path string) ([]byte, os.FileInfo, error) {
	return fsys.ReadRegularFileStable(fsys.OSFS{}, path)
}

func writeCodexHookBackup(backupPath string, data []byte, mode os.FileMode) error {
	existing, _, err := readRegularCodexHookFile(backupPath)
	if err == nil {
		if !bytes.Equal(existing, data) {
			return fmt.Errorf("refusing to overwrite non-matching Codex hook backup %s", backupPath)
		}
		return nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspecting Codex hook backup %s: %w", backupPath, err)
	}
	if err := codexhooks.WriteFileAtomicNoFollow(fsys.OSFS{}, backupPath, data, fsys.ComparableMode(mode)); err != nil {
		return fmt.Errorf("writing Codex hook backup %s: %w", backupPath, err)
	}
	verified, _, err := readRegularCodexHookFile(backupPath)
	if err != nil {
		return fmt.Errorf("verifying Codex hook backup %s: %w", backupPath, err)
	}
	if !bytes.Equal(verified, data) {
		return fmt.Errorf("verifying Codex hook backup %s: content mismatch", backupPath)
	}
	return nil
}

func (c *codexHooksDriftCheck) Run(_ *doctor.CheckContext) *doctor.CheckResult {
	sessionDetail, sessionAudit, sessionErr := codexSessionFlagsAudit(c.cityPath, c.ownership.sessionFlagsActive)
	details := []string{sessionDetail}
	activeHandlers := map[string]int{}
	activeManaged := map[string]int{}
	managedFileSources := 0
	invalidSources := 0
	if sessionErr != nil {
		invalidSources++
	} else {
		addCodexHookCounts(activeHandlers, sessionAudit.EventHandlerCounts)
		addCodexHookCounts(activeManaged, sessionAudit.ManagedBehaviorCounts)
	}
	for _, source := range c.codexHookSources() {
		if source.err != nil {
			invalidSources++
			details = append(details, formatCodexHookSourceError(source, "unresolved", source.err))
			continue
		}
		data, _, err := readRegularCodexHookFile(source.path)
		if errors.Is(err, os.ErrNotExist) {
			details = append(details, fmt.Sprintf("source=%s active=%t path=%s state=missing", source.kind, source.active, source.path))
			continue
		}
		if err != nil {
			invalidSources++
			details = append(details, formatCodexHookSourceError(source, "unreadable", err))
			continue
		}
		audit, err := hooks.AuditCodexHooks(data)
		if err != nil {
			invalidSources++
			details = append(details, formatCodexHookSourceError(source, "malformed", err))
			continue
		}
		details = append(details, formatCodexHookAuditDetail(source.kind, source.path, source.active, data, audit))
		if source.active {
			addCodexHookCounts(activeHandlers, audit.EventHandlerCounts)
			addCodexHookCounts(activeManaged, audit.ManagedBehaviorCounts)
		}
		if len(audit.ManagedBehaviorCounts) > 0 {
			managedFileSources++
		}
	}
	details = append(details, fmt.Sprintf("source=active-total active=%t managed=%s handlers=%s",
		c.ownership.fileSourcesActive || c.ownership.sessionFlagsActive,
		formatCodexHookCounts(activeManaged, []string{"mail", "nudge", "pre-compact", "session-start"}),
		formatCodexHookCounts(activeHandlers, []string{"SessionStart", "PreCompact", "UserPromptSubmit"})))
	if invalidSources > 0 {
		return &doctor.CheckResult{
			Name:     c.Name(),
			Status:   doctor.StatusError,
			Severity: doctor.SeverityAdvisory,
			Message:  fmt.Sprintf("%d Codex hook source(s) could not be audited safely", invalidSources),
			FixHint:  "repair the reported source or restore valid JSON before running `gc doctor --fix`",
			Details:  details,
		}
	}
	if c.ownership.sessionFlagsActive && c.ownership.filesystemOwned {
		return warnCheck(c.Name(),
			fmt.Sprintf("%d Codex hook source(s) overlap under mixed Codex hook ownership", managedFileSources),
			"keep filesystem handlers while non-T3 Codex consumers remain; align every Codex consumer before migrating",
			details)
	}
	if c.ownership.filesystemOwned {
		consumers, err := c.activeConsumers()
		if err != nil {
			return warnCheck(c.Name(), fmt.Sprintf("cannot enumerate runtime-active Codex consumers: %v", err), "repair session inventory or runtime observation before trusting Codex hook ownership", details)
		}
		grouped, err := c.groupActiveCodexConsumers(consumers)
		if err != nil {
			return warnCheck(c.Name(), fmt.Sprintf("cannot resolve runtime-active Codex consumer: %v", err), "repair the reported runtime workdir before trusting Codex hook ownership", details)
		}
		activeRoots := make(map[string]bool, len(grouped))
		for _, consumer := range grouped {
			activeRoots[consumer.workDir] = true
		}
		for _, dir := range c.dirs {
			root := filepath.Clean(dir)
			if c.resolveProjectRoot != nil {
				if resolved, resolveErr := c.resolveProjectRoot(root); resolveErr == nil && strings.TrimSpace(resolved) != "" {
					root = filepath.Clean(resolved)
				}
			}
			if !activeRoots[root] {
				details = append(details, fmt.Sprintf("consumer=configured-dormant workdir=%s", dir))
			}
		}
		for _, consumer := range grouped {
			dir := consumer.workDir
			counts, paths, ownerConverged, err := c.codexConsumerManagedCounts(dir)
			if err != nil {
				return warnCheck(c.Name(), fmt.Sprintf("cannot audit Codex consumer=%s: %v", dir, err), "repair the reported source manually; automatic filesystem migration is unavailable", details)
			}
			details = append(details, fmt.Sprintf("consumer=runtime-active workdir=%s sessions=%s managed=%s paths=%s", dir, strings.Join(consumer.sessions, ","), formatCodexHookCounts(counts, []string{"mail", "nudge", "pre-compact", "session-start"}), strings.Join(paths, ",")))
			if !ownerConverged {
				return warnCheck(c.Name(), fmt.Sprintf("Codex consumer=%s canonical project hooks are not the exact current-city owner", dir), "restart the managed tmux session to converge its canonical project hooks; inspect global hooks manually if the warning remains", details)
			}
			if invalid := invalidCodexManagedBehaviors(counts, true); len(invalid) > 0 {
				return warnCheck(c.Name(), fmt.Sprintf("Codex consumer=%s has invalid managed behavior cardinality: %s", dir, strings.Join(invalid, ", ")), "remove redundant managed handlers from the reported active non-owner manually; automatic filesystem migration is unavailable", details)
			}
		}
	}
	if managedFileSources == 0 {
		result := okCheck(c.Name(), "Codex additive hook sources contain no legacy Gas City handlers")
		result.Details = details
		return result
	}
	if c.ownership.migrationEligible() {
		return warnCheck(c.Name(),
			fmt.Sprintf("%d Codex hook source(s) contain legacy Gas City handlers", managedFileSources),
			"run `gc doctor --fix` to back up the files and remove only legacy Gas City handlers",
			details)
	}
	result := okCheck(c.Name(), "Codex filesystem hook sources match the active provider ownership policy")
	result.Details = details
	return result
}

type groupedCodexHookConsumer struct {
	workDir  string
	sessions []string
}

func (c *codexHooksDriftCheck) groupActiveCodexConsumers(consumers []codexHookConsumer) ([]groupedCodexHookConsumer, error) {
	byRoot := map[string]*groupedCodexHookConsumer{}
	for _, consumer := range consumers {
		workDir := filepath.Clean(consumer.workDir)
		if c.resolveProjectRoot != nil {
			root, err := c.resolveProjectRoot(workDir)
			if err != nil {
				return nil, fmt.Errorf("resolving %s: %w", workDir, err)
			}
			if strings.TrimSpace(root) != "" {
				workDir = filepath.Clean(root)
			}
		}
		group := byRoot[workDir]
		if group == nil {
			group = &groupedCodexHookConsumer{workDir: workDir}
			byRoot[workDir] = group
		}
		if name := strings.TrimSpace(consumer.sessionName); name != "" {
			group.sessions = append(group.sessions, name)
		}
	}
	keys := make([]string, 0, len(byRoot))
	for root := range byRoot {
		keys = append(keys, root)
	}
	sort.Strings(keys)
	grouped := make([]groupedCodexHookConsumer, 0, len(keys))
	for _, root := range keys {
		group := byRoot[root]
		sort.Strings(group.sessions)
		group.sessions = slices.Compact(group.sessions)
		grouped = append(grouped, *group)
	}
	return grouped, nil
}

func (c *codexHooksDriftCheck) codexConsumerManagedCounts(dir string) (map[string]int, []string, bool, error) {
	paths := []string{c.userHooksPath}
	root := dir
	if c.resolveProjectRoot != nil {
		resolved, err := c.resolveProjectRoot(dir)
		if err != nil {
			return nil, nil, false, err
		}
		if resolved != "" {
			root = resolved
		}
	}
	ownerPath := filepath.Clean(filepath.Join(root, ".codex", "hooks.json"))
	paths = append(paths, ownerPath)
	counts := map[string]int{}
	seen := map[string]bool{}
	used := []string{}
	ownerConverged := false
	for _, path := range paths {
		path = filepath.Clean(path)
		if path == "." || seen[path] {
			continue
		}
		seen[path] = true
		data, _, err := readRegularCodexHookFile(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, nil, false, err
		}
		audit, err := hooks.AuditCodexHooks(data)
		if err != nil {
			return nil, nil, false, err
		}
		addCodexHookCounts(counts, audit.ManagedBehaviorCounts)
		used = append(used, path)
		if path == ownerPath {
			ownerConverged = hooks.CodexHooksAreConverged(data, c.cityPath)
		}
	}
	return counts, used, ownerConverged, nil
}

func invalidCodexManagedBehaviors(counts map[string]int, requireComplete bool) []string {
	keys := []string{"session-start", "pre-compact", "mail", "nudge"}
	duplicates := make([]string, 0, len(keys))
	for _, key := range keys {
		if count := counts[key]; count > 1 || (requireComplete && count == 0) {
			duplicates = append(duplicates, fmt.Sprintf("%s:%d", key, count))
		}
	}
	return duplicates
}

func addCodexHookCounts(total, counts map[string]int) {
	for key, count := range counts {
		total[key] += count
	}
}

func formatCodexHookSourceError(source codexHookSource, state string, err error) string {
	return fmt.Sprintf("source=%s active=%t path=%s state=%s error=%q", source.kind, source.active, source.path, state, err)
}

type codexHookSource struct {
	kind   string
	path   string
	active bool
	err    error
}

func (c *codexHooksDriftCheck) codexHookSources() []codexHookSource {
	var sources []codexHookSource
	seen := map[string]int{}
	add := func(source codexHookSource) {
		source.path = filepath.Clean(source.path)
		if source.path == "." || source.path == "" {
			return
		}
		if index, ok := seen[source.path]; ok {
			if source.active && !sources[index].active {
				sources[index] = source
			} else if source.kind == "project-root" && sources[index].kind == "project" {
				sources[index].kind = source.kind
			}
			return
		}
		seen[source.path] = len(sources)
		sources = append(sources, source)
	}
	if strings.TrimSpace(c.userHooksPath) != "" {
		add(codexHookSource{kind: "user", path: c.userHooksPath, active: c.ownership.fileSourcesActive})
	}
	for _, dir := range c.dirs {
		root := dir
		if c.resolveProjectRoot != nil {
			resolved, err := c.resolveProjectRoot(dir)
			if err != nil {
				add(codexHookSource{kind: "project-resolution", path: dir, err: err})
				continue
			}
			if strings.TrimSpace(resolved) != "" {
				root = resolved
			}
		}
		if filepath.Clean(root) == filepath.Clean(dir) {
			add(codexHookSource{kind: "project", path: filepath.Join(root, ".codex", "hooks.json"), active: c.ownership.fileSourcesActive})
			continue
		}
		add(codexHookSource{kind: "project-root", path: filepath.Join(root, ".codex", "hooks.json"), active: c.ownership.fileSourcesActive})
		add(codexHookSource{kind: "inert-worktree", path: filepath.Join(dir, ".codex", "hooks.json"), active: false})
	}
	return sources
}

func codexSessionFlagsAudit(cityPath string, active bool) (string, hooks.CodexHooksAudit, error) {
	if !active {
		return "source=sessionFlags active=false state=inactive", hooks.CodexHooksAudit{}, nil
	}
	payload, err := hooks.ManagedCodexSessionFlags(cityPath)
	if err != nil {
		return fmt.Sprintf("source=sessionFlags active=true state=malformed error=%q", err), hooks.CodexHooksAudit{}, err
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return fmt.Sprintf("source=sessionFlags active=true state=malformed error=%q", err), hooks.CodexHooksAudit{}, err
	}
	document := struct {
		Hooks struct {
			SessionStart     []runtime.CodexHookEntry `json:"SessionStart"`
			PreCompact       []runtime.CodexHookEntry `json:"PreCompact"`
			UserPromptSubmit []runtime.CodexHookEntry `json:"UserPromptSubmit"`
		} `json:"hooks"`
	}{}
	document.Hooks.SessionStart = payload.Config.SessionStart
	document.Hooks.PreCompact = payload.Config.PreCompact
	document.Hooks.UserPromptSubmit = payload.Config.UserPromptSubmit
	documentData, err := json.Marshal(document)
	if err != nil {
		return fmt.Sprintf("source=sessionFlags active=true state=malformed error=%q", err), hooks.CodexHooksAudit{}, err
	}
	audit, err := hooks.AuditCodexHooks(documentData)
	if err != nil {
		return fmt.Sprintf("source=sessionFlags active=true state=malformed error=%q", err), hooks.CodexHooksAudit{}, err
	}
	return formatCodexHookAuditDetail("sessionFlags", "", true, data, audit), audit, nil
}

func formatCodexHookAuditDetail(source, path string, active bool, data []byte, audit hooks.CodexHooksAudit) string {
	hash := fmt.Sprintf("%x", sha256.Sum256(data))
	parts := []string{fmt.Sprintf("source=%s", source), fmt.Sprintf("active=%t", active)}
	if path != "" {
		parts = append(parts, "path="+path)
	}
	parts = append(parts, "sha256="+hash)
	if handlers := formatCodexHookCounts(audit.EventHandlerCounts, []string{"SessionStart", "PreCompact", "UserPromptSubmit"}); handlers != "" {
		parts = append(parts, "handlers="+handlers)
	}
	if managed := formatCodexHookCounts(audit.ManagedBehaviorCounts, []string{"mail", "nudge", "pre-compact", "session-start"}); managed != "" {
		parts = append(parts, "managed="+managed)
	}
	return strings.Join(parts, " ")
}

func formatCodexHookCounts(counts map[string]int, keys []string) string {
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		if count := counts[key]; count > 0 {
			parts = append(parts, fmt.Sprintf("%s:%d", key, count))
		}
	}
	return strings.Join(parts, ",")
}
