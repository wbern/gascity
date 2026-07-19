package main

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
)

func TestExtractScopeFlags(t *testing.T) {
	tests := []struct {
		name     string
		args     []string
		wantCity string
		wantRig  string
		wantRest []string
	}{
		{"no scope flags", []string{"ready", "--json"}, "", "", []string{"ready", "--json"}},
		{"--city space", []string{"--city", "gc2", "list"}, "gc2", "", []string{"list"}},
		{"--city equals", []string{"--city=gc2", "list"}, "gc2", "", []string{"list"}},
		{"--rig space", []string{"--rig", "architect", "ready"}, "", "architect", []string{"ready"}},
		{"--rig equals", []string{"--rig=architect", "ready"}, "", "architect", []string{"ready"}},
		{"both interleaved", []string{"--city", "gc2", "show", "--rig=a", "x"}, "gc2", "a", []string{"show", "x"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			city, rig, rest := extractScopeFlags(tc.args)
			if city != tc.wantCity || rig != tc.wantRig || !reflect.DeepEqual(rest, tc.wantRest) {
				t.Fatalf("extractScopeFlags(%v) = (%q,%q,%v); want (%q,%q,%v)",
					tc.args, city, rig, rest, tc.wantCity, tc.wantRig, tc.wantRest)
			}
		})
	}
}

func TestRewriteHeartbeatArgs(t *testing.T) {
	// Non-heartbeat args pass through unchanged.
	in := []string{"ready", "--json"}
	got, err := rewriteHeartbeatArgs(in)
	if err != nil || !reflect.DeepEqual(got, in) {
		t.Fatalf("passthrough: got %v, %v; want %v, nil", got, err, in)
	}
	// A heartbeat rewrites to an update --set-metadata write.
	got, err = rewriteHeartbeatArgs([]string{"heartbeat", "gcw-1"})
	if err != nil {
		t.Fatalf("heartbeat: unexpected err %v", err)
	}
	if len(got) != 4 || got[0] != "update" || got[1] != "gcw-1" || got[2] != "--set-metadata" ||
		!strings.HasPrefix(got[3], heartbeatMetadataKey+"=") {
		t.Fatalf("heartbeat rewrite = %v; want update gcw-1 --set-metadata %s=<ts>", got, heartbeatMetadataKey)
	}
	// Malformed heartbeat ids are rejected.
	for _, bad := range [][]string{{"heartbeat"}, {"heartbeat", ""}, {"heartbeat", "-x"}, {"heartbeat", "a b"}, {"heartbeat", "a", "b"}} {
		if _, err := rewriteHeartbeatArgs(bad); err == nil {
			t.Fatalf("rewriteHeartbeatArgs(%v): want error, got nil", bad)
		}
	}
}

// TestHeartbeatKeyInSync guards the hardcoded key against drift from the canonical
// beadmeta constant (kept out of the shipped binary to avoid the go/ast dep).
func TestHeartbeatKeyInSync(t *testing.T) {
	if heartbeatMetadataKey != beadmeta.LastHeartbeatAtMetadataKey {
		t.Fatalf("heartbeatMetadataKey=%q out of sync with beadmeta.LastHeartbeatAtMetadataKey=%q",
			heartbeatMetadataKey, beadmeta.LastHeartbeatAtMetadataKey)
	}
}

func TestResolveCityName(t *testing.T) {
	t.Setenv("GC_CITY_PATH", "")
	t.Setenv("GC_CITY", "")
	if got := resolveCityName("override-city"); got != "override-city" {
		t.Fatalf("override: got %q", got)
	}
	t.Setenv("GC_CITY_PATH", "/Users/x/gc2/")
	if got := resolveCityName(""); got != "gc2" {
		t.Fatalf("GC_CITY_PATH basename: got %q, want gc2", got)
	}
	t.Setenv("GC_CITY_PATH", "")
	t.Setenv("GC_CITY", "/srv/mycity")
	if got := resolveCityName(""); got != "mycity" {
		t.Fatalf("GC_CITY basename: got %q, want mycity", got)
	}
	t.Setenv("GC_CITY", "")
	if got := resolveCityName(""); got != "" {
		t.Fatalf("nothing resolvable: got %q, want empty", got)
	}
}

func TestControllerBaseURL(t *testing.T) {
	t.Setenv("GC_API_URL", "http://127.0.0.1:9999/")
	if got := controllerBaseURL(); got != "http://127.0.0.1:9999" {
		t.Fatalf("GC_API_URL override: got %q", got)
	}
	t.Setenv("GC_API_URL", "")
	t.Setenv("GC_HOME", t.TempDir()) // no supervisor.toml there
	if got := controllerBaseURL(); got != defaultControllerBaseURL {
		t.Fatalf("default: got %q, want %q", got, defaultControllerBaseURL)
	}
}

// fakeBd writes an executable script to dir that appends its args to outFile and
// exits with code. Returns its path. Skips on Windows (shell script).
func fakeBd(t *testing.T, dir, outFile string, code int) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake bd shell script is POSIX-only")
	}
	p := filepath.Join(dir, "bd.real")
	script := "#!/bin/sh\necho \"$@\" >> " + outFile + "\nexit " + itoa(code) + "\n"
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	if neg {
		b = append([]byte{'-'}, b...)
	}
	return string(b)
}

// TestRunPassthroughExecsRealBd verifies a passthrough verb execs GC_BD_REAL with
// the original args and propagates its exit code.
func TestRunPassthroughExecsRealBd(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "calls.txt")
	bd := fakeBd(t, dir, out, 7)
	t.Setenv("GC_BD_REAL", bd)
	t.Setenv("GC_BDSHIM_LOG", "")

	var stdout, stderr bytes.Buffer
	// "log" is not in RoutedVerbs -> passthrough.
	code := run([]string{"log", "--follow"}, strings.NewReader(""), &stdout, &stderr)
	if code != 7 {
		t.Fatalf("exit code = %d; want 7 (propagated from fake bd)", code)
	}
	got, _ := os.ReadFile(out)
	if !strings.Contains(string(got), "log --follow") {
		t.Fatalf("fake bd not called with args; calls=%q", string(got))
	}
}

// TestRunRoutedReadFailsLoudWhenControllerDown verifies a routable READ dispatches
// (and fails loudly rc!=0) rather than silently passing through to the work-only
// bd when the controller is down — bd.real's cwd scope cannot answer a city-wide
// read, so a silent passthrough would return wrong/empty output.
func TestRunRoutedReadFailsLoudWhenControllerDown(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "calls.txt")
	bd := fakeBd(t, dir, out, 0)
	t.Setenv("GC_BD_REAL", bd)
	t.Setenv("GC_API_URL", "http://127.0.0.1:1") // nothing listens on port 1
	t.Setenv("GC_CITY_PATH", "/tmp/gc2")
	t.Setenv("GC_BDSHIM_LOG", "")

	var stdout, stderr bytes.Buffer
	code := run([]string{"list", "--json"}, strings.NewReader(""), &stdout, &stderr)
	if code == 0 {
		t.Fatalf("routed read with controller down: exit code = 0; want non-zero (loud fail)")
	}
	if got, _ := os.ReadFile(out); strings.Contains(string(got), "list") {
		t.Fatalf("routed read must NOT silently passthrough to bd; calls=%q", string(got))
	}
}

// TestRunRoutedCityUnresolvablePassesThrough verifies that a routable verb passes
// through when no city is resolvable (non-agent context), since routing requires a
// city and passthrough is byte-identical in the identity phase.
func TestRunRoutedCityUnresolvablePassesThrough(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "calls.txt")
	bd := fakeBd(t, dir, out, 0)
	t.Setenv("GC_BD_REAL", bd)
	t.Setenv("GC_CITY_PATH", "")
	t.Setenv("GC_CITY", "")
	t.Setenv("GC_BDSHIM_LOG", "")

	var stdout, stderr bytes.Buffer
	code := run([]string{"list", "--json"}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d; want 0 (passthrough to fake bd)", code)
	}
	if got, _ := os.ReadFile(out); !strings.Contains(string(got), "list --json") {
		t.Fatalf("expected passthrough to fake bd; calls=%q", string(got))
	}
}

// TestRunClaimFallsBackWhenControllerDown verifies the claim shape falls back to
// the real bd's atomic claim when the controller is unreachable.
func TestRunClaimFallsBackWhenControllerDown(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "calls.txt")
	bd := fakeBd(t, dir, out, 0)
	t.Setenv("GC_BD_REAL", bd)
	t.Setenv("GC_API_URL", "http://127.0.0.1:1") // nothing listens on port 1
	t.Setenv("GC_CITY_PATH", "/tmp/gc2")
	t.Setenv("BEADS_ACTOR", "gas-city-wbern/architect")
	t.Setenv("GC_BDSHIM_LOG", "")

	var stdout, stderr bytes.Buffer
	code := run([]string{"update", "gcw-1", "--claim"}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d; want 0 (passthrough claim to fake bd)", code)
	}
	if got, _ := os.ReadFile(out); !strings.Contains(string(got), "update gcw-1 --claim") {
		t.Fatalf("expected claim passthrough to fake bd; calls=%q", string(got))
	}
}
