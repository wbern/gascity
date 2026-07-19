package bddispatch

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

func bead(id, typ string, md map[string]string) beads.Bead {
	return beads.Bead{ID: id, Type: typ, Metadata: md}
}

// TestApplyListMetadataFilter covers equality, presence, exclude-type, and the
// post-filter limit — the correctness core of the routed metadata list.
func TestApplyListMetadataFilter(t *testing.T) {
	in := []beads.Bead{
		bead("a", "task", map[string]string{"pr_number": "5", "branch": "x"}),
		bead("b", "task", map[string]string{"pr_number": "9"}),
		bead("c", "epic", map[string]string{"pr_number": "5", "branch": "y"}),
		bead("d", "task", nil),
	}
	cases := []struct {
		name string
		f    listMetadataFilter
		want []string
	}{
		{"equals", listMetadataFilter{equals: map[string]string{"pr_number": "5"}}, []string{"a", "c"}},
		{"equals+exclude-type", listMetadataFilter{equals: map[string]string{"pr_number": "5"}, excludeTypes: map[string]bool{"epic": true}}, []string{"a"}},
		{"has-key presence", listMetadataFilter{hasKeys: []string{"branch"}}, []string{"a", "c"}},
		{"equals+has-key", listMetadataFilter{equals: map[string]string{"pr_number": "5"}, hasKeys: []string{"branch"}}, []string{"a", "c"}},
		{"no-match on missing metadata", listMetadataFilter{equals: map[string]string{"pr_number": "1"}}, []string{}},
		{"limit applied after filter", listMetadataFilter{equals: map[string]string{"pr_number": "5"}, limit: 1}, []string{"a"}},
		{"limit 0 = unlimited", listMetadataFilter{hasKeys: []string{"pr_number"}, limit: 0}, []string{"a", "b", "c"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.f.equals == nil {
				tc.f.equals = map[string]string{}
			}
			if tc.f.excludeTypes == nil {
				tc.f.excludeTypes = map[string]bool{}
			}
			got := applyListMetadataFilter(in, tc.f)
			var ids []string
			for _, b := range got {
				ids = append(ids, b.ID)
			}
			if len(ids) != len(tc.want) {
				t.Fatalf("ids=%v, want %v", ids, tc.want)
			}
			for i := range tc.want {
				if ids[i] != tc.want[i] {
					t.Fatalf("ids=%v, want %v", ids, tc.want)
				}
			}
		})
	}
}

// TestParseListMetadataFilter covers both space-separated and =-joined forms.
func TestParseListMetadataFilter(t *testing.T) {
	f, err := parseListMetadataFilter([]string{
		"--metadata-field", "pr_number=5",
		"--has-metadata-key=branch",
		"--exclude-type", "epic",
		"--limit", "3",
		"--json",
	})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if f.equals["pr_number"] != "5" {
		t.Errorf("equals=%v, want pr_number=5", f.equals)
	}
	if len(f.hasKeys) != 1 || f.hasKeys[0] != "branch" {
		t.Errorf("hasKeys=%v, want [branch]", f.hasKeys)
	}
	if !f.excludeTypes["epic"] {
		t.Errorf("excludeTypes=%v, want epic", f.excludeTypes)
	}
	if f.limit != 3 {
		t.Errorf("limit=%d, want 3", f.limit)
	}
}

// TestParseListMetadataFilterRejectsBadEquals pins the k=v guard.
func TestParseListMetadataFilterRejectsBadEquals(t *testing.T) {
	if _, err := parseListMetadataFilter([]string{"--metadata-field", "novalue"}); err == nil {
		t.Fatal("expected error for --metadata-field without =value")
	}
}
