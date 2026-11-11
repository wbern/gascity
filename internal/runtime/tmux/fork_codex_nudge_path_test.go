package tmux

import "testing"

// skipUpstreamCodexNudgePath skips a test that pins upstream's Codex submit
// turn guard (rollout verdicts, ErrSessionBusy deferral, held submits).
//
// T6 (upstream-intake, PROVISIONAL, William's decision): the wbern fork has its
// own Codex nudge path in nudgeSession (composer ownership before the paste,
// queue-action staging, ErrNudgeInputNotReady with nothing injected, submitCodexDraft),
// and it is the path production runs. Upstream's #8100-series turn guard is a
// competing design for the same delivery. Merging the two is an owner-level
// choice, so the fork's path is kept and upstream's tests of the other design
// are skipped here, by name, until that choice is made.
func skipUpstreamCodexNudgePath(t *testing.T) {
	t.Helper()
	t.Skip("pins upstream's Codex submit turn guard; the fork keeps its own Codex nudge path (T6 PROVISIONAL)")
}
