package doctor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
)

// newTestBdShimCheck builds a check whose gcExe points at a temp dir, optionally
// with a fake executable bdshim installed beside it, and whose config carries the
// given bd_shim mode.
func newTestBdShimCheck(t *testing.T, mode string, bdshimPresent bool) *BdShimCheck {
	t.Helper()
	dir := t.TempDir()
	exe := filepath.Join(dir, "gc")
	if err := os.WriteFile(exe, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if bdshimPresent {
		if err := os.WriteFile(filepath.Join(dir, bdshimBinName), []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	cfg := &config.City{}
	cfg.Session.BdShim = mode
	return &BdShimCheck{cfg: cfg, gcExe: exe}
}

func TestBdShimCheck(t *testing.T) {
	cases := []struct {
		name          string
		mode          string
		bdshimPresent bool
		wantStatus    CheckStatus
		wantContains  string
	}{
		{"off ignores presence", config.BdShimModeOff, true, StatusOK, "disabled"},
		{"off no binary", config.BdShimModeOff, false, StatusOK, "disabled"},
		{"on missing warns", config.BdShimModeOn, false, StatusWarning, "falls back to real bd"},
		{"on present active", config.BdShimModeOn, true, StatusOK, "routing active (mode=on"},
		{"auto present active", config.BdShimModeAuto, true, StatusOK, "routing active (mode=auto"},
		{"auto missing no-op", config.BdShimModeAuto, false, StatusOK, "no-op"},
		{"empty defaults auto no-op", "", false, StatusOK, "no-op"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := newTestBdShimCheck(t, tc.mode, tc.bdshimPresent)
			r := c.Run(&CheckContext{})
			if r.Status != tc.wantStatus {
				t.Fatalf("status = %d, want %d; msg = %s", r.Status, tc.wantStatus, r.Message)
			}
			if !strings.Contains(r.Message, tc.wantContains) {
				t.Errorf("message = %q, want contains %q", r.Message, tc.wantContains)
			}
		})
	}
}

// TestBdShimCheck_NilConfigDefaultsAuto verifies a broken city.toml (nil cfg)
// resolves to auto and does not panic.
func TestBdShimCheck_NilConfigDefaultsAuto(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "gc")
	if err := os.WriteFile(exe, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	c := &BdShimCheck{cfg: nil, gcExe: exe}
	r := c.Run(&CheckContext{})
	if r.Status != StatusOK {
		t.Fatalf("status = %d, want OK; msg = %s", r.Status, r.Message)
	}
	if !strings.Contains(r.Message, "no-op") {
		t.Errorf("message = %q, want auto no-op", r.Message)
	}
}

// TestBdShimBesidePath_NonExecutableIgnored verifies a non-executable bdshim is
// not treated as an installed thin client.
func TestBdShimBesidePath_NonExecutableIgnored(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "gc")
	if err := os.WriteFile(exe, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	bp := filepath.Join(dir, bdshimBinName)
	if err := os.WriteFile(bp, []byte("#!/bin/sh\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if bdshimBesidePath(exe) {
		t.Errorf("non-executable bdshim should not count as present")
	}
}
