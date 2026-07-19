package config

import "testing"

// TestBdShimMode_DefaultsToAuto pins the default: an unset bd_shim resolves to
// "auto" (route through the bd-shim thin client when it is installed beside gc,
// else the real bd — the current behavior, upstream-safe no-op).
func TestBdShimMode_DefaultsToAuto(t *testing.T) {
	var s SessionConfig
	if got := s.BdShimMode(); got != BdShimModeAuto {
		t.Errorf("BdShimMode() = %q, want %q (unset default)", got, BdShimModeAuto)
	}
}

// TestBdShimMode_ConfiguredValues pins that on/off/auto round-trip through the
// accessor, and that an unrecognized value falls back defensively to auto
// (validation rejects it at load; the accessor must not panic or route on it).
func TestBdShimMode_ConfiguredValues(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"auto", BdShimModeAuto},
		{"on", BdShimModeOn},
		{"off", BdShimModeOff},
		{"  on  ", BdShimModeOn}, // trimmed
		{"", BdShimModeAuto},
		{"bogus", BdShimModeAuto}, // defensive fallback
	}
	for _, tc := range cases {
		s := SessionConfig{BdShim: tc.in}
		if got := s.BdShimMode(); got != tc.want {
			t.Errorf("BdShimMode(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestValidateBdShim_RejectsUnknown pins that config load rejects an
// unrecognized bd_shim value with a clear error listing the valid options,
// mirroring the default_sling_strategy enum guard.
func TestValidateBdShim_RejectsUnknown(t *testing.T) {
	cfg := &City{Session: SessionConfig{BdShim: "proxy-ish"}}
	err := ValidateBdShim(cfg)
	if err == nil {
		t.Fatal("ValidateBdShim accepted an invalid bd_shim value; want error")
	}
}

// TestValidateBdShim_AcceptsValid pins that the three valid values (and empty)
// pass validation.
func TestValidateBdShim_AcceptsValid(t *testing.T) {
	for _, v := range []string{"", "auto", "on", "off"} {
		cfg := &City{Session: SessionConfig{BdShim: v}}
		if err := ValidateBdShim(cfg); err != nil {
			t.Errorf("ValidateBdShim(%q) = %v, want nil", v, err)
		}
	}
}
