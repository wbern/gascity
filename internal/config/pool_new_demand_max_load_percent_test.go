package config

import "testing"

func TestPoolNewDemandMaxLoadPercent(t *testing.T) {
	cases := []struct {
		name  string
		value *int
		want  int
	}{
		{"unset defaults to 0 (disabled)", nil, DefaultPoolNewDemandMaxLoadPercent},
		{"zero stays zero", intPtr(0), 0},
		{"positive value passes through", intPtr(75), 75},
		{"negative value clamps to zero", intPtr(-5), 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := &DaemonConfig{PoolNewDemandMaxLoadPercentValue: tc.value}
			if got := d.PoolNewDemandMaxLoadPercent(); got != tc.want {
				t.Errorf("PoolNewDemandMaxLoadPercent() = %d, want %d", got, tc.want)
			}
		})
	}
}
