package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLogDispositionWritesRedactedCommandShape(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bdshim.jsonl")
	t.Setenv("GC_BDSHIM_LOG", path)

	logDisposition("list", []string{
		"--status=in_progress",
		"--assignee", "gas-city-wbern/architect",
		"--json",
		"--future-private-flag=do-not-log-me",
	}, "passthrough", 0, time.Now().Add(-time.Millisecond))

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read route log: %v", err)
	}
	for _, secret := range []string{"gas-city-wbern/architect", "do-not-log-me", "in_progress"} {
		if strings.Contains(string(data), secret) {
			t.Fatalf("route log leaked %q: %s", secret, data)
		}
	}
	var got routeLogLine
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal route log: %v", err)
	}
	if got.Verb != "list" || got.Disposition != "passthrough" || got.Exit != 0 {
		t.Fatalf("legacy fields = %+v, want list/passthrough/0", got)
	}
	const wantShape = "flags=--assignee,--json,--status,unknown"
	if got.Shape != wantShape {
		t.Fatalf("shape = %q, want %q", got.Shape, wantShape)
	}
}

func TestRouteLogLineReadsLegacyRecordsWithoutShape(t *testing.T) {
	var got routeLogLine
	if err := json.Unmarshal([]byte(`{"ts":"2026-07-21T00:00:00Z","verb":"list","disposition":"passthrough","exit":0,"dur_ms":12}`), &got); err != nil {
		t.Fatalf("unmarshal legacy record: %v", err)
	}
	if got.Shape != "" {
		t.Fatalf("legacy shape = %q, want empty", got.Shape)
	}
}

func TestLogDispositionRedactsUnknownVerb(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bdshim.jsonl")
	t.Setenv("GC_BDSHIM_LOG", path)

	logDisposition("gcw-private-id", []string{"/Users/willi/private/path"}, "passthrough", 1, time.Now())

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read route log: %v", err)
	}
	for _, secret := range []string{"gcw-private-id", "/Users/willi/private/path"} {
		if strings.Contains(string(data), secret) {
			t.Fatalf("route log leaked %q: %s", secret, data)
		}
	}
	var got routeLogLine
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal route log: %v", err)
	}
	if got.Verb != "unknown" || got.Shape != "flags=none" {
		t.Fatalf("redacted record = %+v, want unknown/flags=none", got)
	}
}

// The route log is written on every bd dispatch and had no size bound: measured
// 2026-08-08 on gc2 it had reached 73.5 MiB since 2026-07-19 and was still
// growing, with no rotated files and no rotation logic anywhere (gcw-yr0o.8).
func TestLogDispositionRotatesWhenOversized(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bdshim.log")
	t.Setenv("GC_BDSHIM_LOG", path)
	t.Setenv("GC_BDSHIM_LOG_MAX_BYTES", "200")

	// Fill past the cap so the next write must rotate.
	if err := os.WriteFile(path, make([]byte, 500), 0o644); err != nil {
		t.Fatalf("seeding log: %v", err)
	}
	logDisposition("list", []string{"--json"}, "passthrough", 0, time.Now())

	rotated := path + ".1"
	if _, err := os.Stat(rotated); err != nil {
		t.Fatalf("expected rotated file at %s: %v", rotated, err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading live log: %v", err)
	}
	// The live log must now hold ONLY the new record, not the 500 seeded bytes.
	if len(got) >= 500 {
		t.Fatalf("live log was not truncated on rotation: %d bytes", len(got))
	}
	if !strings.Contains(string(got), `"disposition":"passthrough"`) {
		t.Fatalf("new record missing after rotation: %q", string(got))
	}
	// Rotating must not lose the previous contents.
	old, err := os.ReadFile(rotated)
	if err != nil || len(old) != 500 {
		t.Fatalf("rotated file should hold the prior 500 bytes, got %d (%v)", len(old), err)
	}
}

// Control: under the cap, nothing rotates. Without this the test above could
// pass because rotation happens unconditionally.
func TestLogDispositionDoesNotRotateUnderCap(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bdshim.log")
	t.Setenv("GC_BDSHIM_LOG", path)
	t.Setenv("GC_BDSHIM_LOG_MAX_BYTES", "1000000")

	logDisposition("list", []string{"--json"}, "passthrough", 0, time.Now())
	logDisposition("show", []string{"x"}, "route", 0, time.Now())

	if _, err := os.Stat(path + ".1"); !os.IsNotExist(err) {
		t.Fatalf("must not rotate under the cap (err=%v)", err)
	}
}
