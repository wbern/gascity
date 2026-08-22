package runtime

import (
	"strings"
	"testing"
)

func TestArgvSecretEnvValueDefaultsUnknownNonEmptyValuesToSecret(t *testing.T) {
	if !ArgvSecretEnvValue("OPENAI_API_KEY", "canary-not-a-credential") {
		t.Fatal("credential-shaped environment value must not be argv-safe")
	}
	if ArgvSecretEnvValue("GC_RIG", "rig-a") {
		t.Fatal("inert identity value must remain argv-safe")
	}
	if ArgvSecretEnvValue("OPENAI_API_KEY", "") {
		t.Fatal("empty values do not require secret staging")
	}
}

func TestArgvAllowListContainsNoCredentialShapedNames(t *testing.T) {
	for key := range envArgvSafe {
		upper := strings.ToUpper(key)
		for _, banned := range []string{"TOKEN", "SECRET", "PASSWORD", "PASSWD", "CREDENTIAL", "PRIVATE", "API_KEY", "APIKEY", "AUTH"} {
			if strings.Contains(upper, banned) {
				t.Errorf("%q is argv-safe but contains %q", key, banned)
			}
		}
	}
}

func TestSplitEnvByArgvSafetyPreservesEveryEntry(t *testing.T) {
	env := map[string]string{"LANG": "C", "GC_RIG": "rig", "LC_ALL": "", "OPENAI_API_KEY": "canary", "GC_INSTANCE_TOKEN": "capability"}
	safe, secret := SplitEnvByArgvSafety(env)
	if len(safe)+len(secret) != len(env) || safe["LANG"] != "C" || safe["LC_ALL"] != "" || secret["OPENAI_API_KEY"] != "canary" || secret["GC_INSTANCE_TOKEN"] != "capability" {
		t.Fatal("environment partition did not preserve the safe and secret halves")
	}
	if safe, secret := SplitEnvByArgvSafety(nil); safe == nil || secret == nil {
		t.Fatal("nil environment must yield two non-nil maps")
	}
}
