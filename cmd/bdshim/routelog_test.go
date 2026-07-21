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
