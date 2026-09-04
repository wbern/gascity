package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/orders"
)

// `gc order list` filters disabled orders out of its result and says nothing
// about having done so. A disabled order is then indistinguishable from an
// order that does not exist -- on the table surface AND, more damagingly, on
// the --json surface, where every emitted row carries enabled=true because the
// only rows that survive the filter are the enabled ones. A machine reader sees
// a field that looks like a discriminator but can never be false.
//
// Measured on the live gc2 city 2026-09-04: `gc order list` printed 73 rows
// against 89 order files on disk, and `gc order list --json` returned
// enabled=true for 73 of 73 rows. gc2-fork-pr-review appeared 0 times in the
// list while `gc order show gc2-fork-pr-review` returned rc=0 with its full
// manifest -- it exists, it loads, and it is invisible.
//
// That gap produced a wrong decision brief (gci-1ygxc) which recorded
// gas-city-wbern as having NO PR-review lane when the lane exists and is
// configured, and proposed building one that was already built.
//
// These tests pin the contract that a list surface must disclose its own
// filter: the counts are always stated, --all reaches the hidden rows, and the
// empty case never reports absence when the rows were merely filtered.

// TestOrderListStatesItsFilterCounts locks that the default table always says
// how many orders it filtered out, so "not listed" can never be read as
// "not present".
func TestOrderListStatesItsFilterCounts(t *testing.T) {
	var stdout bytes.Buffer
	code := doOrderList(orderListFixture(), false, &stdout)
	if code != 0 {
		t.Fatalf("doOrderList = %d, want 0", code)
	}

	out := stdout.String()
	if !strings.Contains(out, "2 enabled") || !strings.Contains(out, "1 disabled") {
		t.Fatalf("stdout does not state its filter counts; a reader cannot tell listed-from-present:\n%s", out)
	}
	// The affordance must be named, or the counts are a dead end.
	if !strings.Contains(out, "--all") {
		t.Fatalf("stdout reports hidden orders but does not name the flag that shows them:\n%s", out)
	}
}

// TestOrderListDefaultStillOmitsDisabledRows pins backward compatibility: the
// disclosure is additive, and the default row set is unchanged.
func TestOrderListDefaultStillOmitsDisabledRows(t *testing.T) {
	var stdout bytes.Buffer
	if code := doOrderList(orderListFixture(), false, &stdout); code != 0 {
		t.Fatalf("doOrderList = %d, want 0", code)
	}

	out := stdout.String()
	if !strings.Contains(out, "digest") || !strings.Contains(out, "cleanup") {
		t.Fatalf("default listing dropped an enabled order:\n%s", out)
	}
	// "dark-lane" may appear only in the footer's counts, never as a row.
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "dark-lane") {
			t.Fatalf("default listing emitted a disabled order as a row:\n%s", out)
		}
	}
}

// TestOrderListAllRevealsDisabledOrders locks the affordance: --all reaches the
// hidden rows and marks which ones they are, so the reader is not left to guess
// which of the returned rows was the disabled one.
func TestOrderListAllRevealsDisabledOrders(t *testing.T) {
	var stdout bytes.Buffer
	if code := doOrderList(orderListFixture(), true, &stdout); code != 0 {
		t.Fatalf("doOrderList = %d, want 0", code)
	}

	out := stdout.String()
	var darkLine string
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "dark-lane") {
			darkLine = line
		}
	}
	if darkLine == "" {
		t.Fatalf("--all did not emit the disabled order as a row:\n%s", out)
	}
	if !strings.Contains(out, "ENABLED") {
		t.Fatalf("--all lists disabled rows without an ENABLED column, so they are unmarked:\n%s", out)
	}
	if !strings.Contains(darkLine, "no") {
		t.Fatalf("disabled row is not marked disabled: %q\nfull output:\n%s", darkLine, out)
	}
}

// TestOrderListEmptyDistinguishesFilteredFromAbsent is the highest-value edge:
// a city whose only orders are disabled must not report "No orders found."
// That exact sentence is what turns a filtered view into a claim of absence.
func TestOrderListEmptyDistinguishesFilteredFromAbsent(t *testing.T) {
	onlyDisabled := []orders.Order{
		{Name: "dark-lane", Trigger: "cooldown", Interval: "3m", Exec: "scripts/dark.sh", Enabled: boolPtr(false)},
		{Name: "dark-two", Trigger: "cooldown", Interval: "9m", Exec: "scripts/dark2.sh", Enabled: boolPtr(false)},
	}

	var stdout bytes.Buffer
	if code := doOrderList(onlyDisabled, false, &stdout); code != 0 {
		t.Fatalf("doOrderList = %d, want 0", code)
	}

	out := stdout.String()
	if strings.Contains(out, "No orders found") {
		t.Fatalf("2 disabled orders reported as 'No orders found' -- a filtered view claiming absence:\n%s", out)
	}
	if !strings.Contains(out, "2 disabled") {
		t.Fatalf("empty-after-filter output does not say what was filtered:\n%s", out)
	}
}

// TestOrderListEmptyWhenGenuinelyAbsent keeps the honest case honest: with no
// orders at all, absence is the correct report.
func TestOrderListEmptyWhenGenuinelyAbsent(t *testing.T) {
	var stdout bytes.Buffer
	if code := doOrderList(nil, false, &stdout); code != 0 {
		t.Fatalf("doOrderList = %d, want 0", code)
	}
	if !strings.Contains(stdout.String(), "No orders found") {
		t.Fatalf("genuinely empty city should report absence plainly:\n%s", stdout.String())
	}
}

// TestOrderListJSONSummaryEchoesItsFilter is the agent-facing half. A machine
// reader must be able to tell "these are all the orders" from "these are the
// orders that survived a filter" without knowing which flags were passed.
func TestOrderListJSONSummaryEchoesItsFilter(t *testing.T) {
	var stdout bytes.Buffer
	code := doOrderListJSON("/city", &config.City{Workspace: config.Workspace{Name: "bright-lights"}}, orderListFixture(), false, &stdout)
	if code != 0 {
		t.Fatalf("doOrderListJSON = %d, want 0", code)
	}

	got := decodeOrderListSummary(t, stdout.Bytes())
	if got.Summary.Count != 2 {
		t.Fatalf("summary.count = %d, want 2 rows emitted", got.Summary.Count)
	}
	if got.Summary.Enabled != 2 || got.Summary.Disabled != 1 || got.Summary.Total != 3 {
		t.Fatalf("summary = %+v, want enabled=2 disabled=1 total=3", got.Summary)
	}
	if got.Summary.Filter != "enabled" {
		t.Fatalf("summary.filter = %q, want %q -- the payload must name the filter that produced it", got.Summary.Filter, "enabled")
	}
}

// TestOrderListJSONEnabledFieldCanBeFalse is the direct regression guard for
// the measured defect: pre-filtering made the per-row `enabled` field
// constant-true across all 73 live rows, so it read as a checked discriminator
// while being incapable of reporting the thing it names.
func TestOrderListJSONEnabledFieldCanBeFalse(t *testing.T) {
	var stdout bytes.Buffer
	code := doOrderListJSON("/city", &config.City{Workspace: config.Workspace{Name: "bright-lights"}}, orderListFixture(), true, &stdout)
	if code != 0 {
		t.Fatalf("doOrderListJSON = %d, want 0", code)
	}

	got := decodeOrderListSummary(t, stdout.Bytes())
	if got.Summary.Filter != "all" {
		t.Fatalf("summary.filter = %q, want %q under --all", got.Summary.Filter, "all")
	}
	if got.Summary.Count != 3 {
		t.Fatalf("summary.count = %d, want all 3 rows under --all", got.Summary.Count)
	}

	var sawDisabled bool
	for _, row := range got.Orders {
		if row.Name == "dark-lane" {
			sawDisabled = true
			if row.Enabled {
				t.Fatal("dark-lane is disabled but serialized enabled=true; the field cannot report what it names")
			}
		}
	}
	if !sawDisabled {
		t.Fatalf("--all JSON omitted the disabled order entirely: %+v", got.Orders)
	}
}

// orderListFixture is two enabled orders and one explicitly disabled one.
func orderListFixture() []orders.Order {
	return []orders.Order{
		{Name: "digest", Trigger: "cooldown", Interval: "24h", Pool: "dog", Formula: "mol-digest"},
		{Name: "cleanup", Trigger: "cron", Schedule: "0 3 * * *", Formula: "mol-cleanup"},
		{Name: "dark-lane", Trigger: "cooldown", Interval: "3m", Exec: "scripts/dark.sh", Enabled: boolPtr(false)},
	}
}

type orderListSummaryProbe struct {
	Orders []struct {
		Name    string `json:"name"`
		Enabled bool   `json:"enabled"`
	} `json:"orders"`
	Summary struct {
		Count    int    `json:"count"`
		Enabled  int    `json:"enabled"`
		Disabled int    `json:"disabled"`
		Total    int    `json:"total"`
		Filter   string `json:"filter"`
	} `json:"summary"`
}

func decodeOrderListSummary(t *testing.T, payload []byte) orderListSummaryProbe {
	t.Helper()

	var got orderListSummaryProbe
	if err := json.Unmarshal(payload, &got); err != nil {
		t.Fatalf("order list JSON invalid: %v\n%s", err, string(payload))
	}
	return got
}
