package sling

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/molecule"
	"github.com/gastownhall/gascity/internal/runtime"
)

type review172RouteFenceStore struct {
	*beads.MemStore
	timing string
	t      *testing.T
	writes int
}

func (s *review172RouteFenceStore) UpdateIfMatch(id string, revision int64, opts beads.UpdateOpts) error {
	s.writes++
	if opts.Metadata[beadmeta.RoutedToMetadataKey] == "" || opts.Metadata[beadmeta.MoleculeIDMetadataKey] == "" {
		s.t.Fatal("route and attachment were not in the same fenced write")
	}
	if s.timing == "before" {
		if err := s.Close(id); err != nil {
			return err
		}
	}
	err := s.MemStore.UpdateIfMatch(id, revision, opts)
	if err == nil && s.timing == "after" {
		return s.Close(id)
	}
	return err
}

func TestReview172SingleAndBatchRouteFence(t *testing.T) {
	for _, batch := range []bool{false, true} {
		for _, timing := range []string{"before", "after", "open"} {
			t.Run(fmt.Sprintf("batch=%v/close=%s", batch, timing), func(t *testing.T) {
				mem := beads.NewMemStoreFrom(0, []beads.Bead{{ID: "group", Type: "convoy", Status: "open"}, {ID: "work", Type: "task", Status: "open"}}, nil)
				if err := mem.DepAdd("group", "work", "tracks"); err != nil {
					t.Fatal(err)
				}
				store := &review172RouteFenceStore{MemStore: mem, timing: timing, t: t}
				runner := func(string, string, map[string]string) (string, error) {
					t.Error("unfenced Runner executed after publication")
					return "", nil
				}
				deps := testDeps(&config.City{Workspace: config.Workspace{Name: "test"}}, runtime.NewFake(), runner)
				deps.Store = store
				deps.CityPath = t.TempDir()
				router := &fakeBeadRouter{}
				deps.Router = router
				opts := SlingOpts{Target: config.Agent{Name: "worker", MaxActiveSessions: intPtr(1)}, BeadOrFormula: "work", OnFormula: "code-review", NoConvoy: true}
				var err error
				if batch {
					opts.BeadOrFormula = "group"
					_, err = DoSlingBatch(opts, deps, store)
				} else {
					_, err = DoSling(opts, deps, store)
				}
				source, _ := mem.Get("work")
				if store.writes != 1 || len(router.routed) != 0 {
					t.Fatalf("fenced writes=%d unfenced routes=%d error=%v", store.writes, len(router.routed), err)
				}
				if timing == "before" {
					if err == nil || source.Metadata[beadmeta.RoutedToMetadataKey] != "" {
						t.Fatalf("closed source routed: %v, %v", source, err)
					}
				} else if err != nil || source.Metadata[beadmeta.RoutedToMetadataKey] != "worker" {
					t.Fatalf("atomic route failed: %v, %v", source, err)
				}
			})
		}
	}
}

type review172PublicationFailure struct {
	*beads.MemStore
	calls int
}

type patchOnlyPublicationStore struct {
	beads.Store
	writes int
}

func (s *patchOnlyPublicationStore) CompareAndSetMetadataPatch(id string, expected beads.Bead, patch map[string]string) (bool, error) {
	s.writes++
	current, err := s.Get(id)
	if err != nil {
		return false, err
	}
	if current.Status != expected.Status || current.Assignee != expected.Assignee || current.ParentID != expected.ParentID || !maps.Equal(current.Metadata, expected.Metadata) {
		return false, nil
	}
	return true, s.Update(id, beads.UpdateOpts{Metadata: patch})
}

func TestLegacyAttachmentPublishesThroughAtomicMetadataPatch(t *testing.T) {
	mem := seededStore("work")
	store := &patchOnlyPublicationStore{Store: mem}
	_, err := withLegacyAttachment(context.Background(), SlingDeps{Store: store, CityPath: t.TempDir()}, "work", "review", nil,
		func() (*molecule.Result, error) {
			root, createErr := mem.Create(beads.Bead{Type: "molecule", Status: "open", ParentID: "work"})
			return &molecule.Result{RootID: root.ID}, createErr
		},
		func(r *molecule.Result) (SlingResult, error) { return SlingResult{WispRootID: r.RootID}, nil },
		"reviewer")
	if err != nil {
		t.Fatalf("withLegacyAttachment: %v", err)
	}
	source, _ := mem.Get("work")
	if store.writes != 1 || source.Metadata[beadmeta.MoleculeIDMetadataKey] == "" || source.Metadata[beadmeta.RoutedToMetadataKey] != "reviewer" {
		t.Fatalf("atomic patch writes=%d source=%+v", store.writes, source)
	}
}

func (s *review172PublicationFailure) UpdateIfMatch(string, int64, beads.UpdateOpts) error {
	s.calls++
	return errors.New("publication failed")
}

// One logical family member acquired custody outside the cache coordinator.
func TestReview172RollbackCannotCloseCachedClaimedMember(t *testing.T) {
	mem := seededStore("work")
	root, _ := mem.Create(beads.Bead{Type: "molecule", Status: "open"})
	child, _ := mem.Create(beads.Bead{Type: "step", Status: "open", Metadata: map[string]string{beadmeta.RootBeadIDMetadataKey: root.ID}})
	cache := beads.NewCachingStoreForTest(mem, nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatal(err)
	}
	owner := "live-worker"
	if err := mem.Update(child.ID, beads.UpdateOpts{Assignee: &owner}); err != nil {
		t.Fatal(err)
	}
	source := &review172PublicationFailure{MemStore: mem}
	_, err := withLegacyAttachment(context.Background(), SlingDeps{Store: source, GraphStore: cache, StoreRef: "rig:here", CityPath: t.TempDir()}, "work", "review", nil,
		func() (*molecule.Result, error) { return &molecule.Result{RootID: root.ID}, nil },
		func(*molecule.Result) (SlingResult, error) {
			t.Fatal("failed publication finished")
			return SlingResult{}, nil
		})
	if source.calls != 1 {
		t.Fatal("did not exercise failing publication")
	}
	current, _ := mem.Get(child.ID)
	if err == nil || current.Status == "closed" {
		t.Fatalf("claimed family was retired through stale cache: error=%v child=%+v", err, current)
	}
}

type review172ClaimDuringClose struct {
	beads.Store
	root, child string
	scans       int
}

func (s *review172ClaimDuringClose) Children(id string, opts ...beads.QueryOpt) ([]beads.Bead, error) {
	if id == s.root {
		s.scans++
		if s.scans == 2 {
			owner := "concurrent-worker"
			if err := s.Update(s.child, beads.UpdateOpts{Assignee: &owner}); err != nil {
				return nil, err
			}
		}
	}
	return s.Store.Children(id, opts...)
}

func TestReview172RollbackCannotRaceClaim(t *testing.T) {
	mem := seededStore("work")
	root, _ := mem.Create(beads.Bead{Type: "molecule", Status: "open"})
	child, _ := mem.Create(beads.Bead{Type: "step", Status: "open", ParentID: root.ID})
	s := &review172ClaimDuringClose{Store: mem, root: root.ID, child: child.ID}
	source := &review172PublicationFailure{MemStore: mem}
	_, err := withLegacyAttachment(context.Background(), SlingDeps{Store: source, GraphStore: s, StoreRef: "rig:here", CityPath: t.TempDir()}, "work", "review", nil,
		func() (*molecule.Result, error) { return &molecule.Result{RootID: root.ID}, nil },
		func(*molecule.Result) (SlingResult, error) {
			t.Fatal("failed publication finished")
			return SlingResult{}, nil
		})
	if err == nil || source.calls != 1 {
		t.Fatal("did not exercise failing publication")
	}
	current, _ := mem.Get(child.ID)
	if current.Status == "closed" {
		t.Fatalf("rollback closed newly claimed work: %+v", current)
	}
}

func TestReview172SameIDInIndependentStoresDoesNotReuseForeignFamily(t *testing.T) {
	source := seededStore("work")
	roots := beads.NewMemStore()
	foreign, _ := roots.Create(beads.Bead{Type: "molecule", Status: "open", Metadata: map[string]string{
		beadmeta.SourceBeadIDMetadataKey: "work", beadmeta.SourceStoreRefMetadataKey: "rig:other",
		beadmeta.FormulaNameMetadataKey: "review", legacyAttachmentStateKey: "ready", "gc.var.issue": "work",
	}})
	deps := SlingDeps{Store: source, GraphStore: roots, StoreRef: "rig:here", CityPath: t.TempDir()}
	result, err := withLegacyAttachment(context.Background(), deps, "work", "review", map[string]string{"issue": "work"}, func() (*molecule.Result, error) {
		own, e := roots.Create(beads.Bead{Type: "molecule", Status: "open", Metadata: map[string]string{beadmeta.SourceBeadIDMetadataKey: "work", beadmeta.SourceStoreRefMetadataKey: "rig:here"}})
		return &molecule.Result{RootID: own.ID}, e
	}, func(r *molecule.Result) (SlingResult, error) { return SlingResult{WispRootID: r.RootID}, nil })
	if err == nil && result.WispRootID == foreign.ID {
		t.Fatalf("foreign family adopted across source-store boundary: %+v", result)
	}
}

type review172UnconditionalRace struct{ beads.Store }

func (s review172UnconditionalRace) SetMetadata(id, key, value string) error {
	if id == "work" && key == beadmeta.MoleculeIDMetadataKey {
		if err := s.Store.SetMetadata(id, key, "independently-published-root"); err != nil {
			return err
		}
	}
	return s.Store.SetMetadata(id, key, value)
}

func TestReview172NonCASCannotOverwriteInterveningPointer(t *testing.T) {
	mem := seededStore("work")
	s := review172UnconditionalRace{Store: mem}
	_, err := withLegacyAttachment(context.Background(), SlingDeps{Store: s, CityPath: t.TempDir()}, "work", "review", nil, func() (*molecule.Result, error) {
		root, e := mem.Create(beads.Bead{Type: "molecule", Status: "open", ParentID: "work"})
		return &molecule.Result{RootID: root.ID}, e
	}, func(r *molecule.Result) (SlingResult, error) { return SlingResult{WispRootID: r.RootID}, nil })
	source, _ := mem.Get("work")
	if err == nil || source.Metadata[beadmeta.MoleculeIDMetadataKey] != "" {
		t.Fatalf("unsupported publication made a write: error=%v metadata=%v", err, source.Metadata)
	}
	roots, _ := mem.List(beads.ListQuery{Type: "molecule", IncludeClosed: true})
	if len(roots) != 0 {
		t.Fatalf("unsupported publication materialized roots: %v", roots)
	}
}

type review172CloseAfterRead struct{ beads.Store }

func (s review172CloseAfterRead) Get(id string) (beads.Bead, error) {
	b, err := s.Store.Get(id)
	if err == nil && id == "work" && b.Metadata[beadmeta.MoleculeIDMetadataKey] != "" && b.Status == "open" {
		if e := s.Close(id); e != nil {
			return b, e
		}
	}
	return b, err
}

func TestReview172ClosedSourceCannotBeRoutedAfterPublication(t *testing.T) {
	mem := seededStore("work")
	s := review172CloseAfterRead{Store: mem}
	_, err := withLegacyAttachment(context.Background(), SlingDeps{Store: s, CityPath: t.TempDir()}, "work", "review", nil, func() (*molecule.Result, error) {
		root, e := mem.Create(beads.Bead{Type: "molecule", Status: "open", ParentID: "work"})
		return &molecule.Result{RootID: root.ID}, e
	}, func(r *molecule.Result) (SlingResult, error) {
		e := mem.SetMetadata("work", beadmeta.RoutedToMetadataKey, "new-worker")
		return SlingResult{WispRootID: r.RootID}, e
	})
	source, _ := mem.Get("work")
	if err == nil && source.Status == "closed" && source.Metadata[beadmeta.RoutedToMetadataKey] != "" {
		t.Fatalf("closed source was routed after readback: %+v", source)
	}
}
