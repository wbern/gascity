package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestPruneRetainedWorktreeDoesNotCreateOrOverwriteMarker(t *testing.T) {
	for _, existing := range []bool{false, true} {
		t.Run(map[bool]string{false: "no_marker", true: "user_marker"}[existing], func(t *testing.T) {
			fx := newPruneFixture(t)
			fx.setProbe(fx.workerDir, &fakeGitProbe{isRepo: true, hasIgnored: true, currentBranch: "builder/ga-test"})
			p := filepath.Join(fx.workerDir, worktreeStaleFileName)
			custom := []byte("user-owned note\n")
			if existing {
				if err := os.WriteFile(p, custom, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			var stderr bytes.Buffer
			if pruneAgentHomeWorktreeIfSafe(fx.sessionBead(), fx.cityPath, fx.cfg, &stderr) {
				t.Fatal("retained tree removed")
			}
			if existing {
				got, err := os.ReadFile(p)
				if err != nil || !bytes.Equal(got, custom) {
					t.Fatalf("user marker changed: %q %v", got, err)
				}
			} else {
				assertNoWorktreeStaleMarker(t, fx.workerDir)
			}
		})
	}
}
