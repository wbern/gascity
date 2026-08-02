package builtinpacks

import (
	"testing"
)

// BenchmarkValidateSyntheticRepo measures the tree validation directly.
//
// BenchmarkBuiltinReadinessPassCold (cmd/gc) under-resolves changes to this
// function: the validation is a fraction of that pass, so a saving here lands
// inside its noise. A change to the traversal must be measured against the
// traversal.
func BenchmarkValidateSyntheticRepo(b *testing.B) {
	dir := b.TempDir()
	if err := MaterializeSyntheticRepo(dir, Repository, "deadbeef"); err != nil {
		b.Fatalf("MaterializeSyntheticRepo: %v", err)
	}
	if err := ValidateSyntheticRepo(dir, Repository, "deadbeef"); err != nil {
		b.Fatalf("fixture invalid: %v", err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := ValidateSyntheticRepo(dir, Repository, "deadbeef"); err != nil {
			b.Fatalf("ValidateSyntheticRepo: %v", err)
		}
	}
}
