package doctor

import (
	"testing"

	"github.com/gastownhall/gascity/internal/config"
)

// The fork's dispatcher runs a larger build default than config's when
// orders.max_dispatches_per_tick is unset (cmd/gc passes it in as the default
// budget). The floor must be computed from the cap in force, or the check
// flags intervals the live budget meets and advises a budget below it
// (T10 arming-review).
func TestOrderDispatchFloorUsesTheCallersDefaultBudgetOnlyWhenUnset(t *testing.T) {
	intPtr := func(v int) *int { return &v }
	const ring = 65

	unset := &config.City{}
	if _, budget, passes := orderDispatchFloor(unset, ring, 0); budget != config.DefaultMaxOrderDispatchesPerTick || passes != (ring+budget-1)/budget {
		t.Fatalf("no caller default: budget=%d passes=%d, want config's default %d", budget, passes, config.DefaultMaxOrderDispatchesPerTick)
	}
	if _, budget, passes := orderDispatchFloor(unset, ring, 32); budget != 32 || passes != 3 {
		t.Fatalf("caller default 32, unset: budget=%d passes=%d, want 32 and 3", budget, passes)
	}
	for _, configured := range []*int{intPtr(8)} {
		set := &config.City{Orders: config.OrdersConfig{MaxDispatchesPerTick: configured}}
		if _, budget, passes := orderDispatchFloor(set, ring, 32); budget != 8 || passes != 9 {
			t.Fatalf("configured %d with caller default 32: budget=%d passes=%d, want the configured 8 and 9 passes", *configured, budget, passes)
		}
	}
	for _, notPositive := range []int{0, -3} {
		bad := &config.City{Orders: config.OrdersConfig{MaxDispatchesPerTick: intPtr(notPositive)}}
		if _, budget, _ := orderDispatchFloor(bad, ring, 32); budget != 32 {
			t.Fatalf("configured %d (not positive) with caller default 32: budget=%d, want the caller default 32", notPositive, budget)
		}
	}
}
