package main

import (
	"bytes"
	"strings"
	"testing"
)

// TestGCBdRefusesMistypedMetadataPairs pins that `gc bd update` refuses a bare
// key=value in bd's positional issue-id slot.
//
// `--set-metadata` is a repeatable stringArray taking ONE pair per flag, so in
// `--set-metadata a=1 b=2 c=3` only `a=1` is the flag's value. bd reads `b=2` and
// `c=3` as issue ids, fails to resolve them, prints the failures to stderr — and
// still prints its success line and EXITS 0. Measured on bd 1.1.0 and unchanged
// in 1.1.2: six pairs in, one pair written, exit 0, so no caller can tell.
//
// gc bd writes exec raw bd rather than the routed fastpath, so the classifier's
// guard does not cover this path; it needs its own. The refusal must come before
// any store work, so nothing is written and the exit code is honest.
func TestGCBdRefusesMistypedMetadataPairs(t *testing.T) {
	cases := []struct {
		name string
		args []string
	}{
		{"trailing pairs", []string{"update", "bd-1", "--set-metadata", "a=1", "b=2", "c=3"}},
		{"one trailing pair", []string{"update", "bd-1", "--set-metadata", "a=1", "b=2"}},
		{"inline flag form", []string{"update", "bd-1", "--set-metadata=a=1", "b=2"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := doBd(tc.args, &stdout, &stderr)
			if code == 0 {
				t.Fatalf("doBd(%v) = 0, want non-zero; stderr=%q", tc.args, stderr.String())
			}
			msg := stderr.String()
			if !strings.Contains(msg, "--set-metadata") {
				t.Errorf("stderr %q does not name --set-metadata", msg)
			}
			// The message must name the offending token, so the caller can see
			// which pair bd would have dropped.
			if !strings.Contains(msg, "b=2") {
				t.Errorf("stderr %q does not name the dropped pair b=2", msg)
			}
		})
	}
}

// TestGCBdAllowsCorrectMetadataForms pins that the guard does not fire on the
// shapes that actually work: one --set-metadata per pair, a plain id, and
// several ids sharing one pair (bd applies the update to every id). These must
// get past the guard and continue into normal resolution — the guard is not
// allowed to become a reason a working invocation stops working.
func TestGCBdAllowsCorrectMetadataForms(t *testing.T) {
	cases := []struct {
		name string
		args []string
	}{
		{"repeated flag", []string{"update", "bd-1", "--set-metadata", "a=1", "--set-metadata", "b=2"}},
		{"single pair", []string{"update", "bd-1", "--set-metadata", "a=1"}},
		{"multi-id one pair", []string{"update", "bd-1", "bd-2", "--set-metadata", "a=1"}},
		{"flag value before id", []string{"update", "--set-metadata", "a=1", "bd-1"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			_ = doBd(tc.args, &stdout, &stderr)
			// The call fails later for want of a city/store in this environment;
			// what matters is that it was NOT stopped by the mistyped-pair guard.
			if strings.Contains(stderr.String(), "would be dropped") {
				t.Fatalf("guard fired on a valid form %v: %s", tc.args, stderr.String())
			}
		})
	}
}
