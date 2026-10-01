package config

import (
	"strings"
	"testing"
)

func TestValidateDurationsWarnsOnNegativeMaxDispatchesPerTick(t *testing.T) {
	neg, ok := -2, 8
	cfg := &City{}
	cfg.Orders.MaxDispatchesPerTick = &neg
	warnings := ValidateDurations(cfg, "city.toml")
	if len(warnings) != 1 || !strings.Contains(warnings[0], "max_dispatches_per_tick = -2 is negative") {
		t.Fatalf("warnings = %q, want one negative max_dispatches_per_tick warning", warnings)
	}
	cfg.Orders.MaxDispatchesPerTick = &ok
	if got := ValidateDurations(cfg, "city.toml"); len(got) != 0 {
		t.Fatalf("warnings = %q for a valid cap", got)
	}
}
