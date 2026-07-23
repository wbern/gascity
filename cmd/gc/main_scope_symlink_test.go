package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolveStoreScopeRootResolvesSymlinks(t *testing.T) {
	realDir := t.TempDir()
	link := filepath.Join(t.TempDir(), "city-link")
	if err := os.Symlink(realDir, link); err != nil {
		t.Skipf("cannot create symlink: %v", err)
	}
	if got, want := resolveStoreScopeRoot(link, ""), resolveStoreScopeRoot(realDir, ""); got != want {
		t.Fatalf("symlinked city path produced different scope root:\n  link: %s\n  real: %s", got, want)
	}
}
