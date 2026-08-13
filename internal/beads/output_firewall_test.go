package beads

import (
	"errors"
	"strings"
	"testing"
)

const testFirewallManifest = `{"schema_version":"1","kind":"gc.output_firewall","reason":"byte_budget_exceeded",` +
	`"command_class":"managed_bd_passthrough","budget_bytes":32768,"serialized_bytes":90520,` +
	`"sha256":"abc123","spill":{"mode":"secure","path":"/city/.gc/spill/output-deadbeef","expires_at":"2026-08-14T10:00:00Z"}}`

func TestOutputFirewallTruncationDetectsManifest(t *testing.T) {
	err := OutputFirewallTruncation([]byte(testFirewallManifest + "\n"))
	if err == nil {
		t.Fatal("OutputFirewallTruncation() = nil, want a truncation error")
	}
	if !errors.Is(err, ErrOutputTruncated) {
		t.Fatalf("OutputFirewallTruncation() = %v, want ErrOutputTruncated", err)
	}
	if errors.Is(err, ErrNotFound) {
		t.Fatalf("a truncated read must never be reported as absence: %v", err)
	}
	for _, want := range []string{"byte_budget_exceeded", "32768", "90520", "/city/.gc/spill/output-deadbeef"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q is missing evidence %q", err, want)
		}
	}
}

func TestOutputFirewallTruncationDetectsMinimalManifest(t *testing.T) {
	err := OutputFirewallTruncation([]byte(`{"kind":"gc.output_firewall","reason":"budget_too_small"}` + "\n"))
	if !errors.Is(err, ErrOutputTruncated) {
		t.Fatalf("OutputFirewallTruncation() = %v, want ErrOutputTruncated", err)
	}
	if !strings.Contains(err.Error(), "budget_too_small") {
		t.Errorf("error %q is missing its reason", err)
	}
}

func TestOutputFirewallTruncationIgnoresRealPayloads(t *testing.T) {
	for name, payload := range map[string]string{
		"empty":         "",
		"issue array":   `[{"id":"gcw-1","status":"open"}]`,
		"issue object":  `{"id":"gcw-1","status":"open","description":"mentions gc.output_firewall in prose"}`,
		"summary array": `[{"id":"gcw-1","status":"open","source_serialized_bytes":90520,"details_omitted":["notes"]}]`,
		"not JSON":      "no issue found matching \"gcw-1\"",
	} {
		if err := OutputFirewallTruncation([]byte(payload)); err != nil {
			t.Errorf("%s: OutputFirewallTruncation() = %v, want nil", name, err)
		}
	}
}

func TestGetReportsTruncationRatherThanAbsence(t *testing.T) {
	runner := func(_, _ string, _ ...string) ([]byte, error) {
		return []byte(testFirewallManifest + "\n"), nil
	}
	store := NewBdStore(t.TempDir(), runner)

	_, err := store.Get("gcw-qap3")
	if !errors.Is(err, ErrOutputTruncated) {
		t.Fatalf("Get() = %v, want ErrOutputTruncated", err)
	}
	if errors.Is(err, ErrNotFound) {
		t.Fatalf("a firewalled read must not be indistinguishable from a missing bead: %v", err)
	}
	if !strings.Contains(err.Error(), "gcw-qap3") {
		t.Errorf("error %q does not name the bead that was read", err)
	}
}

func TestListReportsTruncationRatherThanEmptiness(t *testing.T) {
	runner := func(_, _ string, _ ...string) ([]byte, error) {
		return []byte(testFirewallManifest + "\n"), nil
	}
	store := NewBdStore(t.TempDir(), runner)

	got, err := store.List(ListQuery{Status: "open"})
	if !errors.Is(err, ErrOutputTruncated) {
		t.Fatalf("List() = (%d beads, %v), want ErrOutputTruncated", len(got), err)
	}
	if len(got) != 0 {
		t.Fatalf("List() returned %d beads from a truncated read", len(got))
	}
}

func TestUnboundedReadStoreNeverSeesManifest(t *testing.T) {
	var got []string
	runner := func(_, _ string, args ...string) ([]byte, error) {
		got = args
		return []byte(`[{"id":"gcw-qap3","status":"in_progress"}]`), nil
	}
	store := NewBdStore(t.TempDir(), runner, WithBdStoreAllowUnboundedReads())

	if _, err := store.Get("gcw-qap3"); err != nil {
		t.Fatalf("Get(): %v", err)
	}
	if !containsArg(got, "--allow-unbounded") {
		t.Fatalf("read args %v are missing --allow-unbounded", got)
	}
}

func containsArg(args []string, want string) bool {
	for _, arg := range args {
		if arg == want {
			return true
		}
	}
	return false
}
