package main

import (
	"testing"

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
		{"unset keeps the upstream default", nil, defaultMaxOrderDispatchesPerTick},
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
