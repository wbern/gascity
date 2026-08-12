package testutil

import "os"

var managedOutputFirewallEnvKeys = []string{
	"GC_MANAGED_OUTPUT_FIREWALL",
	"GC_MANAGED_OUTPUT_FIREWALL_BUDGET",
	"GC_MANAGED_OUTPUT_FIREWALL_READ_VERBS",
	"GC_MANAGED_OUTPUT_FIREWALL_SPILL_MODE",
	"GC_MANAGED_OUTPUT_FIREWALL_SPILL_ROOT",
	"GC_MANAGED_OUTPUT_FIREWALL_SPILL_PATH",
	"GC_MANAGED_OUTPUT_FIREWALL_RETENTION",
}

// ClearManagedOutputFirewallEnv removes the managed output firewall settings
// inherited by a test process. Tests that require the firewall configure it
// explicitly with testing.T.Setenv.
func ClearManagedOutputFirewallEnv() {
	for _, key := range managedOutputFirewallEnvKeys {
		_ = os.Unsetenv(key)
	}
}
