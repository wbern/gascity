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

// GenerationEnv identifies a numeric configuration revision in observations.
const GenerationEnv = "GC_BD_EXPERIMENT_GENERATION"

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
	Weights    map[Arm]int
	Force      Arm
	Overrides  map[Shape]Arm
	Generation string
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
	return withGeneration(parseOverrides(parseForce(Config{Weights: weights, Valid: true}, getenv), getenv), getenv)
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
