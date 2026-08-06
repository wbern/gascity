package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// cityWithClaudeSettings creates a city dir containing .gc/settings.json and
// returns (cityPath, settingsPath).
func cityWithClaudeSettings(t *testing.T) (string, string) {
	t.Helper()
	cityPath := t.TempDir()
	gcDir := filepath.Join(cityPath, ".gc")
	if err := os.MkdirAll(gcDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	settings := filepath.Join(gcDir, "settings.json")
	if err := os.WriteFile(settings, []byte(`{"hooks":{}}`), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return cityPath, settings
}

// TestBuildProviderResumeCommandCarriesSettings pins gcw-0ut6: a provider with
// an explicit resume_command must still receive the provider-owned --settings
// file, exactly as BuildProviderLaunchCommand does.
//
// Without it a resumed Claude session runs with NO gc hooks at all —
// .gc/settings.json is the sole delivery route for SessionStart priming,
// UserPromptSubmit injection, and the PreCompact auto-handoff. Measured on the
// live fleet: two resumed sessions, 9.2MB of transcript, zero hook injections.
func TestBuildProviderResumeCommandCarriesSettings(t *testing.T) {
	cityPath, settings := cityWithClaudeSettings(t)
	resolved := &ResolvedProvider{
		Name:            "claude-account",
		BuiltinAncestor: "claude",
		ResumeCommand:   `/bin/sh -c 'exec launcher -- claude --resume {{.SessionKey}} "$@"' launcher`,
		ResumeFlag:      "--resume",
	}

	got, err := BuildProviderResumeCommand(cityPath, resolved, nil)
	if err != nil {
		t.Fatalf("BuildProviderResumeCommand: %v", err)
	}
	want := fmt.Sprintf("--settings %q", settings)
	if !strings.Contains(got, want) {
		t.Fatalf("resume command lost the provider settings file:\n got:  %s\n want: ...%s...", got, want)
	}
	if n := strings.Count(got, "--settings"); n != 1 {
		t.Fatalf("resume command has %d --settings flags, want exactly 1:\n got: %s", n, got)
	}
	// The settings arg must land AFTER the wrapper's closing quote so the
	// shell passes it through "$@" — appending inside would corrupt the script.
	if idx := strings.Index(got, "--settings"); idx < strings.LastIndex(got, "'") {
		t.Fatalf("--settings landed inside the shell script body, not as a passed-through arg:\n got: %s", got)
	}
}

// TestBuildProviderResumeCommandDoesNotDoubleSettings guards the idempotent
// case: a resume_command that already names a settings file must not get a
// second one, which would leave Claude with two conflicting --settings args.
func TestBuildProviderResumeCommandDoesNotDoubleSettings(t *testing.T) {
	cityPath, settings := cityWithClaudeSettings(t)
	resolved := &ResolvedProvider{
		Name:            "claude-account",
		BuiltinAncestor: "claude",
		ResumeCommand:   fmt.Sprintf(`/bin/sh -c 'exec claude --resume {{.SessionKey}} --settings %q'`, settings),
		ResumeFlag:      "--resume",
	}

	got, err := BuildProviderResumeCommand(cityPath, resolved, nil)
	if err != nil {
		t.Fatalf("BuildProviderResumeCommand: %v", err)
	}
	if n := strings.Count(got, "--settings"); n != 1 {
		t.Fatalf("resume command has %d --settings flags, want exactly 1:\n got: %s", n, got)
	}
}

// TestBuildProviderResumeCommandNonClaudeUnchanged pins that only the
// claude family owns a settings file; other providers must be untouched.
func TestBuildProviderResumeCommandNonClaudeUnchanged(t *testing.T) {
	cityPath, _ := cityWithClaudeSettings(t)
	original := `/bin/sh -c 'exec codex resume {{.SessionKey}} "$@"' codex`
	resolved := &ResolvedProvider{
		Name:            "codex-account",
		BuiltinAncestor: "codex",
		ResumeCommand:   original,
		ResumeFlag:      "resume",
	}

	got, err := BuildProviderResumeCommand(cityPath, resolved, nil)
	if err != nil {
		t.Fatalf("BuildProviderResumeCommand: %v", err)
	}
	if got != original {
		t.Fatalf("non-claude resume command was modified:\n got:  %s\n want: %s", got, original)
	}
}

// TestBuildProviderResumeCommandEmptyStaysEmpty pins that a provider with no
// explicit resume_command does not gain one. Those providers resume off
// Info.Command, which already carries --settings.
func TestBuildProviderResumeCommandEmptyStaysEmpty(t *testing.T) {
	cityPath, _ := cityWithClaudeSettings(t)
	resolved := &ResolvedProvider{
		Name:            "claude-account",
		BuiltinAncestor: "claude",
		ResumeCommand:   "",
	}

	got, err := BuildProviderResumeCommand(cityPath, resolved, nil)
	if err != nil {
		t.Fatalf("BuildProviderResumeCommand: %v", err)
	}
	if strings.TrimSpace(got) != "" {
		t.Fatalf("empty resume_command gained content: %q", got)
	}
}
