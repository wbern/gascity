package beads

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/gastownhall/gascity/internal/rollout/gate"
	beadslib "github.com/steveyegge/beads"
)

// TestNativeDoltStoreMetadataCASPreservesMixedJSONSiblingTypes protects the
// native metadata boundary that the string-valued Store projection otherwise
// hides. A value-CAS changes exactly one logical string key; boolean, number,
// null, object, array, and string siblings must retain their JSON types.
func TestNativeDoltStoreMetadataCASPreservesMixedJSONSiblingTypes(t *testing.T) {
	const id = "gc-mixed-metadata"
	durable := &beadslib.Issue{
		ID:        id,
		Title:     "mixed metadata CAS",
		Status:    beadslib.StatusOpen,
		IssueType: beadslib.TypeTask,
		Priority:  2,
		Metadata: json.RawMessage(`{
		"lease":"old",
		"bool_sibling":true,
		"number_sibling":42,
		"large_number_sibling":9007199254740993123456789,
		"null_sibling":null,
		"object_sibling":{"nested":"value"},
		"array_sibling":[1,"two",false],
		"string_sibling":"preserved"
	}`),
	}
	storage := &nativeDoltStorageSpy{
		getIssue: func(context.Context, string) (*beadslib.Issue, error) {
			return cloneNativeIssueForTest(durable), nil
		},
		updateIssue: func(_ context.Context, _ string, updates map[string]interface{}, _ string) error {
			raw, ok := updates["metadata"].(json.RawMessage)
			if !ok {
				t.Fatalf("metadata update type = %T, want json.RawMessage", updates["metadata"])
			}
			durable.Metadata = append(json.RawMessage(nil), raw...)
			return nil
		},
	}
	store := newNativeDoltStoreForTest(storage)

	swapped, err := store.CompareAndSetMetadataKey(id, "lease", "old", "1")
	if err != nil || !swapped {
		t.Fatalf("CompareAndSetMetadataKey = (%v, %v), want (true, nil)", swapped, err)
	}
	assertMixedMetadataCASResult(t, durable.Metadata, "9007199254740993123456789")
}

func assertMixedMetadataCASResult(t *testing.T, raw json.RawMessage, wantLargeNumber string) {
	t.Helper()
	var got map[string]interface{}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("decode durable metadata: %v", err)
	}
	var want map[string]interface{}
	if err := json.Unmarshal([]byte(`{
		"lease":"1",
		"bool_sibling":true,
		"number_sibling":42,
		"large_number_sibling":9007199254740993123456789,
		"null_sibling":null,
		"object_sibling":{"nested":"value"},
		"array_sibling":[1,"two",false],
		"string_sibling":"preserved"
	}`), &want); err != nil {
		t.Fatalf("decode expected metadata: %v", err)
	}
	var wantLarge interface{}
	if err := json.Unmarshal([]byte(wantLargeNumber), &wantLarge); err != nil {
		t.Fatalf("decode expected large number: %v", err)
	}
	want["large_number_sibling"] = wantLarge
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("durable metadata = %#v, want %#v; raw=%s", got, want, raw)
	}
	var rawValues map[string]json.RawMessage
	if err := json.Unmarshal(raw, &rawValues); err != nil {
		t.Fatalf("decode raw durable metadata: %v", err)
	}
	if value := string(rawValues["large_number_sibling"]); value != wantLargeNumber {
		t.Fatalf("large numeric sibling = %s, want exact %s", value, wantLargeNumber)
	}
}

func TestNativeDoltStoreDeclaresConditionalWriterAndProbesPinnedStorageContract(t *testing.T) {
	store := newNativeDoltStoreForTest(newNativeDoltMemStorage())

	if _, ok := ConditionalWriterFor(store); !ok {
		t.Fatal("NativeDoltStore does not resolve a ConditionalWriter")
	}
	if _, ok := MetadataCASWriterFor(store); !ok {
		t.Fatal("NativeDoltStore does not resolve a MetadataCASWriter")
	}
	if capable, reason := store.probeConditionalWriteCapability(); !capable {
		t.Fatalf("pinned backend capability = false (%s), want true", reason)
	}

	compiledStorage := newNativeDoltStoreForTest(&nativeDoltStorageSpy{})
	if capable, reason := compiledStorage.probeConditionalWriteCapability(); !capable {
		t.Fatalf("compiled Storage capability = false (%s), want true", reason)
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

// TestNativeDoltStoreConditionalWritesResolveForPinnedStorageModes pins the
// mode seam over the compile-time Storage contract. The pinned upstream
// interface requires checked update/close and transactions, so there is no
// runtime "older backend" shape hidden behind the same interface.
func TestNativeDoltStoreConditionalWritesResolveForPinnedStorageModes(t *testing.T) {
	for _, mode := range []gate.Mode{gate.Require, gate.Auto} {
		t.Run(string(mode), func(t *testing.T) {
			store := newNativeDoltStoreForTest(newNativeDoltMemStorage())
			store.stampConditionalWritesMode(mode, false)

			writer, diag, err := ResolveConditionalWriter(store)
			if writer == nil || diag != nil || err != nil {
				t.Fatalf("ResolveConditionalWriter = (%T, %+v, %v), want writer, nil, nil", writer, diag, err)
			}
		})
	}
}

// TestCachingStoreOverNativeDoltStoreForwardsConditionalWrites covers the
// production wrapper shape: the cache must preserve both metadata CAS and the
// guarded whole-row writer advertised by its native backing.
func TestCachingStoreOverNativeDoltStoreForwardsConditionalWrites(t *testing.T) {
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

	if capable, reason := cache.probeConditionalWriteCapability(); !capable {
		t.Fatalf("CachingStore reports conditional-write capability = false (%s)", reason)
	}
	if _, ok := ConditionalWriterFor(cache); !ok {
		t.Fatal("CachingStore over NativeDoltStore does not resolve a ConditionalWriter")
	}
}

// TestNativeDoltStoreMetadataCASRetryDoesNotLeakPriorAttemptResult models the
// whole-callback retry performed by beads/Dolt RunInTransaction. A first
// callback reaches UpdateIssue, then the retry observes a competitor's value.
// The durable result is a lost race, regardless of what the abandoned callback
// wrote before the retry.
func TestNativeDoltStoreMetadataCASRetryDoesNotLeakPriorAttemptResult(t *testing.T) {
	storage := &nativeDoltMetadataCASRetryStorage{
		nativeDoltMemStorage: newNativeDoltMemStorage(),
		key:                  "lease",
		competitor:           "holder-2",
	}
	store := newNativeDoltStoreForTest(storage)
	created, err := store.Create(Bead{
		Title:    "retry-safe-metadata-cas",
		Metadata: map[string]string{"lease": "unclaimed"},
	})
	if err != nil {
		t.Fatal(err)
	}
	storage.id = created.ID
	storage.retryCAS = true

	writer, ok := MetadataCASWriterFor(store)
	if !ok {
		t.Fatal("NativeDoltStore does not resolve a MetadataCASWriter")
	}
	swapped, err := writer.CompareAndSetMetadataKey(
		created.ID,
		storage.key,
		"unclaimed",
		"holder-1",
	)
	if err != nil {
		t.Fatalf("CompareAndSetMetadataKey: %v", err)
	}
	if swapped {
		t.Fatal("CompareAndSetMetadataKey = true after retry lost to competitor")
	}
	if storage.callbackCalls != 2 {
		t.Fatalf("transaction callback calls = %d, want 2", storage.callbackCalls)
	}
	got, err := store.Get(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if value := got.Metadata[storage.key]; value != storage.competitor {
		t.Fatalf("durable metadata[%q] = %q, want competitor %q", storage.key, value, storage.competitor)
	}
}

type nativeDoltMetadataCASRetryStorage struct {
	*nativeDoltMemStorage
	id            string
	key           string
	competitor    string
	retryCAS      bool
	callbackCalls int
}

func (s *nativeDoltMetadataCASRetryStorage) RunInTransaction(
	ctx context.Context,
	_ string,
	fn func(beadslib.Transaction) error,
) error {
	if !s.retryCAS {
		return s.nativeDoltMemStorage.RunInTransaction(ctx, "", fn)
	}

	tx := nativeDoltTransactionForTest{storage: s.nativeDoltMemStorage}
	// Snapshot the durable state before the first callback. The callback's
	// UpdateIssue is deliberately rolled back below to model a commit-phase
	// failure: it may have set the caller's local result flag, but it never
	// linearized in the store.
	s.store.mu.Lock()
	seq, beads, deps := s.store.snapshot()
	s.store.mu.Unlock()
	s.callbackCalls++
	if err := fn(tx); err != nil {
		return err
	}
	s.store.restoreFrom(seq, beads, deps)

	raw, err := metadataRawFromMap(map[string]string{s.key: s.competitor})
	if err != nil {
		return err
	}
	if err := s.UpdateIssue(
		ctx,
		s.id,
		map[string]interface{}{"metadata": raw},
		"competitor",
	); err != nil {
		return err
	}

	s.callbackCalls++
	return fn(tx)
}
