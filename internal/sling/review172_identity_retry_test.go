package sling

import (
	"context"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/molecule"
	"github.com/gastownhall/gascity/internal/runtime"
)

func TestReview172MissingSharedSourceRefCannotPublishUnretryableFamily(t *testing.T) {
	source := seededStore("work")
	deps := testDeps(&config.City{Workspace: config.Workspace{Name: "test"}}, runtime.NewFake(), func(string, string, map[string]string) (string, error) { t.Fatal("runner called"); return "", nil })
	deps.Store = source
	deps.GraphStore = beads.NewMemStore()
	deps.StoreRef = ""
	deps.StoreRef = ""
	deps.CityPath = t.TempDir()
	opts := SlingOpts{Target: config.Agent{Name: "worker", MaxActiveSessions: intPtr(1)}, NoConvoy: true}
	first, err := attachFormulaToBead(opts, deps, source, "work", "code-review", "on-formula", "formula", false, SlingResult{})
	if err != nil {
		roots, listErr := deps.GraphStore.List(beads.ListQuery{Type: "molecule", IncludeClosed: true})
		current, getErr := source.Get("work")
		if listErr != nil || getErr != nil || len(roots) != 0 || current.Metadata[beadmeta.MoleculeIDMetadataKey] != "" || current.Metadata[beadmeta.RoutedToMetadataKey] != "" {
			t.Fatalf("missing identity was refused only after mutation: roots=%v source=%v errors=%v/%v", roots, current, listErr, getErr)
		}
		return
	} // Safe before-publication refusal is also acceptable.
	second, err := attachFormulaToBead(opts, deps, source, "work", "code-review", "on-formula", "formula", false, SlingResult{})
	if err != nil || second.WispRootID != first.WispRootID {
		t.Fatalf("published family cannot be retried: first=%+v second=%+v error=%v", first, second, err)
	}
}

func TestReview172InterveningSourceRouteMustNotBeOverwritten(t *testing.T) {
	source := seededStore("work")
	deps := SlingDeps{Store: source, CityPath: t.TempDir()}
	_, err := withLegacyAttachment(context.Background(), deps, "work", "review", nil, func() (*molecule.Result, error) {
		root, e := source.Create(beads.Bead{Type: "molecule", Status: "open", ParentID: "work"})
		if e != nil {
			return nil, e
		}
		if e = source.SetMetadata("work", beadmeta.RoutedToMetadataKey, "new-owner-queue"); e != nil {
			return nil, e
		}
		return &molecule.Result{RootID: root.ID}, nil
	}, func(r *molecule.Result) (SlingResult, error) { return SlingResult{WispRootID: r.RootID}, nil }, "old-owner-queue")
	current, _ := source.Get("work")
	if err == nil || current.Metadata[beadmeta.RoutedToMetadataKey] != "new-owner-queue" {
		t.Fatalf("route changed during creation was overwritten: error=%v metadata=%v", err, current.Metadata)
	}
}
