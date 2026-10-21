//go:build integration

package tmux

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestNudgeRecoversStagedDraftSwallowedByPasteIngest reproduces the staged-draft
// stall against a REAL, isolated tmux server (its own -L socket, killed on
// cleanup). A codex-shaped fake TUI collapses the pasted nudge into
// "[Pasted Content N chars]" and eats the first few submits, the way a loaded
// codex TUI still ingesting a large paste does. Before the fix every submit in
// the ~2.4s confirm window was eaten, NudgeSession gave up unconfirmed, and the
// draft stayed staged forever. The fake TUI logs every submit it takes, so each
// case can assert both the verdict and that the message was taken exactly once.
func TestNudgeRecoversStagedDraftSwallowedByPasteIngest(t *testing.T) {
	if os.Getenv("GC_TMUX_INTEGRATION") != "1" {
		t.Skip("set GC_TMUX_INTEGRATION=1 to run this real-tmux reproduction (spins a throwaway tmux server)")
	}
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 required for the fake TUI")
	}
	script, err := filepath.Abs(filepath.Join("testdata", "nudge_faketui_codex.py"))
	if err != nil {
		t.Fatalf("resolving fake TUI: %v", err)
	}

	start := func(t *testing.T, socket string, swallow int, busyDelay time.Duration, alreadyBusy ...bool) (*Tmux, string) {
		t.Helper()
		logPath := filepath.Join(t.TempDir(), "tui.log")
		tm := NewTmuxWithConfig(Config{
			SocketName:        socket,
			NudgeReadyTimeout: 10 * time.Second,
			NudgeLockTimeout:  30 * time.Second,
		})
		_, _ = tm.run("kill-server")
		t.Cleanup(func() { _, _ = tm.run("kill-server") })
		cmd := fmt.Sprintf("python3 %s %s %d %.3f 30", shellQuote(script), shellQuote(logPath), swallow, busyDelay.Seconds())
		if len(alreadyBusy) > 0 && alreadyBusy[0] {
			cmd += " busy"
		}
		if _, err := tm.run("new-session", "-d", "-s", "draft", "-x", "120", "-y", "30", cmd); err != nil {
			t.Skipf("cannot create tmux session (tmux unavailable?): %v", err)
		}
		// GC_PROVIDER=codex routes NudgeSession through the verified submit
		// path, codex's Escape+Enter submit sequence, and its draft marker.
		if err := tm.SetEnvironment("draft", "GC_PROVIDER", "codex"); err != nil {
			t.Fatalf("SetEnvironment: %v", err)
		}
		// Let the TUI paint its first frame before any keystroke.
		poll := time.NewTicker(100 * time.Millisecond)
		defer poll.Stop()
		deadline := time.After(5 * time.Second)
		for {
			if out, err := tm.CapturePane("draft", 40); err == nil && strings.Contains(out, "fake-codex-tui") {
				return tm, logPath
			}
			select {
			case <-poll.C:
			case <-deadline:
				t.Fatal("fake TUI never painted")
				return nil, ""
			}
		}
	}

	count := func(t *testing.T, logPath, event string) int {
		t.Helper()
		raw, err := os.ReadFile(logPath)
		if err != nil {
			t.Fatalf("reading TUI log: %v", err)
		}
		return strings.Count(string(raw), "\t"+event+"\t")
	}

	largeNudge := "gc-staged-draft-probe " + strings.Repeat("work the claimed bead ", 60)

	t.Run("submits eaten past the confirm window: recovered, submitted once", func(t *testing.T) {
		const swallow = submitEnterMaxSends + 2
		tm, logPath := start(t, "gcstageddraftbusy", swallow, 100*time.Millisecond)

		if err := tm.NudgeSession("draft", largeNudge); err != nil {
			t.Fatalf("NudgeSession = %v, want nil (recovery should land the staged draft)", err)
		}
		if got := count(t, logPath, "SUBMIT"); got != 1 {
			t.Fatalf("TUI submits = %d, want exactly 1", got)
		}
		if got := count(t, logPath, "SWALLOWED"); got != swallow {
			t.Fatalf("TUI swallowed submits = %d, want %d", got, swallow)
		}
	})

	t.Run("draft clears but busy never renders: delivered-unobserved, submitted once", func(t *testing.T) {
		tm, logPath := start(t, "gcstageddraftclear", submitEnterMaxSends+1, 60*time.Second)

		began := time.Now()
		err := tm.NudgeSession("draft", largeNudge)
		t.Logf("NudgeSession returned after %v: %v", time.Since(began), err)
		if err != nil {
			t.Fatalf("err = %v, want draft-consumption proof even without busy rendering", err)
		}
		if got := count(t, logPath, "SUBMIT"); got != 1 {
			t.Fatalf("TUI submits = %d, want exactly 1", got)
		}
	})

	t.Run("raw draft also recovers swallowed submits", func(t *testing.T) {
		tm, logPath := start(t, "gcstageddraftraw", submitEnterMaxSends+2, 100*time.Millisecond)
		if err := tm.NudgeSession("draft", "gc-staged-draft-short-probe"); err != nil {
			t.Fatalf("raw submit failed: %v", err)
		}
		if got := count(t, logPath, "SUBMIT"); got != 1 {
			t.Fatalf("submits=%d want1", got)
		}
	})
	t.Run("startup short multiline is one submission", func(t *testing.T) {
		tm, logPath := start(t, "gccodexstartupmultiline", 0, 100*time.Millisecond)
		message := "<system-reminder>\nwork the exact claimed bead\n</system-reminder>"
		if err := tm.nudgeStartupSession("draft", message); err != nil {
			lines, captureErr := tm.captureCodexComposer("draft")
			t.Fatalf("startup failed: %v; capture=%q captureErr=%v", err, lines, captureErr)
		}
		if got := count(t, logPath, "SUBMIT"); got != 1 {
			t.Fatalf("startup submits=%d want1", got)
		}
		if got := count(t, logPath, "ESCAPE"); got != 0 {
			t.Fatalf("startup Escape sends=%d want0", got)
		}
	})
	for _, message := range []string{"gc-busy-raw-injection", "<system-reminder>\nwork the exact claimed bead\n</system-reminder>", largeNudge} {
		t.Run(fmt.Sprintf("busy queue %d chars", len(message)), func(t *testing.T) {
			tm, logPath := start(t, fmt.Sprintf("gcbusyqueue%d", len(message)), 0, 0, true)
			if err := tm.NudgeSession("draft", message); err != nil {
				lines, captureErr := tm.captureCodexComposer("draft")
				t.Fatalf("busy queue failed: %v; capture=%q captureErr=%v observation=%+v", err, lines, captureErr, readCodexComposer(lines, message))
			}
			if got := count(t, logPath, "QUEUE"); got != 1 {
				t.Fatalf("queues=%d want1", got)
			}
			if got := count(t, logPath, "SUBMIT"); got != 0 {
				t.Fatalf("busy received Enter submits=%d", got)
			}
			if got := count(t, logPath, "ESCAPE"); got != 0 {
				t.Fatalf("busy received Escape=%d", got)
			}
		})
	}
}
