package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRunRefusesMistypedMetadataPairs pins the EXIT CODE, not the message.
//
// The whole reason this defect survived is that everything downstream trusted an
// exit code that lied: bd applies the first --set-metadata pair, reports the
// trailing ones as unresolvable issue ids on stderr, prints its success line and
// exits 0. A caller cannot distinguish a full write from a 1-of-N write, so every
// `|| exit` guard in the fleet is blind to it.
//
// The shim must refuse before it dispatches anything, so no partial write
// happens at all: an unreachable controller address here would surface as a
// dispatch error rather than a refusal if the guard ever stopped firing first.
func TestRunRefusesMistypedMetadataPairs(t *testing.T) {
	cases := []struct {
		name string
		args []string
	}{
		{"the reported defect", []string{"update", "gcw-1", "--set-metadata", "a=1", "b=2", "c=3"}},
		{"one trailing pair", []string{"update", "gcw-1", "--set-metadata", "a=1", "b=2"}},
		{"inline flag form", []string{"update", "gcw-1", "--set-metadata=a=1", "b=2"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "bdshim.jsonl")
			t.Setenv("GC_BDSHIM_LOG", path)
			t.Setenv("GC_CITY_PATH", "/tmp/gc2")
			// Point the passthrough escape at a binary that would fail loudly, so a
			// guard that stopped firing could not masquerade as a clean refusal.
			t.Setenv("GC_BD_REAL", filepath.Join(t.TempDir(), "no-such-bd"))

			var stdout, stderr bytes.Buffer
			if code := run(tc.args, strings.NewReader(""), &stdout, &stderr); code != 1 {
				t.Fatalf("exit code = %d, want 1 (stdout=%q stderr=%q)", code, stdout.String(), stderr.String())
			}
			if stdout.Len() != 0 {
				t.Errorf("stdout = %q, want empty: a refusal must not print a success line", stdout.String())
			}
			if msg := stderr.String(); !strings.Contains(msg, "b=2") || !strings.Contains(msg, "--set-metadata") {
				t.Errorf("stderr = %q, want it to name the dropped pair and the flag", msg)
			}

			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read route log: %v", err)
			}
			var got routeLogLine
			if err := json.Unmarshal(data, &got); err != nil {
				t.Fatalf("unmarshal route log: %v", err)
			}
			if got.Verb != "update" || got.Disposition != "refuse" || got.Exit != 1 {
				t.Fatalf("refusal record = %+v, want update/refuse/1", got)
			}
		})
	}
}

// TestRunKeepsCorrectMetadataFormsWorking pins that the guard rejects only what
// bd was already failing to resolve. One --set-metadata per pair is the correct
// form and must not be refused; neither may a multi-id update, which real bd
// applies to every id.
func TestRunKeepsCorrectMetadataFormsWorking(t *testing.T) {
	cases := []struct {
		name string
		args []string
	}{
		{"repeated flag", []string{"update", "gcw-1", "--set-metadata", "a=1", "--set-metadata", "b=2"}},
		{"multi-id one pair", []string{"update", "gcw-1", "gcw-2", "--set-metadata", "a=1"}},
		{"flag value before id", []string{"update", "--set-metadata", "a=1", "gcw-1"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("GC_BDSHIM_LOG", filepath.Join(t.TempDir(), "bdshim.jsonl"))
			t.Setenv("GC_CITY_PATH", "/tmp/gc2")
			t.Setenv("GC_BD_REAL", filepath.Join(t.TempDir(), "no-such-bd"))

			var stdout, stderr bytes.Buffer
			_ = run(tc.args, strings.NewReader(""), &stdout, &stderr)
			if msg := stderr.String(); strings.Contains(msg, "would be dropped") {
				t.Fatalf("guard fired on a valid form %v: %s", tc.args, msg)
			}
		})
	}
}
