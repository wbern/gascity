package config

import "testing"

func TestValidateOutputFirewallRejectsUnsafePolicy(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  OutputFirewallConfig
	}{
		{"small budget", OutputFirewallConfig{ByteBudget: 1}},
		{"mutation verb", OutputFirewallConfig{ReadVerbs: []string{"update"}}},
		{"absolute spill", OutputFirewallConfig{SpillPath: "/tmp/output"}},
		{"bad retention", OutputFirewallConfig{RetentionTTL: "0s"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := ValidateOutputFirewall(&City{OutputFirewall: tc.cfg}); err == nil {
				t.Fatal("ValidateOutputFirewall() succeeded")
			}
		})
	}
}

func TestValidateOutputFirewallAcceptsKnownReadPolicy(t *testing.T) {
	cfg := OutputFirewallConfig{ByteBudget: 32768, ReadVerbs: []string{"show", "ready", "list"}, SpillMode: "secure", SpillPath: ".gc/evidence/output", RetentionTTL: "24h"}
	if err := ValidateOutputFirewall(&City{OutputFirewall: cfg}); err != nil {
		t.Fatalf("ValidateOutputFirewall() = %v", err)
	}
}
