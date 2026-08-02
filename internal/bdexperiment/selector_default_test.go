package bdexperiment

import "testing"

// TestUnsetArmsDefaultsToInProcess pins that gc serves an approved shape itself
// rather than spawning the bdshim binary when no arm weights are configured.
//
// The default used to be shim=100, which made every `gc bd` on an approved read
// shape locate and exec a sibling binary gc does not ship. That is the
// separate-artifact coupling, and it was never justified on performance:
// measured on `gc bd ready --json --limit 1`, CPU time, interleaved, n=13,
// the exec arm costs 58.8 ms against the in-process arm's 55.6 ms — spawning
// the shim is 3.3 ms MORE expensive, because gc has already paid its own
// startup by the time it decides.
//
// Byte parity was verified live across all approved shapes before this flipped
// (5/5 identical stdout, stderr and exit code between the two arms):
// ready --limit 5, ready --limit 1, ready --unassigned, query ephemeral,
// mol current.
//
// An explicit GC_BD_EXPERIMENT_ARMS still wins, and an absent shim already fell
// back to in-process (cmd/gc/bd_fastpath.go), so this only changes which arm is
// preferred when the binary happens to be present.
func TestUnsetArmsDefaultsToInProcess(t *testing.T) {
	cfg := Parse(func(string) string { return "" })
	if !cfg.Valid {
		t.Fatal("unset arms should still be a valid config")
	}
	if got := cfg.Weights[ArmDirect]; got != 100 {
		t.Errorf("default ArmDirect weight = %d, want 100", got)
	}
	if got := cfg.Weights[ArmShim]; got != 0 {
		t.Errorf("default ArmShim weight = %d, want 0 (gc should not prefer spawning a sibling binary)", got)
	}
	if arm := SelectForShape(cfg, ShapeReadyJSON, func(int) int { return 50 }); arm != ArmDirect {
		t.Errorf("SelectForShape with unset arms = %v, want %v", arm, ArmDirect)
	}
}

// TestExplicitArmsStillOverrideTheDefault pins that operators keep control.
func TestExplicitArmsStillOverrideTheDefault(t *testing.T) {
	cfg := Parse(func(k string) string {
		if k == ArmsEnv {
			return "shim=100,direct=0,legacy=0"
		}
		return ""
	})
	if !cfg.Valid {
		t.Fatal("explicit arms should parse")
	}
	if arm := SelectForShape(cfg, ShapeReadyJSON, func(int) int { return 50 }); arm != ArmShim {
		t.Errorf("explicit shim=100 selected %v, want %v", arm, ArmShim)
	}
}
