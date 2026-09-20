package config

import (
	"testing"

	"github.com/BurntSushi/toml"
)

func TestWorkKindConfigDecoding(t *testing.T) {
	cityTOML := `
[work_kinds.review]
name = "review"
require_metadata = ["molecule_id", "review_context"]
require_route_target = "agent"
freshness_binding = "head_matches_pr"
match_metadata = ["review_context"]
`
	var city City
	if _, err := toml.Decode(cityTOML, &city); err != nil {
		t.Fatalf("decode city TOML: %v", err)
	}

	if len(city.WorkKinds) != 1 {
		t.Fatalf("got %d work kinds, want 1", len(city.WorkKinds))
	}
	wk, ok := city.WorkKinds["review"]
	if !ok {
		t.Fatalf("missing 'review' work kind")
	}
	if wk.Name != "review" {
		t.Errorf("wk.Name = %q, want 'review'", wk.Name)
	}
	if len(wk.RequireMetadata) != 2 || wk.RequireMetadata[0] != "molecule_id" || wk.RequireMetadata[1] != "review_context" {
		t.Errorf("wk.RequireMetadata = %v, want ['molecule_id', 'review_context']", wk.RequireMetadata)
	}
	if wk.RequireRouteTarget != "agent" {
		t.Errorf("wk.RequireRouteTarget = %q, want 'agent'", wk.RequireRouteTarget)
	}
	if wk.FreshnessBinding != "head_matches_pr" {
		t.Errorf("wk.FreshnessBinding = %q, want 'head_matches_pr'", wk.FreshnessBinding)
	}
	if len(wk.MatchMetadata) != 1 || wk.MatchMetadata[0] != "review_context" {
		t.Errorf("wk.MatchMetadata = %v, want ['review_context']", wk.MatchMetadata)
	}
}

func TestMergeCityWorkKinds(t *testing.T) {
	city := &City{
		WorkKinds: map[string]WorkKind{
			"custom": {Name: "city-custom"},
			"shared": {Name: "city-shared"},
		},
	}

	packWorkKinds := map[string]WorkKind{
		"shared": {Name: "pack-shared"},
		"extra":  {Name: "pack-extra"},
	}

	mergeCityWorkKinds(city, packWorkKinds)

	if len(city.WorkKinds) != 3 {
		t.Fatalf("got %d work kinds, want 3", len(city.WorkKinds))
	}
	if city.WorkKinds["custom"].Name != "city-custom" {
		t.Errorf("custom = %s, want city-custom", city.WorkKinds["custom"].Name)
	}
	if city.WorkKinds["shared"].Name != "city-shared" {
		t.Errorf("shared = %s, want city-shared (city wins)", city.WorkKinds["shared"].Name)
	}
	if city.WorkKinds["extra"].Name != "pack-extra" {
		t.Errorf("extra = %s, want pack-extra", city.WorkKinds["extra"].Name)
	}
}

func TestDeepCopyWorkKinds(t *testing.T) {
	orig := map[string]WorkKind{
		"review": {
			Name:            "review",
			RequireMetadata: []string{"a", "b"},
			MatchMetadata:   []string{"c"},
		},
	}

	copied := DeepCopyWorkKinds(orig)
	copied["review"].RequireMetadata[0] = "mutated"

	if orig["review"].RequireMetadata[0] != "a" {
		t.Errorf("DeepCopyWorkKinds did not perform deep copy of slice")
	}
}
