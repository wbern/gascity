package main

import (
	"testing"

	"github.com/gastownhall/gascity/internal/pathutil"
)

// resolvedPath returns p canonicalized the same way production canonicalizes a
// path it discovers. Use it when a fixture root comes from a shared helper that
// other tests depend on keeping UNRESOLVED — normalizing at the single call site
// is safer than changing that helper, which can flip a sibling test that
// deliberately asserts on an unresolved path.
//
// Like resolvedTempDir, this normalizes through pathutil rather than bare
// filepath.EvalSymlinks: EvalSymlinks leaves the darwin /private alias in place
// ("/private/var/..."), while the discovery and store-scope paths this is
// compared against collapse it ("/var/..."). See resolvedTempDir for the full
// rationale.
func resolvedPath(t *testing.T, p string) string {
	t.Helper()
	resolved := pathutil.NormalizePathForCompare(p)
	if resolved == "" {
		t.Fatalf("NormalizePathForCompare(%q) returned empty", p)
	}
	return resolved
}

// resolvedTempDir returns t.TempDir() canonicalized the same way production
// canonicalizes a path it discovers.
//
// On macOS the temp root is itself a symlink (/var -> /private/var), so
// t.TempDir() hands back an UNRESOLVED path. A test that seeds a fixture from
// the raw t.TempDir() and then compares it against production output is really
// comparing "/var/folders/..." against "/private/var/folders/...": it fails on
// Darwin for a reason that has nothing to do with the behavior under test,
// while passing on Linux CI where the temp root is a real directory.
//
// This MUST normalize through pathutil, not bare filepath.EvalSymlinks. The two
// disagree on exactly the case that bites here: EvalSymlinks leaves the darwin
// /private alias in place, so it yields "/private/var/...", whereas
// pathutil.NormalizePathForCompare collapses the alias and yields "/var/...".
// City/rig discovery and store-scope resolution route through pathutil, so a
// fixture seeded with EvalSymlinks is pinned to the opposite spelling from the
// production code it is compared against.
//
// Seeding the fixture from the normalized path makes the comparison
// platform-independent without weakening it — the assertion still compares full
// absolute paths for equality. Use this instead of t.TempDir() in any test whose
// expectations are compared against a path that production code has discovered
// or canonicalized.
func resolvedTempDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	resolved := pathutil.NormalizePathForCompare(dir)
	if resolved == "" {
		t.Fatalf("NormalizePathForCompare(%q) returned empty", dir)
	}
	return resolved
}
