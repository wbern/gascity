package bddispatch

import (
	"testing"

	"github.com/gastownhall/gascity/internal/api"
)

// TestParseListOpts pins the arg->ListBeadsOpts mapping, including bd's
// default page size of 50 when --limit is absent.
func TestParseListOpts(t *testing.T) {
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
			got, err := ParseListOpts(tc.args)
			if err != nil {
				t.Fatalf("ParseListOpts: %v", err)
			}
			if got != tc.want {
				t.Fatalf("ParseListOpts(%v) = %+v, want %+v", tc.args, got, tc.want)
			}
		})
	}
}

func TestParseListOptsBadLimit(t *testing.T) {
	if _, err := ParseListOpts([]string{"--limit", "notanint", "--json"}); err == nil {
		t.Fatal("expected error for non-integer --limit")
	}
}
