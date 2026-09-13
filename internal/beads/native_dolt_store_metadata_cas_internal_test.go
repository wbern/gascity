package beads

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/gastownhall/gascity/internal/rollout/gate"
	beadslib "github.com/steveyegge/beads"
)

// TestNativeDoltStoreDeclaresNarrowCASButNotConditionalWriter pins the exact
// capability split the narrow interface exists to express: NativeDoltStore
// offers a sound metadata value-CAS and makes NO revision-fence claim.
//
// Declaring the full ConditionalWriter would make ResolveConditionalWriter
// RESOLVE under require mode and hand the revision-CAS trio's callers a
// silently wrong-fenced write, because no sound revision token exists at
// beads v1.1.0 (see internal/beads/metadata_cas.go). This asserts the ABSENCE
// of that capability, which no conformance suite can do.
func TestNativeDoltStoreDeclaresNarrowCASButNotConditionalWriter(t *testing.T) {
	store := newNativeDoltStoreForTest(newNativeDoltMemStorage())

	if w, ok := ConditionalWriterFor(store); ok {
		t.Fatalf("NativeDoltStore resolved a ConditionalWriter (%T); the revision-CAS trio has "+
			"no sound backend fence at beads v1.1.0, so declaring it is a safety regression", w)
	}
	if _, ok := MetadataCASWriterFor(store); !ok {
		t.Fatal("NativeDoltStore does not resolve a MetadataCASWriter; the narrow value-CAS " +
			"capability is what unblocks target_scope member-declaration and the D3/D5 lease lane")
	}
	if _, ok := MetadataPatchCASWriterFor(store); !ok {
		t.Fatal("NativeDoltStore does not resolve the atomic metadata-patch capability required by legacy publication")
	}
}

func TestNativeDoltStoreMetadataPatchCASAppliesCompletePatchAndRejectsStaleSnapshot(t *testing.T) {
	storage := newNativeDoltMemStorage()
	store := newNativeDoltStoreForTest(storage)
	b, err := store.Create(Bead{Title: "native-patch", Metadata: map[string]string{"molecule_id": "", "gc.routed_to": ""}})
	if err != nil {
		t.Fatal(err)
	}
	expected, err := store.Get(b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if swapped, err := store.CompareAndSetMetadataPatch(b.ID, expected, map[string]string{"molecule_id": "root", "gc.routed_to": "reviewer"}); err != nil || !swapped {
		t.Fatalf("patch = (%v, %v), want (true, nil)", swapped, err)
	}
	issue, err := storage.GetIssue(context.Background(), b.ID)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(issue.Metadata, &raw); err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]string{"molecule_id": `"root"`, "gc.routed_to": `"reviewer"`} {
		if string(raw[key]) != want {
			t.Fatalf("raw metadata[%q] = %s, want %s", key, raw[key], want)
		}
	}
	if swapped, err := store.CompareAndSetMetadataPatch(b.ID, expected, map[string]string{"molecule_id": "stale"}); err != nil || swapped {
		t.Fatalf("stale patch = (%v, %v), want (false, nil)", swapped, err)
	}
}

func TestNativeDoltStoreMetadataPatchCASRejectsMalformedWithoutUpdate(t *testing.T) {
	const malformed = `{"broken":`
	updates := 0
	storage := &nativeDoltStorageSpy{
		getIssue: func(context.Context, string) (*beadslib.Issue, error) {
			return &beadslib.Issue{ID: "gc-malformed", Status: "open", Metadata: json.RawMessage(malformed)}, nil
		},
		updateIssue: func(context.Context, string, map[string]interface{}, string) error {
			updates++
			return nil
		},
	}
	store := newNativeDoltStoreForTest(storage)
	swapped, err := store.CompareAndSetMetadataPatch("gc-malformed", Bead{ID: "gc-malformed", Status: "open"}, map[string]string{"molecule_id": "unsafe"})
	if err == nil || swapped {
		t.Fatalf("malformed patch = (%v, %v), want fail closed", swapped, err)
	}
	if updates != 0 {
		t.Fatalf("malformed patch issued %d updates, want 0", updates)
	}
}

func TestNativeDoltStoreMetadataPatchCASRejectsStalePlainFields(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*NativeDoltStore, string) error
	}{
		{name: "status", mutate: func(store *NativeDoltStore, id string) error {
			status := "in_progress"
			return store.Update(id, UpdateOpts{Status: &status})
		}},
		{name: "assignee", mutate: func(store *NativeDoltStore, id string) error {
			assignee := "other-writer"
			return store.Update(id, UpdateOpts{Assignee: &assignee})
		}},
		{name: "parent", mutate: func(store *NativeDoltStore, id string) error {
			parent, err := store.Create(Bead{Title: "new parent"})
			if err != nil {
				return err
			}
			return store.Update(id, UpdateOpts{ParentID: &parent.ID})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := newNativeDoltStoreForTest(newNativeDoltMemStorage())
			b, err := store.Create(Bead{Title: "snapshot", Metadata: map[string]string{"molecule_id": ""}})
			if err != nil {
				t.Fatal(err)
			}
			expected, err := store.Get(b.ID)
			if err != nil {
				t.Fatal(err)
			}
			if err := tc.mutate(store, b.ID); err != nil {
				t.Fatal(err)
			}
			swapped, err := store.CompareAndSetMetadataPatch(b.ID, expected, map[string]string{"molecule_id": "unsafe"})
			if err != nil || swapped {
				t.Fatalf("patch after stale %s = (%v, %v), want (false, nil)", tc.name, swapped, err)
			}
			got, err := store.Get(b.ID)
			if err != nil {
				t.Fatal(err)
			}
			if got.Metadata["molecule_id"] != "" {
				t.Fatalf("stale %s snapshot wrote metadata: %#v", tc.name, got.Metadata)
			}
		})
	}
}

func TestNativeDoltStoreMetadataPatchCASInternalCallbackReplayCannotLeakSuccess(t *testing.T) {
	metadata := json.RawMessage(`{"molecule_id":"","gc.routed_to":""}`)
	callbackCalls := 0
	storage := &nativeDoltStorageSpy{}
	storage.getIssue = func(context.Context, string) (*beadslib.Issue, error) {
		return &beadslib.Issue{ID: "gc-replay", Status: "open", Metadata: append(json.RawMessage(nil), metadata...)}, nil
	}
	storage.updateIssue = func(_ context.Context, _ string, updates map[string]interface{}, _ string) error {
		metadata = append(json.RawMessage(nil), updates["metadata"].(json.RawMessage)...)
		return nil
	}
	storage.runInTransaction = func(_ context.Context, _ string, fn func(beadslib.Transaction) error) error {
		tx := nativeDoltTransactionForTest{storage: storage}
		callbackCalls++
		if err := fn(tx); err != nil {
			return err
		}
		// Model upstream replay after the first callback's commit conflict: its
		// write was rolled back, then an independent writer committed first.
		metadata = json.RawMessage(`{"molecule_id":"root-other","gc.routed_to":"reviewer-other"}`)
		callbackCalls++
		return fn(tx)
	}
	store := newNativeDoltStoreForTest(storage)
	expected := Bead{ID: "gc-replay", Status: "open", Metadata: map[string]string{"molecule_id": "", "gc.routed_to": ""}}
	swapped, err := store.CompareAndSetMetadataPatch("gc-replay", expected, map[string]string{"molecule_id": "root-us", "gc.routed_to": "reviewer-us"})
	if err != nil || swapped {
		t.Fatalf("callback replay result = (%v, %v), want (false, nil)", swapped, err)
	}
	if callbackCalls != 2 {
		t.Fatalf("callback calls = %d, want 2", callbackCalls)
	}
	if string(metadata) != `{"molecule_id":"root-other","gc.routed_to":"reviewer-other"}` {
		t.Fatalf("winner metadata changed: %s", metadata)
	}
}

// TestNativeDoltStoreConditionalWritesStillRefuseOrDegrade pins the seam
// behavior the condWritesStamp comment in native_dolt_store.go guarantees:
// require yields a typed refusal and auto yields a loud degrade — never a
// silent legacy write under require. Adding the narrow CAS must not move
// either verdict.
func TestNativeDoltStoreConditionalWritesStillRefuseOrDegrade(t *testing.T) {
	t.Run("require_refuses", func(t *testing.T) {
		store := newNativeDoltStoreForTest(newNativeDoltMemStorage())
		store.stampConditionalWritesMode(gate.Require, false)

		writer, diag, err := ResolveConditionalWriter(store)
		if writer != nil {
			t.Fatalf("writer = %T, want nil (require must fail closed)", writer)
		}
		if !IsConditionalWritesRequired(err) {
			t.Fatalf("err = %v, want *ConditionalWritesRequiredError", err)
		}
		if diag == nil {
			t.Fatal("diagnostic = nil, want a refusal diagnostic")
		}
	})

	t.Run("auto_degrades_loudly", func(t *testing.T) {
		store := newNativeDoltStoreForTest(newNativeDoltMemStorage())
		store.stampConditionalWritesMode(gate.Auto, false)

		writer, diag, err := ResolveConditionalWriter(store)
		if writer != nil {
			t.Fatalf("writer = %T, want nil (auto must take the legacy path)", writer)
		}
		if err != nil {
			t.Fatalf("err = %v, want nil (auto degrades, it does not refuse)", err)
		}
		if diag == nil {
			t.Fatal("diagnostic = nil, want a loud-degrade diagnostic")
		}
	})
}

// TestCachingStoreOverNativeDoltStoreForwardsNarrowCAS covers the wrapper
// shape the plan calls out: a CachingStore whose backing offers only the
// narrow capability must still forward the metadata CAS. The cache resolves
// its trio verbs through ConditionalWriterFor, so without a narrow fallback
// this path would answer ErrConditionalWriteUnsupported and the lease lane
// would be blocked behind the cache.
func TestCachingStoreOverNativeDoltStoreForwardsNarrowCAS(t *testing.T) {
	backing := newNativeDoltStoreForTest(newNativeDoltMemStorage())
	cache := NewCachingStore(backing, nil)

	b, err := cache.Create(Bead{Title: "cache-over-native-cas"})
	if err != nil {
		t.Fatal(err)
	}

	writer, ok := MetadataCASWriterFor(cache)
	if !ok {
		t.Fatal("CachingStore over a narrow-CAS backing does not resolve a MetadataCASWriter")
	}
	if swapped, err := writer.CompareAndSetMetadataKey(b.ID, "lease", "", "holder-1"); err != nil || !swapped {
		t.Fatalf("claim through cache: (%v, %v), want (true, nil)", swapped, err)
	}
	// A stale expectation loses cleanly rather than erroring.
	if swapped, err := writer.CompareAndSetMetadataKey(b.ID, "lease", "", "holder-2"); err != nil || swapped {
		t.Fatalf("stale claim through cache: (%v, %v), want (false, nil)", swapped, err)
	}
	// The winner's value is visible through the cache (the CAS evicted, so the
	// next read consults the backing).
	got, err := cache.Get(b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Metadata["lease"] != "holder-1" {
		t.Fatalf("lease through cache = %q, want %q", got.Metadata["lease"], "holder-1")
	}

	// The trio stays refused: the backing makes no revision claim, so the
	// cache must not report itself conditionally capable over it.
	if capable, _ := cache.probeConditionalWriteCapability(); capable {
		t.Fatal("CachingStore reports conditional-write capability over a narrow-only backing; " +
			"the revision-CAS trio has no sound fence there")
	}
}
