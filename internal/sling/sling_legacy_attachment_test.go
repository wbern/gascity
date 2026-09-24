package sling

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/molecule"
	"github.com/gastownhall/gascity/internal/runtime"
)

type legacyLinkFailureStore struct{ *beads.MemStore }

func TestLegacyAttachmentPointerCannotOverrideLiveRootState(t *testing.T) {
	store := seededStore("work")
	root, _ := store.Create(beads.Bead{Type: "molecule", Status: "open", ParentID: "work"})
	if err := store.SetMetadata("work", beadmeta.MoleculeIDMetadataKey, root.ID); err != nil {
		t.Fatal(err)
	}
	cache := beads.NewCachingStoreForTest(store, nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(root.ID); err != nil {
		t.Fatal(err)
	}
	source, _ := store.Get("work")
	roots, err := CollectAttachedBeads(source, cache, cache)
	if err != nil || len(roots) != 1 || roots[0].Status != "closed" {
		t.Fatalf("cached attachment pointer overrode live closure: %v, %v", roots, err)
	}
}

type legacyUncertainLinkStore struct{ *beads.MemStore }

func (s legacyUncertainLinkStore) UpdateIfMatch(id string, revision int64, opts beads.UpdateOpts) error {
	if err := s.MemStore.UpdateIfMatch(id, revision, opts); err != nil {
		return err
	}
	return errors.New("publication acknowledgement lost")
}

func TestLegacyAmbiguousPublicationRetainsPublishedFamily(t *testing.T) {
	store := legacyUncertainLinkStore{MemStore: seededStore("work")}
	deps := testDeps(&config.City{Workspace: config.Workspace{Name: "test"}}, runtime.NewFake(), func(string, string, map[string]string) (string, error) { return "", nil })
	deps.Store, deps.CityPath = store, t.TempDir()
	a := config.Agent{Name: "worker", MaxActiveSessions: intPtr(1)}
	opts := SlingOpts{Target: a, BeadOrFormula: "work", OnFormula: "code-review", NoConvoy: true}
	if _, err := DoSling(opts, deps, store); err == nil {
		t.Fatal("lost acknowledgement reported success")
	}
	source, _ := store.Get("work")
	root, err := store.Get(source.Metadata[beadmeta.MoleculeIDMetadataKey])
	if err != nil || root.Status != "open" {
		t.Fatalf("ambiguous publication rolled back published family: %+v, %v", root, err)
	}
	res, err := DoSling(opts, deps, store)
	if err != nil || !res.Idempotent {
		t.Fatalf("retry did not converge to published family: %+v, %v", res, err)
	}
}

func TestLegacyBatchAndSingleEntryUseSameAdmission(t *testing.T) {
	store := seededStore("work")
	deps := testDeps(&config.City{Workspace: config.Workspace{Name: "test"}}, runtime.NewFake(), func(string, string, map[string]string) (string, error) { return "", nil })
	deps.Store, deps.CityPath = store, t.TempDir()
	a := config.Agent{Name: "worker", MaxActiveSessions: intPtr(1)}
	child, _ := store.Get("work")
	opts := SlingOpts{Target: a, NoConvoy: true}
	batch, err := attachBatchFormula(context.Background(), opts, deps, child, a, "code-review", "formula", "batch-on", false)
	if err != nil {
		t.Fatal(err)
	}
	single, err := attachFormulaToBead(opts, deps, store, child.ID, "code-review", "on-formula", "formula", false, SlingResult{})
	if err != nil || single.WispRootID != batch.WispRootID {
		t.Fatalf("batch and single attachment diverged: %+v / %+v, %v", batch, single, err)
	}
}

type legacyClaimRaceStore struct{ *beads.MemStore }

func (s legacyClaimRaceStore) UpdateIfMatch(id string, revision int64, opts beads.UpdateOpts) error {
	assignee := "new-owner"
	if err := s.Update(id, beads.UpdateOpts{Assignee: &assignee}); err != nil {
		return err
	}
	return s.MemStore.UpdateIfMatch(id, revision, opts)
}

func TestLegacyPublicationCASPreservesConcurrentClaim(t *testing.T) {
	store := legacyClaimRaceStore{MemStore: seededStore("work")}
	deps := testDeps(&config.City{Workspace: config.Workspace{Name: "test"}}, runtime.NewFake(), func(string, string, map[string]string) (string, error) {
		t.Error("source claimed during publication was routed")
		return "", nil
	})
	deps.Store, deps.CityPath = store, t.TempDir()
	_, err := DoSling(SlingOpts{Target: config.Agent{Name: "worker", MaxActiveSessions: intPtr(1)}, BeadOrFormula: "work", OnFormula: "code-review", NoConvoy: true}, deps, store)
	if err == nil {
		t.Fatal("CAS lost a concurrent claim but returned success")
	}
	source, _ := store.Get("work")
	if source.Assignee != "new-owner" || source.Metadata[beadmeta.MoleculeIDMetadataKey] != "" {
		t.Fatalf("publication overwrote new custody: %+v", source)
	}
}

func TestLegacyClosedRootWithLiveDescendantIsNotForgotten(t *testing.T) {
	store := seededStore("work")
	root, _ := store.Create(beads.Bead{Type: "molecule", Status: "closed", Metadata: map[string]string{"gc.var.issue": "work"}})
	child, _ := store.Create(beads.Bead{Type: "step", Status: "open", ParentID: root.ID, Assignee: "live-reviewer"})
	deps := SlingDeps{Store: store, CityPath: t.TempDir()}
	_, err := withLegacyAttachment(context.Background(), deps, "work", "review", nil, func() (*molecule.Result, error) {
		t.Fatal("forgot the open descendant beneath historical closed root")
		return nil, nil
	}, func(*molecule.Result) (SlingResult, error) { return SlingResult{}, nil })
	if err == nil {
		t.Fatal("expected family reconciliation refusal")
	}
	after, _ := store.Get(child.ID)
	if after.Status != "open" || after.Assignee != "live-reviewer" {
		t.Fatalf("live descendant was mutated: %+v", after)
	}
}

func (s legacyLinkFailureStore) UpdateIfMatch(string, int64, beads.UpdateOpts) error {
	return errors.New("link write failed")
}

func TestLegacyRepeatedRouteKeepsOneFamily(t *testing.T) {
	store := seededStore("work")
	deps := testDeps(&config.City{Workspace: config.Workspace{Name: "test"}}, runtime.NewFake(), func(string, string, map[string]string) (string, error) {
		t.Error("legacy route invoked unfenced Runner")
		return "", nil
	})
	deps.Store, deps.CityPath = store, t.TempDir()
	a := config.Agent{Name: "worker", MaxActiveSessions: intPtr(1)}
	opts := SlingOpts{Target: a, BeadOrFormula: "work", OnFormula: "code-review", NoConvoy: true}
	if _, err := DoSling(opts, deps, store); err != nil {
		t.Fatal(err)
	}
	before, _ := store.Get("work")
	result, err := DoSling(opts, deps, store)
	if err != nil || !result.Idempotent || before.Metadata[beadmeta.RoutedToMetadataKey] != "worker" {
		t.Fatalf("retry failed to reuse family: %+v, %v", result, err)
	}
	roots, _ := store.List(beads.ListQuery{Type: "molecule", IncludeClosed: true})
	if len(roots) != 1 {
		t.Fatalf("retry created/retired extra families: %v", roots)
	}
}

func TestLegacyClosedSourceAndChangedCustodyNeverRoute(t *testing.T) {
	for _, closeBefore := range []bool{false, true} {
		store := seededStore("work")
		deps := SlingDeps{Store: store, CityPath: t.TempDir()}
		if closeBefore {
			if err := store.Close("work"); err != nil {
				t.Fatal(err)
			}
		}
		created := false
		_, err := withLegacyAttachment(context.Background(), deps, "work", "review", nil, func() (*molecule.Result, error) {
			created = true
			root, err := store.Create(beads.Bead{Type: "molecule", Status: "open", ParentID: "work"})
			if err != nil {
				return nil, err
			}
			if err := store.Close("work"); err != nil {
				return nil, err
			}
			return &molecule.Result{RootID: root.ID}, nil
		}, func(*molecule.Result) (SlingResult, error) {
			t.Error("closed source was routed")
			return SlingResult{}, nil
		})
		if err == nil || (closeBefore && created) {
			t.Fatalf("terminal source created=%v err=%v", created, err)
		}
		roots, _ := store.List(beads.ListQuery{Type: "molecule"})
		if !closeBefore && (len(roots) != 1 || roots[0].Status != "open") {
			t.Fatalf("failed source fence did not retain its family %v", roots)
		}
	}
}

func TestLegacyDetachedOrIncompleteFamiliesRequireReconciliation(t *testing.T) {
	for _, state := range []string{"", "preparing"} {
		store := seededStore("work")
		root, _ := store.Create(beads.Bead{Type: "molecule", Status: "open", Metadata: map[string]string{
			"gc.var.issue": "work", beadmeta.FormulaNameMetadataKey: "review", legacyAttachmentStateKey: state,
		}})
		deps := SlingDeps{Store: store, CityPath: t.TempDir()}
		_, err := withLegacyAttachment(context.Background(), deps, "work", "review", nil, func() (*molecule.Result, error) {
			t.Fatal("created another family while a detached/incomplete root exists")
			return nil, nil
		}, func(*molecule.Result) (SlingResult, error) {
			t.Fatal("routed incomplete/unverified historical family")
			return SlingResult{}, nil
		})
		if err == nil {
			t.Fatal("missing reconciliation error")
		}
		after, _ := store.Get(root.ID)
		if after.Status != "open" || after.ParentID != "" {
			t.Fatal("admission mutated historical family")
		}
	}
}

func TestLegacyIndependentReviewSourcesRemainConcurrent(t *testing.T) {
	store := seededStore("code", "ux")
	deps := SlingDeps{Store: store, CityPath: t.TempDir()}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	entered := make(chan struct{}, 2)
	release := make(chan struct{})
	errs := make(chan error, 2)
	for _, source := range []string{"code", "ux"} {
		go func() {
			_, err := withLegacyAttachment(ctx, deps, source, "review", nil, func() (*molecule.Result, error) {
				entered <- struct{}{}
				select {
				case <-release:
				case <-ctx.Done():
					return nil, ctx.Err()
				}
				root, err := store.Create(beads.Bead{Type: "molecule", Status: "open", ParentID: source})
				return &molecule.Result{RootID: root.ID}, err
			}, func(root *molecule.Result) (SlingResult, error) { return SlingResult{WispRootID: root.RootID}, nil })
			errs <- err
		}()
	}
	for range 2 {
		select {
		case <-entered:
		case <-ctx.Done():
			t.Fatal("independent review contexts were serialized")
		}
	}
	close(release)
	for range 2 {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
}

func TestLegacyAttachmentProbeFailureAndDetachedFamilies(t *testing.T) {
	store := seededStore("work")
	probeErr := errors.New("probe unavailable")
	if err := CheckNoMoleculeChildren(listErrStore{Store: store, err: probeErr}, "work", store, &SlingResult{}); !errors.Is(err, probeErr) {
		t.Fatalf("attachment probe = %v, want fail closed", err)
	}
	for _, key := range []string{"gc.var.issue", beadmeta.SourceBeadIDMetadataKey} {
		if _, err := store.Create(beads.Bead{Type: "molecule", Status: "open", Metadata: map[string]string{key: "work"}}); err != nil {
			t.Fatal(err)
		}
	}
	parent, _ := store.Get("work")
	got, err := CollectAttachedBeads(parent, store, store)
	if err != nil || len(got) != 2 {
		t.Fatalf("detached source families = %v, %v; want both", got, err)
	}
}

func TestLegacyAttachmentPublishesParentAndRollsBackLinkFailure(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "linked", true: "link failure"}[fail], func(t *testing.T) {
			mem := seededStore("work")
			var store beads.Store = mem
			if fail {
				store = legacyLinkFailureStore{MemStore: mem}
			}
			routed := false
			deps := testDeps(&config.City{Workspace: config.Workspace{Name: "test"}}, runtime.NewFake(), func(string, string, map[string]string) (string, error) {
				routed = true
				source, _ := mem.Get("work")
				root, rootErr := mem.Get(source.Metadata[beadmeta.MoleculeIDMetadataKey])
				if rootErr != nil || root.ParentID != "work" || root.Metadata[beadmeta.SourceBeadIDMetadataKey] != "work" || root.Metadata[legacyAttachmentStateKey] != "ready" {
					t.Errorf("routing preceded durable attachment: root=%+v error=%v", root, rootErr)
				}
				return "", nil
			})
			deps.Store, deps.CityPath = store, t.TempDir()
			a := config.Agent{Name: "worker", MaxActiveSessions: intPtr(1)}
			result, err := DoSling(SlingOpts{Target: a, BeadOrFormula: "work", OnFormula: "code-review", NoConvoy: true}, deps, store)
			if fail {
				if err == nil || routed {
					t.Fatalf("failed metadata routed=%v err=%v", routed, err)
				}
				roots, _ := mem.List(beads.ListQuery{Type: "molecule"})
				if len(roots) != 1 || roots[0].Status != "open" {
					t.Fatalf("failed publication did not retain discoverable family: %v", roots)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			root, _ := mem.Get(result.WispRootID)
			if root.ParentID != "work" || root.Metadata[beadmeta.SourceBeadIDMetadataKey] != "work" {
				t.Fatalf("root was not discoverably attached: %+v", root)
			}
		})
	}
}

func TestLegacyConcurrentAttachmentConverges(t *testing.T) {
	store := seededStore("work")
	deps := testDeps(&config.City{Workspace: config.Workspace{Name: "test"}}, runtime.NewFake(), func(string, string, map[string]string) (string, error) { return "", nil })
	deps.Store, deps.CityPath = store, t.TempDir()
	a := config.Agent{Name: "worker", MaxActiveSessions: intPtr(1)}
	start := make(chan struct{})
	results := make(chan SlingResult, 8)
	errs := make(chan error, 8)
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			res, err := attachFormulaToBead(SlingOpts{Target: a, NoConvoy: true}, deps, store, "work", "code-review", "on-formula", "formula", false, SlingResult{})
			results <- res
			errs <- err
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	ids := map[string]bool{}
	for res := range results {
		ids[res.WispRootID] = true
	}
	if len(ids) != 1 || ids[""] {
		t.Fatalf("concurrent delivery created roots %v; want one", ids)
	}
}

func TestLegacyClosedHistoricalRootWithoutStoreRefDoesNotBlockSling(t *testing.T) {
	source := seededStore("work")
	graph := beads.NewMemStore()
	// Historical closed root has NO SourceStoreRefMetadataKey.
	histRoot, err := graph.Create(beads.Bead{
		Type: "molecule",
		Metadata: map[string]string{
			"gc.var.issue": "work",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := graph.Close(histRoot.ID); err != nil {
		t.Fatal(err)
	}
	_ = source.SetMetadata("work", beadmeta.MoleculeIDMetadataKey, histRoot.ID)
	deps := SlingDeps{
		Store:      source,
		GraphStore: graph,
		StoreRef:   "rig:crm",
		CityPath:   t.TempDir(),
	}
	poured := false
	_, err = withLegacyAttachment(context.Background(), deps, "work", "review", nil, func() (*molecule.Result, error) {
		poured = true
		newRoot, e := graph.Create(beads.Bead{Type: "molecule", Status: "open"})
		return &molecule.Result{RootID: newRoot.ID}, e
	}, func(r *molecule.Result) (SlingResult, error) {
		return SlingResult{WispRootID: r.RootID}, nil
	})
	if err != nil {
		t.Fatalf("closed historical root without store ref blocked sling: %v", err)
	}
	if !poured {
		t.Fatal("expected new molecule to be poured")
	}
}

func TestLegacyMissingStoreRefFallsBackToDepsStoreRef(t *testing.T) {
	source := seededStore("work")
	graph := beads.NewMemStore()
	// Open legacy root has NO SourceStoreRefMetadataKey, but is in the same store.
	openRoot, _ := graph.Create(beads.Bead{
		Type:   "molecule",
		Status: "open",
		Metadata: map[string]string{
			beadmeta.SourceBeadIDMetadataKey: "work",
			beadmeta.FormulaNameMetadataKey:  "review",
			legacyAttachmentStateKey:         "ready",
			"gc.var.issue":                   "work",
		},
	})
	_ = source.SetMetadata("work", beadmeta.MoleculeIDMetadataKey, openRoot.ID)
	deps := SlingDeps{
		Store:      source,
		GraphStore: graph,
		StoreRef:   "rig:crm",
		CityPath:   t.TempDir(),
	}
	// Should adopt the openRoot rather than failing with "no source-store identity"
	res, err := withLegacyAttachment(context.Background(), deps, "work", "review", map[string]string{"issue": "work"}, func() (*molecule.Result, error) {
		t.Fatal("unexpectedly poured fresh instead of adopting matching legacy root")
		return nil, nil
	}, func(r *molecule.Result) (SlingResult, error) {
		return SlingResult{WispRootID: r.RootID}, nil
	})
	if err != nil {
		t.Fatalf("missing store ref failed instead of falling back: %v", err)
	}
	if res.WispRootID != openRoot.ID {
		t.Fatalf("adopted root = %s, want %s", res.WispRootID, openRoot.ID)
	}
}

func TestLegacyReworkRetiresFailedMoleculeAndPoursFresh(t *testing.T) {
	source := seededStore("work")
	graph := beads.NewMemStore()
	// Molecule that failed on previous run:
	failedRoot, _ := graph.Create(beads.Bead{
		Type:   "molecule",
		Status: "open",
		Metadata: map[string]string{
			beadmeta.SourceBeadIDMetadataKey:   "work",
			beadmeta.SourceStoreRefMetadataKey: "rig:crm",
			beadmeta.FormulaNameMetadataKey:    "review",
			beadmeta.MoleculeFailedMetadataKey: "lint failure: 1001 lines exceeds 1000",
			legacyAttachmentStateKey:           "ready",
			"gc.var.issue":                     "work",
		},
	})
	failedChild, _ := graph.Create(beads.Bead{
		Type:     "step",
		Status:   "open",
		ParentID: failedRoot.ID,
	})
	_ = source.SetMetadata("work", beadmeta.MoleculeIDMetadataKey, failedRoot.ID)
	deps := SlingDeps{
		Store:      source,
		GraphStore: graph,
		StoreRef:   "rig:crm",
		CityPath:   t.TempDir(),
	}
	poured := false
	var newRootID string
	res, err := withLegacyAttachment(context.Background(), deps, "work", "review", map[string]string{"issue": "work"}, func() (*molecule.Result, error) {
		poured = true
		newRoot, e := graph.Create(beads.Bead{
			Type:   "molecule",
			Status: "open",
			Metadata: map[string]string{
				beadmeta.SourceBeadIDMetadataKey:   "work",
				beadmeta.SourceStoreRefMetadataKey: "rig:crm",
			},
		})
		newRootID = newRoot.ID
		return &molecule.Result{RootID: newRoot.ID}, e
	}, func(r *molecule.Result) (SlingResult, error) {
		return SlingResult{WispRootID: r.RootID}, nil
	})
	if err != nil {
		t.Fatalf("failed molecule blocked rework sling: %v", err)
	}
	if !poured {
		t.Fatal("expected fresh molecule to be poured for rework")
	}
	if res.WispRootID != newRootID {
		t.Fatalf("returned root = %s, want %s", res.WispRootID, newRootID)
	}
	// Verify failed root and its child were retired (closed):
	retiredRoot, _ := graph.Get(failedRoot.ID)
	if retiredRoot.Status != "closed" {
		t.Fatalf("failed root status = %s, want closed", retiredRoot.Status)
	}
	retiredChild, _ := graph.Get(failedChild.ID)
	if retiredChild.Status != "closed" {
		t.Fatalf("failed child status = %s, want closed", retiredChild.Status)
	}
}

// legacyReadyStampFailureStore fails the post-create "ready" stamp, leaving the
// freshly created root in gc.legacy_attachment_state=preparing. It stands in
// for a sling process that died (e.g. order timeout) between create() and the
// ready stamp (gci-142rk9).
type legacyReadyStampFailureStore struct{ *beads.MemStore }

func (s legacyReadyStampFailureStore) SetMetadata(id, key, value string) error {
	if key == legacyAttachmentStateKey && value == "ready" {
		return errors.New("process died before ready")
	}
	return s.MemStore.SetMetadata(id, key, value)
}

func legacyTestCreate(t *testing.T, store beads.Store, vars map[string]string, calls *int) func() (*molecule.Result, error) {
	t.Helper()
	return func() (*molecule.Result, error) {
		*calls++
		meta := map[string]string{
			beadmeta.SourceBeadIDMetadataKey:   "work",
			beadmeta.SourceStoreRefMetadataKey: "city:test",
			beadmeta.FormulaNameMetadataKey:    "review",
			legacyAttachmentStateKey:           "preparing",
		}
		for k, v := range vars {
			meta["gc.var."+k] = v
		}
		root, err := store.Create(beads.Bead{Type: "molecule", Status: "open", ParentID: "work", Metadata: meta})
		return &molecule.Result{RootID: root.ID}, err
	}
}

func legacyTestFinish(r *molecule.Result) (SlingResult, error) {
	return SlingResult{WispRootID: r.RootID}, nil
}

func TestLegacyAbandonedPreparingRootIsRetiredAndReplaced(t *testing.T) {
	for _, tc := range []struct {
		name     string
		rootVars map[string]string
	}{
		{"matching vars", map[string]string{"issue": "work"}},
		{"differing vars", map[string]string{"issue": "work", "pr": "old"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mem := seededStore("work")
			deps := SlingDeps{Store: legacyReadyStampFailureStore{MemStore: mem}, StoreRef: "city:test", CityPath: t.TempDir()}
			vars := map[string]string{"issue": "work", "pr": "new"}
			if tc.name == "matching vars" {
				vars = map[string]string{"issue": "work"}
			}
			calls := 0
			// First attempt dies between create() and the ready stamp.
			if _, err := withLegacyAttachment(context.Background(), deps, "work", "review", vars, legacyTestCreate(t, mem, tc.rootVars, &calls), legacyTestFinish, "worker"); err == nil {
				t.Fatal("first attempt should fail before ready")
			}
			stuck, _ := mem.List(beads.ListQuery{Type: "molecule"})
			if len(stuck) != 1 || stuck[0].Metadata[legacyAttachmentStateKey] != "preparing" {
				t.Fatalf("setup: want one preparing root, got %+v", stuck)
			}
			abandonedID := stuck[0].ID

			// Retry under the source lock must heal instead of reporting conflicting families.
			deps.Store = mem
			result, err := withLegacyAttachment(context.Background(), deps, "work", "review", vars, legacyTestCreate(t, mem, vars, &calls), legacyTestFinish, "worker")
			if err != nil {
				t.Fatalf("retry after abandoned preparing root: %v", err)
			}
			if calls != 2 || result.WispRootID == "" || result.WispRootID == abandonedID {
				t.Fatalf("retry did not materialize a fresh family: calls=%d result=%+v", calls, result)
			}
			old, _ := mem.Get(abandonedID)
			if old.Status != "closed" || !strings.Contains(old.Metadata["close_reason"], "abandoned preparing") {
				t.Fatalf("abandoned root not retired: %+v", old)
			}
			fresh, _ := mem.Get(result.WispRootID)
			if fresh.Status != "open" || fresh.Metadata[legacyAttachmentStateKey] != "ready" || fresh.Metadata[beadmeta.SourceStoreRefMetadataKey] != "city:test" {
				t.Fatalf("fresh family not ready: %+v", fresh)
			}
			source, _ := mem.Get("work")
			if source.Metadata[beadmeta.MoleculeIDMetadataKey] != result.WispRootID {
				t.Fatalf("source molecule_id = %q, want new root %q", source.Metadata[beadmeta.MoleculeIDMetadataKey], result.WispRootID)
			}
		})
	}
}

func TestLegacyAttachmentStateBranchesUnchanged(t *testing.T) {
	newRoot := func(store *beads.MemStore, extra map[string]string) beads.Bead {
		meta := map[string]string{
			beadmeta.SourceBeadIDMetadataKey: "work", beadmeta.SourceStoreRefMetadataKey: "city:test",
			beadmeta.FormulaNameMetadataKey: "review", legacyAttachmentStateKey: "ready", "gc.var.issue": "work",
		}
		for k, v := range extra {
			if v == "" {
				delete(meta, k)
				continue
			}
			meta[k] = v
		}
		root, _ := store.Create(beads.Bead{Type: "molecule", Status: "open", ParentID: "work", Metadata: meta})
		return root
	}
	vars := map[string]string{"issue": "work"}

	t.Run("ready matching root is reused", func(t *testing.T) {
		mem := seededStore("work")
		root := newRoot(mem, nil)
		calls := 0
		result, err := withLegacyAttachment(context.Background(), SlingDeps{Store: mem, StoreRef: "city:test", CityPath: t.TempDir()}, "work", "review", vars, legacyTestCreate(t, mem, vars, &calls), legacyTestFinish)
		if err != nil || calls != 0 || result.WispRootID != root.ID {
			t.Fatalf("ready root not reused: err=%v calls=%d result=%+v", err, calls, result)
		}
	})
	t.Run("ready non-matching root still conflicts", func(t *testing.T) {
		mem := seededStore("work")
		root := newRoot(mem, map[string]string{"gc.var.issue": "other"})
		calls := 0
		_, err := withLegacyAttachment(context.Background(), SlingDeps{Store: mem, StoreRef: "city:test", CityPath: t.TempDir()}, "work", "review", vars, legacyTestCreate(t, mem, vars, &calls), legacyTestFinish)
		if err == nil || !strings.Contains(err.Error(), "conflicting families") || calls != 0 {
			t.Fatalf("err=%v calls=%d; want conflicting families", err, calls)
		}
		if after, _ := mem.Get(root.ID); after.Status != "open" {
			t.Fatal("ready non-matching root was mutated")
		}
	})
	t.Run("failed root is still retired", func(t *testing.T) {
		mem := seededStore("work")
		root := newRoot(mem, map[string]string{beadmeta.MoleculeFailedMetadataKey: "true"})
		calls := 0
		result, err := withLegacyAttachment(context.Background(), SlingDeps{Store: mem, StoreRef: "city:test", CityPath: t.TempDir()}, "work", "review", vars, legacyTestCreate(t, mem, vars, &calls), legacyTestFinish)
		if err != nil || calls != 1 || result.WispRootID == root.ID {
			t.Fatalf("failed root not replaced: err=%v calls=%d result=%+v", err, calls, result)
		}
		if after, _ := mem.Get(root.ID); after.Status != "closed" || after.Metadata["close_reason"] != "retired: superseded by rework" {
			t.Fatalf("failed root not retired: %+v", after)
		}
	})
	for _, tc := range []struct {
		name  string
		extra map[string]string
		deps  func(*beads.MemStore, string) SlingDeps
	}{
		{"preparing root for another formula", map[string]string{legacyAttachmentStateKey: "preparing", beadmeta.FormulaNameMetadataKey: "other"}, nil},
		{"preparing root without source identity", map[string]string{legacyAttachmentStateKey: "preparing", beadmeta.SourceBeadIDMetadataKey: ""}, nil},
		// No recorded store ref: the creator may have held a different lock scope.
		{"preparing root without recorded store ref", map[string]string{legacyAttachmentStateKey: "preparing", beadmeta.SourceStoreRefMetadataKey: ""}, nil},
	} {
		t.Run(tc.name+" is not retired", func(t *testing.T) {
			mem := seededStore("work")
			root := newRoot(mem, tc.extra)
			calls := 0
			_, err := withLegacyAttachment(context.Background(), SlingDeps{Store: mem, StoreRef: "city:test", CityPath: t.TempDir()}, "work", "review", vars, legacyTestCreate(t, mem, vars, &calls), legacyTestFinish)
			if err == nil || calls != 0 {
				t.Fatalf("err=%v calls=%d; want refusal without materialization", err, calls)
			}
			if after, _ := mem.Get(root.ID); after.Status != "open" {
				t.Fatal("unproven preparing root was retired")
			}
		})
	}
}
