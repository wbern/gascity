package main

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
)

// The preflight identity memo caches a process-stable, per-scope value (bd
// context identity / Dolt project id) so gc stops re-spawning `bd context` and
// re-pinging Dolt on every store-open. These tests pin the two load-bearing
// properties: a successful value is computed once and reused, and an error is
// never cached (a transient probe failure must retry, not permanently degrade
// the scope).

func TestPreflightScopeMemo_CachesSuccessComputeOnce(t *testing.T) {
	memo := newPreflightScopeMemo[string]()
	calls := 0
	compute := func() (string, error) {
		calls++
		return "identity-v1", nil
	}

	for i := 0; i < 3; i++ {
		got, err := memo.getOrCompute("k", compute)
		if err != nil {
			t.Fatalf("call %d: unexpected error: %v", i, err)
		}
		if got != "identity-v1" {
			t.Fatalf("call %d: got %q, want identity-v1", i, got)
		}
	}
	if calls != 1 {
		t.Fatalf("compute ran %d times, want exactly 1 (the value is process-stable)", calls)
	}
}

func TestPreflightScopeMemo_DoesNotCacheErrors(t *testing.T) {
	memo := newPreflightScopeMemo[string]()
	calls := 0
	boom := errors.New("dolt ping timed out")
	compute := func() (string, error) {
		calls++
		if calls < 3 {
			return "", boom // transient failure on the first two calls
		}
		return "recovered", nil
	}

	// First two calls error and must NOT be cached (each retries the probe).
	for i := 0; i < 2; i++ {
		if _, err := memo.getOrCompute("k", compute); !errors.Is(err, boom) {
			t.Fatalf("call %d: err = %v, want the transient error (uncached)", i, err)
		}
	}
	// Third call recovers and is cached.
	got, err := memo.getOrCompute("k", compute)
	if err != nil || got != "recovered" {
		t.Fatalf("recovery call: got %q err %v, want recovered/nil", got, err)
	}
	// Fourth call is served from cache — compute does not run again.
	if _, err := memo.getOrCompute("k", compute); err != nil {
		t.Fatalf("cached call errored: %v", err)
	}
	if calls != 3 {
		t.Fatalf("compute ran %d times, want 3 (2 uncached errors + 1 cached success)", calls)
	}
}

func TestPreflightScopeMemo_KeysAreIndependent(t *testing.T) {
	memo := newPreflightScopeMemo[string]()
	if v, _ := memo.getOrCompute("a", func() (string, error) { return "A", nil }); v != "A" {
		t.Fatalf("key a = %q, want A", v)
	}
	if v, _ := memo.getOrCompute("b", func() (string, error) { return "B", nil }); v != "B" {
		t.Fatalf("key b = %q, want B", v)
	}
	// Re-reading key a returns A, not B — keys do not collide.
	if v, _ := memo.getOrCompute("a", func() (string, error) { return "SHOULD-NOT-RUN", nil }); v != "A" {
		t.Fatalf("key a after b = %q, want cached A", v)
	}
}

func TestPreflightScopeMemo_ConcurrentSafeAndBounded(t *testing.T) {
	// Store-opens race from many goroutines; the memo must be race-free and must
	// not recompute unboundedly. compute runs outside the lock, so a cold-start
	// burst may compute a few times, but once populated all callers hit the cache.
	memo := newPreflightScopeMemo[int]()
	var computes int64
	var wg sync.WaitGroup
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				v, err := memo.getOrCompute("shared", func() (int, error) {
					atomic.AddInt64(&computes, 1)
					return 42, nil
				})
				if err != nil || v != 42 {
					t.Errorf("got %d err %v, want 42/nil", v, err)
				}
			}
		}()
	}
	wg.Wait()
	// 3200 calls total; without caching that would be 3200 computes. A cold burst
	// permits a handful; assert it collapsed to well under the call count.
	if n := atomic.LoadInt64(&computes); n == 0 || n > 64 {
		t.Fatalf("computes = %d, want a small cold-start count well below 3200", n)
	}
}

// preflightScopeKey must fold the resolved backend target into the key so a
// config/backend change invalidates the cache; the same (cityPath, scope) with a
// different target must produce a different key.
func TestPreflightScopeKey_IncludesScopeAndTarget(t *testing.T) {
	k1 := preflightScopeKeyFor("/city", "rig-a", "host-1:3306/db|ext=false")
	k2 := preflightScopeKeyFor("/city", "rig-a", "host-2:3306/db|ext=false") // target changed
	k3 := preflightScopeKeyFor("/city", "rig-b", "host-1:3306/db|ext=false") // scope changed
	if k1 == k2 {
		t.Fatalf("a changed backend target must change the key: %q == %q", k1, k2)
	}
	if k1 == k3 {
		t.Fatalf("a different scope must change the key: %q == %q", k1, k3)
	}
	if k1 != preflightScopeKeyFor("/city", "rig-a", "host-1:3306/db|ext=false") {
		t.Fatalf("same inputs must produce the same key (deterministic)")
	}
}
