package main

import (
	"os/exec"
	"path/filepath"
	"testing"
)

// resolvedPath returns p with symlinks resolved. Use it when a fixture root
// comes from a shared helper that other tests depend on keeping UNRESOLVED —
// resolving at the single call site is safer than changing the helper, which
// can flip a sibling test that deliberately asserts on an unresolved path.
func resolvedPath(t *testing.T, p string) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(p)
	if err != nil {
		t.Fatalf("EvalSymlinks(%q): %v", p, err)
	}
	return resolved
}

// trueBinaryPath returns an absolute path to a no-op "true" executable that
// exits 0 without reading stdin.
//
// /bin/true exists on Linux but NOT on macOS, which ships only /usr/bin/true.
// A hardcoded "/bin/true" therefore fails on Darwin with "fork/exec
// /bin/true: no such file or directory" — and in a test that asserts a child
// process RAN, that failure is indistinguishable from the behavior under test
// regressing. Resolving through PATH keeps the fixture portable.
func trueBinaryPath(t *testing.T) string {
	t.Helper()
	path, err := exec.LookPath("true")
	if err != nil {
		t.Fatalf("LookPath(\"true\"): %v", err)
	}
	return path
}

// resolvedTempDir returns t.TempDir() with symlinks resolved.
//
// On macOS the temp root is itself a symlink (/var -> /private/var), so
// t.TempDir() hands back an UNRESOLVED path. Production city/rig discovery
// canonicalizes the path it finds (filepath.EvalSymlinks), so it returns the
// resolved form. A test that seeds a fixture from the raw t.TempDir() and then
// compares it against production output is really comparing "/var/folders/..."
// against "/private/var/folders/...": it fails on Darwin for a reason that has
// nothing to do with the behavior under test, while passing on Linux CI where
// the temp root is a real directory.
//
// Seeding the fixture from the resolved path makes the comparison
// platform-independent without weakening it — the assertion still compares full
// absolute paths for equality. Use this instead of t.TempDir() in any test whose
// expectations are compared against a path that production code has discovered
// or canonicalized.
func resolvedTempDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatalf("EvalSymlinks(%q): %v", dir, err)
	}
	return resolved
}
