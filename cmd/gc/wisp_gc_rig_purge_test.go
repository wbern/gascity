package main

import (
	"bytes"
	"context"
	"errors"
	"log"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
)

func setRigPurgeMode(t *testing.T, mode rigPurgeMode) {
	t.Helper()
	prev := rigPurgeModeFn
	rigPurgeModeFn = func() rigPurgeMode { return mode }
	t.Cleanup(func() { rigPurgeModeFn = prev })
}

func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prevOut, prevFlags := log.Writer(), log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	t.Cleanup(func() { log.SetOutput(prevOut); log.SetFlags(prevFlags) })
	return &buf
}

func TestParseRigPurgeMode(t *testing.T) {
	for in, want := range map[string]rigPurgeMode{
		"": rigPurgeDryRun, "dry-run": rigPurgeDryRun, "garbage": rigPurgeDryRun,
		"off": rigPurgeOff, "OFF": rigPurgeOff, "on": rigPurgeOn, " On ": rigPurgeOn,
	} {
		if got := parseRigPurgeMode(in); got != want {
			t.Errorf("parseRigPurgeMode(%q) = %q, want %q", in, got, want)
		}
	}
}

// rigPurgeFixture: every row is old enough except mol-young.
func rigPurgeFixture(now time.Time) *beads.MemStore {
	old, young := now.Add(-48*time.Hour), now.Add(-time.Hour)
	b := func(id, status, typ, parent string, at time.Time) beads.Bead {
		return beads.Bead{ID: id, Status: status, Type: typ, ParentID: parent, CreatedAt: at}
	}
	holder := b("route-1", "open", "task", "", old)
	holder.Metadata = map[string]string{beadmeta.MoleculeIDMetadataKey: "mol-held"}
	return beads.NewMemStoreFrom(0, []beads.Bead{
		b("mol-old", "closed", "molecule", "", old),
		b("mol-old.1", "closed", "step", "mol-old", old),
		b("mol-old.2", "closed", "step", "mol-old", old),
		b("mol-young", "closed", "molecule", "", young),
		b("mol-young.1", "closed", "step", "mol-young", young),
		b("mol-open", "open", "molecule", "", old),
		b("mol-open.1", "open", "step", "mol-open", old),
		b("mol-openmember", "closed", "molecule", "", old),
		b("mol-openmember.1", "open", "step", "mol-openmember", old),
		b("mol-held", "closed", "molecule", "", old),
		b("mol-held.1", "closed", "step", "mol-held", old),
		holder,
	}, nil)
}

func exists(t *testing.T, s beads.Store, id string) bool {
	t.Helper()
	_, err := s.Get(id)
	if err == nil {
		return true
	}
	if errors.Is(err, beads.ErrNotFound) {
		return false
	}
	t.Fatalf("Get(%s): %v", id, err)
	return false
}

func TestPurgeClosedRigRoots_Modes(t *testing.T) {
	now := time.Now()
	ttl := 24 * time.Hour
	gone := []string{"mol-old", "mol-old.1", "mol-old.2"}
	kept := []string{"mol-young", "mol-young.1", "mol-open", "mol-open.1", "mol-openmember", "mol-openmember.1", "mol-held", "mol-held.1", "route-1"}
	tests := []struct {
		name       string
		mode       rigPurgeMode
		wantPurged int
		wantGone   bool
		wantLog    string
	}{
		{"off", rigPurgeOff, 0, false, ""},
		{"dry-run", rigPurgeDryRun, 0, false, "rig=crm mode=dry-run would_delete_roots=1 would_delete_members=3"},
		{"on", rigPurgeOn, 1, true, "mode=on"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setRigPurgeMode(t, tt.mode)
			buf := captureLog(t)
			store := rigPurgeFixture(now)
			purged, err := purgeClosedRigRoots(context.Background(), map[string]beads.Store{"crm": store}, ttl, now)
			if err != nil {
				t.Fatalf("purgeClosedRigRoots: %v", err)
			}
			if purged != tt.wantPurged {
				t.Errorf("purged = %d, want %d", purged, tt.wantPurged)
			}
			for _, id := range gone {
				if exists(t, store, id) == tt.wantGone {
					t.Errorf("exists(%s) = %t, want %t", id, !tt.wantGone, !tt.wantGone)
				}
			}
			for _, id := range kept {
				if !exists(t, store, id) {
					t.Errorf("%s was deleted, want kept", id)
				}
			}
			if !strings.Contains(buf.String(), tt.wantLog) {
				t.Errorf("log %q missing %q", buf.String(), tt.wantLog)
			}
			if tt.mode == rigPurgeOff && buf.Len() != 0 {
				t.Errorf("off mode logged %q", buf.String())
			}
		})
	}
}

func TestPurgeClosedRigRoots_BatchCapHonoured(t *testing.T) {
	setRigPurgeMode(t, rigPurgeOn)
	prev := wispGCClosurePurgeBatchCap
	wispGCClosurePurgeBatchCap = 2
	t.Cleanup(func() { wispGCClosurePurgeBatchCap = prev })
	now := time.Now()
	old := now.Add(-48 * time.Hour)
	var rows []beads.Bead
	for _, id := range []string{"m1", "m2", "m3", "m4"} {
		rows = append(rows, beads.Bead{ID: id, Status: "closed", Type: "molecule", CreatedAt: old})
	}
	store := beads.NewMemStoreFrom(0, rows, nil)
	stores := map[string]beads.Store{"crm": store}
	for sweep := 1; sweep <= 2; sweep++ {
		purged, err := purgeClosedRigRoots(context.Background(), stores, time.Hour, now)
		if err != nil || purged != 2 {
			t.Fatalf("sweep %d: purged=%d err=%v, want 2 nil", sweep, purged, err)
		}
	}
}

type rigPurgeListFailStore struct{ beads.Store }

func (rigPurgeListFailStore) List(beads.ListQuery) ([]beads.Bead, error) {
	return nil, errors.New("boom")
}

func TestPurgeClosedRigRoots_StoreErrorDoesNotAbortOtherRigs(t *testing.T) {
	setRigPurgeMode(t, rigPurgeOn)
	now := time.Now()
	good := rigPurgeFixture(now)
	stores := map[string]beads.Store{
		"a-bad":  rigPurgeListFailStore{beads.NewMemStore()},
		"z-good": good,
	}
	purged, err := purgeClosedRigRoots(context.Background(), stores, 24*time.Hour, now)
	if err == nil || !strings.Contains(err.Error(), "rig a-bad") || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("err = %v, want it to name rig a-bad and boom", err)
	}
	if purged != 1 || exists(t, good, "mol-old") {
		t.Fatalf("purged=%d, want the healthy rig still purged", purged)
	}
}

func TestPurgeClosedRigRoots_ZeroTTLAndCancelledContext(t *testing.T) {
	setRigPurgeMode(t, rigPurgeOn)
	now := time.Now()
	store := rigPurgeFixture(now)
	stores := map[string]beads.Store{"crm": store}
	if n, err := purgeClosedRigRoots(context.Background(), stores, 0, now); n != 0 || err != nil {
		t.Fatalf("ttl 0: purged=%d err=%v, want no-op", n, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if n, err := purgeClosedRigRoots(ctx, stores, time.Hour, now); n != 0 || err == nil {
		t.Fatalf("cancelled: purged=%d err=%v, want abandoned error", n, err)
	}
}

// The sweep runs the rig purge after the city-store arm, which is unchanged.
func TestLaunchWispGC_RigPurgeRunsAfterUnchangedCityPurge(t *testing.T) {
	setRigPurgeMode(t, rigPurgeOn)
	now := time.Now()
	old := now.Add(-48 * time.Hour)
	city := beads.NewMemStoreFrom(0, []beads.Bead{{ID: "city-mol", Status: "closed", Type: "molecule", CreatedAt: old}}, nil)
	rig := rigPurgeFixture(now)

	var stdout, stderr bytes.Buffer
	cr := newWispGCTestRuntime(t, newWispGC(time.Second, 24*time.Hour, 0), &stdout, &stderr)
	cr.standaloneCityStore = city
	cr.standaloneRigStores = map[string]beads.Store{"crm": rig}
	cr.launchWispGC(now)
	if !cr.wispSweeps.waitIdle(10 * time.Second) {
		t.Fatal("sweep did not finish")
	}
	cr.reportWispGC(nil)
	if exists(t, city, "city-mol") {
		t.Error("city-mol kept; city-store purge must be unchanged")
	}
	if exists(t, rig, "mol-old") {
		t.Error("rig mol-old kept; rig purge should have run in the sweep")
	}
}
