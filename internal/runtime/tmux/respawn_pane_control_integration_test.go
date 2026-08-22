//go:build integration

package tmux

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	runtimepkg "github.com/gastownhall/gascity/internal/runtime"
)

// TestProviderRelaunchSurvivesLaggingControlClient exercises the containment
// for tmux versions affected by the upstream respawn-pane control-client
// crash. It deliberately runs only on request because it starts real tmux
// control clients, but owns the real-provider boundary the unit test cannot:
// the server and an unrelated session survive a warm relaunch.
func TestProviderRelaunchSurvivesLaggingControlClient(t *testing.T) {
	if os.Getenv("GC_TMUX_RESPAWN_CONTROL_INTEGRATION") != "1" {
		t.Skip("set GC_TMUX_RESPAWN_CONTROL_INTEGRATION=1 to run the isolated control-client regression")
	}
	if !hasTmux() {
		t.Skip("tmux not installed")
	}

	tmuxTmpDir, err := os.MkdirTemp("/var/tmp", "gc-tmux-respawn-control-")
	if err != nil {
		t.Fatalf("create private TMUX_TMPDIR: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(tmuxTmpDir) })
	t.Setenv("TMUX_TMPDIR", tmuxTmpDir)
	cfg := DefaultConfig()
	cfg.SocketName = fmt.Sprintf("gc-respawn-control-%d", time.Now().UnixNano())
	tm := NewTmuxWithConfig(cfg)
	t.Cleanup(func() { _ = tm.KillServer() })

	name := "respawn-target"
	sibling := "respawn-sibling"
	reader := "while IFS= read -r line; do sh -c \"$line\"; done"
	if _, err := tm.run("new-session", "-d", "-s", name, "sh", "-c", reader); err != nil {
		t.Fatalf("create target session: %v", err)
	}
	if _, err := tm.run("new-session", "-d", "-s", sibling, "sleep", "600"); err != nil {
		t.Fatalf("create sibling session: %v", err)
	}
	targetBefore := paneIdentity(t, tm, name)
	siblingBefore := paneIdentity(t, tm, sibling)

	startControlClient(t, cfg.SocketName, name, nil)
	startControlClient(t, cfg.SocketName, name, io.Discard)
	if err := waitForControlClients(context.Background(), tm, 2, 5*time.Second); err != nil {
		t.Fatalf("wait for control clients: %v", err)
	}
	startFiniteProducer(t, tm, name, tmuxTmpDir+"/written-after-attach")

	serverBefore, err := tm.run("display-message", "-p", "-t", name, "#{pid}")
	if err != nil {
		t.Fatalf("read server PID before relaunch: %v", err)
	}
	p := NewProviderWithConfig(cfg)
	if err := p.Relaunch(context.Background(), name, runtimepkg.Config{Command: "printf first-replacement > " + tmuxTmpDir + "/first-replacement; exec sleep 600"}); err != nil {
		t.Fatalf("Relaunch with lagging control client: %v", err)
	}

	serverAfter, err := tm.run("display-message", "-p", "-t", name, "#{pid}")
	if err != nil {
		t.Fatalf("read server PID after relaunch: %v", err)
	}
	if strings.TrimSpace(serverAfter) != strings.TrimSpace(serverBefore) {
		t.Fatalf("server PID after relaunch = %q, want unchanged %q", serverAfter, serverBefore)
	}
	if got := paneIdentity(t, tm, name); got == targetBefore {
		t.Fatalf("target pane identity after relaunch = %q, want replacement", got)
	}
	_ = waitForFileWithin(t, tmuxTmpDir+"/first-replacement")
	assertPaneLive(t, tm, name)
	hasSibling, err := tm.HasSession(sibling)
	if err != nil {
		t.Fatalf("check sibling session: %v", err)
	}
	if !hasSibling {
		t.Fatalf("sibling session %q disappeared during relaunch", sibling)
	}
	if got := paneIdentity(t, tm, sibling); got != siblingBefore {
		t.Fatalf("sibling identity after relaunch = %q, want %q", got, siblingBefore)
	}
	if err := waitForControlClients(context.Background(), tm, 2, 5*time.Second); err != nil {
		t.Fatalf("control clients after relaunch: %v", err)
	}

	// The second upstream trigger: move a producing window out of the session
	// watched by both clients, then replace it while their offsets are stale.
	if _, err := tm.run("new-window", "-d", "-t", name, "sh", "-c", reader); err != nil {
		t.Fatalf("create moved-window producer: %v", err)
	}
	if _, err := tm.run("new-session", "-d", "-s", "offset-other", "sleep", "600"); err != nil {
		t.Fatalf("create moved-window destination: %v", err)
	}
	if _, err := tm.run("move-window", "-d", "-s", name+":1", "-t", "offset-other:1"); err != nil {
		t.Fatalf("move producing window out of watched session: %v", err)
	}
	startFiniteProducer(t, tm, "offset-other:1", tmuxTmpDir+"/written-after-move")
	movedBefore := paneIdentity(t, tm, "offset-other:1")
	if err := tm.RespawnPaneWithWorkDir("offset-other:1", "", "printf moved-replacement > "+tmuxTmpDir+"/moved-replacement; exec sleep 600"); err != nil {
		t.Fatalf("replace moved window with lagging control clients: %v", err)
	}
	serverAfterMoved, err := tm.run("display-message", "-p", "-t", name, "#{pid}")
	if err != nil {
		t.Fatalf("read server PID after moved-window replacement: %v", err)
	}
	if strings.TrimSpace(serverAfterMoved) != strings.TrimSpace(serverBefore) {
		t.Fatalf("server PID after moved-window replacement = %q, want unchanged %q", serverAfterMoved, serverBefore)
	}
	if got := paneIdentity(t, tm, "offset-other:1"); got == movedBefore {
		t.Fatalf("moved-window pane identity after replacement = %q, want replacement", got)
	}
	_ = waitForFileWithin(t, tmuxTmpDir+"/moved-replacement")
	assertPaneLive(t, tm, "offset-other:1")
	if got := paneIdentity(t, tm, sibling); got != siblingBefore {
		t.Fatalf("sibling identity after moved-window replacement = %q, want %q", got, siblingBefore)
	}
	if err := waitForControlClients(context.Background(), tm, 2, 5*time.Second); err != nil {
		t.Fatalf("control clients after moved-window replacement: %v", err)
	}
}

func TestRespawnPaneInstantExitRemainsObservable(t *testing.T) {
	if os.Getenv("GC_TMUX_RESPAWN_CONTROL_INTEGRATION") != "1" {
		t.Skip("set GC_TMUX_RESPAWN_CONTROL_INTEGRATION=1 to run the isolated control-client regression")
	}
	if !hasTmux() {
		t.Skip("tmux not installed")
	}
	tmuxTmpDir, err := os.MkdirTemp("/var/tmp", "gc-tmux-respawn-exit-")
	if err != nil {
		t.Fatalf("create private TMUX_TMPDIR: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(tmuxTmpDir) })
	t.Setenv("TMUX_TMPDIR", tmuxTmpDir)
	cfg := DefaultConfig()
	cfg.SocketName = fmt.Sprintf("gc-respawn-exit-%d", time.Now().UnixNano())
	tm := NewTmuxWithConfig(cfg)
	t.Cleanup(func() { _ = tm.KillServer() })
	if _, err := tm.run("new-session", "-d", "-s", "instant-exit", "sleep", "600"); err != nil {
		t.Fatalf("create session: %v", err)
	}
	if err := tm.RespawnPane("instant-exit", "false"); err != nil {
		t.Fatalf("replace with instant exit: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		out, err := tm.runCtx(ctx, "list-panes", "-t", "instant-exit", "-F", "#{pane_dead}")
		if err == nil && strings.TrimSpace(out) == "1" {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("instant-exit replacement was not retained: %v", ctx.Err())
		case <-ticker.C:
		}
	}
}

func TestRespawnPaneRunsCompoundCommand(t *testing.T) {
	if os.Getenv("GC_TMUX_RESPAWN_CONTROL_INTEGRATION") != "1" {
		t.Skip("set GC_TMUX_RESPAWN_CONTROL_INTEGRATION=1 to run the isolated control-client regression")
	}
	if !hasTmux() {
		t.Skip("tmux not installed")
	}
	tmuxTmpDir, err := os.MkdirTemp("/var/tmp", "gc-tmux-respawn-command-")
	if err != nil {
		t.Fatalf("create private TMUX_TMPDIR: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(tmuxTmpDir) })
	t.Setenv("TMUX_TMPDIR", tmuxTmpDir)
	workDir := t.TempDir()
	cfg := DefaultConfig()
	cfg.SocketName = fmt.Sprintf("gc-respawn-command-%d", time.Now().UnixNano())
	tm := NewTmuxWithConfig(cfg)
	t.Cleanup(func() { _ = tm.KillServer() })
	if _, err := tm.run("new-session", "-d", "-s", "compound-command", "sleep", "600"); err != nil {
		t.Fatalf("create session: %v", err)
	}
	if err := tm.RespawnPaneWithWorkDir("compound-command", workDir, "false || printf recovered > marker"); err != nil {
		t.Fatalf("replace with compound command: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	contents := waitForFile(t, ctx, workDir+"/marker")
	if got := string(contents); got != "recovered" {
		t.Fatalf("compound-command marker = %q, want recovered", got)
	}
}

func TestRespawnPaneRefusesMultiPaneWindow(t *testing.T) {
	if os.Getenv("GC_TMUX_RESPAWN_CONTROL_INTEGRATION") != "1" {
		t.Skip("set GC_TMUX_RESPAWN_CONTROL_INTEGRATION=1 to run the isolated control-client regression")
	}
	if !hasTmux() {
		t.Skip("tmux not installed")
	}
	tmuxTmpDir, err := os.MkdirTemp("/var/tmp", "gc-tmux-respawn-multipane-")
	if err != nil {
		t.Fatalf("create private TMUX_TMPDIR: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(tmuxTmpDir) })
	t.Setenv("TMUX_TMPDIR", tmuxTmpDir)
	cfg := DefaultConfig()
	cfg.SocketName = fmt.Sprintf("gc-respawn-multipane-%d", time.Now().UnixNano())
	tm := NewTmuxWithConfig(cfg)
	t.Cleanup(func() { _ = tm.KillServer() })
	if _, err := tm.run("new-session", "-d", "-s", "multi-pane", "sleep", "600"); err != nil {
		t.Fatalf("create session: %v", err)
	}
	if _, err := tm.run("split-window", "-d", "-t", "multi-pane", "sleep", "600"); err != nil {
		t.Fatalf("create sibling pane: %v", err)
	}
	before := paneIdentity(t, tm, "multi-pane")
	if err := tm.RespawnPane("multi-pane:0.0", "sleep 600"); err == nil {
		t.Fatal("RespawnPane on multi-pane window succeeded, want refusal")
	}
	after := paneIdentity(t, tm, "multi-pane")
	if after != before {
		t.Fatalf("multi-pane identity after refused replacement = %q, want %q", after, before)
	}
	if got := strings.Count(after, "\n") + 1; got != 2 {
		t.Fatalf("pane count after refused replacement = %d, want 2", got)
	}
}

func paneIdentity(t *testing.T, tm *Tmux, target string) string {
	t.Helper()
	out, err := tm.run("list-panes", "-t", target, "-F", "#{session_id}\t#{window_id}\t#{pane_id}\t#{pane_pid}")
	if err != nil {
		t.Fatalf("read pane identity for %q: %v", target, err)
	}
	return strings.TrimSpace(out)
}

func assertPaneLive(t *testing.T, tm *Tmux, target string) {
	t.Helper()
	out, err := tm.run("display-message", "-p", "-t", target, "#{pane_dead}")
	if err != nil {
		t.Fatalf("read pane liveness for %q: %v", target, err)
	}
	if got := strings.TrimSpace(out); got != "0" {
		t.Fatalf("pane %q dead status = %q, want 0", target, got)
	}
}

func startFiniteProducer(t *testing.T, tm *Tmux, target, marker string) {
	t.Helper()
	// 4,096 lines of 33 bytes is 135,168 bytes: enough to exceed a typical
	// pipe buffer while remaining bounded. The marker is written only after the
	// post-precondition burst is complete.
	command := "(i=0; while [ \"$i\" -lt 4096 ]; do printf 'xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx\\n'; i=$((i + 1)); done; printf complete > " + marker + ") &"
	if _, err := tm.run("send-keys", "-t", target, command, "C-m"); err != nil {
		t.Fatalf("launch finite producer in %q: %v", target, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if got := string(waitForFile(t, ctx, marker)); got != "complete" {
		t.Fatalf("producer marker %q = %q, want complete", marker, got)
	}
}

func waitForFile(t *testing.T, ctx context.Context, path string) []byte {
	t.Helper()
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		contents, err := os.ReadFile(path)
		if err == nil {
			return contents
		}
		if !os.IsNotExist(err) {
			t.Fatalf("read marker %q: %v", path, err)
		}
		select {
		case <-ctx.Done():
			t.Fatalf("marker %q was not created: %v", path, ctx.Err())
		case <-ticker.C:
		}
	}
}

func waitForFileWithin(t *testing.T, path string) []byte {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return waitForFile(t, ctx, path)
}

func startControlClient(t *testing.T, socketName, session string, stdout io.Writer) *exec.Cmd {
	t.Helper()
	cmd := exec.Command("tmux", "-L", socketName, "-C", "attach-session", "-t", session)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("open control-client stdin: %v", err)
	}
	if stdout == nil {
		if _, err := cmd.StdoutPipe(); err != nil {
			t.Fatalf("open lagging control-client stdout: %v", err)
		}
	} else {
		cmd.Stdout = stdout
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start control client: %v", err)
	}
	t.Cleanup(func() {
		_ = stdin.Close()
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		_ = cmd.Wait()
	})
	return cmd
}

func waitForControlClients(ctx context.Context, tm *Tmux, want int, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		out, err := tm.runCtx(ctx, "list-clients", "-F", "#{client_control_mode}")
		if err == nil && strings.Count(strings.TrimSpace(out), "1") >= want {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("wait for %d control clients: %w", want, ctx.Err())
		case <-ticker.C:
		}
	}
}
