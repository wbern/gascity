package transcript

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func codexMetaLine(cwd string) string {
	return fmt.Sprintf(`{"timestamp":"2026-08-06T07:00:00Z","type":"session_meta","payload":{"id":"019d9845-4273-7ee3-a7d7-15b71ec6f096","timestamp":"2026-08-06T07:00:00Z","cwd":%q,"originator":"codex-tui","cli_version":"0.146.0","source":"cli","model_provider":"openai"}}`, cwd)
}

func codexTurnContext() string {
	return `{"timestamp":"2026-08-06T07:00:01Z","type":"turn_context","payload":{"model":"gpt-5-codex"}}`
}

func codexTokenCount(total, input, cached, window int) string {
	windowField := "null"
	if window > 0 {
		windowField = fmt.Sprintf("%d", window)
	}
	return fmt.Sprintf(`{"timestamp":"2026-08-06T07:00:02Z","type":"event_msg","payload":{"type":"token_count","info":{`+
		`"total_token_usage":{"input_tokens":%d,"cached_input_tokens":%d,"output_tokens":0,"reasoning_output_tokens":0,"total_tokens":%d},`+
		`"last_token_usage":{"input_tokens":%d,"cached_input_tokens":%d,"output_tokens":0,"reasoning_output_tokens":0,"total_tokens":%d},`+
		`"model_context_window":%s}}}`,
		input, cached, total, input, cached, total, windowField)
}

func writeRollout(t *testing.T, dir, name string, lines ...string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatalf("write rollout: %v", err)
	}
	return p
}

func TestCodexTailContextForReadsProviderWindow(t *testing.T) {
	// The real footprint/window pair measured on rollout 019fcb62.
	p := writeRollout(t, t.TempDir(), "rollout.jsonl",
		codexMetaLine("/tmp/work"),
		codexTurnContext(),
		codexTokenCount(140_000, 132_383, 60_000, 258_400),
	)
	got, ok := CodexTailContextFor(p)
	if !ok {
		t.Fatal("expected codex usage to parse")
	}
	if got.Tokens != 132_383 {
		t.Errorf("Tokens = %d, want 132383", got.Tokens)
	}
	if got.ProviderWindowTokens != 258_400 {
		t.Errorf("ProviderWindowTokens = %d, want 258400", got.ProviderWindowTokens)
	}
	if len(got.Models) == 0 || got.Models[0] != "gpt-5-codex" {
		t.Errorf("Models = %v, want [gpt-5-codex]", got.Models)
	}
}

func TestCodexTailContextForNewestEntryWins(t *testing.T) {
	// Post-compaction shape: the live context is the newest entry, not the peak.
	p := writeRollout(t, t.TempDir(), "rollout.jsonl",
		codexMetaLine("/tmp/work"),
		codexTurnContext(),
		codexTokenCount(250_000, 241_537, 120_000, 258_400),
		codexTokenCount(30_000, 25_000, 10_000, 258_400),
	)
	got, ok := CodexTailContextFor(p)
	if !ok {
		t.Fatal("expected codex usage to parse")
	}
	if got.Tokens != 25_000 {
		t.Errorf("Tokens = %d, want the newest entry 25000", got.Tokens)
	}
}

func TestCodexTailContextForOmittedProviderWindow(t *testing.T) {
	// model_context_window is null: report 0 so the caller falls back to the
	// model table rather than inventing a window.
	p := writeRollout(t, t.TempDir(), "rollout.jsonl",
		codexMetaLine("/tmp/work"),
		codexTurnContext(),
		codexTokenCount(140_000, 132_383, 60_000, 0),
	)
	got, ok := CodexTailContextFor(p)
	if !ok {
		t.Fatal("expected codex usage to parse")
	}
	if got.ProviderWindowTokens != 0 {
		t.Errorf("ProviderWindowTokens = %d, want 0 when the provider reported none", got.ProviderWindowTokens)
	}
}

func TestCodexTailContextForRejectsClaudeTranscript(t *testing.T) {
	// The two dialects are disjoint, which is what lets the injector try both.
	p := writeRollout(t, t.TempDir(), "transcript.jsonl",
		`{"type":"assistant","message":{"model":"claude-fable-5","usage":{"input_tokens":10,"cache_read_input_tokens":20,"cache_creation_input_tokens":30}}}`,
	)
	if _, ok := CodexTailContextFor(p); ok {
		t.Error("a Claude transcript must not parse as Codex usage")
	}
}

func TestCodexTailContextForMissingFile(t *testing.T) {
	if _, ok := CodexTailContextFor(filepath.Join(t.TempDir(), "absent.jsonl")); ok {
		t.Error("missing file must report not-ok, never a bogus footprint")
	}
}

func TestDiscoverCodexPathByIDFindsRolloutForSession(t *testing.T) {
	root := t.TempDir()
	workDir := filepath.Join(root, "work")
	sessionID := "019fcb62-1111-2222-3333-444455556666"
	sessions := filepath.Join(root, "sessions")
	want := writeRollout(t, filepath.Join(sessions, "2026", "08", "06"),
		"rollout-2026-08-06T07-00-00-"+sessionID+".jsonl",
		codexMetaLine(workDir),
		codexTurnContext(),
		codexTokenCount(140_000, 132_383, 60_000, 258_400),
	)
	got := DiscoverCodexPathByID([]string{sessions}, workDir, sessionID)
	if got != want {
		t.Errorf("DiscoverCodexPathByID = %q, want %q", got, want)
	}
}

func TestDiscoverCodexPathByIDRejectsForeignWorkdir(t *testing.T) {
	// A rollout for the same session id but a different cwd must not match:
	// the fallback has to be at least as safe as an explicit transcript_path.
	root := t.TempDir()
	sessionID := "019fcb62-1111-2222-3333-444455556666"
	sessions := filepath.Join(root, "sessions")
	writeRollout(t, filepath.Join(sessions, "2026", "08", "06"),
		"rollout-2026-08-06T07-00-00-"+sessionID+".jsonl",
		codexMetaLine(filepath.Join(root, "somewhere-else")),
		codexTokenCount(140_000, 132_383, 60_000, 258_400),
	)
	if got := DiscoverCodexPathByID([]string{sessions}, filepath.Join(root, "work"), sessionID); got != "" {
		t.Errorf("DiscoverCodexPathByID = %q, want empty on cwd mismatch", got)
	}
}

func TestDiscoverCodexPathByIDRejectsTraversal(t *testing.T) {
	for _, id := range []string{"../escape", "a/b", `a\b`, ""} {
		if got := DiscoverCodexPathByID(nil, t.TempDir(), id); got != "" {
			t.Errorf("DiscoverCodexPathByID(%q) = %q, want empty", id, got)
		}
	}
}
