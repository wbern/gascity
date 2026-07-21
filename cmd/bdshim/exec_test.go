package main

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestExecRealBdFailsLoudlyWhenChildCannotStart(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX executable permission semantics")
	}
	path := filepath.Join(t.TempDir(), "not-executable")
	if err := os.WriteFile(path, []byte("not executable\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(realBdEnvVar, path)

	var stderr bytes.Buffer
	code := execRealBd([]string{"update", "gcw-1"}, "", nil, strings.NewReader(""), &bytes.Buffer{}, &stderr)
	if code == 0 {
		t.Fatalf("execRealBd() = 0 when child cannot start; stderr=%q", stderr.String())
	}
	if strings.TrimSpace(stderr.String()) == "" {
		t.Fatal("execRealBd() wrote no error when child cannot start")
	}
}

// TestResolveRealBdPathRefusesSelfOnPATH guards the production incident where
// bdshim was installed as shimbin/bd but GC_BD_REAL was absent. A normal PATH
// lookup then found the shim itself and each passthrough spawned another copy.
// This test calls the resolver only: a regression fails without forking even
// once, so it remains safe to run while diagnosing the failure mode.
func TestResolveRealBdPathRefusesSelfOnPATH(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink semantics differ on Windows")
	}
	shimDir := t.TempDir()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(self, filepath.Join(shimDir, "bd")); err != nil {
		t.Fatal(err)
	}
	t.Setenv(realBdEnvVar, "")
	t.Setenv("PATH", shimDir)

	_, err = resolveRealBdPath()
	if err == nil {
		t.Fatal("resolveRealBdPath() = nil error with bdshim itself on PATH; want recursion refusal")
	}
	if !strings.Contains(err.Error(), "recurs") {
		t.Fatalf("resolveRealBdPath() error = %q, want recursion diagnostic", err)
	}
}

func TestResolveRealBdPathRefusesSelfViaExplicitTarget(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink semantics differ on Windows")
	}
	shimDir := t.TempDir()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	selfLink := filepath.Join(shimDir, "bd")
	if err := os.Symlink(self, selfLink); err != nil {
		t.Fatal(err)
	}
	t.Setenv(realBdEnvVar, selfLink)

	_, err = resolveRealBdPath()
	if err == nil {
		t.Fatal("resolveRealBdPath() = nil error with GC_BD_REAL pointing at bdshim; want recursion refusal")
	}
	if !strings.Contains(err.Error(), "recurs") {
		t.Fatalf("resolveRealBdPath() error = %q, want recursion diagnostic", err)
	}
}

func TestResolveRealBdPathAllowsDistinctBdOnPATH(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX executable permission semantics")
	}
	binDir := t.TempDir()
	realBd := filepath.Join(binDir, "bd")
	if err := os.WriteFile(realBd, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv(realBdEnvVar, "")
	t.Setenv("PATH", binDir)

	got, err := resolveRealBdPath()
	if err != nil {
		t.Fatalf("resolveRealBdPath() error = %v", err)
	}
	if got != realBd {
		t.Fatalf("resolveRealBdPath() = %q, want distinct PATH bd %q", got, realBd)
	}
}
