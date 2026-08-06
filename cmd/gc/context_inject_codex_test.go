package main

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// Codex reports context through event_msg token_count payloads rather than the
// Claude message.usage shape, so these tests pin the second transcript dialect
// the advisory has to understand. The two shapes are disjoint: a Codex rollout
// carries no message.usage entry and a Claude transcript carries no
// event_msg/token_count entry, which is why every reader can be attempted
// against every transcript with no risk of a cross-family misread.
//
// Except where a case is specifically about hookFormat, these tests pass an
// EMPTY hookFormat on purpose. That is the pessimistic ordering for a Codex
// fixture — Claude is tried first and misses — so it exercises the fallthrough
// rather than the hinted fast path. It is also what a provider whose hook omits
// --hook-format actually sends.

// codexTurnContextLine renders the turn_context entry that names the model for
// the token_count entries following it. Codex places this entry once per turn,
// which is why it frequently falls outside the tail read window.
func codexTurnContextLine() string {
	return `{"timestamp":"2026-08-06T07:00:00Z","type":"turn_context","payload":{"model":"gpt-5-codex"}}`
}

// codexTokenCountLine renders one event_msg token_count entry. Codex's
// input_tokens already includes cached_input_tokens, so input is the whole
// prompt-side footprint for the invocation. window <= 0 omits
// model_context_window, reproducing the rollouts where the provider does not
// report it.
func codexTokenCountLine(total, input, cached, window int) string {
	windowField := "null"
	if window > 0 {
		windowField = fmt.Sprintf("%d", window)
	}
	return fmt.Sprintf(`{"timestamp":"2026-08-06T07:00:01Z","type":"event_msg","payload":{"type":"token_count","info":{`+
		`"total_token_usage":{"input_tokens":%d,"cached_input_tokens":%d,"output_tokens":0,"reasoning_output_tokens":0,"total_tokens":%d},`+
		`"last_token_usage":{"input_tokens":%d,"cached_input_tokens":%d,"output_tokens":0,"reasoning_output_tokens":0,"total_tokens":%d},`+
		`"model_context_window":%s}}}`,
		input, cached, total, input, cached, total, windowField)
}

func writeCodexRollout(t *testing.T, lines ...string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "rollout-2026-08-06T07-00-00-0199aaaa-bbbb-cccc-dddd-eeeeffff0000.jsonl")
	if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatalf("write codex rollout: %v", err)
	}
	return p
}

func TestContextInjectCodexUrgentBand(t *testing.T) {
	t.Setenv("GC_INJECT_CONTEXT", "")
	t.Setenv("GC_CONTEXT_WINDOW_TOKENS", "")
	// 241537 of a provider-reported 258400 window = 93.5% — the real peak
	// measured on rollout 019fcb62 on 2026-08-04, which produced no advisory.
	p := writeCodexRollout(t,
		codexTurnContextLine(),
		codexTokenCountLine(250_000, 241_537, 120_000, 258_400),
	)
	got := contextInjectLine(hookInputFor(p), "")
	if got == "" {
		t.Fatal("codex rollout at 93.5% must produce an urgent advisory, got silence")
	}
	if !strings.Contains(got, "HIGH") || !strings.Contains(got, "gc handoff") {
		t.Errorf("urgent line must direct to gc handoff: %q", got)
	}
	if !strings.Contains(got, "~93%") {
		t.Errorf("percentage must use the provider-reported window (241537/258400 = 93%%): %q", got)
	}
}

func TestContextInjectCodexAdvisoryBand(t *testing.T) {
	t.Setenv("GC_INJECT_CONTEXT", "")
	t.Setenv("GC_CONTEXT_WINDOW_TOKENS", "")
	// 180880 of 258400 = 70% — advisory band.
	p := writeCodexRollout(t,
		codexTurnContextLine(),
		codexTokenCountLine(190_000, 180_880, 90_000, 258_400),
	)
	got := contextInjectLine(hookInputFor(p), "")
	if !strings.Contains(got, "~70%") || !strings.Contains(got, "clean seam") {
		t.Errorf("advisory band line wrong: %q", got)
	}
	if strings.Contains(got, "HIGH") {
		t.Errorf("advisory band must not be marked HIGH: %q", got)
	}
}

func TestContextInjectCodexSilentBelowAdvisory(t *testing.T) {
	t.Setenv("GC_INJECT_CONTEXT", "")
	t.Setenv("GC_CONTEXT_WINDOW_TOKENS", "")
	// 132383 of 258400 = 51.2% — below the advisory threshold, must stay silent.
	// This is the exact footprint measured on rollout 019fcb62; resolving the
	// window from the model string alone would floor it to 200k and fire at
	// 66.2%, the false positive that makes 875d29094 unsafe to port.
	p := writeCodexRollout(t,
		codexTurnContextLine(),
		codexTokenCountLine(140_000, 132_383, 60_000, 258_400),
	)
	if got := contextInjectLine(hookInputFor(p), ""); got != "" {
		t.Errorf("51.2%% of the provider-reported window must be silent, got %q", got)
	}
}

func TestContextInjectCodexProviderWindowBeatsModelTable(t *testing.T) {
	t.Setenv("GC_INJECT_CONTEXT", "")
	t.Setenv("GC_CONTEXT_WINDOW_TOKENS", "")
	// The model string is absent (turn_context fell outside the read window —
	// measured at 21.5% of real rollouts). The provider-reported window must
	// still be used rather than flooring to the conservative default.
	p := writeCodexRollout(t, codexTokenCountLine(140_000, 132_383, 60_000, 258_400))
	if got := contextInjectLine(hookInputFor(p), ""); got != "" {
		t.Errorf("provider window must be honored with no model string, got %q", got)
	}
}

func TestContextInjectCodexFallsBackToModelTableWithoutProviderWindow(t *testing.T) {
	t.Setenv("GC_INJECT_CONTEXT", "")
	t.Setenv("GC_CONTEXT_WINDOW_TOKENS", "")
	// model_context_window absent: fall through to the shared model table,
	// which maps codex/gpt-5 to 258000. 200000/258000 = 77.5% — advisory band.
	p := writeCodexRollout(t,
		codexTurnContextLine(),
		codexTokenCountLine(210_000, 200_000, 90_000, 0),
	)
	got := contextInjectLine(hookInputFor(p), "")
	if !strings.Contains(got, "~78%") {
		t.Errorf("model-table window (258000) should give ~78%%: %q", got)
	}
	if strings.Contains(got, "HIGH") {
		t.Errorf("77.5%% is the advisory band, not urgent: %q", got)
	}
}

func TestContextInjectCodexNoWindowAndNoModelFloorsToDefault(t *testing.T) {
	t.Setenv("GC_INJECT_CONTEXT", "")
	t.Setenv("GC_CONTEXT_WINDOW_TOKENS", "")
	// Neither a provider window nor a model string: the conservative default
	// (200k) is the only honest answer. 190000/200000 = 95% — urgent. This is
	// the one shape where over-reporting is possible, and it is deliberate:
	// with no window evidence at all, erring toward an early handoff is safer
	// than silence.
	p := writeCodexRollout(t, codexTokenCountLine(200_000, 190_000, 90_000, 0))
	if got := contextInjectLine(hookInputFor(p), ""); !strings.Contains(got, "HIGH") {
		t.Errorf("no window evidence should floor to the 200k default and fire: %q", got)
	}
}

func TestContextInjectCodexEnvWindowOverrideStillWins(t *testing.T) {
	t.Setenv("GC_INJECT_CONTEXT", "")
	// GC_CONTEXT_WINDOW_TOKENS is documented as the operator's last word when
	// detection is wrong, so it must outrank the provider-reported window.
	t.Setenv("GC_CONTEXT_WINDOW_TOKENS", "150000")
	p := writeCodexRollout(t,
		codexTurnContextLine(),
		codexTokenCountLine(140_000, 132_383, 60_000, 258_400),
	)
	got := contextInjectLine(hookInputFor(p), "")
	if !strings.Contains(got, "HIGH") {
		t.Errorf("132383/150000 = 88%% must be urgent under the env override: %q", got)
	}
}

func TestContextInjectCodexDisableEnvStillHonored(t *testing.T) {
	t.Setenv("GC_INJECT_CONTEXT", "off")
	p := writeCodexRollout(t,
		codexTurnContextLine(),
		codexTokenCountLine(250_000, 241_537, 120_000, 258_400),
	)
	if got := contextInjectLine(hookInputFor(p), ""); got != "" {
		t.Errorf("GC_INJECT_CONTEXT=off must silence codex too, got %q", got)
	}
}

func TestContextInjectCodexLastEntryWins(t *testing.T) {
	t.Setenv("GC_INJECT_CONTEXT", "")
	t.Setenv("GC_CONTEXT_WINDOW_TOKENS", "")
	// Post-compaction shape: a high entry followed by a low one. The live
	// context is the last entry, so this must be silent — same invariant the
	// Claude path pins in TestContextInjectLastUsageEntryWins.
	p := writeCodexRollout(t,
		codexTurnContextLine(),
		codexTokenCountLine(250_000, 241_537, 120_000, 258_400),
		codexTokenCountLine(30_000, 25_000, 10_000, 258_400),
	)
	if got := contextInjectLine(hookInputFor(p), ""); got != "" {
		t.Errorf("last codex entry (9.7%%) should win and be silent, got %q", got)
	}
}

func TestContextInjectClaudeTranscriptUnaffectedByCodexPath(t *testing.T) {
	t.Setenv("GC_INJECT_CONTEXT", "")
	t.Setenv("GC_CONTEXT_WINDOW_TOKENS", "")
	// Regression guard: adding the codex fallback must not change any Claude
	// verdict. 900k of 1M stays urgent.
	p := writeTranscript(t, usageLine("claude-opus-4-8[1m]", 50_000, 800_000, 50_000))
	if got := contextInjectLine(hookInputFor(p), ""); !strings.Contains(got, "HIGH") {
		t.Errorf("claude urgent verdict must be unchanged: %q", got)
	}
}

func TestContextInjectCodexDiscoversTranscriptWhenPathOmitted(t *testing.T) {
	t.Setenv("GC_INJECT_CONTEXT", "")
	t.Setenv("GC_CONTEXT_WINDOW_TOKENS", "")
	// Codex types transcript_path as nullable. A payload that omits it must
	// still be readable via session_id + cwd, or the injector goes blind for
	// that payload shape — the one residual risk on gcw-w0nl.
	home := t.TempDir()
	t.Setenv("HOME", home)
	workDir := filepath.Join(home, "work")
	sessionID := "019fcb62-1111-2222-3333-444455556666"
	dir := filepath.Join(home, ".codex", "sessions", "2026", "08", "06")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	body := strings.Join([]string{
		fmt.Sprintf(`{"timestamp":"2026-08-06T07:00:00Z","type":"session_meta","payload":{"id":%q,"timestamp":"2026-08-06T07:00:00Z","cwd":%q,"originator":"codex-tui","cli_version":"0.146.0","source":"cli","model_provider":"openai"}}`, sessionID, workDir),
		codexTurnContextLine(),
		codexTokenCountLine(250_000, 241_537, 120_000, 258_400),
	}, "\n") + "\n"
	if err := os.WriteFile(filepath.Join(dir, "rollout-2026-08-06T07-00-00-"+sessionID+".jsonl"), []byte(body), 0o600); err != nil {
		t.Fatalf("write rollout: %v", err)
	}

	payload := fmt.Sprintf(`{"transcript_path":null,"session_id":%q,"cwd":%q,"hook_event_name":"UserPromptSubmit"}`, sessionID, workDir)
	got := contextInjectLine([]byte(payload), "")
	if !strings.Contains(got, "HIGH") {
		t.Errorf("discovered codex rollout at 93.5%% must be urgent, got %q", got)
	}
}

func TestContextInjectNoPathAndNoSessionIDStaysSilent(t *testing.T) {
	t.Setenv("GC_INJECT_CONTEXT", "")
	if got := contextInjectLine([]byte(`{"hook_event_name":"UserPromptSubmit"}`), ""); got != "" {
		t.Errorf("nothing to resolve must stay silent, got %q", got)
	}
	if got := contextInjectLine([]byte(`{"transcript_path":null,"session_id":"abc"}`), ""); got != "" {
		t.Errorf("session id without cwd must stay silent, got %q", got)
	}
}

func TestContextInjectUnparseableTranscriptStaysSilent(t *testing.T) {
	t.Setenv("GC_INJECT_CONTEXT", "")
	// Neither dialect: fail-safe silence, never a panic or a bogus advisory.
	p := writeCodexRollout(t, `{"not":"a transcript"}`, `garbage`)
	if got := contextInjectLine(hookInputFor(p), ""); got != "" {
		t.Errorf("unparseable transcript must stay silent, got %q", got)
	}
}

// TestTranscriptContextReadersOrdering pins that hookFormat reorders the
// dialect readers without ever dropping one. The count assertion is the
// load-bearing half: a future change that turns the hint into a gate would
// shrink the slice, and that is the regression that would silently reintroduce
// the Codex blind spot for any provider whose hook omits --hook-format.
func TestTranscriptContextReadersOrdering(t *testing.T) {
	claude := reflect.ValueOf(readClaudeTranscriptContext).Pointer()
	codex := reflect.ValueOf(readCodexTranscriptContext).Pointer()

	for _, tc := range []struct {
		hookFormat string
		wantFirst  uintptr
		name       string
	}{
		{"", claude, "no hint falls back to claude-first"},
		{hookOutputFormatCodex, codex, "codex hint puts codex first"},
		{"CoDeX", codex, "hint match is case- and space-insensitive"},
		{"  codex  ", codex, "hint match trims whitespace"},
		{hookOutputFormatGemini, claude, "a non-codex hint does not reorder"},
		{"totally-unknown", claude, "an unrecognized hint does not reorder"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			readers := transcriptContextReaders(tc.hookFormat)
			if len(readers) != 2 {
				t.Fatalf("len(readers) = %d, want 2: every dialect must stay reachable, the hint only orders them", len(readers))
			}
			first := reflect.ValueOf(readers[0]).Pointer()
			if first != tc.wantFirst {
				t.Errorf("first reader is not the expected one for hookFormat %q", tc.hookFormat)
			}
			if reflect.ValueOf(readers[1]).Pointer() == first {
				t.Error("the two readers must be distinct")
			}
		})
	}
}

// TestContextInjectHookFormatNeverChangesTheVerdict is the safety property that
// justifies treating hookFormat as a hint: whatever it says — including a wrong
// or absent value — the emitted line must be identical, because both dialects
// are always attempted. Only the order, and therefore the wasted work, differs.
func TestContextInjectHookFormatNeverChangesTheVerdict(t *testing.T) {
	t.Setenv("GC_INJECT_CONTEXT", "")
	t.Setenv("GC_CONTEXT_WINDOW_TOKENS", "")

	codexUrgent := writeCodexRollout(t,
		codexTurnContextLine(),
		codexTokenCountLine(250_000, 241_537, 120_000, 258_400),
	)
	codexSilent := writeCodexRollout(t,
		codexTurnContextLine(),
		codexTokenCountLine(140_000, 132_383, 60_000, 258_400),
	)
	claudeUrgent := writeTranscript(t, usageLine("claude-opus-4-8[1m]", 50_000, 800_000, 50_000))
	claudeSilent := writeTranscript(t, usageLine("claude-fable-5", 1_000, 98_000, 1_000))

	for _, fixture := range []struct {
		name string
		path string
	}{
		{"codex urgent", codexUrgent},
		{"codex silent", codexSilent},
		{"claude urgent", claudeUrgent},
		{"claude silent", claudeSilent},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			// The empty hint is the baseline: it is what Claude's hook sends and
			// what any provider that forgets the flag sends.
			want := contextInjectLine(hookInputFor(fixture.path), "")
			for _, hint := range []string{hookOutputFormatCodex, hookOutputFormatGemini, hookOutputFormatAntigravity, "nonsense"} {
				if got := contextInjectLine(hookInputFor(fixture.path), hint); got != want {
					t.Errorf("hookFormat %q changed the verdict\n got: %q\nwant: %q", hint, got, want)
				}
			}
		})
	}
}
