package config

import (
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
)

func TestDefaultContextAdvisory(t *testing.T) {
	d := DefaultContextAdvisory()
	if d.Enabled == nil || !*d.Enabled {
		t.Fatalf("default should be enabled")
	}
	if len(d.Tiers) != 2 {
		t.Fatalf("want 2 default tiers, got %d", len(d.Tiers))
	}
	if *d.Tiers[0].Threshold != 60 || *d.Tiers[1].Threshold != 80 {
		t.Fatalf("want default thresholds 60/80, got %d/%d", *d.Tiers[0].Threshold, *d.Tiers[1].Threshold)
	}
	for _, tier := range d.Tiers {
		if !strings.Contains(*tier.Message, "gc handoff") {
			t.Errorf("default tier message should recommend `gc handoff`, got: %s", *tier.Message)
		}
		if strings.Contains(*tier.Message, "gc session reset") {
			t.Errorf("default tier message must NOT recommend `gc session reset`: %s", *tier.Message)
		}
	}
	// Defaults must themselves be valid.
	if err := d.Validate(); err != nil {
		t.Fatalf("built-in defaults failed validation: %v", err)
	}
}

func TestContextAdvisoryTOMLParse_TopLevel(t *testing.T) {
	src := `
enabled = true
window_tokens = 120000
[[tiers]]
threshold = 50
message = "half"
[[tiers]]
threshold = 90
message = "danger"
enabled = false
`
	var ca ContextAdvisory
	if _, err := toml.Decode(src, &ca); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if ca.Enabled == nil || !*ca.Enabled || ca.WindowTokens == nil || *ca.WindowTokens != 120000 {
		t.Fatalf("top-level fields not parsed: %+v", ca)
	}
	if len(ca.Tiers) != 2 || *ca.Tiers[0].Threshold != 50 || *ca.Tiers[1].Threshold != 90 {
		t.Fatalf("tier array not parsed: %+v", ca.Tiers)
	}
	if ca.Tiers[1].Enabled == nil || *ca.Tiers[1].Enabled {
		t.Fatalf("per-tier enabled=false not parsed")
	}
}

func TestContextAdvisoryTOMLParse_NestedScope(t *testing.T) {
	// Simulates the per-agent [context_advisory] table (same shape used under
	// [agent_defaults.context_advisory]).
	src := `
[context_advisory]
[[context_advisory.tiers]]
threshold = 80
message = "Approaching limit — run the handoff skill now."
`
	var wrap struct {
		ContextAdvisory ContextAdvisory `toml:"context_advisory"`
	}
	if _, err := toml.Decode(src, &wrap); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(wrap.ContextAdvisory.Tiers) != 1 || *wrap.ContextAdvisory.Tiers[0].Threshold != 80 {
		t.Fatalf("nested scope tier not parsed: %+v", wrap.ContextAdvisory)
	}
}

func TestResolveContextAdvisory_PrecedenceAndFallthrough(t *testing.T) {
	builtin := DefaultContextAdvisory()
	global := &ContextAdvisory{WindowTokens: advPtr(1_000_000)} // only sets window
	perAgent := &ContextAdvisory{                               // replaces tiers, leaves enabled/window to fall through
		Tiers: []ContextAdvisoryTier{{Threshold: advPtr(75), Message: advPtr("agent tier")}},
	}
	env := &ContextAdvisory{WindowTokens: advPtr(200000)} // highest precedence window

	pol := ResolveContextAdvisory(&builtin, global, perAgent, env)

	if !pol.Enabled {
		t.Errorf("enabled should fall through from built-in true")
	}
	if pol.WindowTokens != 200000 {
		t.Errorf("env window override should win, got %d", pol.WindowTokens)
	}
	if len(pol.Tiers) != 1 || pol.Tiers[0].Threshold != 75 {
		t.Errorf("per-agent tiers should replace built-in, got %+v", pol.Tiers)
	}
}

func TestResolveContextAdvisory_EnabledFalseAtScopeDisables(t *testing.T) {
	builtin := DefaultContextAdvisory()
	cases := []struct {
		name   string
		scopes []*ContextAdvisory
	}{
		{"global disables", []*ContextAdvisory{&builtin, {Enabled: advPtr(false)}}},
		{"per-agent disables over enabled global", []*ContextAdvisory{&builtin, {Enabled: advPtr(true)}, {Enabled: advPtr(false)}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if pol := ResolveContextAdvisory(tc.scopes...); pol.Enabled {
				t.Errorf("expected disabled")
			}
		})
	}
}

func TestResolveContextAdvisory_DropsDisabledTiers(t *testing.T) {
	ca := &ContextAdvisory{
		Enabled: advPtr(true),
		Tiers: []ContextAdvisoryTier{
			{Threshold: advPtr(90), Message: advPtr("b")},
			{Threshold: advPtr(60), Message: advPtr("a")},
			{Threshold: advPtr(80), Message: advPtr("disabled"), Enabled: advPtr(false)},
		},
	}
	pol := ResolveContextAdvisory(ca)
	if len(pol.Tiers) != 2 {
		t.Fatalf("disabled tier should be dropped, got %d tiers", len(pol.Tiers))
	}
	if pol.Tiers[0].Threshold != 60 || pol.Tiers[1].Threshold != 90 {
		t.Fatalf("tiers should be sorted ascending, got %+v", pol.Tiers)
	}
}

func TestSelectTier(t *testing.T) {
	pol := ResolveContextAdvisory(&ContextAdvisory{
		Enabled: advPtr(true),
		Tiers: []ContextAdvisoryTier{
			{Threshold: advPtr(60), Message: advPtr("advisory")},
			{Threshold: advPtr(80), Message: advPtr("urgent")},
		},
	})
	cases := []struct {
		pct      float64
		wantMsg  string
		wantFire bool
	}{
		{50, "", false},
		{60, "advisory", true},   // boundary: pct == threshold fires that tier
		{79.9, "advisory", true}, // between tiers: highest crossed
		{80, "urgent", true},     // boundary
		{100, "urgent", true},
	}
	for _, tc := range cases {
		got, fired := pol.SelectTier(tc.pct)
		if fired != tc.wantFire || (fired && got.Message != tc.wantMsg) {
			t.Errorf("SelectTier(%.1f) = (%q,%v), want (%q,%v)", tc.pct, got.Message, fired, tc.wantMsg, tc.wantFire)
		}
	}
	// Disabled policy never fires.
	if _, fired := (ContextAdvisoryPolicy{Enabled: false, Tiers: pol.Tiers}).SelectTier(99); fired {
		t.Errorf("disabled policy should not fire")
	}
}

func TestContextAdvisoryValidate(t *testing.T) {
	good := &ContextAdvisory{Tiers: []ContextAdvisoryTier{
		{Threshold: advPtr(60), Message: advPtr("ok {{.Pct}}")},
		{Threshold: advPtr(80), Message: advPtr("ok2")},
	}}
	if err := good.Validate(); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}

	bad := map[string]*ContextAdvisory{
		"out of range high": {Tiers: []ContextAdvisoryTier{{Threshold: advPtr(101), Message: advPtr("m")}}},
		"out of range low":  {Tiers: []ContextAdvisoryTier{{Threshold: advPtr(-1), Message: advPtr("m")}}},
		"descending":        {Tiers: []ContextAdvisoryTier{{Threshold: advPtr(80), Message: advPtr("m")}, {Threshold: advPtr(60), Message: advPtr("m")}}},
		"duplicate":         {Tiers: []ContextAdvisoryTier{{Threshold: advPtr(60), Message: advPtr("m")}, {Threshold: advPtr(60), Message: advPtr("m")}}},
		"missing threshold": {Tiers: []ContextAdvisoryTier{{Message: advPtr("m")}}},
		"empty message":     {Tiers: []ContextAdvisoryTier{{Threshold: advPtr(60), Message: advPtr("   ")}}},
		"missing message":   {Tiers: []ContextAdvisoryTier{{Threshold: advPtr(60)}}},
		"bad template":      {Tiers: []ContextAdvisoryTier{{Threshold: advPtr(60), Message: advPtr("{{.Pct")}}},
		"unknown var":       {Tiers: []ContextAdvisoryTier{{Threshold: advPtr(60), Message: advPtr("{{.Nope}}")}}},
	}
	for name, c := range bad {
		if err := c.Validate(); err == nil {
			t.Errorf("expected %q to fail validation", name)
		}
	}
}

func TestRenderTier(t *testing.T) {
	view := ContextAdvisoryView{UsedK: "128k", WindowK: "200k", Pct: 64}
	got := RenderTier(ResolvedTier{Threshold: 60, Message: "{{.UsedK}}/{{.WindowK}} ~{{printf \"%.0f\" .Pct}}%"}, view)
	if got != "128k/200k ~64%" {
		t.Errorf("render = %q", got)
	}
	// Fallback: a template that fails at execute returns the raw message rather
	// than blocking the prompt.
	raw := "{{.Nope}} literal"
	if got := RenderTier(ResolvedTier{Message: raw}, view); got != raw {
		t.Errorf("render fallback = %q, want raw %q", got, raw)
	}
}
