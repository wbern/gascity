//go:build integration

package tmux

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

// TestNudgeSessionClearsPendingInputBeforePaste proves an undelivered draft
// cannot be concatenated with the next nudge on a real, private tmux socket.
func TestNudgeSessionClearsPendingInputBeforePaste(t *testing.T) {
	if !hasTmux() {
		t.Skip("tmux not installed")
	}
	tm := testTmux()
	sessionName := fmt.Sprintf("gc-test-nudge-clear-%d", time.Now().UnixNano())
	_ = tm.KillSession(sessionName)
	if err := tm.NewSessionWithCommandAndEnv(sessionName, os.TempDir(), "cat -v", map[string]string{"GC_PROVIDER": "opencode"}); err != nil {
		t.Fatalf("NewSessionWithCommandAndEnv: %v", err)
	}
	defer func() { _ = tm.KillSession(sessionName) }()
	time.Sleep(300 * time.Millisecond)
	if _, err := tm.run("send-keys", "-t", sessionName, "-l", "leftover-draft"); err != nil {
		t.Fatalf("send leftover draft: %v", err)
	}
	time.Sleep(100 * time.Millisecond)
	if err := tm.NudgeSession(sessionName, "fresh-message"); err != nil {
		t.Fatalf("NudgeSession: %v", err)
	}
	time.Sleep(300 * time.Millisecond)
	out, err := tm.CapturePaneAll(sessionName)
	if err != nil {
		t.Fatalf("CapturePaneAll: %v", err)
	}
	if strings.Contains(out, "leftover-draftfresh-message") || !strings.Contains(out, "fresh-message") {
		t.Fatalf("nudge did not replace pending input: %s", out)
	}
}
