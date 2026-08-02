package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/packman"
)

// BenchmarkBuiltinReadinessPassCold measures the readiness pass a FRESH gc
// process pays: a city that pins bundled imports in packs.lock, with the
// per-city ready memo cleared each iteration.
//
// Both of those differ from BenchmarkBuiltinReadinessPass, and both matter.
//
// That distinction is the whole point. The readiness pass validates the shared
// synthetic cache directory from two places: requiredBuiltinSourcesUsable, for
// the sources every city needs, and lockedBundledImportsUsable, for the sources
// packs.lock pins. Every bundled source of a repository resolves to the SAME
// cache directory, and ValidateSyntheticRepo checks every pack layout in it
// regardless of which source asked — so with a lockfile present the two helpers
// ask the identical question, and that question re-reads every cached pack file.
//
// BenchmarkBuiltinReadinessPass writes a city with NO lockfile and measures the
// WARM memo-hit path. On that shape the locked-imports half is empty and the
// ready memo short-circuits, so the duplicate never occurs and a change that
// removes it measures as exactly zero — which is what it did, while being worth
// ~51 ms of CPU on a real invocation. A one-shot `gc bd` is always cold: the
// memo lives in a package-level map that a fresh process starts empty, so it
// runs the repair path, where the required-sources and locked-imports helpers
// each validate the same shared cache directory.
//
// Benchmarking the cheaper shape and concluding "no change" is the trap this
// exists to close.
func BenchmarkBuiltinReadinessPassCold(b *testing.B) {
	b.Setenv("GC_HOME", b.TempDir())
	cityPath := newReadinessCostCity(b)
	writeBenchBundledImportLock(b, cityPath)
	if _, err := loadCityConfig(cityPath, io.Discard); err != nil {
		b.Fatalf("warming: %v", err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		resetBuiltinRuntimeReadyCacheForBench()
		b.StartTimer()
		if err := EnsureBuiltinRuntimeAssets(cityPath, io.Discard); err != nil {
			b.Fatalf("EnsureBuiltinRuntimeAssets: %v", err)
		}
	}
}

// resetBuiltinRuntimeReadyCacheForBench clears the per-city readiness memo so
// the next pass runs as a fresh process would.
func resetBuiltinRuntimeReadyCacheForBench() {
	builtinRuntimeReadyCache.Range(func(k, _ any) bool {
		builtinRuntimeReadyCache.Delete(k)
		return true
	})
}

// writeBenchBundledImportLock pins a bundled source at its canonical commit, so
// the locked-imports half of the readiness pass has real work to do.
func writeBenchBundledImportLock(b *testing.B, cityPath string) {
	b.Helper()
	// gastown's canonical pin is its own public-pack version, NOT
	// BundledPackImportVersion. Locking it at the wrong commit makes it an
	// ordinary remote import, which the preflight skips entirely — leaving the
	// benchmark measuring a city with no locked bundled imports at all.
	source := config.PublicGastownPackSource
	commit := strings.TrimPrefix(config.PublicGastownPackVersion, "sha:")
	lockToml := fmt.Sprintf(`schema = 1

[packs.%q]
version = %q
commit = %q
fetched = "2026-01-01T00:00:00Z"
`, source, "sha:"+commit, commit)
	if err := os.WriteFile(filepath.Join(cityPath, packman.LockfileName), []byte(lockToml), 0o644); err != nil {
		b.Fatalf("writing packs.lock: %v", err)
	}
}
