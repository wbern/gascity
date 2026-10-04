package scripts_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// linkedBeadsVersion is the beads library version the temp go.mod pins.
const linkedBeadsVersion = "1.3.1"

// runCheckBeadsBDVersion runs scripts/check-beads-bd-version.sh against a temp
// go.mod pinning linkedBeadsVersion, with a fake bd reporting bdVersion and a
// stub go that fails so the script takes its offline go.mod parse path.
// Hermetic: no network, no real bd.
func runCheckBeadsBDVersion(t *testing.T, bdVersion string, extraEnv ...string) (string, error) {
	t.Helper()
	root := repoRoot(t)
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	if err := os.Mkdir(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	stubs := map[string]string{
		"bd": "#!/bin/sh\necho \"bd version " + bdVersion + " (fake)\"\n",
		"go": "#!/bin/sh\nexit 1\n",
	}
	for name, body := range stubs {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	gomod := "module example.com/x\n\ngo 1.26\n\nrequire (\n\tgithub.com/steveyegge/beads v" + linkedBeadsVersion + "\n)\n"
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(gomod), 0o644); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command("bash", filepath.Join(root, "scripts", "check-beads-bd-version.sh"), "go.mod")
	cmd.Dir = dir
	cmd.Env = append([]string{
		"PATH=" + bin + string(os.PathListSeparator) + "/usr/bin:/bin",
		"HOME=" + dir,
	}, extraEnv...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func TestCheckBeadsBDVersionPassesOnMatch(t *testing.T) {
	out, err := runCheckBeadsBDVersion(t, "1.3.1")
	if err != nil {
		t.Fatalf("matching versions must pass: %v\n%s", err, out)
	}
	if !strings.Contains(out, "OK") {
		t.Fatalf("expected OK line, got:\n%s", out)
	}
}

func TestCheckBeadsBDVersionBlocksUnacknowledgedSkew(t *testing.T) {
	out, err := runCheckBeadsBDVersion(t, "1.1.0")
	if err == nil {
		t.Fatalf("skew without ack must fail the build, got success:\n%s", out)
	}
	if !strings.Contains(out, "BEADS VERSION SKEW") {
		t.Fatalf("expected skew diagnostic, got:\n%s", out)
	}
	if !strings.Contains(out, "GC_BEADS_BD_SKEW_ACK=1.1.0") {
		t.Fatalf("diagnostic must name the exact ack for a deliberate staged rollout, got:\n%s", out)
	}
}

// TestCheckBeadsBDVersionAllowsAcknowledgedSkew pins the staged bd upgrade
// (gcw-o4qtt phase 1): a gc linked against a newer beads library deployed
// while the installed bd is still the older release. The ack names the
// installed bd version, so it is explicit and loudly reported.
func TestCheckBeadsBDVersionAllowsAcknowledgedSkew(t *testing.T) {
	out, err := runCheckBeadsBDVersion(t, "1.1.0", "GC_BEADS_BD_SKEW_ACK=1.1.0")
	if err != nil {
		t.Fatalf("acknowledged skew must pass: %v\n%s", err, out)
	}
	if !strings.Contains(out, "SKEW ACKNOWLEDGED") {
		t.Fatalf("acknowledged skew must be reported loudly, got:\n%s", out)
	}
}

// TestCheckBeadsBDVersionRejectsStaleAck ensures an ack cannot silently outlive
// the rollout it was given for: once bd changes, an ack naming a different bd
// version no longer suppresses the guard.
func TestCheckBeadsBDVersionRejectsStaleAck(t *testing.T) {
	for _, ack := range []string{"1.0.5", "1", "yes", "v1.1.0x"} {
		out, err := runCheckBeadsBDVersion(t, "1.1.0", "GC_BEADS_BD_SKEW_ACK="+ack)
		if err == nil {
			t.Fatalf("ack %q does not name installed bd 1.1.0 and must not suppress the guard:\n%s", ack, out)
		}
	}
}

func TestCheckBeadsBDVersionAcceptsVPrefixedAck(t *testing.T) {
	out, err := runCheckBeadsBDVersion(t, "1.1.0", "GC_BEADS_BD_SKEW_ACK=v1.1.0")
	if err != nil {
		t.Fatalf("v-prefixed ack naming installed bd must pass: %v\n%s", err, out)
	}
}

func TestCheckBeadsBDVersionIgnoresAckWhenVersionsMatch(t *testing.T) {
	out, err := runCheckBeadsBDVersion(t, "1.3.1", "GC_BEADS_BD_SKEW_ACK=1.1.0")
	if err != nil {
		t.Fatalf("matching versions must pass regardless of a leftover ack: %v\n%s", err, out)
	}
	if strings.Contains(out, "SKEW ACKNOWLEDGED") {
		t.Fatalf("no skew exists, so nothing is acknowledged:\n%s", out)
	}
}
