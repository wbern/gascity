package sling

import (
	"context"
	"errors"
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
