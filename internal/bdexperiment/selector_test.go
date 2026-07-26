package bdexperiment

import (
	"math/rand/v2"
	"testing"
)

func TestSelectDefaultsToShim(t *testing.T) {
	got := Select(Config{}, func(int) int { return 0 })
	if got != ArmShim {
		t.Fatalf("Select(default) = %q, want %q", got, ArmShim)
	}
}

func TestParseRejectsInvalidWeightsToShimControl(t *testing.T) {
	config := Parse(func(key string) string {
		if key == ArmsEnv {
			return "shim=90,direct=10,legacy=10"
		}
		return ""
	})
	if config.Valid {
		t.Fatal("Parse() marked invalid total valid")
	}
	if got := Select(config, func(int) int { return 99 }); got != ArmShim {
		t.Fatalf("Select(invalid) = %q, want %q", got, ArmShim)
	}
}

func TestParseRequiresEveryArmWeight(t *testing.T) {
	config := Parse(func(key string) string {
		if key == ArmsEnv {
			return "shim=100"
		}
		return ""
	})
	if config.Valid {
		t.Fatal("Parse() accepted omitted arm weights")
	}
}

func TestSelectUsesValidatedWeights(t *testing.T) {
	config := Parse(func(key string) string {
		if key == ArmsEnv {
			return "shim=45,direct=45,legacy=10"
		}
		return ""
	})
	for roll, want := range map[int]Arm{0: ArmShim, 44: ArmShim, 45: ArmDirect, 89: ArmDirect, 90: ArmLegacy, 99: ArmLegacy} {
		if got := Select(config, func(int) int { return roll }); got != want {
			t.Fatalf("Select(roll=%d) = %q, want %q", roll, got, want)
		}
	}
}

func TestParseForceArmOverridesWeights(t *testing.T) {
	config := Parse(func(key string) string {
		switch key {
		case ArmsEnv:
			return "shim=100,direct=0,legacy=0"
		case ForceArmEnv:
			return "direct"
		default:
			return ""
		}
	})
	if got := Select(config, func(int) int { return 0 }); got != ArmDirect {
		t.Fatalf("Select(force=direct) = %q, want %q", got, ArmDirect)
	}
}

func TestParseShapeOverrideAppliesOnlyToKnownShapes(t *testing.T) {
	config := Parse(func(key string) string {
		switch key {
		case ArmsEnv:
			return "shim=100,direct=0,legacy=0"
		case ShapeOverridesEnv:
			return "show_json=direct"
		default:
			return ""
		}
	})
	if got := SelectForShape(config, ShapeShowJSON, func(int) int { return 0 }); got != ArmDirect {
		t.Fatalf("SelectForShape(show) = %q, want %q", got, ArmDirect)
	}
	if got := SelectForShape(config, ShapeListJSON, func(int) int { return 0 }); got != ArmShim {
		t.Fatalf("SelectForShape(list) = %q, want %q", got, ArmShim)
	}
}

func TestParseRejectsUnsafeConfigGeneration(t *testing.T) {
	config := Parse(func(key string) string {
		if key == GenerationEnv {
			return "production-secret"
		}
		return ""
	})
	if config.Valid {
		t.Fatal("Parse() accepted non-numeric generation")
	}
}

func TestSelectDistributionStaysWithinConfiguredBands(t *testing.T) {
	config := Parse(func(key string) string {
		if key == ArmsEnv {
			return "shim=45,direct=45,legacy=10"
		}
		return ""
	})
	random := rand.New(rand.NewPCG(1, 2))
	counts := map[Arm]int{}
	const samples = 10000
	for range samples {
		counts[Select(config, random.IntN)]++
	}
	for arm, want := range map[Arm]float64{ArmShim: .45, ArmDirect: .45, ArmLegacy: .10} {
		got := float64(counts[arm]) / samples
		if delta := got - want; delta < -.025 || delta > .025 {
			t.Fatalf("%s fraction = %.3f, want %.2f ± .025", arm, got, want)
		}
	}
}
