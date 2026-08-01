package main

import (
	"os"
	"path/filepath"
	"testing"
)

// TestPreflightBDBinaryPrefersRealBd pins that gc's own preflight spawns the
// real bd directly instead of the literal "bd", which PATH resolves to the shim
// installed ahead of it.
//
// The preflight's `bd context --json` is gc talking to itself: it is the only
// caller of that verb in the tree (cmd/gc/beads_preflight_checker.go), and it
// accounted for 21,780 of 21,782 logged context calls — 24.7% of ALL shim
// traffic. The shim routes none of them (context is 100% passthrough), so every
// one paid a process hop to reach the same binary gc could have spawned itself.
//
// GC_BD_REAL is the path the shim itself uses for passthrough, so honoring it
// here reaches exactly the binary the hop would have ended at.
func TestPreflightBDBinaryPrefersRealBd(t *testing.T) {
	realBd := filepath.Join(t.TempDir(), "bd")
	if err := os.WriteFile(realBd, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write fake bd: %v", err)
	}
	t.Setenv(realBdEnvVar, realBd)
	if got := preflightBDBinary(); got != realBd {
		t.Fatalf("preflightBDBinary() = %q, want %q", got, realBd)
	}
}

// TestPreflightBDBinaryFallsBackToPath pins the conservative fallback. The
// variable is set for sessions and order children, but not provably for every
// context that runs preflight, so an unset/unusable value must leave today's
// behavior exactly as it was rather than fail the store open.
func TestPreflightBDBinaryFallsBackToPath(t *testing.T) {
	dir := t.TempDir()
	cases := map[string]string{
		"unset":       "",
		"relative":    "bd",
		"nonexistent": filepath.Join(dir, "absent"),
		"a directory": dir,
		"whitespace":  "   ",
	}
	for name, val := range cases {
		t.Run(name, func(t *testing.T) {
			t.Setenv(realBdEnvVar, val)
			if got := preflightBDBinary(); got != "bd" {
				t.Fatalf("preflightBDBinary() = %q, want the PATH fallback %q", got, "bd")
			}
		})
	}
}

// TestPreflightBDBinaryRefusesTheShim pins that a GC_BD_REAL pointing at the
// shim is ignored. That value would reintroduce the hop this removes, and the
// shim reads the same variable for its own passthrough — so a shim spawned this
// way would resolve bd back through itself.
func TestPreflightBDBinaryRefusesTheShim(t *testing.T) {
	shim := filepath.Join(t.TempDir(), "bdshim")
	if err := os.WriteFile(shim, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write fake bdshim: %v", err)
	}
	t.Setenv(realBdEnvVar, shim)
	if got := preflightBDBinary(); got != "bd" {
		t.Fatalf("preflightBDBinary() = %q, want the PATH fallback when GC_BD_REAL names the shim", got)
	}
}
