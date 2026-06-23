package config

import "testing"

func TestValidateRigsRejectsInvalidSlingStrategy(t *testing.T) {
	rigs := []Rig{{Name: "demo", Path: "demo", Prefix: "de", DefaultSlingStrategy: "roundrobin"}}
	if err := ValidateRigs(rigs, "hq"); err == nil {
		t.Fatal("expected error for invalid default_sling_strategy, got nil")
	}
}

func TestValidateRigsAcceptsValidSlingStrategies(t *testing.T) {
	for _, s := range []string{"", "random", "round_robin"} {
		rigs := []Rig{{Name: "demo", Path: "demo", Prefix: "de", DefaultSlingStrategy: s}}
		if err := ValidateRigs(rigs, "hq"); err != nil {
			t.Fatalf("strategy %q: unexpected error: %v", s, err)
		}
	}
}
