package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

func TestPreflightBDContextReader_CachesOnlyNotGitRepositorySubprocessFailure(t *testing.T) {
	city := t.TempDir()
	if err := os.WriteFile(filepath.Join(city, "city.toml"), []byte("[workspace]\nname = \"demo\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GC_DOLT", "skip")
	withFreshL1Memo(t)
	advance := withFixedPreflightNow(t, time.Unix(1000, 0))

	originalRunner := beadsExecCommandRunnerWithEnv
	t.Cleanup(func() { beadsExecCommandRunnerWithEnv = originalRunner })
	calls := 0
	beadsExecCommandRunnerWithEnv = func(_ map[string]string) beads.CommandRunner {
		return func(dir, name string, args ...string) ([]byte, error) {
			calls++
			if dir != city || name != "bd" || len(args) != 2 || args[0] != "context" || args[1] != "--json" {
				t.Fatalf("command = dir=%q %s %q, want city bd context --json", dir, name, args)
			}
			return nil, errors.New("exit status 1: cannot resolve repo context: cannot determine repository root: not a git repository")
		}
	}

	reader := preflightBDContextReader(city)
	if _, err := reader(city); !errors.Is(err, errPreflightBDContextNotGitRepository) {
		t.Fatalf("first reader error = %v, want classified non-repository error", err)
	}
	if calls != 1 {
		t.Fatalf("first reader calls = %d, want 1", calls)
	}

	// A fresh process has no L1 cache, so this asserts the persisted result
	// prevents the second preflight from spawning bd context again.
	withFreshL1Memo(t)
	advance(time.Unix(1030, 0))
	if _, err := reader(city); !errors.Is(err, errPreflightBDContextNotGitRepository) {
		t.Fatalf("warm reader error = %v, want classified non-repository error", err)
	}
	if calls != 1 {
		t.Fatalf("warm reader calls = %d, want 1", calls)
	}
}
