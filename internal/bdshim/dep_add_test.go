package bdshim

import "testing"

func TestParseDepAddArgs(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		want    DepAdd
		matches bool
	}{
		{
			name:    "positional pair",
			args:    []string{"dep", "add", "crm-1", "gci-2"},
			want:    DepAdd{FromID: "crm-1", ToID: "gci-2"},
			matches: true,
		},
		{
			name:    "blocked-by flag",
			args:    []string{"dep", "add", "crm-1", "--blocked-by", "gci-2"},
			want:    DepAdd{FromID: "crm-1", ToID: "gci-2"},
			matches: true,
		},
		{
			name:    "depends-on inline value",
			args:    []string{"dep", "add", "crm-1", "--depends-on=gci-2"},
			want:    DepAdd{FromID: "crm-1", ToID: "gci-2"},
			matches: true,
		},
		{
			name:    "type flag value is not read as an ID",
			args:    []string{"dep", "add", "crm-1", "gci-2", "--type", "blocks"},
			want:    DepAdd{FromID: "crm-1", ToID: "gci-2"},
			matches: true,
		},
		{
			// --file may be "-" (stdin), which cannot be read here without
			// consuming what the real bd needs. Non-match means "cannot
			// verify", and the caller documents the gap.
			name:    "bulk file form is not parsed",
			args:    []string{"dep", "add", "--file", "edges.ndjson"},
			matches: false,
		},
		{
			// An unrecognized bare flag might consume the next token, which
			// would make the positional scan a guess.
			name:    "unknown bare flag refuses to guess",
			args:    []string{"dep", "add", "crm-1", "--future-flag", "gci-2"},
			matches: false,
		},
		{name: "not dep", args: []string{"update", "crm-1"}, matches: false},
		{name: "dep list is not dep add", args: []string{"dep", "list", "crm-1"}, matches: false},
		{name: "single id", args: []string{"dep", "add", "crm-1"}, matches: false},
		{name: "three ids", args: []string{"dep", "add", "a-1", "b-2", "c-3"}, matches: false},
		{name: "empty", args: nil, matches: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, matches := ParseDepAddArgs(tt.args)
			if matches != tt.matches {
				t.Fatalf("matches = %v, want %v (got %+v)", matches, tt.matches, got)
			}
			if matches && got != tt.want {
				t.Fatalf("got %+v, want %+v", got, tt.want)
			}
		})
	}
}
