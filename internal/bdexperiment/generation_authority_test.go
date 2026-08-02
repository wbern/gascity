package bdexperiment

import (
	"fmt"
	"testing"
)

// An explicit GC_BD_EXPERIMENT_ARMS outranks the built-in default, which is
// right for a deliberate choice and wrong for an inherited one. Agent sessions
// outlive supervisor bounces, so a weighting exported during a past experiment
// survives in the environment and silently suppresses every later default.
//
// These tests pin that a config declaring an OLDER generation is retired, that
// the two cases which must NOT be retired still are not, and — the part that
// makes it debuggable — that a retired config says so in the observation.
//
// Each asserts the SELECTED ARM, not merely what Parse returned. A test that
// only checked Parse's fields would pass with the whole mechanism inert, which
// is exactly the state this replaces.

// envFrom builds a getenv over a fixed map.
func envFrom(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

// alwaysFirst is a deterministic stand-in for the weighted draw: it always
// selects the first eligible arm, so the outcome is a pure function of weights.
func alwaysFirst(int) int { return 0 }

// TestStaleGenerationIsRetiredInFavourOfTheDefault is the load-bearing case: a
// weighting from an older generation must stop governing.
func TestStaleGenerationIsRetiredInFavourOfTheDefault(t *testing.T) {
	stale := envFrom(map[string]string{
		ArmsEnv:       "shim=95,direct=5,legacy=0",
		GenerationEnv: fmt.Sprint(CurrentGeneration - 1),
	})
	config := Parse(stale)
	if !config.Valid {
		t.Fatal("Parse returned an invalid config for a well-formed stale weighting")
	}
	if !config.Superseded {
		t.Error("a weighting from an older generation was honored; the generation is still inert")
	}
	if got := config.Weights[ArmDirect]; got != 100 {
		t.Errorf("direct weight = %d, want the built-in default 100; the stale weighting still governs", got)
	}
	// The property that actually matters to a caller.
	if arm := Select(config, alwaysFirst); arm != ArmDirect {
		t.Errorf("selected arm = %q, want %q — the stale 95%% shim weighting is still steering traffic", arm, ArmDirect)
	}
}

// TestCurrentGenerationIsHonored pins that the mechanism does not retire the
// configuration the fleet is actually running. If this fails, a deploy would
// silently ignore the weighting devops just set.
func TestCurrentGenerationIsHonored(t *testing.T) {
	current := envFrom(map[string]string{
		ArmsEnv:       "shim=100,direct=0,legacy=0",
		GenerationEnv: fmt.Sprint(CurrentGeneration),
	})
	config := Parse(current)
	if config.Superseded {
		t.Fatal("the current generation was retired; a deploy would ignore the configured weighting")
	}
	if arm := Select(config, alwaysFirst); arm != ArmShim {
		t.Errorf("selected arm = %q, want %q from the honored weighting", arm, ArmShim)
	}
}

// TestNewerGenerationIsHonored pins the direction. An older binary in a mixed
// fleet must not discard a rollout that is ahead of it — retiring a NEWER config
// would invert the mechanism into the very failure it exists to prevent.
func TestNewerGenerationIsHonored(t *testing.T) {
	ahead := envFrom(map[string]string{
		ArmsEnv:       "shim=100,direct=0,legacy=0",
		GenerationEnv: fmt.Sprint(CurrentGeneration + 1),
	})
	config := Parse(ahead)
	if config.Superseded {
		t.Fatal("a newer generation was retired; an older binary would fight a rollout ahead of it")
	}
	if arm := Select(config, alwaysFirst); arm != ArmShim {
		t.Errorf("selected arm = %q, want %q", arm, ArmShim)
	}
}

// TestUnversionedConfigIsHonored pins the backward-compatible case. Pinning arms
// by hand while debugging is legitimate and does not come with a generation;
// refusing it would break that silently, which is the same failure mode in a new
// costume.
func TestUnversionedConfigIsHonored(t *testing.T) {
	manual := envFrom(map[string]string{ArmsEnv: "shim=100,direct=0,legacy=0"})
	config := Parse(manual)
	if config.Superseded {
		t.Fatal("an unversioned hand-set weighting was retired; operator pinning is broken")
	}
	if arm := Select(config, alwaysFirst); arm != ArmShim {
		t.Errorf("selected arm = %q, want %q", arm, ArmShim)
	}
}

// TestSupersededConfigIsObservable pins the half that makes this debuggable. The
// incident behind this mechanism was a config silently outranking a default; a
// config silently IGNORED is the same class of bug, so the observation must
// carry the reason rather than leaving a reader to infer it from a weight.
func TestSupersededConfigIsObservable(t *testing.T) {
	config := Parse(envFrom(map[string]string{
		ArmsEnv:       "shim=95,direct=5,legacy=0",
		GenerationEnv: fmt.Sprint(CurrentGeneration - 1),
	}))
	record := Record{
		Schema: SchemaVersion, Build: "fork-test", Arm: Select(config, alwaysFirst),
		Verb: "ready", Shape: ShapeReadyJSON, Disposition: "controller",
		ConfigGeneration: config.Generation, ConfigSuperseded: config.Superseded,
	}
	if !validRecord(record) {
		t.Fatal("a record carrying ConfigSuperseded is rejected by the validator")
	}
	if !record.ConfigSuperseded {
		t.Error("the observation does not record that the configuration was retired")
	}
}

// TestMalformedGenerationStillInvalidatesTheConfig pins that the new rule did
// not swallow the existing one: a non-numeric generation must invalidate rather
// than be treated as absent, because guessing at a corrupt value is how a
// silently-wrong arm selection starts.
func TestMalformedGenerationStillInvalidatesTheConfig(t *testing.T) {
	config := Parse(envFrom(map[string]string{
		ArmsEnv:       "shim=50,direct=50,legacy=0",
		GenerationEnv: "not-a-number",
	}))
	if config.Valid {
		t.Fatal("a malformed generation produced a valid config")
	}
	if arm := Select(config, alwaysFirst); arm != ArmShim {
		t.Errorf("invalid config selected %q, want the safe control %q", arm, ArmShim)
	}
}
