package main

import (
	"testing"

	"github.com/gastownhall/gascity/internal/api"
)

// TestClassifyBdListRouting pins the `bd list` routing gate: the cache-servable
// shapes route, everything else passes through byte-identically.
func TestClassifyBdListRouting(t *testing.T) {
	cases := []struct {
		args []string
		want bdShimDisposition
	}{
		// The GUPP-hook AssignedInProgressQuery — the dominant live shape.
		{[]string{"--status", "in_progress", "--assignee=gc2-x", "--json", "--limit", "50"}, bdRoute},
		{[]string{"--status", "in_progress", "--json"}, bdRoute},
		{[]string{"-s", "open", "-a", "y", "-n", "10", "--json"}, bdRoute},
		{[]string{"--all", "--json"}, bdRoute},
		// --json is REQUIRED: raw `bd list` defaults to a human tree, so a
		// non-json list must passthrough to preserve the output shape.
		{[]string{"--status", "in_progress"}, bdPassthrough},
		// Flags api.ListBeadsOpts cannot express passthrough (the refinery
		// --metadata-field/--exclude-type shape, --offset, --sort, --no-assignee).
		{[]string{"--metadata-field", "pr_number=5", "--exclude-type=epic", "--json"}, bdPassthrough},
		{[]string{"--json", "--offset", "10"}, bdPassthrough},
		{[]string{"--json", "--no-assignee"}, bdPassthrough},
	}
	for _, tc := range cases {
		if got := classifyBdShimVerb("list", tc.args, false); got != tc.want {
			t.Errorf("classifyBdShimVerb(list, %v) = %v, want %v", tc.args, got, tc.want)
		}
	}
}

// TestParseBdListOpts pins the arg->ListBeadsOpts mapping, including bd's
// default page size of 50 when --limit is absent.
func TestParseBdListOpts(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want api.ListBeadsOpts
	}{
		{
			name: "hook shape space-separated",
			args: []string{"--status", "in_progress", "--assignee=gc2-x", "--json", "--limit", "25"},
			want: api.ListBeadsOpts{Status: "in_progress", Assignee: "gc2-x", Limit: 25},
		},
		{
			name: "short flags",
			args: []string{"-s", "open", "-a", "y", "-t", "task", "-l", "pool:w", "-n", "10", "--json"},
			want: api.ListBeadsOpts{Status: "open", Assignee: "y", Type: "task", Label: "pool:w", Limit: 10},
		},
		{
			name: "default limit is 50",
			args: []string{"--status=open", "--json"},
			want: api.ListBeadsOpts{Status: "open", Limit: 50},
		},
		{
			name: "all",
			args: []string{"--all", "--json"},
			want: api.ListBeadsOpts{All: true, Limit: 50},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseBdListOpts(tc.args)
			if err != nil {
				t.Fatalf("parseBdListOpts: %v", err)
			}
			if got != tc.want {
				t.Fatalf("parseBdListOpts(%v) = %+v, want %+v", tc.args, got, tc.want)
			}
		})
	}
}

func TestParseBdListOptsBadLimit(t *testing.T) {
	if _, err := parseBdListOpts([]string{"--limit", "notanint", "--json"}); err == nil {
		t.Fatal("expected error for non-integer --limit")
	}
}
