package transcript

import (
	"strings"

	"github.com/gastownhall/gascity/internal/sessionlog"
)

// CodexTailContext is the live context footprint at the tail of a Codex
// rollout transcript, in the shape a context-pressure consumer needs.
//
// It exists so callers outside the worker boundary (notably the cmd/gc
// UserPromptSubmit hook) can reach Codex usage without importing
// internal/sessionlog directly, which the worker-boundary guard forbids.
type CodexTailContext struct {
	// Tokens is the prompt-side context footprint of the newest usage entry:
	// non-cached input plus cache reads plus cache writes. Codex reports
	// input_tokens inclusive of cached_input_tokens, and the extractor splits
	// them, so re-summing here yields the original prompt-side total. This is
	// the same formula the worker's session-log adapter uses for
	// ContextUsedTokens, so both agree on what "context used" means.
	Tokens int
	// ProviderWindowTokens is the context window Codex itself reported for the
	// invocation (model_context_window), or 0 when the rollout did not carry
	// one. It is authoritative over any model-string lookup: across sampled
	// rollouts the provider reports it essentially always, while the model
	// string is missing often enough that model-only resolution mis-sizes the
	// window and fires false advisories.
	ProviderWindowTokens int
	// Models lists every non-empty model string seen in the read window, for
	// callers that need a fallback window when the provider reported none.
	Models []string
}

// CodexTailContextFor reads the tail of a Codex rollout transcript at path and
// returns its live context footprint.
//
// ok is false when the file cannot be read or carries no Codex usage entry —
// including when path is in fact a Claude transcript, whose shape shares no
// usage entries with the Codex dialect. Callers are expected to treat a false
// ok as "no signal" and stay silent rather than guessing.
func CodexTailContextFor(path string) (CodexTailContext, bool) {
	usages, err := sessionlog.ExtractCodexTailUsage(path)
	if err != nil || len(usages) == 0 {
		return CodexTailContext{}, false
	}
	out := CodexTailContext{}
	for _, u := range usages {
		if m := strings.TrimSpace(u.Model); m != "" {
			out.Models = append(out.Models, m)
		}
	}
	// The newest entry is the live context size: after a compaction the
	// following entry reads low again, exactly as on the Claude path.
	last := usages[len(usages)-1]
	out.Tokens = last.InputTokens + last.CacheReadTokens + last.CacheCreationTokens
	out.ProviderWindowTokens = last.ContextWindowTokens
	return out, true
}

// DiscoverCodexPathByID resolves the Codex rollout transcript for a session id
// under workDir, or "" when no unambiguous match exists.
//
// This is the fallback for hook payloads that omit a transcript path: Codex
// types transcript_path as nullable, so a consumer that only reads that field
// goes blind for any payload shape that leaves it null. The session id and cwd
// are always present, and the rollout filename embeds the session id, so the
// transcript remains reachable.
func DiscoverCodexPathByID(searchPaths []string, workDir, sessionID string) string {
	return sessionlog.FindCodexSessionFileByIDNoWindow(searchPaths, workDir, sessionID)
}
