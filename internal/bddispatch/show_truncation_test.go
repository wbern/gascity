package bddispatch

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// The show projection may exhaust its budget. When it does it must say so:
// emitting an empty array instead makes a withheld bead indistinguishable from
// a bead that does not exist, which is the defect gcw-qap3.16 records.
func TestShowSummariesReportTruncationInsteadOfEmptiness(t *testing.T) {
	t.Setenv("GC_MANAGED_OUTPUT_FIREWALL", "1")
	t.Setenv("GC_MANAGED_OUTPUT_FIREWALL_BUDGET", "512")
	t.Setenv("GC_MANAGED_OUTPUT_FIREWALL_READ_VERBS", "show")
	t.Setenv("GC_MANAGED_OUTPUT_FIREWALL_SPILL_MODE", "disabled")

	oversized := make([]beads.Bead, 0, 8)
	for _, id := range []string{"gcw-1", "gcw-2", "gcw-3", "gcw-4", "gcw-5", "gcw-6", "gcw-7", "gcw-8"} {
		oversized = append(oversized, beads.Bead{
			ID:       id,
			Status:   "open",
			Assignee: strings.Repeat("worker", 20),
			Metadata: beads.StringMap{"gc.routed_to": strings.Repeat("rig/worker", 20)},
		})
	}

	var stdout, stderr bytes.Buffer
	if code := WriteManagedShowSummaries("managed_bd_read", oversized, &stdout, &stderr); code != 0 {
		t.Fatalf("WriteManagedShowSummaries() = %d; stderr=%q", code, stderr.String())
	}
	if !json.Valid(stdout.Bytes()) {
		t.Fatalf("stdout is not valid JSON: %q", stdout.String())
	}
	if strings.TrimSpace(stdout.String()) == "[]" {
		t.Fatal("a withheld show result was published as an empty array")
	}
	if err := beads.OutputFirewallTruncation(stdout.Bytes()); !errors.Is(err, beads.ErrOutputTruncated) {
		t.Fatalf("stdout=%q is not recognizable as truncation: %v", stdout.String(), err)
	}
}

// Under budget the projection is still the array machine consumers parse.
func TestShowSummariesKeepArrayContractWhenTheyFit(t *testing.T) {
	t.Setenv("GC_MANAGED_OUTPUT_FIREWALL", "1")
	t.Setenv("GC_MANAGED_OUTPUT_FIREWALL_BUDGET", "4096")
	t.Setenv("GC_MANAGED_OUTPUT_FIREWALL_READ_VERBS", "show")
	t.Setenv("GC_MANAGED_OUTPUT_FIREWALL_SPILL_MODE", "disabled")

	var stdout, stderr bytes.Buffer
	if code := WriteManagedShowSummaries("managed_bd_read", []beads.Bead{{ID: "gcw-1", Status: "open"}}, &stdout, &stderr); code != 0 {
		t.Fatalf("WriteManagedShowSummaries() = %d; stderr=%q", code, stderr.String())
	}
	var got []BeadShowSummary
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("stdout=%q: %v", stdout.String(), err)
	}
	if len(got) != 1 || got[0].ID != "gcw-1" {
		t.Fatalf("stdout=%q", stdout.String())
	}
}
