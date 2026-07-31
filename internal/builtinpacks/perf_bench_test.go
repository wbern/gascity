package builtinpacks

import (
	"os"
	"path/filepath"
	"testing"
)

// BenchmarkManifestForFSAllPacks measures the embedded-side cost alone: the
// walk of every bundled pack's embed.FS, reading every file into memory. This
// is the input to validatePackFiles and is recomputed on every call today.
func BenchmarkManifestForFSAllPacks(b *testing.B) {
	layouts := syntheticPackLayouts()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for _, layout := range layouts {
			if _, err := manifestForFS(layout.Pack.FS); err != nil {
				b.Fatalf("manifestForFS(%s): %v", layout.Pack.Name, err)
			}
		}
	}
}

// BenchmarkValidateSyntheticRepo measures the full readiness validation that
// the EnsureBuiltinRuntimeAssets memo-HIT path runs on every config load.
func BenchmarkValidateSyntheticRepo(b *testing.B) {
	dst := filepath.Join(b.TempDir(), "cache")
	if err := MaterializeSyntheticRepo(dst, testCommit); err != nil {
		b.Fatalf("MaterializeSyntheticRepo: %v", err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := ValidateSyntheticRepo(dst, testCommit); err != nil {
			b.Fatalf("ValidateSyntheticRepo: %v", err)
		}
	}
}

// BenchmarkValidateSyntheticRepoFast is the marker-only floor: what the memo
// hit would cost if it did no per-file work at all.
func BenchmarkValidateSyntheticRepoFast(b *testing.B) {
	dst := filepath.Join(b.TempDir(), "cache")
	if err := MaterializeSyntheticRepo(dst, testCommit); err != nil {
		b.Fatalf("MaterializeSyntheticRepo: %v", err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := ValidateSyntheticRepoFast(dst, testCommit); err != nil {
			b.Fatalf("ValidateSyntheticRepoFast: %v", err)
		}
	}
}

// BenchmarkValidatePackFilesStatOnly is a PROTOTYPE FLOOR PROBE, not a
// proposed implementation: it performs the same per-file traversal as
// validatePackFiles but stops at Lstat (mode + size) instead of reading each
// file's content. It exists to answer "how much of the 57ms is the content
// read?" before any contract-affecting change is designed.
func BenchmarkValidatePackFilesStatOnly(b *testing.B) {
	dst := filepath.Join(b.TempDir(), "cache")
	if err := MaterializeSyntheticRepo(dst, testCommit); err != nil {
		b.Fatalf("MaterializeSyntheticRepo: %v", err)
	}
	layouts := syntheticPackLayouts()
	// Pre-build manifests so this measures only the disk-side traversal.
	manifests := make([]map[string]fileEntry, len(layouts))
	for i, layout := range layouts {
		m, err := manifestForFS(layout.Pack.FS)
		if err != nil {
			b.Fatalf("manifestForFS: %v", err)
		}
		manifests[i] = m
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for j, layout := range layouts {
			packDir := filepath.Join(dst, filepath.FromSlash(layout.Subpath))
			for rel, want := range manifests[j] {
				info, err := os.Lstat(filepath.Join(packDir, filepath.FromSlash(rel)))
				if err != nil {
					b.Fatalf("Lstat %s: %v", rel, err)
				}
				if !info.Mode().IsRegular() || info.Mode().Perm() != want.perm.Perm() {
					b.Fatalf("mode mismatch %s", rel)
				}
				if info.Size() != int64(len(want.data)) {
					b.Fatalf("size mismatch %s", rel)
				}
			}
		}
	}
}

// BenchmarkCountSyntheticFiles reports how many files the validation walks,
// so the per-file cost can be reasoned about rather than guessed.
func BenchmarkCountSyntheticFiles(b *testing.B) {
	total := 0
	for _, layout := range syntheticPackLayouts() {
		m, err := manifestForFS(layout.Pack.FS)
		if err != nil {
			b.Fatalf("manifestForFS: %v", err)
		}
		total += len(m)
	}
	b.Logf("synthetic pack files across all layouts: %d", total)
}

// BenchmarkValidateSyntheticRepoFileSet isolates the whole-tree WalkDir that
// runs before any per-file work, to attribute the residual cost.
func BenchmarkValidateSyntheticRepoFileSet(b *testing.B) {
	dst := filepath.Join(b.TempDir(), "cache")
	if err := MaterializeSyntheticRepo(dst, testCommit); err != nil {
		b.Fatalf("MaterializeSyntheticRepo: %v", err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := validateSyntheticRepoFileSet(dst); err != nil {
			b.Fatalf("validateSyntheticRepoFileSet: %v", err)
		}
	}
}

// BenchmarkSyntheticRepoAllowedPaths isolates the allowed-path set build,
// which calls manifestForFS once per layout.
func BenchmarkSyntheticRepoAllowedPaths(b *testing.B) {
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, _, err := syntheticRepoAllowedPaths(); err != nil {
			b.Fatalf("syntheticRepoAllowedPaths: %v", err)
		}
	}
}
