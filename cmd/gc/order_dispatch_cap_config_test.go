package main

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/orders"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
)

// A city with ~65 always-due orders and the fixed cap of 4 rotated through
// them every ~20 minutes on GC3 (2026-10-01): a 15s wake order and 3m review
// patrols each ran every ~20m. The cap must be configurable per city.
func TestOrderDispatcherCapComesFromCityConfig(t *testing.T) {
	intPtr := func(v int) *int { return &v }
	cases := []struct {
		name string
		cfg  *int
		want int
	}{
		{"unset keeps the build default", nil, defaultMaxOrderDispatchesPerTick},
		{"explicit cap", intPtr(32), 32},
		{"zero removes the cap", intPtr(0), 0},
		{"negative falls back to the default", intPtr(-1), defaultMaxOrderDispatchesPerTick},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &config.City{}
			cfg.Orders.MaxDispatchesPerTick = tc.cfg
			m := newMemoryOrderDispatcher(nil, t.TempDir(), cfg, events.Discard, nil)
			defer m.dispatchCancel()
			if m.maxDispatchesPerTick != tc.want {
				t.Fatalf("maxDispatchesPerTick = %d, want %d", m.maxDispatchesPerTick, tc.want)
			}
		})
	}
}

func TestOrdersConfigParsesMaxDispatchesPerTick(t *testing.T) {
	cfg, err := config.Parse([]byte("[workspace]\nname = \"c\"\n\n[orders]\nmax_dispatches_per_tick = 24\n"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if cfg.Orders.MaxDispatchesPerTick == nil || *cfg.Orders.MaxDispatchesPerTick != 24 {
		t.Fatalf("MaxDispatchesPerTick = %v, want 24", cfg.Orders.MaxDispatchesPerTick)
	}
}

// End to end through dispatch(): with the configured cap, every always-due
// order fires in ONE tick; with upstream's 4 only four do.
func TestConfiguredCapFiresThatManyDueOrdersInOneTick(t *testing.T) {
	for _, tc := range []struct {
		cap, orders, want int
	}{{32, 32, 32}, {4, 32, 4}, {0, 40, 40}} {
		t.Run(fmt.Sprintf("cap%d_orders%d", tc.cap, tc.orders), func(t *testing.T) {
			store := beads.NewMemStore()
			var aa []orders.Order
			for i := 0; i < tc.orders; i++ {
				aa = append(aa, orders.Order{Name: fmt.Sprintf("due-%d", i), Trigger: "condition", Check: "true", Exec: "true"})
			}
			ad := buildOrderDispatcherFromListExec(aa, store, nil, func(context.Context, string, string, []string) ([]byte, error) {
				return []byte("ok\n"), nil
			}, nil)
			m := ad.(*memoryOrderDispatcher)
			cfg := &config.City{}
			capValue := tc.cap
			cfg.Orders.MaxDispatchesPerTick = &capValue
			m.maxDispatchesPerTick = orderDispatchesPerTick(cfg)
			ad.dispatch(context.Background(), t.TempDir(), time.Date(2026, 10, 1, 22, 0, 0, 0, time.UTC))
			ad.drain(context.Background())
			if got := countOrderTrackingRuns(t, store); got != tc.want {
				t.Fatalf("one tick dispatched %d orders, want %d", got, tc.want)
			}
		})
	}
}
