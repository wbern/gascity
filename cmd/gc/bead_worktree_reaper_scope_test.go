package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
)

func reapTestScope(stores map[string]beads.Store) reapStoreScope {
	return reapStoreScope{RigStores: stores, CityStore: beads.NewMemStore()}
}

func TestReapBorrowScopeIncludesCityAndOtherRigsOnce(t *testing.T) {
	path := t.TempDir()
	borrower := func(id string) beads.Bead {
		return beads.Bead{ID: id, Status: "open", Metadata: map[string]string{beadmeta.WorkDirMetadataKey: path}}
	}
	city := &reapBorrowVetoCountingStore{Store: beads.NewMemStoreFrom(1, []beads.Bead{borrower("hq-borrow")}, nil)}
	own := &reapBorrowVetoCountingStore{Store: beads.NewMemStore()}
	other := &reapBorrowVetoCountingStore{Store: beads.NewMemStoreFrom(1, []beads.Bead{borrower("other-borrow")}, nil)}
	cfg := &config.City{Rigs: []config.Rig{{Name: "own"}, {Name: "other"}}}
	refs, incomplete := collectReapBorrowScope("", cfg, reapStoreScope{RigStores: map[string]beads.Store{"own": own, "other": other}, CityStore: city}, io.Discard)
	physical, err := normalizeReapBorrowPath(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(incomplete) != 0 || len(refs[physical]) != 2 {
		t.Fatalf("refs=%v incomplete=%v", refs, incomplete)
	}
	for name, store := range map[string]*reapBorrowVetoCountingStore{"city": city, "own": own, "other": other} {
		if store.calls != 1 {
			t.Fatalf("%s calls=%d", name, store.calls)
		}
		if !store.queries[0].Live || store.queries[0].IncludeClosed || store.queries[0].TierMode != beads.TierBoth {
			t.Fatalf("unsafe query %+v", store.queries[0])
		}
	}
}

func TestReapBorrowScopeRefusesMissingOrUnreadableRequiredStore(t *testing.T) {
	for _, failure := range []string{"city_missing", "rig_missing", "rig_unreadable", "city_unreadable"} {
		t.Run(failure, func(t *testing.T) {
			cfg := &config.City{Rigs: []config.Rig{{Name: "own"}, {Name: "other"}}}
			scope := reapTestScope(map[string]beads.Store{"own": beads.NewMemStore(), "other": beads.NewMemStore()})
			switch failure {
			case "city_missing":
				scope.CityStore = nil
			case "rig_missing":
				delete(scope.RigStores, "other")
			case "rig_unreadable":
				scope.RigStores["other"] = &reapBorrowVetoErrorStore{Store: beads.NewMemStore(), err: errors.New("unreadable")}
			case "city_unreadable":
				scope.CityStore = &reapBorrowVetoErrorStore{Store: beads.NewMemStore(), err: errors.New("unreadable")}
			}
			_, incomplete := collectReapBorrowScope("", cfg, scope, io.Discard)
			if len(incomplete) == 0 {
				t.Fatal("incomplete required scope reported complete")
			}
		})
	}
}

func TestReapBorrowScopeUsesRegisteredWorkAndRelocatedBinding(t *testing.T) {
	city := t.TempDir()
	path := t.TempDir()
	work := beads.NewMemStoreFrom(1, []beads.Bead{{ID: "hq-work", Status: "open", Metadata: map[string]string{"worker_dir": path}}}, nil)
	for _, failure := range []bool{false, true} {
		t.Run(fmt.Sprint(failure), func(t *testing.T) {
			var binding beads.Store = beads.NewMemStoreFrom(2, []beads.Bead{{ID: "hq-binding", Status: "open", Metadata: map[string]string{"gc.work_dir": path}}}, nil)
			if failure {
				binding = &reapBorrowVetoErrorStore{Store: binding, err: errors.New("binding unavailable")}
			}
			routes := splitRoutes(binding)
			registerResidencyRoutes(city, routes, func() beads.Store { return work })
			defer unregisterResidencyRoutes(city, routes)
			refs, incomplete := collectReapBorrowScope(city, &config.City{}, reapStoreScope{CityStore: binding}, io.Discard)
			if failure {
				if len(incomplete) == 0 {
					t.Fatal("unreadable class scope accepted")
				}
				return
			}
			normalized, err := normalizeReapBorrowPath(path)
			if err != nil {
				t.Fatal(err)
			}
			if len(incomplete) != 0 || len(refs[normalized]) != 2 {
				t.Fatalf("refs=%v incomplete=%v", refs, incomplete)
			}
		})
	}
}

func TestReapBorrowPathPreservesPhysicalTraversalAndDescendants(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "actual", "child")
	if err := os.MkdirAll(target, 0o700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(dir, "alias")
	if err := os.Symlink(target, alias); err != nil {
		t.Fatal(err)
	}
	physical, err := normalizeReapBorrowPath(alias + "/../project")
	if err != nil {
		t.Fatal(err)
	}
	want, _ := normalizeReapBorrowPath(filepath.Join(dir, "actual", "project"))
	if physical != want {
		t.Fatalf("got %s want %s", physical, want)
	}
	root, _ := normalizeReapBorrowPath(filepath.Join(dir, "task"))
	refs := map[string][]string{root + "/subproject": {"borrower"}, root + "-sibling": {"sibling"}}
	got, err := reapBorrowersUnder(refs, root)
	if err != nil || len(got) != 1 || got[0] != "borrower" {
		t.Fatalf("got=%v err=%v", got, err)
	}
}

type reapStaleOwnerStore struct {
	beads.Store
	stale beads.Bead
}

func (s reapStaleOwnerStore) Get(string) (beads.Bead, error) { return s.stale, nil }
func TestReapOwnerLiveDoesNotTrustCachedClosedGet(t *testing.T) {
	store := reapStaleOwnerStore{Store: beads.NewMemStoreFrom(1, []beads.Bead{{ID: "gcw-task", Status: "open", Revision: 2}}, nil), stale: beads.Bead{ID: "gcw-task", Status: "closed", Revision: 1}}
	owner, err := reapOwnerLive(store, "gcw-task")
	if err != nil || owner.Status != "open" || owner.Revision != 2 {
		t.Fatalf("owner=%+v err=%v", owner, err)
	}
}

func TestReapDiscoveryCompletenessPreservesEmptyControlRig(t *testing.T) {
	for _, populated := range []bool{false, true} {
		t.Run(fmt.Sprint(populated), func(t *testing.T) {
			city, rig := initReapRig(t)
			wt := addClosedWorktree(t, rig, city, "tasks", "ga-disc01")
			cfg := reapTestConfig(rig)
			if id := extractBeadIDFromWorktreePath(cfg, filepath.Join(city, ".gc", "worktrees", reapTestRigName), wt); id != "ga-disc01" {
				t.Fatalf("fixture is not an ordinary reaper candidate: %q", id)
			}
			cfg.Rigs = append(cfg.Rigs, config.Rig{Name: "control", Path: filepath.Join(t.TempDir(), "missing-repository")})
			scope := reapTestScope(map[string]beads.Store{reapTestRigName: beads.NewMemStoreFrom(1, []beads.Bead{{ID: "ga-disc01", Status: "closed"}}, nil), "control": beads.NewMemStore()})
			if populated {
				if err := os.MkdirAll(filepath.Join(city, ".gc", "worktrees", "control", "tasks", "other-task"), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			injectLiveness(t, liveWorktreeState{scanned: true})
			report := reapClosedBeadWorktrees(city, cfg, scope, nil, true, events.Discard, nil, io.Discard)
			if populated {
				if len(report.IncompleteScopes) == 0 || len(report.Reaped) != 0 {
					t.Fatalf("partial discovery admitted: %+v", report)
				}
			} else {
				if len(report.IncompleteScopes) != 0 || len(report.Reaped) != 1 {
					t.Fatalf("empty control blocked healthy inventory: %+v", report)
				}
			}
			if _, err := os.Stat(wt); err != nil {
				t.Fatal("dry-run changed source")
			}
		})
	}
}
