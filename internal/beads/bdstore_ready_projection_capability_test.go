package beads

import (
	"fmt"
	"strings"
	"sync"
	"testing"
)

// countingReadyProjectionRunner returns a CommandRunner that answers every
// `bd version` probe with version, answers `bd sql` with a one-row projection,
// and counts both. The counters are shared so a caller can assert how many
// subprocesses a sequence of stores actually spawned.
func countingReadyProjectionRunner(version string, versionCalls, sqlCalls *int) CommandRunner {
	var mu sync.Mutex
	return func(_, name string, args ...string) ([]byte, error) {
		joined := name + " " + strings.Join(args, " ")
		mu.Lock()
		defer mu.Unlock()
		switch {
		case joined == "bd version":
			*versionCalls++
			return []byte("bd version " + version + "\n"), nil
		case len(args) > 0 && args[0] == "sql":
			*sqlCalls++
			return []byte(`[{"id":"gcg-task","is_blocked":false}]`), nil
		}
		return nil, fmt.Errorf("unexpected command: %s", joined)
	}
}

func readyProjectionTestItems() []Bead {
	return []Bead{{ID: "gcg-task", Type: "task", Status: "open"}}
}

// TestReadyProjectionSharedCapabilityProbesOncePerDir is the regression guard
// for gcw-clnxz.
//
// bdReadyProjectionEnabled memoizes the `bd version` capability probe and its
// comment promised the probe happens "once per process". That promise was false
// in production: the control dispatcher opens a fresh store on every tick
// (makeSourceWorkflowStoresLister), so a memo living on the BdStore instance was
// cold every time and the probe degenerated into one bd subprocess per
// reconcile. Measured on the live gc2 city, `bd version` and `bd sql` appeared
// in the shim route log at exactly 1:1 — 1,488 each in a 50-minute window, ~20
// pairs/min — burning 211 seconds of wall clock re-reading a constant that
// cannot change without an operator restart.
//
// Stores sharing a capability cache must probe once. The projection fetch itself
// is real work and must still run on every store.
func TestReadyProjectionSharedCapabilityProbesOncePerDir(t *testing.T) {
	versionCalls, sqlCalls := 0, 0
	runner := countingReadyProjectionRunner("1.1.0", &versionCalls, &sqlCalls)
	cache := NewReadyProjectionCapabilityCache()
	const dir = "/city/gcw-clnxz/shared"

	for i := 0; i < 3; i++ {
		// A new store per iteration is exactly what the dispatcher tick does.
		store := NewBdStore(dir, runner, WithBdStoreReadyProjectionCapability(cache))
		out, err := store.enrichReadyProjectionForCache(readyProjectionTestItems())
		if err != nil {
			t.Fatalf("enrichReadyProjectionForCache (store %d): %v", i, err)
		}
		if got := out[0].IsBlocked; got == nil || *got {
			t.Fatalf("store %d: IsBlocked = %v, want &false — the projection must still be applied", i, got)
		}
	}

	if versionCalls != 1 {
		t.Errorf("bd version probes = %d, want 1 — a shared capability cache must survive store reconstruction (gcw-clnxz)", versionCalls)
	}
	if sqlCalls != 3 {
		t.Errorf("bd sql fetches = %d, want 3 — the projection fetch is real work and must not be cached", sqlCalls)
	}
}

// TestReadyProjectionPrivateCapabilityIsPerStore pins the DEFAULT: a store built
// without an injected cache keeps its own memo, exactly as before this change.
// Every existing caller and test relies on that isolation, so the shared cache
// must be opt-in rather than an ambient process-wide global.
func TestReadyProjectionPrivateCapabilityIsPerStore(t *testing.T) {
	versionCalls, sqlCalls := 0, 0
	runner := countingReadyProjectionRunner("1.1.0", &versionCalls, &sqlCalls)
	const dir = "/city/gcw-clnxz/private"

	for i := 0; i < 3; i++ {
		if _, err := NewBdStore(dir, runner).enrichReadyProjectionForCache(readyProjectionTestItems()); err != nil {
			t.Fatalf("enrichReadyProjectionForCache (store %d): %v", i, err)
		}
	}

	if versionCalls != 3 {
		t.Errorf("bd version probes = %d, want 3 — an un-injected store must not consult shared state", versionCalls)
	}
}

// TestReadyProjectionCapabilityMemoizesWithinOneStore guards the behavior the
// original instance memo provided and that must survive: repeated enrichment on
// a single store probes once, not once per call.
func TestReadyProjectionCapabilityMemoizesWithinOneStore(t *testing.T) {
	versionCalls, sqlCalls := 0, 0
	store := NewBdStore("/city/gcw-clnxz/one-store", countingReadyProjectionRunner("1.1.0", &versionCalls, &sqlCalls))

	for i := 0; i < 3; i++ {
		if _, err := store.enrichReadyProjectionForCache(readyProjectionTestItems()); err != nil {
			t.Fatalf("enrichReadyProjectionForCache (call %d): %v", i, err)
		}
	}

	if versionCalls != 1 {
		t.Errorf("bd version probes = %d, want 1 — the per-store memo must still hold", versionCalls)
	}
	if sqlCalls != 3 {
		t.Errorf("bd sql fetches = %d, want 3", sqlCalls)
	}
}

// TestReadyProjectionCapabilityFailureIsNotCached guards the error semantics the
// original memo had: readyProjectionChecked was only set on a SUCCESSFUL probe,
// so a transient bd failure could not permanently disable enrichment. Caching a
// failure would turn a blip into a process-lifetime outage of ready enrichment.
func TestReadyProjectionCapabilityFailureIsNotCached(t *testing.T) {
	var mu sync.Mutex
	versionCalls := 0
	runner := func(_, name string, args ...string) ([]byte, error) {
		joined := name + " " + strings.Join(args, " ")
		mu.Lock()
		defer mu.Unlock()
		switch {
		case joined == "bd version":
			versionCalls++
			if versionCalls == 1 {
				return nil, fmt.Errorf("bd exploded")
			}
			return []byte("bd version 1.1.0\n"), nil
		case len(args) > 0 && args[0] == "sql":
			return []byte(`[{"id":"gcg-task","is_blocked":false}]`), nil
		}
		return nil, fmt.Errorf("unexpected command: %s", joined)
	}
	cache := NewReadyProjectionCapabilityCache()
	const dir = "/city/gcw-clnxz/failure"

	first := NewBdStore(dir, runner, WithBdStoreReadyProjectionCapability(cache))
	if _, err := first.enrichReadyProjectionForCache(readyProjectionTestItems()); err == nil {
		t.Fatal("first enrich: want probe error, got nil")
	}

	second := NewBdStore(dir, runner, WithBdStoreReadyProjectionCapability(cache))
	out, err := second.enrichReadyProjectionForCache(readyProjectionTestItems())
	if err != nil {
		t.Fatalf("second enrich: %v — a failed probe must be retried, never cached", err)
	}
	if got := out[0].IsBlocked; got == nil || *got {
		t.Errorf("IsBlocked = %v, want &false — enrichment must recover after a transient probe failure", got)
	}
	if versionCalls != 2 {
		t.Errorf("bd version probes = %d, want 2 (one failed, one retried)", versionCalls)
	}
}

// TestReadyProjectionCapabilityIsScopedPerDir guards against the naive fix: a
// single answer for the whole process. Stores rooted at different directories
// can resolve different bd binaries (workspacePinnedBdBinary resolves bd from
// the owning city's workspace PATH), so one directory's verdict must never be
// served to another. Below-minimum stays disabled and is never queried;
// at-or-above stays enabled.
func TestReadyProjectionCapabilityIsScopedPerDir(t *testing.T) {
	oldVersionCalls, oldSQLCalls := 0, 0
	newVersionCalls, newSQLCalls := 0, 0
	cache := NewReadyProjectionCapabilityCache()

	oldStore := NewBdStore("/city/gcw-clnxz/old-bd",
		countingReadyProjectionRunner("1.0.4", &oldVersionCalls, &oldSQLCalls),
		WithBdStoreReadyProjectionCapability(cache))
	outOld, err := oldStore.enrichReadyProjectionForCache(readyProjectionTestItems())
	if err != nil {
		t.Fatalf("old-bd enrich: %v", err)
	}
	if outOld[0].IsBlocked != nil {
		t.Errorf("old-bd IsBlocked = %v, want nil — bd below %s does not support the projection", outOld[0].IsBlocked, bdReadyProjectionMinVersion)
	}
	if oldSQLCalls != 0 {
		t.Errorf("old-bd sql fetches = %d, want 0 — an unsupported bd must not be queried", oldSQLCalls)
	}

	newStore := NewBdStore("/city/gcw-clnxz/new-bd",
		countingReadyProjectionRunner("1.1.0", &newVersionCalls, &newSQLCalls),
		WithBdStoreReadyProjectionCapability(cache))
	outNew, err := newStore.enrichReadyProjectionForCache(readyProjectionTestItems())
	if err != nil {
		t.Fatalf("new-bd enrich: %v", err)
	}
	if got := outNew[0].IsBlocked; got == nil || *got {
		t.Errorf("new-bd IsBlocked = %v, want &false — a different directory must get its own capability answer", got)
	}
}
