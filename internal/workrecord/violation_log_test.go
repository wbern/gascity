package workrecord

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
)

func shippedBead() beads.Bead {
	return beads.Bead{ID: "gci-1", Metadata: map[string]string{
		beadmeta.WorkOutcomeMetadataKey: beadmeta.WorkOutcomeShipped,
		beadmeta.WorkCommitMetadataKey:  "abc123",
		beadmeta.WorkBranchMetadataKey:  "feat/x",
	}}
}

func readLines(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	return strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
}

func TestRecordViolationsWritesOneWellFormedLine(t *testing.T) {
	city := t.TempDir()
	RecordViolations(city, DoorCLI, shippedBead(), false, []string{"commit not reachable"})
	lines := readLines(t, ViolationLogPath(city))
	if len(lines) != 1 {
		t.Fatalf("got %d lines, want 1", len(lines))
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &m); err != nil {
		t.Fatalf("not JSON: %v", err)
	}
	for k, want := range map[string]any{"door": "cli", "bead": "gci-1", "gc.work_outcome": "shipped",
		"commit": "abc123", "branch": "feat/x", "mode": "warn-only", "blocked": false} {
		if m[k] != want {
			t.Errorf("%s = %v, want %v", k, m[k], want)
		}
	}
	if r, _ := m["reasons"].([]any); len(r) != 1 || r[0] != "commit not reachable" {
		t.Errorf("reasons = %v", m["reasons"])
	}
	if ts, _ := m["ts"].(string); !strings.HasSuffix(ts, "Z") {
		t.Errorf("ts = %v", m["ts"])
	}
}

func TestRecordViolationsEnforceMarksBlocked(t *testing.T) {
	city := t.TempDir()
	RecordViolations(city, DoorHTTP, shippedBead(), true, []string{"x"})
	var m map[string]any
	_ = json.Unmarshal([]byte(readLines(t, ViolationLogPath(city))[0]), &m)
	if m["mode"] != "enforce" || m["blocked"] != true || m["door"] != "http" {
		t.Fatalf("got %v", m)
	}
}

func TestRecordViolationsNoViolationWritesNothing(t *testing.T) {
	city := t.TempDir()
	RecordViolations(city, DoorCLI, shippedBead(), true, nil)
	if _, err := os.Stat(ViolationLogPath(city)); !os.IsNotExist(err) {
		t.Fatalf("log exists: %v", err)
	}
}

func TestRecordViolationsUnwritablePathIsSilent(t *testing.T) {
	city := t.TempDir()
	// A file where the .gc directory should be makes every write fail.
	if err := os.WriteFile(filepath.Join(city, ".gc"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	RecordViolations(city, DoorCLI, shippedBead(), true, []string{"x"}) // must not panic
	RecordViolations("", DoorCLI, shippedBead(), true, []string{"x"})
}

func TestRecordViolationsBoundsReasonsAndRotates(t *testing.T) {
	city := t.TempDir()
	reasons := make([]string, 100)
	for i := range reasons {
		reasons[i] = strings.Repeat("r", 5000)
	}
	RecordViolations(city, DoorCLI, shippedBead(), false, reasons)
	var rec violationRecord
	_ = json.Unmarshal([]byte(readLines(t, ViolationLogPath(city))[0]), &rec)
	if len(rec.Reasons) != maxLoggedReasons || len(rec.Reasons[0]) != maxLoggedFieldBytes {
		t.Fatalf("not bounded: %d reasons, first %d bytes", len(rec.Reasons), len(rec.Reasons[0]))
	}

	path := ViolationLogPath(city)
	if err := os.WriteFile(path, make([]byte, ViolationLogMaxBytes), 0o644); err != nil {
		t.Fatal(err)
	}
	RecordViolations(city, DoorCLI, shippedBead(), false, []string{"after"})
	if fi, err := os.Stat(path + ".1"); err != nil || fi.Size() != ViolationLogMaxBytes {
		t.Fatalf("rotation missing: %v", err)
	}
	if n := len(readLines(t, path)); n != 1 {
		t.Fatalf("fresh log has %d lines, want 1", n)
	}
}

func TestRecordViolationsConcurrentWritersDoNotInterleave(t *testing.T) {
	city := t.TempDir()
	var wg sync.WaitGroup
	for i := 0; i < 40; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			RecordViolations(city, DoorCLI, shippedBead(), false, []string{"r"})
		}()
	}
	wg.Wait()
	lines := readLines(t, ViolationLogPath(city))
	if len(lines) != 40 {
		t.Fatalf("got %d lines, want 40", len(lines))
	}
	for _, l := range lines {
		var m map[string]any
		if err := json.Unmarshal([]byte(l), &m); err != nil {
			t.Fatalf("interleaved line %q", l)
		}
	}
}
