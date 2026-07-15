package main

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestResolveRealBdPathHonorsGCBDReal proves the shim resolves the real bd
// binary from GC_BD_REAL by absolute path and does NOT depend on a PATH lookup
// of "bd" — the recursion trap that would resolve back to the shim once it is
// installed as `bd` first on an agent's PATH (graph-store-rollout-plan.md §C2).
func TestResolveRealBdPathHonorsGCBDReal(t *testing.T) {
	dir := t.TempDir()
	realBd := filepath.Join(dir, "bd-real")
	if err := os.WriteFile(realBd, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GC_BD_REAL", realBd)
	// Empty PATH: any accidental exec.LookPath("bd") would fail, so a successful
	// resolve proves GC_BD_REAL is honored without consulting PATH.
	t.Setenv("PATH", "")

	got, err := resolveRealBdPath()
	if err != nil {
		t.Fatalf("resolveRealBdPath: %v", err)
	}
	if got != realBd {
		t.Fatalf("resolveRealBdPath = %q, want %q", got, realBd)
	}
}

// TestResolveRealBdPathRejectsRelativeGCBDReal guards the install-time contract:
// GC_BD_REAL must be an absolute path (a relative one would re-introduce PATH
// ambiguity and the recursion risk).
func TestResolveRealBdPathRejectsRelativeGCBDReal(t *testing.T) {
	t.Setenv("GC_BD_REAL", filepath.Join("relative", "bd"))
	if _, err := resolveRealBdPath(); err == nil {
		t.Fatal("expected an error for a relative GC_BD_REAL, got nil")
	}
}

// TestExecRealBdUsesGCBDRealAndPropagatesExit proves the passthrough path execs
// the GC_BD_REAL binary (no LookPath), streams its stdout, and propagates its
// exit code unchanged — the bd exit-code contract the shim must preserve.
func TestExecRealBdUsesGCBDRealAndPropagatesExit(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-script fake bd is POSIX-only")
	}
	dir := t.TempDir()
	realBd := filepath.Join(dir, "bd-real")
	if err := os.WriteFile(realBd, []byte("#!/bin/sh\necho \"args:$*\"\nexit 7\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GC_BD_REAL", realBd)
	t.Setenv("PATH", "") // prove no dependence on a PATH-resolved bd

	var out, errb bytes.Buffer
	code := execRealBd([]string{"version"}, dir, nil, nil, &out, &errb)
	if code != 7 {
		t.Fatalf("execRealBd exit = %d, want 7 (stderr=%q)", code, errb.String())
	}
	if !strings.Contains(out.String(), "args:version") {
		t.Fatalf("execRealBd stdout = %q, want it to contain %q", out.String(), "args:version")
	}
}

// TestClassifyBdShimVerb pins the three-way disposition policy: routed verbs
// always route; provably-graph-free verbs always passthrough; graph-touching
// unrouted verbs passthrough in the identity phase (byte-identical, safe) but
// are refused in the split phase rather than silently bypassing the graph store.
func TestClassifyBdShimVerb(t *testing.T) {
	cases := []struct {
		verb  string
		args  []string
		split bool
		want  bdShimDisposition
	}{
		{"close", []string{"x"}, false, bdRoute},
		{"close", []string{"x"}, true, bdRoute},
		{"show", []string{"x", "--json"}, true, bdRoute},
		{"version", nil, false, bdPassthrough},
		{"version", nil, true, bdPassthrough},
		{"mol", []string{"current", "m"}, false, bdRoute},  // current|progress + id routes (graph-aware) in both phases
		{"mol", []string{"current", "m"}, true, bdRoute},   // split phase: routes to GET /beads/graph/{root}
		{"mol", []string{"pour", "proto"}, true, bdRefuse}, // non-read mol subcommand: refuse under split
		{"mol", []string{"current"}, true, bdRefuse},       // id-omitted (bd infers it): not routable, refuse under split
		{"gate", []string{"check"}, true, bdRefuse},
		{"query", []string{"ephemeral=true"}, true, bdRefuse}, // no --json: not routable, refuse under split
		// ready: the simple assigned form routes (graph-aware), but predicate
		// flags the Router cannot yet replicate (pool-demand; C3/ga-2gap48.11)
		// passthrough to the work-only bd — byte-identical in the identity phase.
		{"ready", []string{"--assignee=w", "--json", "--limit", "1"}, true, bdRoute},
		// Discovery predicates now route (C3): the shim federates store.Ready() and
		// post-filters, so a graph control bead in SQLite is discoverable.
		{"ready", []string{"--metadata-field", "gc.routed_to=x", "--unassigned", "--json"}, true, bdRoute},
		{"ready", []string{"--exclude-type=epic", "--json"}, false, bdRoute},
		// A ready flag the shim does not model still passes through (byte-identical).
		{"ready", []string{"--label", "pool:worker", "--json"}, true, bdPassthrough},
		// update: the cleanly-mappable flag set routes (the canonical graph-worker
		// close), but flags with no UpdateOpts mapping (--notes/--claim/...)
		// passthrough — byte-identical in the identity phase.
		{"update", []string{"x", "--set-metadata", "gc.outcome=pass", "--status", "closed"}, true, bdRoute},
		{"update", []string{"x", "--notes", "done", "--status=closed"}, true, bdPassthrough},
		{"update", []string{"x", "--claim"}, true, bdPassthrough},
		{"reopen", []string{"x"}, true, bdRoute},
		{"delete", []string{"x", "--force"}, true, bdRoute},
	}
	for _, tc := range cases {
		if got := classifyBdShimVerb(tc.verb, tc.args, tc.split); got != tc.want {
			t.Errorf("classifyBdShimVerb(%q, %v, split=%v) = %v, want %v", tc.verb, tc.args, tc.split, got, tc.want)
		}
	}
}

// TestIsBdShimInvocation pins which argv[0] basenames trigger shim mode: only an
// executable named exactly `bd` (the PATH install symlinks bd -> gc). gc invoked
// normally, or a differently-named helper, must not enter the shim.
func TestIsBdShimInvocation(t *testing.T) {
	cases := map[string]bool{
		"bd":                true,
		"/usr/local/bin/bd": true,
		"./bd":              true,
		"gc":                false,
		"/x/gc":             false,
		"bd-real":           false,
		"/x/bd-shim":        false,
	}
	for arg0, want := range cases {
		if got := isBdShimInvocation(arg0); got != want {
			t.Errorf("isBdShimInvocation(%q) = %v, want %v", arg0, got, want)
		}
	}
}

// TestEnsureRealBdResolvablePrependsGCBDRealDir proves the recursion guard for
// the in-process work BdStore (which execs a bare `bd`, including when the Router
// probes backends by id): the directory of GC_BD_REAL is prepended to PATH so
// `bd` resolves to the real binary, not this shim — and idempotently.
func TestEnsureRealBdResolvablePrependsGCBDRealDir(t *testing.T) {
	dir := t.TempDir()
	realBd := filepath.Join(dir, "bd-real")
	if err := os.WriteFile(realBd, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GC_BD_REAL", realBd)
	t.Setenv("PATH", "/usr/bin")

	ensureRealBdResolvable()
	want := dir + string(os.PathListSeparator) + "/usr/bin"
	if got := os.Getenv("PATH"); got != want {
		t.Fatalf("PATH = %q, want %q", got, want)
	}
	// Idempotent: a second call must not double-prepend.
	ensureRealBdResolvable()
	if got := os.Getenv("PATH"); got != want {
		t.Fatalf("PATH after second call = %q, want %q (no double prepend)", got, want)
	}
}

// TestDispatchBdShimArgv0NonBdIsNotHandled proves gc invoked under its own name
// is left to the normal cobra root (the shim does not hijack it).
func TestDispatchBdShimArgv0NonBdIsNotHandled(t *testing.T) {
	if code, handled := dispatchBdShimArgv0("/x/gc", []string{"version"}, nil, nil, nil); handled {
		t.Fatalf("dispatchBdShimArgv0 handled a non-bd argv0 (code=%d), want not-handled", code)
	}
}

// TestDispatchBdShimArgv0RefusesWithoutGCBDReal proves the misinstall guard: a
// gc symlinked as bd but with no GC_BD_REAL refuses loudly instead of recursing
// (a bare bd lookup would resolve back to the shim).
func TestDispatchBdShimArgv0RefusesWithoutGCBDReal(t *testing.T) {
	t.Setenv("GC_BD_REAL", "")
	var stderr bytes.Buffer
	code, handled := dispatchBdShimArgv0("/agent/bin/bd", []string{"ready"}, nil, nil, &stderr)
	if !handled {
		t.Fatal("dispatchBdShimArgv0 did not handle a bd argv0")
	}
	if code == 0 {
		t.Fatal("expected non-zero exit when GC_BD_REAL is unset, got 0")
	}
	if !strings.Contains(stderr.String(), "GC_BD_REAL") {
		t.Fatalf("stderr = %q, want it to mention GC_BD_REAL", stderr.String())
	}
}

// TestSplitBdGlobalFlags proves the shim finds the bd subcommand past leading
// global flags — the controller discovers work via `bd --readonly --sandbox
// ready ...`, where the verb is not args[0].
func TestSplitBdGlobalFlags(t *testing.T) {
	cases := []struct {
		args []string
		verb string
		rest []string
	}{
		{[]string{"--readonly", "--sandbox", "ready", "--json"}, "ready", []string{"--json"}},
		{[]string{"ready", "--assignee=x"}, "ready", []string{"--assignee=x"}},
		{[]string{"close", "id"}, "close", []string{"id"}},
		{[]string{"--readonly"}, "", nil},
		{nil, "", nil},
	}
	for _, tc := range cases {
		verb, rest := splitBdGlobalFlags(tc.args)
		if verb != tc.verb {
			t.Errorf("splitBdGlobalFlags(%v) verb = %q, want %q", tc.args, verb, tc.verb)
		}
		if len(rest) != len(tc.rest) {
			t.Errorf("splitBdGlobalFlags(%v) rest = %v, want %v", tc.args, rest, tc.rest)
			continue
		}
		for i := range rest {
			if rest[i] != tc.rest[i] {
				t.Errorf("splitBdGlobalFlags(%v) rest = %v, want %v", tc.args, rest, tc.rest)
				break
			}
		}
	}
}

// TestRunBdShimPassthroughProjectsScopeEnv proves runBdShim routes a passthrough
// verb to the real bd — resolved via GC_BD_REAL (no PATH lookup) — in the
// resolved rig scope, with the projected bd env (GC_STORE_* set, BD_EXPORT_AUTO
// suppressed), the same scope/env contract `gc bd` enforces.
func TestRunBdShimPassthroughProjectsScopeEnv(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-script fake bd is POSIX-only")
	}
	origCityFlag := cityFlag
	origRigFlag := rigFlag
	t.Cleanup(func() { cityFlag = origCityFlag; rigFlag = origRigFlag })
	cityFlag = ""
	rigFlag = ""

	cityDir := t.TempDir()
	writeReachableManagedDoltState(t, cityDir) // so bdCommandEnv can project the rig's dolt target
	rigDir := filepath.Join(cityDir, "repo")
	if err := os.MkdirAll(filepath.Join(rigDir, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(`[workspace]
name = "demo"

[[rigs]]
name = "repo"
path = "repo"
prefix = "repo"
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rigDir, ".beads", "config.yaml"), []byte(`issue_prefix: repo
gc.endpoint_origin: inherited_city
gc.endpoint_status: verified
dolt.auto-start: false
`), 0o644); err != nil {
		t.Fatal(err)
	}

	capture := filepath.Join(t.TempDir(), "shim-bd-env.txt")
	realBd := filepath.Join(t.TempDir(), "bd-real")
	if err := os.WriteFile(realBd, []byte(`#!/bin/sh
set -eu
{
  printf 'pwd=%s\n' "$PWD"
  printf 'args=%s\n' "$*"
  printf 'GC_STORE_ROOT=%s\n' "${GC_STORE_ROOT:-}"
  printf 'GC_STORE_SCOPE=%s\n' "${GC_STORE_SCOPE:-}"
  printf 'GC_BEADS_PREFIX=%s\n' "${GC_BEADS_PREFIX:-}"
  printf 'BD_EXPORT_AUTO=%s\n' "${BD_EXPORT_AUTO:-}"
} > "${CAPTURE_PATH}"
`), 0o755); err != nil {
		t.Fatal(err)
	}

	t.Setenv("GC_BD_REAL", realBd)
	t.Setenv("CAPTURE_PATH", capture)
	t.Setenv("GC_CITY_PATH", cityDir)

	var stdout, stderr bytes.Buffer
	if code := runBdShim([]string{"--rig", "repo", "version"}, nil, &stdout, &stderr); code != 0 {
		t.Fatalf("runBdShim passthrough = %d, want 0; stderr=%q", code, stderr.String())
	}
	data, err := os.ReadFile(capture)
	if err != nil {
		t.Fatalf("read capture: %v (stderr=%q)", err, stderr.String())
	}
	got := make(map[string]string)
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if k, v, ok := strings.Cut(line, "="); ok {
			got[k] = v
		}
	}
	if !samePath(got["pwd"], rigDir) {
		t.Fatalf("real bd pwd = %q, want %q", got["pwd"], rigDir)
	}
	if got["args"] != "version" {
		t.Fatalf("real bd args = %q, want %q", got["args"], "version")
	}
	if !samePath(got["GC_STORE_ROOT"], rigDir) {
		t.Fatalf("GC_STORE_ROOT = %q, want %q", got["GC_STORE_ROOT"], rigDir)
	}
	if got["GC_STORE_SCOPE"] != "rig" {
		t.Fatalf("GC_STORE_SCOPE = %q, want %q", got["GC_STORE_SCOPE"], "rig")
	}
	if got["GC_BEADS_PREFIX"] != "repo" {
		t.Fatalf("GC_BEADS_PREFIX = %q, want %q", got["GC_BEADS_PREFIX"], "repo")
	}
	if got["BD_EXPORT_AUTO"] != "false" {
		t.Fatalf("BD_EXPORT_AUTO = %q, want %q (export suppression)", got["BD_EXPORT_AUTO"], "false")
	}
}
