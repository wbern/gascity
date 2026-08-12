package testutil

import (
	"os"
	"testing"
)

func TestClearManagedOutputFirewallEnvClearsEveryFirewallSetting(t *testing.T) {
	keys := []string{
		"GC_MANAGED_OUTPUT_FIREWALL",
		"GC_MANAGED_OUTPUT_FIREWALL_BUDGET",
		"GC_MANAGED_OUTPUT_FIREWALL_READ_VERBS",
		"GC_MANAGED_OUTPUT_FIREWALL_SPILL_MODE",
		"GC_MANAGED_OUTPUT_FIREWALL_SPILL_ROOT",
		"GC_MANAGED_OUTPUT_FIREWALL_SPILL_PATH",
		"GC_MANAGED_OUTPUT_FIREWALL_RETENTION",
	}
	for _, key := range keys {
		t.Setenv(key, "inherited")
	}

	ClearManagedOutputFirewallEnv()

	for _, key := range keys {
		if value, ok := os.LookupEnv(key); ok {
			t.Errorf("%s survived scrub with value %q", key, value)
		}
	}
}
