package doctor

import (
	"testing"

	"github.com/gastownhall/gascity/internal/config"
)

func TestSlingTargetsOverrideCheck(t *testing.T) {
	tests := []struct {
		name string
		rigs []config.Rig
		want CheckStatus
	}{
		{
			name: "both set warns",
			rigs: []config.Rig{{
				Name:                "demo",
				DefaultSlingTarget:  "demo/solo",
				DefaultSlingTargets: []string{"demo/a", "demo/b"},
			}},
			want: StatusWarning,
		},
		{
			name: "only plural is ok",
			rigs: []config.Rig{{Name: "demo", DefaultSlingTargets: []string{"demo/a"}}},
			want: StatusOK,
		},
		{
			name: "only singular is ok",
			rigs: []config.Rig{{Name: "demo", DefaultSlingTarget: "demo/solo"}},
			want: StatusOK,
		},
		{
			name: "neither set is ok",
			rigs: []config.Rig{{Name: "demo"}},
			want: StatusOK,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := NewSlingTargetsOverrideCheck(&config.City{Rigs: tt.rigs})
			got := c.Run(&CheckContext{})
			if got.Status != tt.want {
				t.Fatalf("Status = %v, want %v (msg=%q)", got.Status, tt.want, got.Message)
			}
			if tt.want == StatusWarning && got.Severity != SeverityAdvisory {
				t.Fatalf("Severity = %v, want SeverityAdvisory", got.Severity)
			}
		})
	}
}

func TestSlingTargetsOverrideCheckNilConfig(t *testing.T) {
	c := NewSlingTargetsOverrideCheck(nil)
	if got := c.Run(&CheckContext{}); got.Status != StatusOK {
		t.Fatalf("nil config Status = %v, want StatusOK", got.Status)
	}
}
