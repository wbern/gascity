package main

import (
	"fmt"
	"math/rand"
	"sort"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/orders"
)

// retentionFixture seeds a MemStore with closed order-tracking beads and returns
// the store plus the cutoff/now used by the prune.
type retentionFixture struct {
	store  *beads.MemStore
	now    time.Time
	cutoff time.Time
}

type retentionFixtureOpts struct {
	seed int64
	// consistentUpdate keeps UpdatedAt >= CreatedAt with the SAME ordering as
	// CreatedAt (UpdatedAt = CreatedAt + a constant), the shape real tracking
	// beads have in practice. When false UpdatedAt is created+random, which can
	// invert the created/updated ordering between beads.
	consistentUpdate bool
	// crowd adds closed NON-tracking beads that carry an order-run:<name> label
	// (graph/molecule roots), newer than every tracking bead, so a per-order
	// floor read can be crowded by non-tracking rows.
	crowd bool
	// legacy adds closed tracking beads with no resolvable order name.
	legacy bool
}

func newRetentionFixture(t *testing.T, o retentionFixtureOpts) retentionFixture {
	t.Helper()
	r := rand.New(rand.NewSource(o.seed))
	now := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	ttl := 72 * time.Hour
	cutoff := now.Add(-ttl)
	var all []beads.Bead
	seq := 0
	add := func(b beads.Bead) {
		seq++
		b.ID = fmt.Sprintf("rt-%05d", seq)
		b.Status = "closed"
		b.Type = "task"
		all = append(all, b)
	}
	nOrders := 3 + r.Intn(6)
	for i := 0; i < nOrders; i++ {
		name := fmt.Sprintf("order-%d", i)
		// Mix of fast orders (hundreds of runs spanning days) and slow orders
		// (a few runs, ALL older than the cutoff) so the floor must anchor on
		// old rows for the slow ones.
		var count int
		var span time.Duration
		switch r.Intn(3) {
		case 0: // fast
			count, span = 40+r.Intn(160), 8*24*time.Hour
		case 1: // slow, entirely old
			count, span = 2+r.Intn(25), 6*24*time.Hour
		default: // medium
			count, span = 5+r.Intn(40), 5*24*time.Hour
		}
		for k := 0; k < count; k++ {
			age := time.Duration(r.Int63n(int64(span)))
			if i%3 == 1 {
				age += ttl + time.Hour // slow orders: everything older than cutoff
			}
			created := now.Add(-age)
			updated := created.Add(5 * time.Minute)
			if !o.consistentUpdate {
				updated = created.Add(time.Duration(r.Int63n(int64(36 * time.Hour))))
			}
			add(beads.Bead{
				Title:     "order:" + name,
				Labels:    []string{"order-run:" + name, "order-tracking"},
				CreatedAt: created,
				UpdatedAt: updated,
			})
		}
		if o.crowd {
			for k := 0; k < 30; k++ {
				c := now.Add(-time.Duration(r.Int63n(int64(time.Hour))))
				add(beads.Bead{Title: "mol-root", Labels: []string{"order-run:" + name}, CreatedAt: c, UpdatedAt: c})
			}
		}
	}
	if o.legacy {
		for k := 0; k < 25; k++ {
			created := now.Add(-time.Duration(r.Int63n(int64(7 * 24 * time.Hour))))
			add(beads.Bead{Title: "foreign", Labels: []string{"order-tracking"}, CreatedAt: created, UpdatedAt: created.Add(time.Minute)})
		}
	}
	return retentionFixture{store: beads.NewMemStoreFrom(seq, all, nil), now: now, cutoff: cutoff}
}

func deletableIDs(runs []orders.OrderRun, cutoff time.Time) map[string]string {
	out := map[string]string{}
	for _, run := range closedRetentionDeletable(runs, nil, minClosedOrderTrackingRetained, cutoff) {
		out[run.ID] = run.Scoped
	}
	return out
}

func retentionSortedIDs(m map[string]string) []string {
	ids := make([]string, 0, len(m))
	for id := range m {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// TestClosedRetentionBoundedReadNeverDeletesMoreThanFullRead is the safety
// property of the bounded retention read: for any store, anything the prune
// would delete using the bounded read it would also delete using the full read.
// It must hold even when created/updated orderings invert, a floor query is
// crowded by non-tracking rows, and some runs have no resolvable order name.
func TestClosedRetentionBoundedReadNeverDeletesMoreThanFullRead(t *testing.T) {
	const floor = minClosedOrderTrackingRetained
	for seed := int64(1); seed <= 120; seed++ {
		for _, o := range []retentionFixtureOpts{
			{seed: seed, consistentUpdate: true},
			{seed: seed, consistentUpdate: false},
			{seed: seed, consistentUpdate: true, crowd: true, legacy: true},
			{seed: seed, consistentUpdate: false, crowd: true, legacy: true},
		} {
			fx := newRetentionFixture(t, o)
			st := orders.NewStore(beads.OrdersStore{Store: fx.store})
			full, err := st.ClosedRunsForRetention()
			if err != nil {
				t.Fatal(err)
			}
			bounded, err := st.ClosedRunsForRetentionBounded(fx.cutoff, floor)
			if err != nil {
				t.Fatal(err)
			}
			dOld := deletableIDs(full, fx.cutoff)
			dNew := deletableIDs(bounded, fx.cutoff)
			for id, name := range dNew {
				if _, ok := dOld[id]; !ok {
					t.Fatalf("seed=%d opts=%+v: bounded read would delete %s (order %q) that the full read keeps", seed, o, id, name)
				}
			}
			if len(bounded) > len(full) {
				t.Fatalf("seed=%d: bounded read returned %d rows, more than the full read's %d", seed, len(bounded), len(full))
			}
		}
	}
}

// TestClosedRetentionBoundedReadEqualsFullReadWhenOrderingIsConsistent proves the
// bounded read is not merely safe but EXACT for the shape real tracking beads
// have: UpdatedAt follows CreatedAt, floor queries are not crowded, and every
// run resolves to an order name.
func TestClosedRetentionBoundedReadEqualsFullReadWhenOrderingIsConsistent(t *testing.T) {
	const floor = minClosedOrderTrackingRetained
	for seed := int64(1); seed <= 120; seed++ {
		fx := newRetentionFixture(t, retentionFixtureOpts{seed: seed, consistentUpdate: true})
		st := orders.NewStore(beads.OrdersStore{Store: fx.store})
		full, err := st.ClosedRunsForRetention()
		if err != nil {
			t.Fatal(err)
		}
		bounded, err := st.ClosedRunsForRetentionBounded(fx.cutoff, floor)
		if err != nil {
			t.Fatal(err)
		}
		dOld := retentionSortedIDs(deletableIDs(full, fx.cutoff))
		dNew := retentionSortedIDs(deletableIDs(bounded, fx.cutoff))
		if fmt.Sprint(dOld) != fmt.Sprint(dNew) {
			t.Fatalf("seed=%d: delete sets differ: full=%d bounded=%d", seed, len(dOld), len(dNew))
		}
		if len(dOld) == 0 {
			t.Fatalf("seed=%d: fixture produced nothing deletable; the equality check proves nothing", seed)
		}
	}
}

// TestClosedRetentionBoundedReadKeepsSlowOrderFloor is the case the floor anchor
// exists for: a slow order whose every run is older than the cutoff must still
// keep its newest `floor` runs, not delete them because nothing newer exists in
// the candidate set.
func TestClosedRetentionBoundedReadKeepsSlowOrderFloor(t *testing.T) {
	const floor = minClosedOrderTrackingRetained
	now := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	cutoff := now.Add(-72 * time.Hour)
	var all []beads.Bead
	for k := 0; k < 25; k++ {
		c := now.Add(-100*time.Hour - time.Duration(k)*time.Hour)
		all = append(all, beads.Bead{
			ID: fmt.Sprintf("slow-%02d", k), Title: "order:weekly", Status: "closed", Type: "task",
			Labels: []string{"order-run:weekly", "order-tracking"}, CreatedAt: c, UpdatedAt: c.Add(time.Minute),
		})
	}
	st := orders.NewStore(beads.OrdersStore{Store: beads.NewMemStoreFrom(len(all), all, nil)})
	bounded, err := st.ClosedRunsForRetentionBounded(cutoff, floor)
	if err != nil {
		t.Fatal(err)
	}
	got := deletableIDs(bounded, cutoff)
	if len(got) != 25-floor {
		t.Fatalf("slow order: %d deletable, want %d (25 runs, floor %d)", len(got), 25-floor, floor)
	}
	for k := 0; k < floor; k++ {
		if _, bad := got[fmt.Sprintf("slow-%02d", k)]; bad {
			t.Fatalf("slow-%02d is among the newest %d runs and must be kept", k, floor)
		}
	}
}

// TestClosedRetentionBoundedReadSteadyStateIsSmall pins the point of the change:
// when nothing is past the cutoff the bounded read returns nothing, instead of
// listing the whole closed corpus.
func TestClosedRetentionBoundedReadSteadyStateIsSmall(t *testing.T) {
	now := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	var all []beads.Bead
	for k := 0; k < 5000; k++ {
		c := now.Add(-time.Duration(k) * time.Second)
		all = append(all, beads.Bead{
			ID: fmt.Sprintf("hot-%05d", k), Title: "order:busy", Status: "closed", Type: "task",
			Labels: []string{"order-run:busy", "order-tracking"}, CreatedAt: c, UpdatedAt: c.Add(time.Second),
		})
	}
	st := orders.NewStore(beads.OrdersStore{Store: beads.NewMemStoreFrom(len(all), all, nil)})
	full, err := st.ClosedRunsForRetention()
	if err != nil {
		t.Fatal(err)
	}
	bounded, err := st.ClosedRunsForRetentionBounded(now.Add(-72*time.Hour), minClosedOrderTrackingRetained)
	if err != nil {
		t.Fatal(err)
	}
	if len(full) != 5000 || len(bounded) != 0 {
		t.Fatalf("full read=%d (want 5000), bounded read=%d (want 0: nothing is past the cutoff)", len(full), len(bounded))
	}
}
