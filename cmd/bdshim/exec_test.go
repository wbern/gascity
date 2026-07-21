package main

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

const passthroughChildHelperEnv = "GC_BDSHIM_TEST_PASSTHROUGH_CHILD"

func init() {
	if os.Getenv(passthroughChildHelperEnv) != "1" {
		return
	}
	os.Exit(execRealBd([]string{"version"}, nil, os.Stdin, os.Stdout, os.Stderr))
}

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
	code := execRealBd([]string{"update", "gcw-1"}, nil, strings.NewReader(""), &bytes.Buffer{}, &stderr)
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

func TestExecRealBdRefusesNestedPassthrough(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "calls.txt")
	bd := fakeBd(t, dir, out, 0)
	t.Setenv(realBdEnvVar, bd)

	var stderr bytes.Buffer
	code := execRealBd([]string{"version"}, []string{passthroughSentinelEnv + "=1"}, strings.NewReader(""), &bytes.Buffer{}, &stderr)
	if code == 0 {
		t.Fatalf("execRealBd() = 0 with nested passthrough marker; want refusal, stderr=%q", stderr.String())
	}
	if !strings.Contains(stderr.String(), "recursive") {
		t.Fatalf("stderr = %q, want recursive-passthrough diagnostic", stderr.String())
	}
	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Fatalf("real bd ran despite nested passthrough marker (stat err=%v)", err)
	}
}

// TestCopiedBdshimChildReceivesPassthroughSentinel covers the residual case
// that file identity cannot catch: PATH resolves to a copied bdshim binary with
// a different inode. The first shim starts that one child with the sentinel;
// the production child path refuses before it can start a third process.
func TestCopiedBdshimChildReceivesPassthroughSentinel(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX executable permission semantics")
	}
	dir := t.TempDir()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	contents, err := os.ReadFile(self)
	if err != nil {
		t.Fatal(err)
	}
	copyPath := filepath.Join(dir, "bd")
	if err := os.WriteFile(copyPath, contents, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv(realBdEnvVar, "")
	t.Setenv("PATH", dir)
	t.Setenv(passthroughChildHelperEnv, "1")

	var stderr bytes.Buffer
	code := execRealBd([]string{"version"}, nil, strings.NewReader(""), &bytes.Buffer{}, &stderr)
	if code == 0 {
		t.Fatalf("execRealBd() = 0 with copied bdshim child; stderr=%q", stderr.String())
	}
	if got := strings.Count(stderr.String(), "passthrough child already active"); got != 1 {
		t.Fatalf("copied bdshim refusal count = %d, want exactly 1 (one child at most); stderr=%q", got, stderr.String())
	}
}
