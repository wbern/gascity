package tmux

import (
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
)

// skipUpstreamRespawnList skips an upstream test that pins respawn-pane -k as
// one tmux list. This fork replaces the agent's window (replacePaneWindow)
// instead, so a respawn is a metadata read, a guarded new-window and a follow-up
// arm (T8 PROVISIONAL: port these tests to the window swap).
func skipUpstreamRespawnList(t *testing.T) {
	t.Helper()
	t.Skip("fork replaces the agent window instead of respawn-pane -k; upstream's single respawn list does not apply")
}

// An armed respawn wraps the command in the relay, swaps the window, and then
// arms the replacement pane (a fresh pane, so nothing needs disarming).
func TestForkRespawnWindowSwapArmsTheReplacementPane(t *testing.T) {
	tm, rec, warnings, _ := captureTestTmux(t)
	// Window metadata, the session's recorded owned scope (none), the guarded
	// new-window, then the replacement pane id.
	rec.outs = []string{"@1\tmain\t1\t/cur\tgc-swap\t0", "-" + ownedScopeEnv, "", "%6"}
	arm := runtime.ProgramStatusArm{Capture: true, App: "claude-code", Relay: true}
	if err := tm.respawnPaneWithWorkDir("%5", "/w", "claude --x", arm); err != nil {
		t.Fatalf("respawnPaneWithWorkDir: %v", err)
	}
	var swapped, armed bool
	for i, c := range rec.calls {
		joined := strings.Join(c, " ")
		switch {
		case strings.Contains(joined, "new-window") && strings.Contains(joined, testCompanion+" relay"):
			swapped = true
		case strings.Contains(joined, "pipe-pane") && strings.Contains(joined, "-t =gc-swap:0"):
			if !swapped {
				t.Fatalf("armed before the window swap: call %d of %q", i, rec.calls)
			}
			armed = true
		case strings.Contains(joined, "respawn-pane"):
			t.Fatalf("this fork never sends respawn-pane: %q", c)
		}
	}
	if !swapped || !armed {
		t.Fatalf("swapped=%v armed=%v; calls %q", swapped, armed, rec.calls)
	}
	if warnings.Len() != 0 {
		t.Fatalf("unexpected capture warning: %q", warnings.String())
	}
}
