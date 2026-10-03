package main

import (
	"io"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/orders"
)

func TestOrderTrackingWatchdogStaleAfterOutlastsConfiguredTimeouts(t *testing.T) {
	for _, tc := range []struct {
		name string
		aa   []orders.Order
		want time.Duration
	}{
		{name: "no orders keeps the base cutoff", want: orderTrackingSweepWatchdogStaleAfter},
		{name: "short formula order keeps the base cutoff", aa: []orders.Order{{Name: "f", Formula: "mol-x"}}, want: orderTrackingSweepWatchdogStaleAfter},
		{name: "exec order default timeout (300s) plus grace", aa: []orders.Order{{Name: "e", Exec: "true"}}, want: 300*time.Second + orderTrackingWatchdogTimeoutGrace},
		{name: "longest explicit timeout wins", aa: []orders.Order{{Name: "a", Exec: "true", Timeout: "90s"}, {Name: "b", Exec: "true", Timeout: "10m"}}, want: 10*time.Minute + orderTrackingWatchdogTimeoutGrace},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := orderTrackingWatchdogStaleAfter(tc.aa); got != tc.want {
				t.Fatalf("orderTrackingWatchdogStaleAfter = %s, want %s", got, tc.want)
			}
		})
	}
}

// TestOrderTrackingSweepWatchdogSparesARunInsideItsTimeout pins gcw-gpefh: a
// 10m-timeout exec order whose run is 3m old must keep its tracking bead, so the
// cooldown order is not re-dispatched while the first run is still live. A
// tracking bead older than the order's timeout plus grace is still recovered.
func TestOrderTrackingSweepWatchdogSparesARunInsideItsTimeout(t *testing.T) {
	store := beads.NewMemStore()
	live, err := store.Create(beads.Bead{
		Title:  "order:pr-merge-queue",
		Labels: []string{"order-run:pr-merge-queue", labelOrderTracking},
	})
	if err != nil {
		t.Fatalf("Create(live): %v", err)
	}
	cr := &CityRuntime{
		cityName:            "test-city",
		cfg:                 &config.City{Workspace: config.Workspace{Name: "test-city"}},
		standaloneCityStore: store,
		od:                  &memoryOrderDispatcher{aa: []orders.Order{{Name: "pr-merge-queue", Exec: "true", Timeout: "10m"}}},
		stdout:              io.Discard,
		stderr:              io.Discard,
		logPrefix:           "gc test",
	}

	cr.runOrderTrackingSweepWatchdog(cr.cfg, live.CreatedAt.Add(3*time.Minute))
	got, err := store.Get(live.ID)
	if err != nil {
		t.Fatalf("Get(live): %v", err)
	}
	if got.Status == "closed" {
		t.Fatalf("watchdog closed the tracking bead of a run 3m into its 10m timeout; the order would re-fire while still running")
	}

	cr.orderSweepWatchdogLast = time.Time{}
	cr.runOrderTrackingSweepWatchdog(cr.cfg, live.CreatedAt.Add(10*time.Minute+orderTrackingWatchdogTimeoutGrace+time.Second))
	got, err = store.Get(live.ID)
	if err != nil {
		t.Fatalf("Get(stale): %v", err)
	}
	if got.Status != "closed" {
		t.Fatalf("tracking bead past timeout+grace status = %s, want closed (the watchdog must still recover crashed runs)", got.Status)
	}
}
