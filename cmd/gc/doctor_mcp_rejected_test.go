package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/doctor"
)

// mcpRejectionFixture is a city whose single city-scoped agent projects the
// "notes" MCP server into <cityDir>/.mcp.json. cityDir sits one level below a
// git root so the check has to consult both the projected target's directory
// and the git root Claude resolves project settings through.
type mcpRejectionFixture struct {
	gitRoot   string
	cityDir   string
	configDir string
}

func newMCPRejectionFixture(t *testing.T, provider string) mcpRejectionFixture {
	t.Helper()
	clearGCEnv(t)
	gitRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(gitRoot, ".git"), 0o755); err != nil {
		t.Fatalf("MkdirAll(.git): %v", err)
	}
	cityDir := filepath.Join(gitRoot, "city")
	configDir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", configDir)
	writeProjectedMCPCity(t, cityDir, `[beads]
provider = "file"

[session]
provider = "tmux"

[providers.`+provider+`]
command = "echo"
prompt_mode = "none"
`, `
provider = "`+provider+`"
scope = "city"
`)
	writeCatalogFile(t, cityDir, "agents/mayor/mcp/notes.toml", `
name = "notes"
command = "npx"
`)
	return mcpRejectionFixture{gitRoot: gitRoot, cityDir: cityDir, configDir: configDir}
}

func writeJSONFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll(%s): %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile(%s): %v", path, err)
	}
}

func runMCPRejectedCheck(t *testing.T, cityDir string) (*mcpRejectedServersDoctorCheck, *doctor.CheckResult) {
	t.Helper()
	cfg, err := loadCityConfig(cityDir)
	if err != nil {
		t.Fatalf("loadCityConfig: %v", err)
	}
	check := newMCPRejectedServersDoctorCheck(cityDir, cfg, stubLookPath)
	return check, check.Run(&doctor.CheckContext{CityPath: cityDir, Verbose: true})
}

func TestMCPRejectedServersDoctorCheck(t *testing.T) {
	for _, tc := range []struct {
		name     string
		provider string
		// setup writes settings relative to the fixture and returns the file the
		// finding must name ("" when the check must pass).
		setup func(t *testing.T, fx mcpRejectionFixture) string
	}{
		{
			name:     "rejected in local settings at the projected target",
			provider: "claude",
			setup: func(t *testing.T, fx mcpRejectionFixture) string {
				p := filepath.Join(fx.cityDir, ".claude", "settings.local.json")
				writeJSONFile(t, p, `{"disabledMcpjsonServers": ["notes"]}`)
				return p
			},
		},
		{
			name:     "rejected in git-root local settings but not in the target dir",
			provider: "claude",
			setup: func(t *testing.T, fx mcpRejectionFixture) string {
				p := filepath.Join(fx.gitRoot, ".claude", "settings.local.json")
				writeJSONFile(t, p, `{"disabledMcpjsonServers": ["context7", "notes"]}`)
				return p
			},
		},
		{
			name:     "rejected in committed project settings",
			provider: "claude",
			setup: func(t *testing.T, fx mcpRejectionFixture) string {
				p := filepath.Join(fx.gitRoot, ".claude", "settings.json")
				writeJSONFile(t, p, `{"disabledMcpjsonServers": ["notes"]}`)
				return p
			},
		},
		{
			name:     "rejected at user scope",
			provider: "claude",
			setup: func(t *testing.T, fx mcpRejectionFixture) string {
				p := filepath.Join(fx.configDir, "settings.json")
				writeJSONFile(t, p, `{"disabledMcpjsonServers": ["notes"]}`)
				return p
			},
		},
		{
			name:     "rejected in the per-project entry of the Claude state file",
			provider: "claude",
			setup: func(t *testing.T, fx mcpRejectionFixture) string {
				p := filepath.Join(fx.configDir, ".claude.json")
				writeJSONFile(t, p, `{"projects": {"`+fx.gitRoot+`": {"disabledMcpjsonServers": ["notes"]}}}`)
				return p
			},
		},
		{
			name:     "allowlist without rejection is clean",
			provider: "claude",
			setup: func(t *testing.T, fx mcpRejectionFixture) string {
				writeJSONFile(t, filepath.Join(fx.gitRoot, ".claude", "settings.local.json"), `{"enabledMcpjsonServers": ["notes"]}`)
				return ""
			},
		},
		{
			name:     "rejection of a server gc does not project is clean",
			provider: "claude",
			setup: func(t *testing.T, fx mcpRejectionFixture) string {
				writeJSONFile(t, filepath.Join(fx.gitRoot, ".claude", "settings.local.json"), `{"disabledMcpjsonServers": ["context7"]}`)
				return ""
			},
		},
		{
			name:     "non-claude provider is skipped",
			provider: "gemini",
			setup: func(t *testing.T, fx mcpRejectionFixture) string {
				writeJSONFile(t, filepath.Join(fx.cityDir, ".claude", "settings.local.json"), `{"disabledMcpjsonServers": ["notes"]}`)
				return ""
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fx := newMCPRejectionFixture(t, tc.provider)
			wantFile := tc.setup(t, fx)
			_, result := runMCPRejectedCheck(t, fx.cityDir)
			if wantFile == "" {
				if result.Status != doctor.StatusOK {
					t.Fatalf("status = %v (%s, %v), want OK", result.Status, result.Message, result.Details)
				}
				return
			}
			if result.Status != doctor.StatusWarning {
				t.Fatalf("status = %v (%s), want warning", result.Status, result.Message)
			}
			joined := strings.Join(result.Details, "\n")
			for _, want := range []string{"notes", "mayor", wantFile} {
				if !strings.Contains(joined, want) {
					t.Fatalf("details missing %q:\n%s", want, joined)
				}
			}
		})
	}
}

func TestMCPRejectedServersDoctorCheckReportsUnreadableSettings(t *testing.T) {
	fx := newMCPRejectionFixture(t, "claude")
	p := filepath.Join(fx.gitRoot, ".claude", "settings.local.json")
	writeJSONFile(t, p, `{"disabledMcpjsonServers": [`)
	_, result := runMCPRejectedCheck(t, fx.cityDir)
	if result.Status != doctor.StatusWarning {
		t.Fatalf("status = %v (%s), want warning for unreadable settings", result.Status, result.Message)
	}
	if !strings.Contains(strings.Join(result.Details, "\n"), p) {
		t.Fatalf("details = %v, want the unreadable file named", result.Details)
	}
}

func TestMCPRejectedServersDoctorCheckFixOnlyEditsLocalSettings(t *testing.T) {
	fx := newMCPRejectionFixture(t, "claude")
	local := filepath.Join(fx.gitRoot, ".claude", "settings.local.json")
	writeJSONFile(t, local, `{"permissions": {"allow": ["Bash(ls)"]}, "disabledMcpjsonServers": ["context7", "notes"]}`)
	committed := filepath.Join(fx.gitRoot, ".claude", "settings.json")
	committedBody := `{"disabledMcpjsonServers": ["notes"]}`
	writeJSONFile(t, committed, committedBody)

	check, before := runMCPRejectedCheck(t, fx.cityDir)
	if before.Status != doctor.StatusWarning {
		t.Fatalf("pre-fix status = %v, want warning", before.Status)
	}
	if !check.CanFix() {
		t.Fatal("CanFix() = false, want true")
	}
	if err := check.Fix(&doctor.CheckContext{CityPath: fx.cityDir}); err != nil {
		t.Fatalf("Fix: %v", err)
	}

	var got struct {
		Permissions json.RawMessage `json:"permissions"`
		Disabled    []string        `json:"disabledMcpjsonServers"`
	}
	data, err := os.ReadFile(local)
	if err != nil {
		t.Fatalf("ReadFile(local): %v", err)
	}
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("local settings no longer valid JSON: %v\n%s", err, data)
	}
	if len(got.Disabled) != 1 || got.Disabled[0] != "context7" {
		t.Fatalf("disabledMcpjsonServers = %v, want only the unprojected context7 kept", got.Disabled)
	}
	if !strings.Contains(string(got.Permissions), "Bash(ls)") {
		t.Fatalf("unrelated settings lost: %s", data)
	}
	if body, _ := os.ReadFile(committed); string(body) != committedBody {
		t.Fatalf("committed settings.json was modified: %s", body)
	}

	_, after := runMCPRejectedCheck(t, fx.cityDir)
	if after.Status != doctor.StatusWarning || !strings.Contains(strings.Join(after.Details, "\n"), committed) {
		t.Fatalf("post-fix result = %v %v, want the committed rejection still reported", after.Status, after.Details)
	}
	if strings.Contains(strings.Join(after.Details, "\n"), local) {
		t.Fatalf("post-fix details still name the fixed local file: %v", after.Details)
	}
}

func TestMCPRejectedServersDoctorCheckFixDropsEmptiedKey(t *testing.T) {
	fx := newMCPRejectionFixture(t, "claude")
	local := filepath.Join(fx.cityDir, ".claude", "settings.local.json")
	writeJSONFile(t, local, `{"disabledMcpjsonServers": ["notes"]}`)
	check, _ := runMCPRejectedCheck(t, fx.cityDir)
	if err := check.Fix(&doctor.CheckContext{CityPath: fx.cityDir}); err != nil {
		t.Fatalf("Fix: %v", err)
	}
	data, err := os.ReadFile(local)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if strings.Contains(string(data), "disabledMcpjsonServers") {
		t.Fatalf("emptied disabledMcpjsonServers key kept: %s", data)
	}
	if _, after := runMCPRejectedCheck(t, fx.cityDir); after.Status != doctor.StatusOK {
		t.Fatalf("post-fix status = %v %v, want OK", after.Status, after.Details)
	}
}

// A provider-level CLAUDE_CONFIG_DIR is an explicit config dir: its user
// settings and its state file both live inside it, even when the directory is
// named ".claude" and the process environment sets no override.
func TestMCPRejectedServersDoctorCheckHonoursProviderClaudeConfigDir(t *testing.T) {
	clearGCEnv(t)
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	cityDir := t.TempDir()
	accountDir := filepath.Join(t.TempDir(), "account", ".claude")
	writeProjectedMCPCity(t, cityDir, `[beads]
provider = "file"

[session]
provider = "tmux"

[providers.claude]
command = "echo"
prompt_mode = "none"

[providers.claude.env]
CLAUDE_CONFIG_DIR = "`+accountDir+`"
`, `
provider = "claude"
scope = "city"
`)
	writeCatalogFile(t, cityDir, "agents/mayor/mcp/notes.toml", `
name = "notes"
command = "npx"
`)
	state := filepath.Join(accountDir, ".claude.json")
	writeJSONFile(t, state, `{"projects": {"`+cityDir+`": {"disabledMcpjsonServers": ["notes"]}}}`)

	_, result := runMCPRejectedCheck(t, cityDir)
	if result.Status != doctor.StatusWarning || !strings.Contains(strings.Join(result.Details, "\n"), state) {
		t.Fatalf("result = %v %v, want the rejection in %s reported", result.Status, result.Details, state)
	}
}

// The gci-2ia9yv shape: the agent runs in a linked git worktree whose .git is a
// file, and the rejection lives in the MAIN checkout's local settings, which
// Claude resolves project settings through. The check must follow the
// worktree's commondir back to the main checkout, and --fix must clean it.
func TestMCPRejectedServersDoctorCheckFollowsLinkedWorktreeToMainCheckout(t *testing.T) {
	clearGCEnv(t)
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	mainRoot := t.TempDir()
	gitDir := filepath.Join(mainRoot, ".git")
	worktreeGitDir := filepath.Join(gitDir, "worktrees", "agent")
	if err := os.MkdirAll(worktreeGitDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(worktreeGitDir, "commondir"), []byte("../..\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(commondir): %v", err)
	}
	cityDir := filepath.Join(t.TempDir(), "worktrees", "agent")
	writeProjectedMCPCity(t, cityDir, `[beads]
provider = "file"

[session]
provider = "tmux"

[providers.claude]
command = "echo"
prompt_mode = "none"
`, `
provider = "claude"
scope = "city"
`)
	writeCatalogFile(t, cityDir, "agents/mayor/mcp/notes.toml", `
name = "notes"
command = "npx"
`)
	if err := os.WriteFile(filepath.Join(cityDir, ".git"), []byte("gitdir: "+worktreeGitDir+"\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(.git): %v", err)
	}
	local := filepath.Join(mainRoot, ".claude", "settings.local.json")
	writeJSONFile(t, local, `{"disabledMcpjsonServers": ["context7", "notes"]}`)

	check, result := runMCPRejectedCheck(t, cityDir)
	if result.Status != doctor.StatusWarning || !strings.Contains(strings.Join(result.Details, "\n"), local) {
		t.Fatalf("result = %v %v, want the main-checkout rejection in %s reported", result.Status, result.Details, local)
	}
	if err := check.Fix(&doctor.CheckContext{CityPath: cityDir}); err != nil {
		t.Fatalf("Fix: %v", err)
	}
	if _, after := runMCPRejectedCheck(t, cityDir); after.Status != doctor.StatusOK {
		t.Fatalf("post-fix result = %v %v, want OK", after.Status, after.Details)
	}
}
