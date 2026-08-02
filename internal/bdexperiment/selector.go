// Package bdexperiment selects and observes the bounded gc bd read experiment.
package bdexperiment

import (
	"strconv"
	"strings"
)

// Arm identifies one implementation used for an eligible controller read.
type Arm string

const (
	// ArmShim preserves the current child-bdshim control path.
	ArmShim Arm = "shim"
	// ArmDirect invokes the controller dispatcher in this gc process.
	ArmDirect Arm = "direct"
	// ArmLegacy leaves the early path and runs ordinary gc bd.
	ArmLegacy Arm = "legacy"
)

// ArmsEnv configures the global experiment arm weights.
const ArmsEnv = "GC_BD_EXPERIMENT_ARMS"

// ForceArmEnv pins every eligible invocation to one troubleshooting arm.
const ForceArmEnv = "GC_BD_EXPERIMENT_FORCE_ARM"

// ShapeOverridesEnv pins individual approved command shapes to an arm.
const ShapeOverridesEnv = "GC_BD_EXPERIMENT_SHAPE_OVERRIDES"

// GenerationEnv identifies a numeric configuration revision. It is both stamped
// onto observations and load-bearing: see CurrentGeneration.
const GenerationEnv = "GC_BD_EXPERIMENT_GENERATION"

// CurrentGeneration is the arm-configuration revision this binary expects.
//
// An explicit GC_BD_EXPERIMENT_ARMS wins over the built-in default by design,
// which is correct for a deliberate choice and wrong for an inherited one. Agent
// sessions outlive supervisor bounces, so a weighting exported during some past
// experiment survives in the environment indefinitely and silently outranks
// every later default. Measured on 2026-08-02: five of six live sessions carried
// shim=95,direct=5 from an earlier generation, suppressing a default that had
// already been deployed, and the value existed in no config file or shell
// profile anyone could point at.
//
// Bumping this retires such a config: an explicitly-versioned OLDER one is
// ignored in favor of the built-in default, so a stale weighting expires at the
// next binary upgrade instead of persisting until someone recycles the session.
const CurrentGeneration = 4

// Shape is the closed, value-free command shape vocabulary used by this experiment.
type Shape string

const (
	// ShapeShowJSON is the JSON point-lookup read shape.
	ShapeShowJSON Shape = "show_json"
	// ShapeListJSON is the proven JSON list read shape.
	ShapeListJSON Shape = "list_json"
	// ShapeQueryEphemeral is the proven ephemeral query read shape.
	ShapeQueryEphemeral Shape = "query_ephemeral"
	// ShapeReadyJSON is the federated ready read shape. bd ready has always
	// classified as Route; naming it here is what lets gc serve it in-process
	// instead of only by exec'ing the bdshim binary.
	ShapeReadyJSON Shape = "ready_json"
	// ShapeMolCurrent is the molecule current-state read shape.
	ShapeMolCurrent Shape = "mol_current"
	// ShapeMolProgress is the molecule progress read shape.
	ShapeMolProgress Shape = "mol_progress"
)

// Config holds the experiment weights. Its zero value is the safe control.
type Config struct {
	Weights   map[Arm]int
	Force     Arm
	Overrides map[Shape]Arm
	// Generation is the configuration revision this selection was made under.
	Generation string
	// Superseded records that an explicit arm weighting was IGNORED because it
	// declared a generation older than CurrentGeneration. It exists so the
	// observation can say so: a config silently ignored is the same class of bug
	// as a config silently honored, and the incident this mechanism answers was
	// precisely a silent override.
	Superseded bool
	Valid      bool
}

// Parse reads a closed arm-weight configuration. Any malformed configuration
// is marked invalid so callers retain the shim control path.
func Parse(getenv func(string) string) Config {
	raw := strings.TrimSpace(getenv(ArmsEnv))
	if raw == "" {
		// Default to serving approved shapes in-process rather than spawning the
		// bdshim binary. gc has already paid its own startup by the time it
		// decides, so the exec arm adds a second process for nothing: measured on
		// `gc bd ready --json --limit 1` (CPU time, interleaved, n=13) the exec arm
		// costs 58.8 ms against the in-process arm's 55.6 ms. Byte parity was
		// verified live across every approved shape first — ready (three shapes),
		// query ephemeral and mol current — 5/5 identical stdout, stderr and exit
		// code. An explicit GC_BD_EXPERIMENT_ARMS still wins, and an absent shim
		// already fell back here anyway, so this only changes which arm is
		// preferred when the binary happens to be installed.
		return withGeneration(parseOverrides(parseForce(Config{Weights: map[Arm]int{ArmDirect: 100}, Valid: true}, getenv), getenv), getenv)
	}
	weights := make(map[Arm]int, 3)
	for _, item := range strings.Split(raw, ",") {
		name, value, ok := strings.Cut(strings.TrimSpace(item), "=")
		arm := Arm(strings.TrimSpace(name))
		if !ok || (arm != ArmShim && arm != ArmDirect && arm != ArmLegacy) {
			return Config{}
		}
		weight, err := strconv.Atoi(strings.TrimSpace(value))
		if err != nil || weight < 0 || weight > 100 {
			return Config{}
		}
		if _, duplicate := weights[arm]; duplicate {
			return Config{}
		}
		weights[arm] = weight
	}
	if len(weights) != 3 || weights[ArmShim]+weights[ArmDirect]+weights[ArmLegacy] != 100 || weights[ArmLegacy] > 10 {
		return Config{}
	}
	config := Config{Weights: weights, Valid: true}
	if supersededGeneration(getenv) {
		config = Config{Weights: map[Arm]int{ArmDirect: 100}, Valid: true, Superseded: true}
	}
	return withGeneration(parseOverrides(parseForce(config, getenv), getenv), getenv)
}

// supersededGeneration reports whether an explicit arm weighting declares a
// generation this binary has moved past.
//
// The three cases are deliberate:
//
//   - ABSENT generation: honored. A hand-set GC_BD_EXPERIMENT_ARMS with no
//     generation is how an operator pins arms while debugging, and refusing it
//     would break that with no warning. Unversioned means "I mean it now".
//   - OLDER than CurrentGeneration: superseded. This is the inherited-stale case
//     the mechanism exists for.
//   - NEWER than CurrentGeneration: honored. An older binary in a mixed fleet
//     must not discard a rollout that is ahead of it; that would invert the
//     intended direction.
//
// A malformed generation is left to withGeneration, which invalidates the whole
// config rather than guessing.
func supersededGeneration(getenv func(string) string) bool {
	raw := strings.TrimSpace(getenv(GenerationEnv))
	if raw == "" {
		return false
	}
	declared, err := strconv.ParseUint(raw, 10, 32)
	if err != nil {
		return false
	}
	return declared < CurrentGeneration
}

func withGeneration(config Config, getenv func(string) string) Config {
	if !config.Valid {
		return config
	}
	generation := strings.TrimSpace(getenv(GenerationEnv))
	if generation == "" {
		config.Generation = "0"
		return config
	}
	if _, err := strconv.ParseUint(generation, 10, 32); err != nil {
		return Config{}
	}
	config.Generation = generation
	return config
}

func parseOverrides(config Config, getenv func(string) string) Config {
	raw := strings.TrimSpace(getenv(ShapeOverridesEnv))
	if raw == "" {
		return config
	}
	overrides := make(map[Shape]Arm)
	for _, item := range strings.Split(raw, ",") {
		name, value, ok := strings.Cut(strings.TrimSpace(item), "=")
		shape := Shape(strings.TrimSpace(name))
		arm := Arm(strings.TrimSpace(value))
		if !ok || !knownShape(shape) || (arm != ArmShim && arm != ArmDirect && arm != ArmLegacy) {
			return Config{}
		}
		if _, duplicate := overrides[shape]; duplicate {
			return Config{}
		}
		overrides[shape] = arm
	}
	config.Overrides = overrides
	return config
}

func parseForce(config Config, getenv func(string) string) Config {
	raw := strings.TrimSpace(getenv(ForceArmEnv))
	if raw == "" {
		return config
	}
	arm := Arm(raw)
	if arm != ArmShim && arm != ArmDirect && arm != ArmLegacy {
		return Config{}
	}
	config.Force = arm
	return config
}

// Select returns the selected experiment arm. The zero configuration always
// selects the existing shim control.
func Select(config Config, next func(int) int) Arm {
	return selectArm(config, next)
}

// SelectForShape selects an arm for one approved command shape.
func SelectForShape(config Config, shape Shape, next func(int) int) Arm {
	if !config.Valid || !knownShape(shape) {
		return ArmShim
	}
	if arm, ok := config.Overrides[shape]; ok {
		return arm
	}
	return selectArm(config, next)
}

func selectArm(config Config, next func(int) int) Arm {
	if !config.Valid {
		return ArmShim
	}
	if config.Force != "" {
		return config.Force
	}
	roll := next(100)
	if roll < 0 || roll >= 100 {
		return ArmShim
	}
	if roll < config.Weights[ArmShim] {
		return ArmShim
	}
	if roll < config.Weights[ArmShim]+config.Weights[ArmDirect] {
		return ArmDirect
	}
	return ArmLegacy
}

func knownShape(shape Shape) bool {
	switch shape {
	case ShapeShowJSON, ShapeListJSON, ShapeQueryEphemeral, ShapeReadyJSON, ShapeMolCurrent, ShapeMolProgress:
		return true
	default:
		return false
	}
}
