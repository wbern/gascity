package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/internal/orders"
)

// Order condition and body commands run `bd` under a PATH that may front the
// gc-as-bd shim bin dir. orderExecEnvWithError must inject GC_BD_REAL (resolved
// outside the shim dir) so a shimmed `bd` passes through instead of refusing —
// the durable fix for the order-exec failure class DevOps band-aided in the
// supervisor plist.
func TestOrderExecEnvInjectsGCBdReal(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("exec-bit semantics differ on windows")
	}
	cityDir := t.TempDir()
	shimDir := citylayout.ShimbinDir(cityDir)
	if err := os.MkdirAll(shimDir, 0o755); err != nil {
		t.Fatalf("mkdir shim dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(shimDir, "bd"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("write shim bd: %v", err)
	}
	realDir := t.TempDir()
	realBd := filepath.Join(realDir, "bd")
	if err := os.WriteFile(realBd, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("write real bd: %v", err)
	}
	t.Setenv("PATH", shimDir+string(os.PathListSeparator)+realDir)

	target := execStoreTarget{ScopeRoot: cityDir, ScopeKind: "city", Prefix: "ct"}
	a := orders.Order{Name: "gate-sweep", Trigger: "condition", Exec: "true"}
	envSlice, err := orderExecEnvWithError(cityDir, nil, target, a, nil)
	if err != nil {
		t.Fatalf("orderExecEnvWithError: %v", err)
	}
	got := map[string]string{}
	for _, entry := range envSlice {
		if key, value, ok := strings.Cut(entry, "="); ok {
			got[key] = value
		}
	}
	if got[citylayout.RealBdEnvVar] != realBd {
		t.Fatalf("env[%s] = %q, want the real bd %q (shim refuses without it)",
			citylayout.RealBdEnvVar, got[citylayout.RealBdEnvVar], realBd)
	}
}

// GC_BD_REAL is controller-owned: an order attempting to override it via
// [order.env] must be rejected (it is a reserved exec-env key), so a malicious
// or misconfigured order cannot redirect the shim's passthrough target.
func TestOrderExecEnvRejectsOrderOverridingGCBdReal(t *testing.T) {
	cityDir := t.TempDir()
	target := execStoreTarget{ScopeRoot: cityDir, ScopeKind: "city", Prefix: "ct"}
	a := orders.Order{
		Name: "custom", Trigger: "condition", Exec: "true",
		Env: map[string]string{citylayout.RealBdEnvVar: "/opt/custom/bd"},
	}
	if _, err := orderExecEnvWithError(cityDir, nil, target, a, nil); err == nil {
		t.Fatalf("expected [order.env] override of reserved %s to be rejected", citylayout.RealBdEnvVar)
	}
}
