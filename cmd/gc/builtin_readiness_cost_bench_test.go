package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
)

// newReadinessCostCity writes a minimal bd-provider city.
func newReadinessCostCity(b *testing.B) string {
	b.Helper()
	cityPath := b.TempDir()
	toml := "name = \"bench\"\nprefix = \"bc\"\n\n[beads]\nprovider = \"bd\"\n"
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(toml), 0o644); err != nil {
		b.Fatalf("writing city.toml: %v", err)
	}
	return cityPath
}

// BenchmarkBuiltinReadinessPass measures EnsureBuiltinRuntimeAssets on its
// warm memo-hit path: the readiness revalidation that reads every file of
// every cached builtin pack before a config load parses anything.
//
// Read this against BenchmarkCityConfigParseOnly. The readiness pass, not the
// parse, is what a config load costs — which is why skipping a redundant load
// is worth anything, and why the pass itself must still run once per process.
//
// This shape deliberately measures the cheapest one: no packs.lock and a warm
// memo. Use BenchmarkBuiltinReadinessPassCold to measure the pass a one-shot
// process actually pays; that benchmark's comment explains what this one
// cannot see.
func BenchmarkBuiltinReadinessPass(b *testing.B) {
	b.Setenv("GC_HOME", b.TempDir())
	cityPath := newReadinessCostCity(b)
	if _, err := loadCityConfig(cityPath, io.Discard); err != nil {
		b.Fatalf("warming: %v", err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := EnsureBuiltinRuntimeAssets(cityPath, io.Discard); err != nil {
			b.Fatalf("EnsureBuiltinRuntimeAssets: %v", err)
		}
	}
}

// BenchmarkBuiltinReadinessPassCold measures the readiness pass a FRESH gc
// process pays: a city whose packs.lock pins a bundled source at its canonical
// commit, with the per-city ready memo cleared before each iteration.
//
// It is the cold companion to BenchmarkBuiltinReadinessPass, which measures a
// city with no packs.lock on the warm memo-hit path. Two things separate the
// two shapes, and a one-shot command sits on the cold side of both:
//
//   - No lockfile means lockedBundledCanonicalImports finds nothing, so the
//     locked-imports half of the pass — one of the two places
//     EnsureBuiltinRuntimeAssets validates a synthetic pack cache — never runs
//     at all. A locked bundled source resolves to its own cache directory, and
//     MaterializeSyntheticRepo writes every bundled pack layout into every such
//     directory, so validating it is a second full walk of the whole bundled
//     pack set rather than an incremental check.
//   - builtinRuntimeReadyCache is a package-level sync.Map, so from the second
//     iteration onward the warm benchmark takes the ready fast path. A one-shot
//     `gc` process starts that map empty and always takes the repair path.
//
// The consequence is that the warm benchmark is insensitive to changes in most
// of the pass it is named after: work that only the repair path or only the
// locked-imports half performs measures as zero against it. Clearing the memo
// and pinning a lockfile is what makes those changes visible.
func BenchmarkBuiltinReadinessPassCold(b *testing.B) {
	b.Setenv("GC_HOME", b.TempDir())
	cityPath := newReadinessCostCity(b)
	// The canonical pin for gastown is its own public-pack version, not
	// BundledPackImportVersion. A bundled source locked at any other commit is
	// an ordinary remote import that the preflight skips entirely, which would
	// leave this measuring a city with no locked bundled imports after all.
	writePreflightImportLock(b, cityPath, strings.TrimPrefix(config.PublicGastownPackVersion, "sha:"))
	// Fail loudly rather than quietly measuring the warm shape twice if that
	// ever stops holding.
	locked, err := lockedBundledCanonicalImports(cityPath)
	if err != nil {
		b.Fatalf("lockedBundledCanonicalImports: %v", err)
	}
	if len(locked) == 0 {
		b.Fatal("fixture pinned no canonical bundled imports; the locked-imports half of the pass would not run")
	}
	if _, err := loadCityConfig(cityPath, io.Discard); err != nil {
		b.Fatalf("warming: %v", err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		resetBuiltinRuntimeReadyCache()
		b.StartTimer()
		if err := EnsureBuiltinRuntimeAssets(cityPath, io.Discard); err != nil {
			b.Fatalf("EnsureBuiltinRuntimeAssets: %v", err)
		}
	}
}

// resetBuiltinRuntimeReadyCache clears the per-city builtin readiness memo so
// the next EnsureBuiltinRuntimeAssets call runs the pass a fresh process would.
func resetBuiltinRuntimeReadyCache() {
	builtinRuntimeReadyCache.Range(func(key, _ any) bool {
		builtinRuntimeReadyCache.Delete(key)
		return true
	})
}

// BenchmarkCityConfigParseOnly measures the config parse plus pack expansion
// with the readiness pass skipped.
func BenchmarkCityConfigParseOnly(b *testing.B) {
	b.Setenv("GC_HOME", b.TempDir())
	cityPath := newReadinessCostCity(b)
	if _, err := loadCityConfig(cityPath, io.Discard); err != nil {
		b.Fatalf("warming: %v", err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := loadCityConfigWithoutBuiltinPackRefresh(cityPath, io.Discard); err != nil {
			b.Fatalf("loadCityConfigWithoutBuiltinPackRefresh: %v", err)
		}
	}
}

// BenchmarkSuppliedConfigReadinessGuard measures what a store open handed an
// already-loaded config now pays to keep the self-heal contract: a memo lookup
// for a city this process already readied, instead of a second readiness pass.
func BenchmarkSuppliedConfigReadinessGuard(b *testing.B) {
	b.Setenv("GC_HOME", b.TempDir())
	cityPath := newReadinessCostCity(b)
	if _, err := loadCityConfig(cityPath, io.Discard); err != nil {
		b.Fatalf("warming: %v", err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := ensureBuiltinRuntimeAssetsForSuppliedConfig(cityPath, io.Discard); err != nil {
			b.Fatalf("ensureBuiltinRuntimeAssetsForSuppliedConfig: %v", err)
		}
	}
}
