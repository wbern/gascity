package main

import (
	"bytes"
	"strings"
	"testing"
)

// TestRunRefusesRigScope pins the shim's core contract: it is a pure latency
// optimization, so any invocation it cannot serve byte-identically to raw bd
// must fail loudly rather than answer differently.
//
// --rig is a `gc bd` extension. Raw bd rejects it with "unknown flag: --rig".
// The shim used to strip it and answer from its own rig's store with exit 0,
// so `bd list --rig other-rig` returned THIS rig's beads and looked like a
// successful cross-rig query. A caller had no signal at all.
func TestRunRefusesRigScope(t *testing.T) {
	for _, args := range [][]string{
		{"--rig", "gas-city-infra", "list"},
		{"--rig=gas-city-infra", "list"},
		{"list", "--rig", "gas-city-infra"},
		{"show", "--rig=crm", "crm-1234"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := run(args, strings.NewReader(""), &stdout, &stderr)

			if code == 0 {
				t.Fatalf("run(%v) = 0; a --rig query the shim cannot honor must not report success (stdout=%q)", args, stdout.String())
			}
			if stdout.Len() != 0 {
				t.Errorf("run(%v) wrote %q to stdout; a refusal must not emit bead data that could be mistaken for a result", args, stdout.String())
			}
			msg := stderr.String()
			if !strings.Contains(msg, "--rig") {
				t.Errorf("stderr = %q, want it to name the offending flag", msg)
			}
			if !strings.Contains(msg, "gc bd") {
				t.Errorf("stderr = %q, want it to point at `gc bd`, which is the form that actually resolves cross-rig", msg)
			}
		})
	}
}

// TestRunAllowsCityScope guards the fix's blast radius: --city is a scope flag
// the shim genuinely honors (it overrides the routed target city), so it must
// keep working. Refusing both would be an over-correction.
func TestRunAllowsCityScope(t *testing.T) {
	city, rig, rest := extractScopeFlags([]string{"--city", "gc2", "list"})
	if city != "gc2" || rig != "" {
		t.Fatalf("extractScopeFlags = (%q,%q); want city gc2 and no rig", city, rig)
	}
	if len(rest) != 1 || rest[0] != "list" {
		t.Fatalf("rest = %v; want [list]", rest)
	}
}
