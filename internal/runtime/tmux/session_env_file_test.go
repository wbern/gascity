package tmux

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestSecretSessionEnvUsesPrivateCommandFile(t *testing.T) {
	exec := &fakeExecutor{}
	tm := NewTmux()
	tm.exec = exec
	const secret = "generated-secret-canary"
	if err := tm.NewSessionWithCommandAndEnv("gc-secret-file", "/work", "claude", map[string]string{"OPENAI_API_KEY": secret, "GC_RIG": "rig"}); err != nil {
		t.Fatalf("NewSessionWithCommandAndEnv: %v", err)
	}
	for _, call := range exec.calls {
		if strings.Contains(strings.Join(call, "\x00"), secret) {
			t.Fatal("secret canary reached tmux argv")
		}
	}
	if got := exec.calls[0]; !containsArgs(got, "start-server", ";", "source-file") || containsArg(got, "-e") {
		t.Fatal("secret environment did not use source-file-only argv")
	}
}

func TestInertSessionEnvRemainsOnArgv(t *testing.T) {
	exec := &fakeExecutor{}
	tm := NewTmux()
	tm.exec = exec
	if err := tm.NewSessionWithCommandAndEnv("gc-inert-file", "/work", "claude", map[string]string{"GC_RIG": "rig", "LANG": "C"}); err != nil {
		t.Fatalf("NewSessionWithCommandAndEnv: %v", err)
	}
	if got := strings.Join(exec.calls[0], "\x00"); !strings.Contains(got, "\x00-e\x00GC_RIG=rig\x00") || strings.Contains(got, "source-file") {
		t.Fatal("inert environment unexpectedly left the argv path")
	}
}

func TestSecretSessionEnvFailsClosedWhenStagingFails(t *testing.T) {
	t.Setenv("TMPDIR", filepath.Join(t.TempDir(), "absent"))
	exec := &fakeExecutor{}
	tm := NewTmux()
	tm.exec = exec
	if err := tm.NewSessionWithCommandAndEnv("gc-stage-fail", "", "claude", map[string]string{"OPENAI_API_KEY": "generated-secret-canary"}); err == nil {
		t.Fatal("secret session must fail when private staging fails")
	}
	if len(exec.calls) != 0 {
		t.Fatal("tmux must not run after secret staging failure")
	}
}

func TestSecretSessionEnvPreservesErrSessionExists(t *testing.T) {
	exec := &fakeExecutor{err: ErrSessionExists}
	tm := NewTmux()
	tm.exec = exec
	err := tm.NewSessionWithCommandAndEnv("gc-existing-file", "", "claude", map[string]string{"OPENAI_API_KEY": "generated-secret-canary"})
	if !errors.Is(err, ErrSessionExists) {
		t.Fatalf("error = %v, want ErrSessionExists", err)
	}
}

func TestStageTmuxCommandFileIsPrivateAndCleansUp(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	path, cleanup, err := stageTmuxCommandFile("new-session -d")
	if err != nil {
		t.Fatalf("stage: %v", err)
	}
	if got, err := os.Stat(path); err != nil || got.Mode().Perm() != 0o600 {
		t.Fatal("staged file must be mode 0600")
	}
	if got, err := os.Stat(filepath.Dir(path)); err != nil || got.Mode().Perm() != 0o700 {
		t.Fatal("staged directory must be mode 0700")
	}
	if body, err := os.ReadFile(path); err != nil || string(body) != "new-session -d\n" {
		t.Fatal("staged command was not byte-identical")
	}
	cleanup()
	if _, err := os.Stat(filepath.Dir(path)); !os.IsNotExist(err) {
		t.Fatal("staged directory survived cleanup")
	}
}

func TestStageTmuxCommandFileConcurrentCreation(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	const workers = 24
	paths := make(chan string, workers)
	errs := make(chan error, workers)
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			path, cleanup, err := stageTmuxCommandFile("new-session -d")
			if err == nil {
				paths <- path
				cleanup()
			} else {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(paths)
	close(errs)
	for err := range errs {
		t.Errorf("concurrent stage: %v", err)
	}
	seen := map[string]bool{}
	for path := range paths {
		if seen[path] {
			t.Error("concurrent stages reused a path")
		}
		seen[path] = true
	}
	if len(seen) != workers {
		t.Fatalf("created %d paths, want %d", len(seen), workers)
	}
}

func TestSweepStaleStagedDirs(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("TMPDIR", dir)
	old := time.Now().Add(-2 * staleStagedDirAge)
	stale := filepath.Join(dir, stagedDirPrefix+"orphan")
	fresh := filepath.Join(dir, stagedDirPrefix+"inflight")
	bystander := filepath.Join(dir, "bystander")
	for _, path := range []string{stale, fresh, bystander} {
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(bystander, old, old); err != nil {
		t.Fatal(err)
	}
	sweepStaleStagedDirs()
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatal("stale staged directory survived")
	}
	for _, path := range []string{fresh, bystander} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("sweep removed %s", filepath.Base(path))
		}
	}
}

func TestNewSessionSweepsStaleStagedDirs(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("TMPDIR", dir)
	stale := filepath.Join(dir, stagedDirPrefix+"orphan")
	if err := os.Mkdir(stale, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stale, stagedFileName), []byte("new-session -e K=generated-canary\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-2 * staleStagedDirAge)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatal(err)
	}
	tm := NewTmux()
	tm.exec = &fakeExecutor{}
	if err := tm.NewSessionWithCommandAndEnv("gc-sweep-call", "", "claude", map[string]string{"OPENAI_API_KEY": "generated-secret-canary"}); err != nil {
		t.Fatalf("NewSessionWithCommandAndEnv: %v", err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatal("secret session creation did not sweep stale staging directory")
	}
}

func TestTmuxCommandFileQuoting(t *testing.T) {
	args := []string{"new-session", "-e", "K=a'b$c#d;\\x\nnext", `agent "quoted"`}
	want := "'new-session' '-e' 'K=a'\\''b$c#d;\\x\nnext' 'agent \"quoted\"'"
	if got := tmuxCommandLine(args); got != want {
		t.Errorf("tmux command quoting changed")
	}
}

func containsArg(args []string, want string) bool {
	for _, arg := range args {
		if arg == want {
			return true
		}
	}
	return false
}

func containsArgs(args []string, want ...string) bool {
	for index := range args {
		if len(args)-index >= len(want) {
			match := true
			for offset := range want {
				if args[index+offset] != want[offset] {
					match = false
					break
				}
			}
			if match {
				return true
			}
		}
	}
	return false
}
