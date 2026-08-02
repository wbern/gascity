package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/builtinpacks"
	"github.com/gastownhall/gascity/internal/packman"
)

// core, bd, dolt and gastown all resolve to ONE synthetic cache directory, and
// ValidateSyntheticRepo checks every pack layout in that directory regardless
// of which source asked. The required-sources helper and the locked-imports
// helper therefore asked the identical question, and that question re-reads
// every cached pack file to compare it against the embedded copy.
//
// syntheticCacheVerifier collapses that within one readiness pass — and only
// within one, so a verdict is never carried across passes and the self-healing
// contract still runs fresh every time.

// TestSyntheticCacheVerifierReusesPositiveVerdictWithinAPass pins the dedupe
// AND its scope in one shot: the same verifier reuses a verdict it already
// reached, while a fresh verifier re-validates and sees the corruption.
//
// If the memo were absent, both would report false. If it leaked across passes,
// both would report true. Only the correct behavior distinguishes them.
func TestSyntheticCacheVerifierReusesPositiveVerdictWithinAPass(t *testing.T) {
	cacheDir, commit := materializeVerifierCacheForTest(t)

	pass := newSyntheticCacheVerifier()
	if !pass.Valid(cacheDir, commit) {
		t.Fatal("freshly materialized cache reported invalid")
	}

	corruptVerifierCacheFile(t, cacheDir)

	if !pass.Valid(cacheDir, commit) {
		t.Error("verifier re-validated inside a single pass; the duplicate walk is back")
	}
	if newSyntheticCacheVerifier().Valid(cacheDir, commit) {
		t.Error("a new pass reused a stale verdict; corruption would never self-heal")
	}
}

// TestSyntheticCacheVerifierDoesNotMemoizeNegatives pins that a failed verdict
// is never cached. A negative means the caller is about to repair the cache, so
// caching it would make the post-repair re-check answer about the pre-repair
// state.
func TestSyntheticCacheVerifierDoesNotMemoizeNegatives(t *testing.T) {
	cacheDir, commit := materializeVerifierCacheForTest(t)
	corrupted := filepath.Join(cacheDir, "internal", "bootstrap", "packs", "core", "pack.toml")
	original, err := os.ReadFile(corrupted)
	if err != nil {
		t.Skipf("bundled layout does not carry the expected pack file: %v", err)
	}

	pass := newSyntheticCacheVerifier()
	corruptVerifierCacheFile(t, cacheDir)
	if pass.Valid(cacheDir, commit) {
		t.Fatal("corrupted cache reported valid")
	}

	// Stand in for the repair the caller would perform.
	if err := os.WriteFile(corrupted, original, 0o644); err != nil {
		t.Fatalf("restore pack file: %v", err)
	}
	if !pass.Valid(cacheDir, commit) {
		t.Error("verifier cached the negative verdict; a repaired cache still reads as broken")
	}
}

// TestSyntheticCacheVerifierInvalidateDropsAVerdict pins the explicit drop the
// repair paths perform after rewriting a cache.
func TestSyntheticCacheVerifierInvalidateDropsAVerdict(t *testing.T) {
	cacheDir, commit := materializeVerifierCacheForTest(t)

	pass := newSyntheticCacheVerifier()
	if !pass.Valid(cacheDir, commit) {
		t.Fatal("freshly materialized cache reported invalid")
	}
	corruptVerifierCacheFile(t, cacheDir)
	pass.Invalidate(cacheDir, commit)

	if pass.Valid(cacheDir, commit) {
		t.Error("Invalidate did not drop the memoized verdict")
	}
}

// TestSyntheticCacheVerifierNilIsUsable pins that the zero value degrades to
// plain validation rather than panicking, so a caller that forgets to build one
// is merely slow, never wrong.
func TestSyntheticCacheVerifierNilIsUsable(t *testing.T) {
	cacheDir, commit := materializeVerifierCacheForTest(t)
	var nilVerifier *syntheticCacheVerifier
	if !nilVerifier.Valid(cacheDir, commit) {
		t.Error("nil verifier rejected a valid cache")
	}
	nilVerifier.Invalidate(cacheDir, commit) // must not panic
	corruptVerifierCacheFile(t, cacheDir)
	if nilVerifier.Valid(cacheDir, commit) {
		t.Error("nil verifier accepted a corrupted cache; it is memoizing something")
	}
}

func materializeVerifierCacheForTest(t *testing.T) (string, string) {
	t.Helper()
	clearGCEnv(t) // fresh GC_HOME → the cache lives under t.TempDir()
	commit := bundledPackImportCommit()
	source, ok := builtinpacks.Source("core")
	if !ok {
		t.Fatal(`builtinpacks.Source("core") not registered`)
	}
	cacheDir, err := packman.RepoCachePath(source, commit)
	if err != nil {
		t.Fatalf("RepoCachePath: %v", err)
	}
	if err := builtinpacks.MaterializeSyntheticRepo(cacheDir, commit); err != nil {
		t.Fatalf("MaterializeSyntheticRepo: %v", err)
	}
	return cacheDir, commit
}

func corruptVerifierCacheFile(t *testing.T, cacheDir string) {
	t.Helper()
	target := filepath.Join(cacheDir, "internal", "bootstrap", "packs", "core", "pack.toml")
	if _, err := os.Stat(target); err != nil {
		t.Skipf("bundled layout does not carry the expected pack file: %v", err)
	}
	if err := os.WriteFile(target, []byte("[pack]\nname = \"tampered\"\nschema = 1\n"), 0o644); err != nil {
		t.Fatalf("corrupt cached pack file: %v", err)
	}
}
