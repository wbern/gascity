package config

import "testing"

func TestCanonicalSingletonSessionAlias(t *testing.T) {
	one, four := 1, 4
	tests := []struct {
		name  string
		cfg   *City
		agent *Agent
		want  string
	}{
		{
			name:  "city-scoped singleton uses its qualified name",
			agent: &Agent{Name: "marketer", MaxActiveSessions: &one},
			want:  "marketer",
		},
		{
			name:  "rig-scoped singleton uses its rig-qualified name",
			agent: &Agent{Name: "witness", Dir: "demo", MaxActiveSessions: &one},
			want:  "demo/witness",
		},
		{
			name:  "multi-session agent has no canonical alias",
			agent: &Agent{Name: "ant", Dir: "demo", MaxActiveSessions: &four},
			want:  "",
		},
		{
			name:  "unbounded agent has no canonical alias",
			agent: &Agent{Name: "ant"},
			want:  "",
		},
		{
			name:  "namepool singleton draws names from its pool",
			agent: &Agent{Name: "ant", MaxActiveSessions: &one, NamepoolNames: []string{"nux"}},
			want:  "",
		},
		{
			name: "configured named session owns the identity",
			cfg: &City{NamedSessions: []NamedSession{{
				Template: "worker",
				Dir:      "myrig",
			}}},
			agent: &Agent{Name: "worker", Dir: "myrig", MaxActiveSessions: &one},
			want:  "",
		},
		{
			name: "named session for another identity does not claim the template",
			cfg: &City{NamedSessions: []NamedSession{{
				Name:     "boot",
				Template: "witness",
				Dir:      "demo",
			}}},
			agent: &Agent{Name: "witness", Dir: "demo", MaxActiveSessions: &one},
			want:  "demo/witness",
		},
		{
			name:  "nil agent",
			agent: nil,
			want:  "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := CanonicalSingletonSessionAlias(tt.cfg, tt.agent); got != tt.want {
				t.Fatalf("CanonicalSingletonSessionAlias() = %q, want %q", got, tt.want)
			}
		})
	}
}
