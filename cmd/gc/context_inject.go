package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/gastownhall/gascity/internal/modelwindow"
	"github.com/gastownhall/gascity/internal/worker/transcript"
)

// Context-usage injection — the context-pressure sibling of clock_inject.go.
//
// Gas City has canonical handoff machinery (`gc handoff`, the PreCompact
// auto-handoff, deployment handoff skills) but agents have no signal for WHEN
// to trigger it: a session cannot see its own context usage (the provider
// footer is rendered for humans only), so unmonitored agents run into context
// compaction by default — losing the deliberate wrap-up (durable notes, bead
// updates, clean seams) the handoff machinery exists to provide.
//
// This reads the provider hook input (UserPromptSubmit JSON on stdin carries
// transcript_path), computes the session's current context footprint from the
// last usage entry in the transcript, and injects ONE line of guidance —
// folded into the same single provider payload as the clock (see
// cmd_nudge.go), so JSON hook formats stay one valid document.
//
// TWO TRANSCRIPT DIALECTS. Claude records usage as message.usage; Codex records
// it as event_msg entries of type token_count. Each reader structurally
// requires fields the other never emits, so no transcript can be misread as the
// wrong dialect. Every reader is therefore always attempted, and the hook's
// --hook-format only decides the ORDER (see transcriptContextReaders for why it
// is a hint and not a gate). Codex usage is reached through
// internal/worker/transcript, the worker-owned seam, because cmd/gc may not
// import internal/sessionlog.
//
// WINDOW RESOLUTION IS PROVIDER-FIRST for Codex. Codex reports
// model_context_window per invocation; that beats any model-string lookup
// because the model string sits in a turn_context entry that often falls
// outside the tail read window (measured empty on ~1 in 5 real rollouts). A
// model-only resolver would then floor to the conservative default and fire
// advisories well below the true threshold — measured on a real 132383-token
// rollout: 51% against the reported 258400 window, but 66% (a false urgent)
// against a floored 200000.
//
// THRESHOLD-GATED BY DESIGN — not an always-on countdown. Model-provider
// guidance (Anthropic, Claude Fable 5 migration notes) documents "context
// anxiety": a continuously visible remaining-context count induces premature
// wrap-up and unprompted session-splitting. Below the advisory threshold this
// injects NOTHING. Above it, the message is actionable ("steer toward a clean
// handoff point", "run your handoff process now") and explicitly tells the
// agent NOT to panic-stop at the advisory tier.
//
//	< advisory (default 60%)  : silent
//	advisory..urgent (60–80%) : plan toward a clean handoff point
//	> urgent (default 80%)    : trigger the canonical handoff now
//
// Knobs: GC_INJECT_CONTEXT=0|false|off disables; GC_CONTEXT_ADVISORY_PCT and
// GC_CONTEXT_URGENT_PCT override the thresholds; GC_CONTEXT_WINDOW_TOKENS
// overrides the context-window size when model-string detection is wrong.
// Fail-safe: any parse/read problem returns "" — never blocks a prompt.

// hookStdinInput is the subset of the provider hook JSON we need. Claude
// always supplies transcript_path; Codex types it nullable but always supplies
// session_id and cwd, from which the rollout is discoverable.
type hookStdinInput struct {
	TranscriptPath string `json:"transcript_path"`
	SessionID      string `json:"session_id"`
	Cwd            string `json:"cwd"`
}

// transcriptUsage is the usage block shape inside provider transcript entries.
type transcriptUsage struct {
	InputTokens              int `json:"input_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
}

// transcriptContext is one dialect's answer for a session's live context size.
type transcriptContext struct {
	// Tokens is the prompt-side footprint of the newest usage entry.
	Tokens int
	// ProviderWindow is the context window the provider reported for that
	// entry, or 0 when the dialect does not report one.
	ProviderWindow int
	// Models lists the model strings seen, for model-table window fallback.
	Models []string
}

// transcriptContextReader extracts the live context size from one transcript
// dialect. ok is false when the file is not in that dialect, or on any read or
// parse failure — the caller cannot distinguish the two, and does not need to:
// both mean "this reader has no answer".
type transcriptContextReader func(path string) (transcriptContext, bool)

// readClaudeTranscriptContext reads the Claude dialect (message.usage entries).
func readClaudeTranscriptContext(path string) (transcriptContext, bool) {
	tokens, models, ok := lastTranscriptUsage(path)
	if !ok {
		return transcriptContext{}, false
	}
	// Claude does not report a per-invocation context window; the window comes
	// from the model table.
	return transcriptContext{Tokens: tokens, Models: models}, true
}

// readCodexTranscriptContext reads the Codex dialect (event_msg token_count
// entries) through the worker-owned seam.
func readCodexTranscriptContext(path string) (transcriptContext, bool) {
	c, ok := transcript.CodexTailContextFor(path)
	if !ok {
		return transcriptContext{}, false
	}
	return transcriptContext{Tokens: c.Tokens, ProviderWindow: c.ProviderWindowTokens, Models: c.Models}, true
}

// transcriptContextReaders orders the dialect readers for a hook invocation.
//
// EVERY reader is always attempted; hookFormat only decides which goes first.
// That is deliberate, and the asymmetry of the signal is why: gc installs the
// UserPromptSubmit hook with an explicit --hook-format for codex, gemini and
// antigravity, but Claude's hook passes none. An empty hookFormat therefore
// means "not one of the families that announce themselves" — NOT "Claude". So
// hookFormat is sound as a hint and unsound as a gate.
//
// Gating on it would also fail in the worst possible direction. A provider
// whose hook is installed without the flag would be routed to the wrong reader
// and the injector would emit nothing — and silence is this feature's fail-safe
// state, indistinguishable from "below threshold". That is exactly how Codex
// went without a single advisory across 346 rollouts before anyone noticed.
// Falling through costs one wasted tail scan (measured 1.9ms on an 18MB
// rollout, against ~650ms already spent resolving the session in the same hook
// invocation); guessing wrong costs the whole feature, silently.
//
// Trying both is safe because the dialects are disjoint by construction, not by
// luck: each reader structurally requires fields the other never emits
// (message.usage vs event_msg/token_count/info). Measured across 346 Codex and
// 64 Claude transcripts, neither reader ever matched the other's file.
func transcriptContextReaders(hookFormat string) []transcriptContextReader {
	if strings.ToLower(strings.TrimSpace(hookFormat)) == hookOutputFormatCodex {
		return []transcriptContextReader{readCodexTranscriptContext, readClaudeTranscriptContext}
	}
	return []transcriptContextReader{readClaudeTranscriptContext, readCodexTranscriptContext}
}

// contextInjectLine returns the context-usage guidance line for the session
// whose hook input JSON is in hookInput, or "" when disabled, below the
// advisory threshold, or on any error (fail-safe silent).
//
// hookFormat is the provider hook-output format for this invocation; it only
// orders the dialect readers (see transcriptContextReaders) and never gates
// them, so an empty or unexpected value costs a wasted scan rather than an
// answer.
func contextInjectLine(hookInput []byte, hookFormat string) string {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("GC_INJECT_CONTEXT"))) {
	case "0", "false", "off":
		return ""
	}
	var in hookStdinInput
	if err := json.Unmarshal(hookInput, &in); err != nil {
		return ""
	}
	path := transcriptPathForHook(in)
	if path == "" {
		return ""
	}
	for _, read := range transcriptContextReaders(hookFormat) {
		if c, ok := read(path); ok {
			return contextUsageMessage(c.Tokens, contextWindowTokens(c.Models, c.ProviderWindow))
		}
	}
	return ""
}

// transcriptPathForHook resolves the transcript to read for a hook payload.
//
// The payload's transcript_path is authoritative when present. The fallback
// exists for Codex alone because Codex types that field nullable while always
// supplying session_id and cwd, and its rollout filename embeds the session id;
// Claude always supplies a path and never reaches the fallback. Discovery is
// not a guess: it requires the rollout's own session_meta cwd to match, so a
// rollout belonging to another working directory is refused.
//
// Returns "" when nothing is resolvable, which the caller treats as silence.
func transcriptPathForHook(in hookStdinInput) string {
	if p := strings.TrimSpace(in.TranscriptPath); p != "" {
		return p
	}
	sessionID := strings.TrimSpace(in.SessionID)
	cwd := strings.TrimSpace(in.Cwd)
	if sessionID == "" || cwd == "" {
		return ""
	}
	return transcript.DiscoverCodexPathByID(nil, cwd, sessionID)
}

// lastTranscriptUsage reads the tail of a provider transcript (JSONL) and
// returns the context footprint of the most recent usage entry (prompt-side
// input tokens + cache reads + cache writes ≈ current context size) plus every
// non-empty model string seen — the window is the MAX over those (see
// contextWindowTokens), so a smaller-window sidecar/compaction call logged in
// the same transcript can't shrink the main-loop session's window.
func lastTranscriptUsage(path string) (tokens int, models []string, ok bool) {
	const tailBytes = 2 << 20 // last 2MiB is ample for the newest entries
	f, err := os.Open(path)   //nolint:gosec // path comes from the provider hook input
	if err != nil {
		return 0, nil, false
	}
	defer f.Close() //nolint:errcheck // read-only
	if st, err := f.Stat(); err == nil && st.Size() > tailBytes {
		if _, err := f.Seek(st.Size()-tailBytes, io.SeekStart); err != nil {
			return 0, nil, false
		}
	}
	data, err := io.ReadAll(io.LimitReader(f, tailBytes))
	if err != nil {
		return 0, nil, false
	}
	for _, line := range strings.Split(string(data), "\n") {
		if !strings.Contains(line, `"usage"`) {
			continue
		}
		var entry struct {
			Message struct {
				Model string           `json:"model"`
				Usage *transcriptUsage `json:"usage"`
			} `json:"message"`
		}
		if err := json.Unmarshal([]byte(line), &entry); err != nil || entry.Message.Usage == nil {
			continue
		}
		u := entry.Message.Usage
		if u.InputTokens == 0 && u.CacheReadInputTokens == 0 && u.CacheCreationInputTokens == 0 {
			continue
		}
		// Tokens: the LAST qualifying entry is the live context size (after a
		// compaction the newest entry reads low again).
		tokens = u.InputTokens + u.CacheReadInputTokens + u.CacheCreationInputTokens
		if m := entry.Message.Model; m != "" {
			models = append(models, m)
		}
		ok = true
	}
	return tokens, models, ok
}

// contextWindowTokens resolves the session's context window as the MAX window
// of any model it ran (they share one context), so a smaller-window sidecar or
// compaction call (e.g. a 200k-window Haiku entry inside a 1M Fable session)
// can't flip the session to the 200k default and fire the urgent tier at ~20%
// of real usage. Per-model windows come from the shared modelwindow package so
// this agrees with the API/session-log path; an unrecognized model (window 0)
// floors to the conservative default. GC_CONTEXT_WINDOW_TOKENS overrides —
// gc-managed deployments that know the launch model should pin it for
// determinism.
//
// providerWindow is the window the provider reported for the invocation, or 0
// when it reported none (always 0 on the Claude path, which has no such field).
// It outranks the model table because a model string can be absent from the
// read window entirely, and flooring to the conservative default in that case
// understates the window and fires advisories far below the real threshold.
// The env override still wins over everything: it is the operator's documented
// last word when detection is wrong.
func contextWindowTokens(models []string, providerWindow int) int {
	if v := strings.TrimSpace(os.Getenv("GC_CONTEXT_WINDOW_TOKENS")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	if providerWindow > 0 {
		return providerWindow
	}
	best := 0
	for _, m := range models {
		if w := modelwindow.Window(m); w > best {
			best = w
		}
	}
	if best == 0 {
		return modelwindow.Default
	}
	return best
}

// contextUsageMessage renders the guidance line for tokens used of window, or
// "" below the advisory threshold.
func contextUsageMessage(tokens, window int) string {
	if window <= 0 {
		return ""
	}
	advisory := thresholdPct("GC_CONTEXT_ADVISORY_PCT", 60)
	urgent := thresholdPct("GC_CONTEXT_URGENT_PCT", 80)
	pct := 100 * float64(tokens) / float64(window)
	k := func(n int) string { return fmt.Sprintf("%dk", (n+500)/1000) }
	switch {
	case pct < float64(advisory):
		return ""
	case pct <= float64(urgent):
		return fmt.Sprintf(
			"Context usage: %s/%s (~%.0f%%). Approaching the recycle zone. Steer toward a clean seam: finish in-flight work, don't open new long-horizon tasks, and keep durable notes/work-items current so a handoff is cheap. Plan to run `gc handoff` and recycle before this climbs into the urgent band — a fresh session from durable notes outperforms riding lossy compaction.\n",
			k(tokens), k(window), pct)
	default:
		return fmt.Sprintf(
			"Context usage: %s/%s (~%.0f%%) — HIGH. Recycle this session now: reach a clean seam, keep durable notes + work-item updates + memory current, then run `gc handoff \"<where you were + next step>\"`. That writes your continuation note and recycles you fresh from it — or, if you are an attended session the controller cannot restart, it hands off and you reattach for the fresh session. Prefer this over riding lossy compaction; repeated compaction degrades awareness. Do this once you are at a seam; do NOT abandon work mid-step. (If an operator has told you to stay up, honor that and just hold at a clean seam instead of recycling.)\n",
			k(tokens), k(window), pct)
	}
}

func thresholdPct(env string, def int) int {
	if v := strings.TrimSpace(os.Getenv(env)); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 100 {
			return n
		}
	}
	return def
}

// readHookStdin returns the provider hook input JSON from stdin when stdin is
// a pipe (the hook invocation shape). Interactive/manual invocations (stdin is
// a terminal) return nil so the command never blocks waiting for input.
func readHookStdin() []byte {
	st, err := os.Stdin.Stat()
	if err != nil || st.Mode()&os.ModeCharDevice != 0 {
		return nil
	}
	data, err := io.ReadAll(io.LimitReader(os.Stdin, 1<<20))
	if err != nil {
		return nil
	}
	return data
}
