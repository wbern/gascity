package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/testutil"
)

func TestMain(m *testing.M) {
	testutil.ClearManagedOutputFirewallEnv()
	os.Exit(m.Run())
}

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

// fakeBdOutput writes an executable stand-in for bd that emits output verbatim.
// It lets compatibility tests prove that the shim preserves the real CLI stream
// rather than re-encoding a lossy controller projection.
func fakeBdOutput(t *testing.T, dir, output string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake bd shell script is POSIX-only")
	}
	fixture := filepath.Join(dir, "bd-output.json")
	if err := os.WriteFile(fixture, []byte(output), 0o600); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "bd.real")
	script := "#!/bin/sh\ncat \"" + fixture + "\"\n"
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

// TestRunPassthroughStripsShimPrivateFlags verifies that a fallback to the
// standalone bd binary never forwards flags implemented only by this shim.
func TestRunPassthroughStripsShimPrivateFlags(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "calls.txt")
	t.Setenv("GC_BD_REAL", fakeBd(t, dir, out, 0))
	t.Setenv("GC_BDSHIM_LOG", "")

	var stdout, stderr bytes.Buffer
	if code := run([]string{"list", "--summary-json", "--limit", "1"}, strings.NewReader(""), &stdout, &stderr); code != 0 {
		t.Fatalf("run() = %d, want passthrough success; stderr=%q", code, stderr.String())
	}
	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("read real bd argv: %v", err)
	}
	if strings.Contains(string(got), "--summary-json") {
		t.Fatalf("real bd argv leaked shim-private flag: %q", got)
	}
	if !strings.Contains(string(got), "list --limit 1") {
		t.Fatalf("real bd argv = %q, want remaining args", got)
	}
}

func TestRunManagedPassthroughShowPreservesJSONArrayWhenOverBudget(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("GC_BD_REAL", fakeBdOutput(t, dir, `[{"id":"gcw-1","status":"open","description":"`+strings.Repeat("secret", 200)+`"}]`))
	t.Setenv("GC_MANAGED_OUTPUT_FIREWALL", "1")
	t.Setenv("GC_MANAGED_OUTPUT_FIREWALL_BUDGET", "1024")
	t.Setenv("GC_MANAGED_OUTPUT_FIREWALL_READ_VERBS", "show,list")
	t.Setenv("GC_MANAGED_OUTPUT_FIREWALL_SPILL_MODE", "disabled")
	t.Setenv("GC_BD_HQ_GUARD", "")
	t.Setenv("GC_BDSHIM_LOG", "")

	var stdout, stderr bytes.Buffer
	if code := run([]string{"show", "gcw-1", "--json"}, strings.NewReader(""), &stdout, &stderr); code != 0 {
		t.Fatalf("run() = %d, stderr=%q", code, stderr.String())
	}
	var got []json.RawMessage
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("show output changed top-level JSON type: %v; output=%q", err, stdout.String())
	}
	if len(got) != 1 {
		t.Fatalf("show result length=%d, want 1", len(got))
	}
	if strings.Contains(stdout.String(), "secret") {
		t.Fatalf("bounded show leaked description: %q", stdout.String())
	}
}

func TestRunShowAllowUnboundedPreservesFullPayloadAndStripsFlag(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake bd shell script is POSIX-only")
	}
	dir := t.TempDir()
	fixture := filepath.Join(dir, "output.json")
	calls := filepath.Join(dir, "calls.txt")
	full := `[{"id":"gcw-1","description":"` + strings.Repeat("secret", 200) + `"}]`
	if err := os.WriteFile(fixture, []byte(full), 0o600); err != nil {
		t.Fatal(err)
	}
	bd := filepath.Join(dir, "bd.real")
	if err := os.WriteFile(bd, []byte("#!/bin/sh\necho \"$@\" > \""+calls+"\"\ncat \""+fixture+"\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GC_BD_REAL", bd)
	t.Setenv("GC_MANAGED_OUTPUT_FIREWALL", "1")
	t.Setenv("GC_MANAGED_OUTPUT_FIREWALL_BUDGET", "1024")
	t.Setenv("GC_MANAGED_OUTPUT_FIREWALL_READ_VERBS", "show")
	t.Setenv("GC_BD_HQ_GUARD", "")

	var stdout, stderr bytes.Buffer
	if code := run([]string{"show", "gcw-1", "--json", "--allow-unbounded"}, strings.NewReader(""), &stdout, &stderr); code != 0 {
		t.Fatalf("run()=%d stderr=%q", code, stderr.String())
	}
	if stdout.String() != full {
		t.Fatalf("stdout=%q, want full payload", stdout.String())
	}
	gotArgs, err := os.ReadFile(calls)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(gotArgs), "--allow-unbounded") {
		t.Fatalf("raw bd received gc-only flag: %q", gotArgs)
	}
}

func TestRunManagedPassthroughReadAdmitsPlainAndInferredMoleculeReads(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
	}{
		{name: "plain show", args: []string{"show", "gcw-1"}},
		{name: "inferred molecule current", args: []string{"mol", "current"}},
		{name: "flag before molecule current", args: []string{"mol", "--json", "current"}},
		{name: "directory value flag before molecule current", args: []string{"mol", "-C", ".", "current", "--json"}},
		{name: "actor value flag before molecule current", args: []string{"mol", "--actor", "reviewer", "current", "--json"}},
		{name: "dolt value flag before molecule current", args: []string{"mol", "--dolt-auto-commit", "off", "current", "--json"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv("GC_BD_REAL", fakeBdOutput(t, dir, strings.Repeat("secret", 600)))
			t.Setenv("GC_MANAGED_OUTPUT_FIREWALL", "1")
			t.Setenv("GC_MANAGED_OUTPUT_FIREWALL_BUDGET", "512")
			t.Setenv("GC_MANAGED_OUTPUT_FIREWALL_READ_VERBS", "show,mol")
			t.Setenv("GC_MANAGED_OUTPUT_FIREWALL_SPILL_MODE", "disabled")
			t.Setenv("GC_BD_HQ_GUARD", "")
			t.Setenv("GC_BDSHIM_LOG", "")

			var stdout, stderr bytes.Buffer
			if code := run(tc.args, strings.NewReader(""), &stdout, &stderr); code != 0 {
				t.Fatalf("run() = %d, stderr=%q", code, stderr.String())
			}
			if stdout.Len() > 512 || !json.Valid(stdout.Bytes()) || strings.Contains(stdout.String(), "secret") {
				t.Fatalf("managed read escaped firewall: %d bytes %q", stdout.Len(), stdout.String())
			}
		})
	}
}

func TestManagedPassthroughReadVerbExcludesMoleculeMutations(t *testing.T) {
	for _, args := range [][]string{{"close", "current"}, {"--json", "close", "current"}} {
		if managedPassthroughReadVerb("mol", args) {
			t.Fatalf("mol %v classified as a read", args)
		}
	}
}

func TestRunManagedPassthroughPlainReadPreservesUnderBudgetBytes(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("GC_BD_REAL", fakeBdOutput(t, dir, "human output\n"))
	t.Setenv("GC_MANAGED_OUTPUT_FIREWALL", "1")
	t.Setenv("GC_MANAGED_OUTPUT_FIREWALL_BUDGET", "512")
	t.Setenv("GC_MANAGED_OUTPUT_FIREWALL_READ_VERBS", "show")
	t.Setenv("GC_MANAGED_OUTPUT_FIREWALL_SPILL_MODE", "disabled")
	t.Setenv("GC_BD_HQ_GUARD", "")
	t.Setenv("GC_BDSHIM_LOG", "")

	var stdout, stderr bytes.Buffer
	if code := run([]string{"show", "gcw-1"}, strings.NewReader(""), &stdout, &stderr); code != 0 {
		t.Fatalf("run() = %d, stderr=%q", code, stderr.String())
	}
	if got, want := stdout.String(), "human output\n"; got != want {
		t.Fatalf("stdout=%q, want byte-exact %q", got, want)
	}
}

func TestRunHQGuardRefusesBareBdBeforeRoutingOrPassthrough(t *testing.T) {
	dir := t.TempDir()
	city := filepath.Join(dir, "city")
	rig := filepath.Join(city, "rigs", "demo")
	if err := os.MkdirAll(filepath.Join(city, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "calls.txt")
	bd := fakeBd(t, dir, out, 0)
	t.Setenv("GC_BD_REAL", bd)
	t.Setenv("GC_BDSHIM_LOG", "")
	t.Setenv("GC_BD_HQ_GUARD", "1")
	t.Setenv("GC_BD_HQ_ACCESS", "")
	t.Setenv("GC_BD_HQ_GUARD_CITY", city)
	t.Setenv("GC_CITY_PATH", city)
	t.Setenv("GC_ALIAS", "demo/worker-2")
	t.Setenv("GC_AGENT", "demo/worker")
	t.Setenv("GC_RIG", "demo")
	t.Setenv("GC_RIG_ROOT", rig)
	t.Setenv("BEADS_DIR", filepath.Join(city, ".beads"))

	var stdout, stderr bytes.Buffer
	code := run([]string{"list", "--json"}, strings.NewReader(""), &stdout, &stderr)
	if code == 0 {
		t.Fatalf("run(bare bd list) = 0, want managed HQ refusal")
	}
	for _, want := range []string{
		`managed agent "demo/worker-2"`,
		`denied HQ store "` + city + `"`,
		`agent rig "demo"`,
		"`gc bd list --rig demo ...`",
		`rig store "` + rig + `"`,
	} {
		if !strings.Contains(stderr.String(), want) {
			t.Errorf("stderr = %q, want %q", stderr.String(), want)
		}
	}
	if got, err := os.ReadFile(out); err == nil && strings.TrimSpace(string(got)) != "" {
		t.Fatalf("HQ-refused command reached real bd: %q", got)
	} else if err != nil && !os.IsNotExist(err) {
		t.Fatalf("read fake bd calls: %v", err)
	}
}

func TestBareBdHQGuardScopeAndAuthorization(t *testing.T) {
	dir := t.TempDir()
	city := filepath.Join(dir, "city")
	rig := filepath.Join(city, "rigs", "demo")
	otherRig := filepath.Join(city, "rigs", "other")
	nestedHQ := filepath.Join(city, "scratch", "nested")
	for _, path := range []string{
		filepath.Join(city, ".beads"),
		filepath.Join(rig, ".beads"),
		filepath.Join(otherRig, ".beads"),
		nestedHQ,
	} {
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("GC_BD_HQ_GUARD", "1")
	t.Setenv("GC_BD_HQ_ACCESS", "")
	t.Setenv("GC_BD_HQ_GUARD_CITY", city)
	t.Setenv("GC_ALIAS", "demo/worker-1")
	t.Setenv("GC_RIG", "demo")
	t.Setenv("GC_RIG_ROOT", rig)

	tests := []struct {
		name     string
		args     []string
		beadsDir string
		access   string
		refuse   bool
	}{
		{name: "own rig store", args: []string{"list"}, beadsDir: filepath.Join(rig, ".beads")},
		{name: "foreign rig directory", args: []string{"list", "-C", otherRig}, beadsDir: filepath.Join(rig, ".beads")},
		{name: "nested HQ directory", args: []string{"list", "-C", nestedHQ}, beadsDir: filepath.Join(rig, ".beads"), refuse: true},
		{name: "missing path still discovers HQ ancestor", args: []string{"list", "-C", filepath.Join(city, "missing", "child")}, beadsDir: filepath.Join(rig, ".beads"), refuse: true},
		{name: "directory overrides rig env with HQ", args: []string{"list", "-C", city}, beadsDir: filepath.Join(rig, ".beads"), refuse: true},
		{name: "explicit HQ store", args: []string{"list"}, beadsDir: filepath.Join(city, ".beads"), refuse: true},
		{name: "authorized HQ store", args: []string{"list"}, beadsDir: filepath.Join(city, ".beads"), access: "1"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("BEADS_DIR", tc.beadsDir)
			t.Setenv("GC_BD_HQ_ACCESS", tc.access)
			msg, refuse := bareBDHQGuardRefusal(tc.args, "list")
			if refuse != tc.refuse {
				t.Fatalf("bareBDHQGuardRefusal(%v) = (%q, %v), want refuse=%v", tc.args, msg, refuse, tc.refuse)
			}
		})
	}
}

// TestRunRefusesUnsupportedBodyMutations prevents an unsupported body or notes
// write from falling through to real bd. On a fastpath city that command may
// write a different Dolt working set while bdshim reports success, so it must
// fail loudly until the controller API has a faithful translation.
func TestRunRefusesUnsupportedBodyMutations(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "calls.txt")
	bd := fakeBd(t, dir, out, 0)
	t.Setenv("GC_BD_REAL", bd)
	t.Setenv("GC_CITY_PATH", "/tmp/gc2")
	t.Setenv("GC_BDSHIM_LOG", "")

	for _, args := range [][]string{
		{"update", "gcw-1", "--body-file", "body.md"},
		{"update", "gcw-1", "--stdin"},
		{"update", "gcw-1", "--append-notes", "progress"},
		{"update", "gcw-1", "--notes", "progress"},
		{"update", "gcw-1", "--allow-empty-description", "-d", "replacement body"},
	} {
		var stdout, stderr bytes.Buffer
		if code := run(args, strings.NewReader("replacement body"), &stdout, &stderr); code == 0 {
			t.Fatalf("run(%v) = 0; want non-zero refusal", args)
		}
		if !strings.Contains(stderr.String(), "refusing") {
			t.Fatalf("run(%v) stderr = %q; want refusal diagnostic", args, stderr.String())
		}
	}
	if got, err := os.ReadFile(out); err == nil && strings.TrimSpace(string(got)) != "" {
		t.Fatalf("unsupported body mutation reached real bd: %q", got)
	} else if err != nil && !os.IsNotExist(err) {
		t.Fatalf("read fake bd calls: %v", err)
	}
}

func TestRunLoggingCannotChangePassthroughBehavior(t *testing.T) {
	dir := t.TempDir()
	noLogCalls := filepath.Join(dir, "without-log.txt")
	withLogCalls := filepath.Join(dir, "with-log.txt")
	noLogBD := fakeBd(t, dir, noLogCalls, 7)

	t.Setenv("GC_BD_REAL", noLogBD)
	t.Setenv("GC_BDSHIM_LOG", "")
	var noLogStdout, noLogStderr bytes.Buffer
	noLogCode := run([]string{"log", "--future-private-option", "secret-value"}, strings.NewReader(""), &noLogStdout, &noLogStderr)

	withLogBD := fakeBd(t, dir, withLogCalls, 7)
	t.Setenv("GC_BD_REAL", withLogBD)
	t.Setenv("GC_BDSHIM_LOG", filepath.Join(dir, "bdshim.jsonl"))
	var withLogStdout, withLogStderr bytes.Buffer
	withLogCode := run([]string{"log", "--future-private-option", "secret-value"}, strings.NewReader(""), &withLogStdout, &withLogStderr)

	if noLogCode != withLogCode || noLogStdout.String() != withLogStdout.String() || noLogStderr.String() != withLogStderr.String() {
		t.Fatalf("logging changed passthrough result: without=(%d,%q,%q), with=(%d,%q,%q)",
			noLogCode, noLogStdout.String(), noLogStderr.String(), withLogCode, withLogStdout.String(), withLogStderr.String())
	}
	noLogData, err := os.ReadFile(noLogCalls)
	if err != nil {
		t.Fatalf("read no-log real-bd call: %v", err)
	}
	withLogData, err := os.ReadFile(withLogCalls)
	if err != nil {
		t.Fatalf("read with-log real-bd call: %v", err)
	}
	if string(noLogData) != string(withLogData) {
		t.Fatalf("logging changed real-bd args: without=%q, with=%q", noLogData, withLogData)
	}
}

func TestRunLogsGlobalFlagsWithoutScopeValues(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "bdshim.jsonl")
	bd := fakeBd(t, dir, filepath.Join(dir, "calls.txt"), 0)
	t.Setenv("GC_BD_REAL", bd)
	t.Setenv("GC_BDSHIM_LOG", logPath)

	// --rig is refused (the shim cannot answer for another rig's store), so this
	// exercises the REFUSAL path. That is the stricter test for this file's
	// subject: scope values must stay out of the route log even when the
	// invocation is rejected, and the refusal message naming the rig goes to
	// stderr, never to the log.
	var stdout, stderr bytes.Buffer
	if code := run([]string{
		"--city", "/Users/willi/private-city",
		"--rig=private-rig",
		"--readonly", "log", "--future-private-option=secret-value",
	}, strings.NewReader(""), &stdout, &stderr); code != 1 {
		t.Fatalf("exit code = %d, want 1 (--rig must be refused)", code)
	}
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read route log: %v", err)
	}
	for _, secret := range []string{"/Users/willi/private-city", "private-rig", "secret-value"} {
		if strings.Contains(string(data), secret) {
			t.Fatalf("route log leaked %q: %s", secret, data)
		}
	}
	var got routeLogLine
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal route log: %v", err)
	}
	if got.Verb != "log" || got.Shape != "flags=--readonly,unknown" {
		t.Fatalf("record = %+v, want log/flags=--readonly,unknown", got)
	}
}

// TestRunRoutedReadFailsLoudWhenControllerDown verifies a routable ready read
// dispatches (and fails loudly rc!=0) rather than silently passing through to
// the work-only bd when the controller is down — bd.real's cwd scope cannot
// answer a city-wide read, so a silent passthrough would return wrong/empty
// output.
func TestRunRoutedReadFailsLoudWhenControllerDown(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "calls.txt")
	bd := fakeBd(t, dir, out, 0)
	t.Setenv("GC_BD_REAL", bd)
	t.Setenv("GC_API_URL", "http://127.0.0.1:1") // nothing listens on port 1
	t.Setenv("GC_CITY_PATH", "/tmp/gc2")
	t.Setenv("GC_BDSHIM_LOG", "")

	var stdout, stderr bytes.Buffer
	code := run([]string{"ready", "--json"}, strings.NewReader(""), &stdout, &stderr)
	if code == 0 {
		t.Fatalf("routed read with controller down: exit code = 0; want non-zero (loud fail)")
	}
	if got, _ := os.ReadFile(out); strings.Contains(string(got), "ready") {
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

// TestRunCompatibilityReadsDelegateToRealBd proves the current compatibility
// boundary for show/list: the shim streams the real bd output unchanged even
// when a city and controller URL are available. This protects fields that bd
// computes in its IssueDetails and IssueWithCounts projections but that the
// controller's Bead model does not carry yet.
func TestRunCompatibilityReadsDelegateToRealBd(t *testing.T) {
	dir := t.TempDir()
	output := "[{\"id\":\"gcw-1\",\"comment_count\":2,\"dependency_count\":1,\"dependent_count\":3,\"created_by\":\"agent\"}]\n"
	bd := fakeBdOutput(t, dir, output)
	t.Setenv("GC_BD_REAL", bd)
	t.Setenv("GC_API_URL", "http://127.0.0.1:1")
	t.Setenv("GC_CITY_PATH", "/tmp/gc2")
	t.Setenv("GC_BDSHIM_LOG", "")
	// A bounded managed session must not alter an already-small show response:
	// bd owns additional IssueDetails fields that the controller model does not.
	t.Setenv("GC_MANAGED_OUTPUT_FIREWALL", "1")
	t.Setenv("GC_MANAGED_OUTPUT_FIREWALL_READ_VERBS", "show")
	t.Setenv("GC_MANAGED_OUTPUT_FIREWALL_BUDGET", "32768")

	for _, args := range [][]string{
		{"show", "gcw-1", "--json"},
		{"list", "--status", "in_progress", "--json"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if code := run(args, strings.NewReader(""), &stdout, &stderr); code != 0 {
				t.Fatalf("run(%v) exit code = %d, want 0; stderr=%s", args, code, stderr.String())
			}
			if got := stdout.String(); got != output {
				t.Fatalf("run(%v) stdout = %q, want exact real-bd output %q", args, got, output)
			}
		})
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

func TestRunMalformedHeartbeatLogsRedactedRefusal(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bdshim.jsonl")
	t.Setenv("GC_BDSHIM_LOG", path)

	var stdout, stderr bytes.Buffer
	if code := run([]string{"heartbeat", "secret id with spaces"}, strings.NewReader(""), &stdout, &stderr); code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read route log: %v", err)
	}
	if strings.Contains(string(data), "secret id with spaces") {
		t.Fatalf("route log leaked malformed positional: %s", data)
	}
	var got routeLogLine
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal route log: %v", err)
	}
	if got.Verb != "heartbeat" || got.Disposition != "refuse" || got.Exit != 1 || got.Shape != "flags=none" {
		t.Fatalf("refusal record = %+v, want heartbeat/refuse/1/flags=none", got)
	}
}

func TestRunClaimWithoutIDLogsRedactedRefusal(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bdshim.jsonl")
	t.Setenv("GC_BDSHIM_LOG", path)
	t.Setenv("GC_CITY_PATH", "/tmp/gc2")
	t.Setenv("BEADS_ACTOR", "gas-city-wbern/architect")

	var stdout, stderr bytes.Buffer
	if code := run([]string{"update", "--claim"}, strings.NewReader(""), &stdout, &stderr); code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read route log: %v", err)
	}
	var got routeLogLine
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal route log: %v", err)
	}
	if got.Verb != "update" || got.Disposition != "refuse" || got.Exit != 1 || got.Shape != "flags=--claim" {
		t.Fatalf("refusal record = %+v, want update/refuse/1/flags=--claim", got)
	}
}

// gc resolves a --rig scope itself and hands the child a BEADS_DIR pinned to
// THAT rig's store, plus GC_STORE_SCOPE=rig / GC_STORE_ROOT to say so. The shim
// cannot represent a rig in a controller request — it is city-scoped by
// construction, which is exactly why bare `bd --rig` is refused outright. So a
// routed verb under a pinned rig scope answered from the CITY, silently
// discarding the scope gc had resolved.
//
// Measured live on gc2 before this guard:
//
//	gc bd --rig statusline ready --json  ->  {gc2: 16, crm: 4}, zero statusline
//	gc bd --rig gas-city-wbern create    ->  bead landed in the HQ store
//
// Passthrough is the correct answer here, and it is available precisely because
// gc already set BEADS_DIR: real bd honors it and hits the right store.
func TestRunPinnedRigScopePassesThroughInsteadOfRouting(t *testing.T) {
	for _, verb := range []struct {
		name string
		args []string
		want string
	}{
		{name: "routed read", args: []string{"ready", "--json"}, want: "ready --json"},
		{name: "routed write", args: []string{"create", "a title", "--json"}, want: "create"},
		{name: "routed update", args: []string{"update", "gcw-1", "--status", "open"}, want: "update gcw-1"},
	} {
		t.Run(verb.name, func(t *testing.T) {
			dir := t.TempDir()
			out := filepath.Join(dir, "calls.txt")
			bd := fakeBd(t, dir, out, 0)
			t.Setenv("GC_BD_REAL", bd)
			// A city and controller ARE resolvable — routing would otherwise happen.
			t.Setenv("GC_CITY_PATH", "/tmp/gc2")
			t.Setenv("GC_API_URL", "http://127.0.0.1:1")
			t.Setenv("GC_BDSHIM_LOG", "")
			// gc pinned a rig store for this invocation.
			t.Setenv("GC_STORE_SCOPE", "rig")
			t.Setenv("GC_STORE_ROOT", "/some/rig")

			var stdout, stderr bytes.Buffer
			if code := run(verb.args, strings.NewReader(""), &stdout, &stderr); code != 0 {
				t.Fatalf("exit = %d, want 0 (passthrough); stderr=%q", code, stderr.String())
			}
			got, _ := os.ReadFile(out)
			if !strings.Contains(string(got), verb.want) {
				t.Fatalf("a pinned rig scope must pass through to real bd; calls=%q", string(got))
			}
		})
	}
}

// The city scope is what the shim CAN represent, so it must keep routing.
func TestRunCityStoreScopeStillRoutes(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "calls.txt")
	bd := fakeBd(t, dir, out, 0)
	t.Setenv("GC_BD_REAL", bd)
	t.Setenv("GC_CITY_PATH", "/tmp/gc2")
	t.Setenv("GC_API_URL", "http://127.0.0.1:1") // controller down -> routed reads fail loud
	t.Setenv("GC_BDSHIM_LOG", "")
	t.Setenv("GC_STORE_SCOPE", "city")
	t.Setenv("GC_STORE_ROOT", "/tmp/gc2")

	var stdout, stderr bytes.Buffer
	if code := run([]string{"ready", "--json"}, strings.NewReader(""), &stdout, &stderr); code == 0 {
		t.Fatalf("city scope must still ROUTE (and fail loud with the controller down); got 0, calls=%q", func() string { b, _ := os.ReadFile(out); return string(b) }())
	}
}
