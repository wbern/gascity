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
// it as event_msg entries of type token_count. The two share no usage entries,
// so the injector simply tries Claude first and Codex second — no provider
// detection, and no way for one family's transcript to be misread as the
// other's. Codex usage is reached through internal/worker/transcript, the
// worker-owned seam, because cmd/gc may not import internal/sessionlog.
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

// contextInjectLine returns the context-usage guidance line for the session
// whose hook input JSON is in hookInput, or "" when disabled, below the
// advisory threshold, or on any error (fail-safe silent).
func contextInjectLine(hookInput []byte) string {
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
	if tokens, models, ok := lastTranscriptUsage(path); ok {
		return contextUsageMessage(tokens, contextWindowTokens(models, 0))
	}
	// Codex reports context through event_msg token_count entries instead of
	// message.usage, so a Codex rollout yields nothing above. The two dialects
	// share no usage entries, so trying both needs no provider detection and
	// cannot change any Claude verdict.
	if codex, ok := transcript.CodexTailContextFor(path); ok {
		return contextUsageMessage(codex.Tokens, contextWindowTokens(codex.Models, codex.ProviderWindowTokens))
	}
	return ""
}

// transcriptPathForHook resolves the transcript to read for a hook payload,
// falling back to Codex session-id discovery when the payload carries no
// transcript path. Returns "" when nothing is resolvable.
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
