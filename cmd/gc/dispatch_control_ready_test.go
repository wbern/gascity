package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/internal/config"
)

func usePathBDAsGCForControlReadyTest(t *testing.T) {
	t.Helper()
	envPath, err := exec.LookPath("env")
	if err != nil {
		t.Skip("env executable unavailable")
	}
	original := controlReadyExecutable
	controlReadyExecutable = func() (string, error) { return envPath, nil }
	t.Cleanup(func() { controlReadyExecutable = original })
}

func TestControlReadyFallbackInvokesAbsoluteCurrentGCWithBDArgv(t *testing.T) {
	poison := t.TempDir()
	if err := os.WriteFile(filepath.Join(poison, "bd"), []byte("#!/bin/sh\nexit 97\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", poison)

	originalExecutable := controlReadyExecutable
	originalRunner := controlReadyCommandRunner
	t.Cleanup(func() {
		controlReadyExecutable = originalExecutable
		controlReadyCommandRunner = originalRunner
	})
	controlReadyExecutable = func() (string, error) { return "/opt/gascity/current/gc", nil }
	var gotName, gotDisplay, gotDir string
	var gotArgs, gotEnv []string
	controlReadyCommandRunner = func(name string, args []string, display, dir string, env []string) (string, error) {
		gotName, gotDisplay, gotDir = name, display, dir
		gotArgs, gotEnv = append([]string(nil), args...), append([]string(nil), env...)
		return "[]", nil
	}

	dir := t.TempDir()
	if _, err := controlReadyFallbackReady(dir, map[string]string{
		citylayout.RealBdEnvVar: "/usr/local/bin/bd",
		"GC_STORE_SCOPE":        "rig",
	}, false); err != nil {
		t.Fatalf("controlReadyFallbackReady: %v", err)
	}
	if gotName != "/opt/gascity/current/gc" {
		t.Fatalf("executable = %q, want absolute current gc", gotName)
	}
	wantArgs := []string{"bd", "--readonly", "--sandbox", "ready", "--json", "--exclude-type=epic", "--limit=5000", "--allow-unbounded"}
	if !reflect.DeepEqual(gotArgs, wantArgs) {
		t.Fatalf("args = %#v, want %#v", gotArgs, wantArgs)
	}
	if gotDir != dir || !strings.Contains(gotDisplay, "/opt/gascity/current/gc bd") || len(gotEnv) == 0 {
		t.Fatalf("runner = name:%q args:%#v display:%q dir:%q env:%#v", gotName, gotArgs, gotDisplay, gotDir, gotEnv)
	}
}

func TestControlReadyExecutablePathRejectsEmptyWithoutAmbientFallback(t *testing.T) {
	original := controlReadyExecutable
	controlReadyExecutable = func() (string, error) { return "", nil }
	t.Cleanup(func() { controlReadyExecutable = original })

	if _, err := controlReadyExecutablePath(); err == nil || !strings.Contains(err.Error(), "empty path") {
		t.Fatalf("controlReadyExecutablePath() error = %v, want empty-path refusal", err)
	}
	query := workflowServeControlReadyQuery(config.Agent{Name: config.ControlDispatcherAgentName})
	if strings.Contains(query, "emit_ready gc ") || strings.Contains(query, "emit_ready bd ") {
		t.Fatalf("resolver failure fell back to ambient PATH: %s", query)
	}
	if !strings.Contains(query, "/__gascity_current_executable_unavailable__") || !strings.Contains(query, ` bd --readonly`) {
		t.Fatalf("resolver failure is not fail-closed: %s", query)
	}
}

func TestWorkflowServeControlReadyQueryUsesAbsoluteCurrentGCAtEveryLegacySeam(t *testing.T) {
	original := controlReadyExecutable
	controlReadyExecutable = func() (string, error) { return "/opt/gas city/gc", nil }
	t.Cleanup(func() { controlReadyExecutable = original })

	query := workflowServeControlReadyQuery(config.Agent{Name: config.ControlDispatcherAgentName, Dir: "fixture"})
	if strings.Contains(query, "emit_ready bd ") {
		t.Fatalf("query retains raw-bd seam: %s", query)
	}
	if got := strings.Count(query, `emit_ready '/opt/gas city/gc' bd `); got != 3 {
		t.Fatalf("absolute gc bd seam count = %d, want 3: %s", got, query)
	}
}

func TestParseControlReadyQueryRecognizesGeneratedQuery(t *testing.T) {
	// Dir+Name shaped so QualifiedName is a rig-scoped, binding-qualified
	// name ("fixture/core.control-dispatcher"): this is the only shape that
	// produces a non-empty bare-route alias (see TestControlDispatcherBareRoute).
	query := workflowServeControlReadyQuery(config.Agent{Name: "core.control-dispatcher", Dir: "fixture"}, "gascity--control-dispatcher")
	parsed, ok := parseControlReadyQuery(query)
	if !ok {
		t.Fatalf("parseControlReadyQuery: not recognized: %q", query)
	}
	if parsed.target != "fixture/core.control-dispatcher" {
		t.Errorf("target = %q, want %q", parsed.target, "fixture/core.control-dispatcher")
	}
	if parsed.controlSessionName != "gascity--control-dispatcher" {
		t.Errorf("controlSessionName = %q, want %q", parsed.controlSessionName, "gascity--control-dispatcher")
	}
	if parsed.bareTarget != "fixture/control-dispatcher" {
		t.Errorf("bareTarget = %q, want %q", parsed.bareTarget, "fixture/control-dispatcher")
	}
	if parsed.includeEphemeral {
		t.Errorf("includeEphemeral = true, want false (bd-1.0.4 default)")
	}
}

func TestParseControlReadyQueryIncludeEphemeralWhenBD105(t *testing.T) {
	query := workflowServeControlReadyQueryForBeads(
		config.Agent{Name: config.ControlDispatcherAgentName, Dir: "gascity"},
		config.BeadsConfig{BDCompatibility: config.BeadsBDCompatibility105},
	)
	parsed, ok := parseControlReadyQuery(query)
	if !ok {
		t.Fatalf("parseControlReadyQuery: not recognized")
	}
	if !parsed.includeEphemeral {
		t.Errorf("includeEphemeral = false, want true under bd-1.0.5 compatibility")
	}
}

func TestParseControlReadyQueryRejectsNonControlQuery(t *testing.T) {
	for _, q := range []string{
		"",
		"bd ready --json --limit=20",
		"GC_CONTROL_TARGET=core.control-dispatcher sh -c 'bd ready'", // missing the BD_EXPORT_AUTO=false marker prefix
	} {
		if _, ok := parseControlReadyQuery(q); ok {
			t.Errorf("parseControlReadyQuery(%q) = ok, want not recognized", q)
		}
	}
}

func TestControlReadyCandidatesPrecedenceDedupAndLegacyExpansion(t *testing.T) {
	query := workflowServeControlReadyQuery(config.Agent{Name: config.ControlDispatcherAgentName, Dir: "gascity"})
	parsed, ok := parseControlReadyQuery(query)
	if !ok {
		t.Fatalf("parseControlReadyQuery: not recognized")
	}
	envList := []string{
		"GC_SESSION_NAME=gascity--control-dispatcher",
		"GC_ALIAS=gascity/control-dispatcher",
	}

	got := controlReadyCandidates(parsed, envList)
	want := []string{
		"gascity--control-dispatcher",
		"gascity--workflow-control",
		"gascity/control-dispatcher",
		"gascity/workflow-control",
	}
	if !stringSlicesEqual(got, want) {
		t.Fatalf("controlReadyCandidates = %#v, want %#v", got, want)
	}
}

func TestControlReadyCandidatesSkipsEmptySlots(t *testing.T) {
	// "control-dispatcher" itself ends in the literal suffix "control-dispatcher",
	// so it also produces the bare "workflow-control" legacy variant.
	parsed := parsedControlReadyQuery{target: "control-dispatcher"}
	got := controlReadyCandidates(parsed, nil)
	want := []string{"control-dispatcher", "workflow-control"}
	if !stringSlicesEqual(got, want) {
		t.Fatalf("controlReadyCandidates = %#v, want %#v", got, want)
	}
}

func TestControlReadyRoutesFiltersEmptyAliases(t *testing.T) {
	parsed := parsedControlReadyQuery{target: "core.control-dispatcher", bareTarget: "control-dispatcher"}
	got := controlReadyRoutes(parsed)
	want := []string{"core.control-dispatcher", "control-dispatcher"}
	if !stringSlicesEqual(got, want) {
		t.Fatalf("controlReadyRoutes = %#v, want %#v", got, want)
	}
}

func TestFilterReadyByAssigneeExcludesEpicAndOtherAssignees(t *testing.T) {
	ready := []beads.Bead{
		{ID: "ga-epic-leak", Assignee: "cand", Type: "epic"},
		{ID: "ga-ready", Assignee: "cand", Type: "task"},
		{ID: "ga-other", Assignee: "someone-else", Type: "task"},
	}
	got := filterReadyByAssignee(ready, "cand", workflowServeScanLimit)
	if len(got) != 1 || got[0].ID != "ga-ready" {
		t.Fatalf("filterReadyByAssignee = %#v, want only ga-ready", got)
	}
}

func TestFilterReadyByAssigneeRespectsLimit(t *testing.T) {
	ready := make([]beads.Bead, 0, 5)
	for i := 0; i < 5; i++ {
		ready = append(ready, beads.Bead{ID: strings.Repeat("z", i+1), Assignee: "cand", Type: "task"})
	}
	got := filterReadyByAssignee(ready, "cand", 2)
	if len(got) != 2 {
		t.Fatalf("filterReadyByAssignee len = %d, want 2", len(got))
	}
}

func TestFilterReadyByRouteRequiresUnassignedAndSortsOldestFirst(t *testing.T) {
	newer := time.Unix(200, 0)
	older := time.Unix(100, 0)
	ready := []beads.Bead{
		{ID: "ga-assigned-routed", CreatedAt: older, Assignee: "someone", Metadata: map[string]string{beadmeta.RunTargetMetadataKey: "core/control-dispatcher"}},
		{ID: "ga-newer", CreatedAt: newer, Metadata: map[string]string{beadmeta.RunTargetMetadataKey: "core/control-dispatcher"}},
		{ID: "ga-older", CreatedAt: older, Metadata: map[string]string{beadmeta.RunTargetMetadataKey: "core/control-dispatcher"}},
		{ID: "ga-epic-routed", CreatedAt: older, Type: "epic", Metadata: map[string]string{beadmeta.RunTargetMetadataKey: "core/control-dispatcher"}},
		{ID: "ga-other-route", CreatedAt: older, Metadata: map[string]string{beadmeta.RunTargetMetadataKey: "other"}},
	}
	got := filterReadyByRoute(ready, beadmeta.RunTargetMetadataKey, "core/control-dispatcher")
	want := []string{"ga-older", "ga-newer"}
	if !stringSlicesEqual(beadIDs(got), want) {
		t.Fatalf("filterReadyByRoute = %#v, want %#v", beadIDs(got), want)
	}
}

func TestMergeControlReadyGroupsDedupsPreservingFirstOccurrence(t *testing.T) {
	assigned := []beads.Bead{
		{ID: "ga-z-assigned"},
		{ID: "ga-dup", Metadata: map[string]string{"source": "assigned"}},
	}
	runTargetRouted := []beads.Bead{
		{ID: "ga-a-routed"},
		{ID: "ga-route-dup", Metadata: map[string]string{"source": "run-target"}},
	}
	routedToRouted := []beads.Bead{
		{ID: "ga-route-dup", Metadata: map[string]string{"source": "routed-to"}},
	}

	got := mergeControlReadyGroups(assigned, runTargetRouted, routedToRouted)
	wantIDs := []string{"ga-z-assigned", "ga-dup", "ga-a-routed", "ga-route-dup"}
	if !stringSlicesEqual(beadIDs(got), wantIDs) {
		t.Fatalf("mergeControlReadyGroups ids = %#v, want %#v", beadIDs(got), wantIDs)
	}
	for _, b := range got {
		if b.ID == "ga-route-dup" && b.Metadata["source"] != "run-target" {
			t.Fatalf("ga-route-dup source = %q, want first-seen %q", b.Metadata["source"], "run-target")
		}
	}
}

func TestMergeControlReadyGroupsSkipsInstantiatingWithoutMarkingSeen(t *testing.T) {
	assigned := []beads.Bead{
		{ID: "ga-instantiating-assigned", Metadata: map[string]string{beadmeta.InstantiatingMetadataKey: "true"}},
		{ID: "ga-assigned", Metadata: map[string]string{"gc.kind": "retry"}},
	}
	runTargetRouted := []beads.Bead{
		{ID: "ga-instantiating-routed", Metadata: map[string]string{beadmeta.InstantiatingMetadataKey: "true"}},
		{ID: "ga-routed", Metadata: map[string]string{"gc.kind": "scope-check"}},
	}
	// A later group re-surfacing the SAME id without the instantiating tag
	// must still be admitted -- the shell's jq reduce never marks an
	// instantiating occurrence as "seen".
	laterNonInstantiating := []beads.Bead{
		{ID: "ga-instantiating-assigned", Metadata: map[string]string{"gc.kind": "now-real"}},
	}

	got := mergeControlReadyGroups(assigned, runTargetRouted, laterNonInstantiating)
	wantIDs := []string{"ga-assigned", "ga-routed", "ga-instantiating-assigned"}
	if !stringSlicesEqual(beadIDs(got), wantIDs) {
		t.Fatalf("mergeControlReadyGroups ids = %#v, want %#v", beadIDs(got), wantIDs)
	}
}

// TestEvaluateControlReadyMatchesShellQueryPriority ports
// TestWorkflowServeControlReadyQueryPreservesQueryPriorityWhenMerging's
// scenario (cmd_convoy_dispatch_test.go) at the Go level: given the same
// parsed query + env, and a ready set shaped like what CachedReady/the
// batched fallback would return, evaluateControlReady must merge candidates
// before routes and drop later ID duplicates exactly like the shell's jq
// reduce does.
func TestEvaluateControlReadyMatchesShellQueryPriority(t *testing.T) {
	query := workflowServeControlReadyQuery(config.Agent{Name: config.ControlDispatcherAgentName, Dir: "gascity"})
	parsed, ok := parseControlReadyQuery(query)
	if !ok {
		t.Fatalf("parseControlReadyQuery: not recognized")
	}
	envList := []string{
		"GC_SESSION_NAME=gascity--control-dispatcher",
		"GC_ALIAS=gascity/control-dispatcher",
	}
	ready := []beads.Bead{
		{ID: "ga-z-assigned", Assignee: "gascity--control-dispatcher"},
		{ID: "ga-dup", Assignee: "gascity--control-dispatcher", Metadata: map[string]string{"source": "assigned"}},
		{ID: "ga-a-routed", Metadata: map[string]string{beadmeta.RunTargetMetadataKey: "gascity/control-dispatcher"}},
		{ID: "ga-route-dup", Metadata: map[string]string{beadmeta.RunTargetMetadataKey: "gascity/control-dispatcher", "source": "run-target"}},
		{ID: "ga-route-dup-2", Metadata: map[string]string{beadmeta.RoutedToMetadataKey: "gascity/control-dispatcher"}},
	}
	// ga-route-dup also appears as a routed_to match with different content;
	// the run_target occurrence (checked first) must win.
	ready = append(ready, beads.Bead{ID: "ga-route-dup", Metadata: map[string]string{beadmeta.RoutedToMetadataKey: "gascity/control-dispatcher", "source": "routed-to"}})

	got := evaluateControlReady(ready, parsed, envList)
	wantIDs := []string{"ga-z-assigned", "ga-dup", "ga-a-routed", "ga-route-dup", "ga-route-dup-2"}
	if !stringSlicesEqual(beadIDs(got), wantIDs) {
		t.Fatalf("evaluateControlReady ids = %#v, want %#v", beadIDs(got), wantIDs)
	}
	for _, b := range got {
		if b.ID == "ga-route-dup" && b.Metadata["source"] != "run-target" {
			t.Fatalf("ga-route-dup source = %q, want first-seen %q", b.Metadata["source"], "run-target")
		}
	}
}

func TestEvaluateControlReadyExcludesEpicAndInstantiating(t *testing.T) {
	query := workflowServeControlReadyQuery(config.Agent{Name: config.ControlDispatcherAgentName, Dir: "gascity"})
	parsed, ok := parseControlReadyQuery(query)
	if !ok {
		t.Fatalf("parseControlReadyQuery: not recognized")
	}
	envList := []string{
		"GC_SESSION_NAME=gascity--control-dispatcher",
		"GC_ALIAS=gascity/control-dispatcher",
	}
	ready := []beads.Bead{
		{ID: "ga-epic-leak", Assignee: "gascity--control-dispatcher", Type: "epic"},
		{ID: "ga-ready", Assignee: "gascity--control-dispatcher", Type: "task"},
		{ID: "ga-instantiating-routed", Metadata: map[string]string{beadmeta.RunTargetMetadataKey: "gascity/control-dispatcher", beadmeta.InstantiatingMetadataKey: "true"}},
		{ID: "ga-routed", Metadata: map[string]string{beadmeta.RunTargetMetadataKey: "gascity/control-dispatcher", "gc.kind": "scope-check"}},
	}

	got := evaluateControlReady(ready, parsed, envList)
	wantIDs := []string{"ga-ready", "ga-routed"}
	if !stringSlicesEqual(beadIDs(got), wantIDs) {
		t.Fatalf("evaluateControlReady ids = %#v, want %#v", beadIDs(got), wantIDs)
	}
}

func beadIDs(items []beads.Bead) []string {
	out := make([]string, len(items))
	for i, b := range items {
		out[i] = b.ID
	}
	return out
}

func stringSlicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// --- End-to-end: nextWorkflowServeBeads wiring (cache + fallback) ---

// setUpControlReadyFileStoreCity builds a scope-local FileStore-backed city
// so tryControlReadyFromCacheOrFallback's cache path can PrimeActive() and
// CachedReady() without any bd/dolt process at all, and returns the opened
// store for seeding fixture beads directly.
func setUpControlReadyFileStoreCity(t *testing.T) (cityDir string, store *beads.FileStore) {
	t.Helper()
	configureIsolatedRuntimeEnv(t)
	t.Setenv("GC_BEADS", "file")

	cityDir = t.TempDir()
	if err := ensureScopedFileStoreLayout(cityDir); err != nil {
		t.Fatalf("ensureScopedFileStoreLayout: %v", err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte("[workspace]\nname = \"test-city\"\n"), 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}
	if err := ensurePersistedScopeLocalFileStore(cityDir); err != nil {
		t.Fatalf("ensurePersistedScopeLocalFileStore: %v", err)
	}
	store, err := openScopeLocalFileStore(cityDir)
	if err != nil {
		t.Fatalf("openScopeLocalFileStore: %v", err)
	}
	return cityDir, store
}

// noBDOnPathForTest ensures no bd (or bd stub) is reachable via PATH, so a
// test can prove a code path made zero subprocess calls: any shell-out would
// fail with "command not found" rather than silently succeeding.
func noBDOnPathForTest(t *testing.T) {
	t.Helper()
	t.Setenv("PATH", t.TempDir())
}

func shimmedBdEnvForTest(t *testing.T, dir string) map[string]string {
	t.Helper()
	realBd := filepath.Join(dir, "real-bd")
	if err := os.WriteFile(realBd, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write fake real bd: %v", err)
	}
	return map[string]string{citylayout.RealBdEnvVar: realBd}
}

// A real-bd marker alone is insufficient to authorize shim-only flags: the
// command runner still resolves `bd` through PATH, which can point directly to
// that raw binary during a partial shim rollout.
func TestControlReadyShimmedRejectsRawBdOnPath(t *testing.T) {
	dir := t.TempDir()
	rawBd := filepath.Join(dir, "bd")
	if err := os.WriteFile(rawBd, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write raw bd: %v", err)
	}

	if controlReadyShimmed(map[string]string{
		citylayout.RealBdEnvVar: rawBd,
		"PATH":                  dir,
	}) {
		t.Fatal("controlReadyShimmed accepted raw bd on PATH")
	}
}

func TestControlReadyShimmedTreatsEmptyPathEntryAsCurrentDirectory(t *testing.T) {
	dir := t.TempDir()
	shim := filepath.Join(dir, "bd")
	realBd := filepath.Join(dir, "real-bd")
	for _, path := range []string{shim, realBd} {
		if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}
	oldWD, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldWD) })

	if !controlReadyShimmed(map[string]string{
		citylayout.RealBdEnvVar: realBd,
		"PATH":                  string(os.PathListSeparator),
	}) {
		t.Fatal("empty PATH component resolving ./bd shim was not recognized")
	}
}

func TestControlReadyCachePrimeUsesUnboundedReadOnlyForShimmedDispatcher(t *testing.T) {
	configureIsolatedRuntimeEnv(t)
	cityDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte("[workspace]\nname = \"test-city\"\n"), 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}

	tmp := t.TempDir()
	argsPath := filepath.Join(tmp, "bd.args")
	bdPath := filepath.Join(tmp, "bd")
	script := fmt.Sprintf(`#!/bin/sh
printf '%%s\n' "$*" >> %q
case "$*" in
  *--allow-unbounded*) printf '[]' ;;
  *) printf '{"reason":"byte_budget_exceeded"}' ;;
esac
`, argsPath)
	if err := os.WriteFile(bdPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake bd: %v", err)
	}
	t.Setenv("PATH", tmp+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("GC_BEADS", "bd")

	cache := controlReadyCacheFor(cityDir, cityDir, nil, shimmedBdEnvForTest(t, tmp))
	if cache == nil {
		t.Fatal("controlReadyCacheFor returned nil; shimmed control cache prime must decode its full read")
	}
	args, err := os.ReadFile(argsPath)
	if err != nil {
		t.Fatalf("read bd args: %v", err)
	}
	if !strings.Contains(string(args), "list") || !strings.Contains(string(args), "--allow-unbounded") {
		t.Fatalf("cache-prime bd args = %q, want a list read with --allow-unbounded", args)
	}
}

func TestTryControlReadyFromCacheOrFallbackAnswersFromCacheWithZeroSubprocessCalls(t *testing.T) {
	cityDir, store := setUpControlReadyFileStoreCity(t)
	noBDOnPathForTest(t)

	target := "gascity/control-dispatcher"
	ready, err := store.Create(beads.Bead{Assignee: target, Type: "task"})
	if err != nil {
		t.Fatalf("create ready bead: %v", err)
	}
	epic, err := store.Create(beads.Bead{Assignee: target, Type: "epic"})
	if err != nil {
		t.Fatalf("create epic bead: %v", err)
	}
	routed, err := store.Create(beads.Bead{Metadata: map[string]string{beadmeta.RoutedToMetadataKey: target}})
	if err != nil {
		t.Fatalf("create routed bead: %v", err)
	}

	agentCfg := config.Agent{Name: config.ControlDispatcherAgentName, Dir: "gascity"}
	query := workflowServeControlReadyQuery(agentCfg)

	queue, handled, err := tryControlReadyFromCacheOrFallback(query, cityDir, nil)
	if err != nil {
		t.Fatalf("tryControlReadyFromCacheOrFallback: %v", err)
	}
	if !handled {
		t.Fatalf("tryControlReadyFromCacheOrFallback: handled = false, want true for a control-ready query")
	}

	var gotIDs []string
	for _, b := range queue {
		gotIDs = append(gotIDs, b.ID)
	}
	wantIDs := []string{ready.ID, routed.ID}
	if !stringSlicesEqual(gotIDs, wantIDs) {
		t.Fatalf("queue ids = %#v, want %#v (epic bead %s must be excluded)", gotIDs, wantIDs, epic.ID)
	}
}

func TestTryControlReadyFromCacheOrFallbackReturnsUnhandledForNonControlQuery(t *testing.T) {
	cityDir := t.TempDir()
	_, handled, err := tryControlReadyFromCacheOrFallback("bd ready --json --limit=20", cityDir, nil)
	if handled {
		t.Fatalf("handled = true, want false for a non-control-ready query")
	}
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
}

// TestTryControlReadyFromCacheOrFallbackUsesSingleBatchedBDCallWhenCacheUnavailable
// forces the cache path to fail (PrimeActive against a bd stub that errors on
// `list`) and asserts the fallback makes exactly one bd invocation covering
// the whole tick, not the shell script's N per-candidate/route calls.
func TestTryControlReadyFromCacheOrFallbackUsesSingleBatchedBDCallWhenCacheUnavailable(t *testing.T) {
	configureIsolatedRuntimeEnv(t)
	cityDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte("[workspace]\nname = \"test-city\"\n"), 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}

	tmp := t.TempDir()
	logPath := filepath.Join(tmp, "bd.log")
	bdPath := filepath.Join(tmp, "bd")
	target := "gascity/control-dispatcher"
	script := fmt.Sprintf(`#!/bin/sh
set -eu
printf '%%s\n' "$*" >> "%s"
case "$1" in
  list)
    exit 7
    ;;
esac
case "$*" in
  "--readonly --sandbox ready --json --exclude-type=epic --limit=%d")
    printf '[{"id":"ga-fallback-ready","assignee":"%s"}]'
    ;;
  *)
    printf '[]'
    ;;
esac
`, logPath, controlReadyFallbackLimit, target)
	if err := os.WriteFile(bdPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake bd: %v", err)
	}
	t.Setenv("PATH", tmp+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("GC_BEADS", "bd")

	agentCfg := config.Agent{Name: config.ControlDispatcherAgentName, Dir: "gascity"}
	query := workflowServeControlReadyQuery(agentCfg)

	queue, handled, err := tryControlReadyFromCacheOrFallback(query, cityDir, nil)
	if err != nil {
		t.Fatalf("tryControlReadyFromCacheOrFallback: %v", err)
	}
	if !handled {
		t.Fatalf("handled = false, want true")
	}
	if len(queue) != 1 || queue[0].ID != "ga-fallback-ready" {
		t.Fatalf("queue = %#v, want single ga-fallback-ready bead", queue)
	}

	logData, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read bd log: %v", err)
	}
	calls := strings.Split(strings.TrimSpace(string(logData)), "\n")
	readyCalls := 0
	for _, c := range calls {
		if strings.HasPrefix(c, "--readonly --sandbox ready") {
			readyCalls++
		}
	}
	if readyCalls != 1 {
		t.Fatalf("bd ready calls = %d, want exactly 1; all calls:\n%s", readyCalls, string(logData))
	}
}

// TestControlReadyFallbackReadyLogsWhenResultHitsLimit is ga-bbj6wv Finding 1:
// a fallback batch that comes back at exactly controlReadyFallbackLimit is a
// truncation signal (some candidate/route may have been starved of ready
// beads that exist but didn't fit) and must be observable, not silent.
func TestControlReadyFallbackReadyLogsWhenResultHitsLimit(t *testing.T) {
	usePathBDAsGCForControlReadyTest(t)
	configureIsolatedRuntimeEnv(t)
	tmp := t.TempDir()

	items := make([]map[string]string, controlReadyFallbackLimit)
	for i := range items {
		items[i] = map[string]string{"id": fmt.Sprintf("ga-fallback-%d", i)}
	}
	payload, err := json.Marshal(items)
	if err != nil {
		t.Fatalf("marshal fixture beads: %v", err)
	}
	payloadPath := filepath.Join(tmp, "payload.json")
	if err := os.WriteFile(payloadPath, payload, 0o644); err != nil {
		t.Fatalf("write payload: %v", err)
	}
	bdPath := filepath.Join(tmp, "bd")
	script := fmt.Sprintf("#!/bin/sh\ncat %q\n", payloadPath)
	if err := os.WriteFile(bdPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake bd: %v", err)
	}
	t.Setenv("PATH", tmp+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("GC_BEADS", "bd")

	var logBuf bytes.Buffer
	restore := captureLogOutput(&logBuf)
	defer restore()

	dir := t.TempDir()
	result, err := controlReadyFallbackReady(dir, nil, false)
	if err != nil {
		t.Fatalf("controlReadyFallbackReady: %v", err)
	}
	if len(result) != controlReadyFallbackLimit {
		t.Fatalf("len(result) = %d, want %d", len(result), controlReadyFallbackLimit)
	}
	if !strings.Contains(logBuf.String(), "may be truncated") {
		t.Fatalf("expected a truncation warning in log output, got: %q", logBuf.String())
	}
	if !strings.Contains(logBuf.String(), dir) {
		t.Fatalf("expected log to name the dir %q, got: %q", dir, logBuf.String())
	}
}

// TestControlReadyFallbackReadyNoWarningBelowLimit is the negative case: a
// batch below the limit is a complete result, not a truncation signal, and
// must not log anything.
func TestControlReadyFallbackReadyNoWarningBelowLimit(t *testing.T) {
	usePathBDAsGCForControlReadyTest(t)
	configureIsolatedRuntimeEnv(t)
	tmp := t.TempDir()
	bdPath := filepath.Join(tmp, "bd")
	if err := os.WriteFile(bdPath, []byte("#!/bin/sh\nprintf '[{\"id\":\"ga-fallback-only\"}]'\n"), 0o755); err != nil {
		t.Fatalf("write fake bd: %v", err)
	}
	t.Setenv("PATH", tmp+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("GC_BEADS", "bd")

	var logBuf bytes.Buffer
	restore := captureLogOutput(&logBuf)
	defer restore()

	result, err := controlReadyFallbackReady(t.TempDir(), nil, false)
	if err != nil {
		t.Fatalf("controlReadyFallbackReady: %v", err)
	}
	if len(result) != 1 {
		t.Fatalf("len(result) = %d, want 1", len(result))
	}
	if logBuf.Len() != 0 {
		t.Fatalf("expected no log output below the limit, got: %q", logBuf.String())
	}
}

func TestControlReadyFallbackReadyConsumesSummaryProjection(t *testing.T) {
	usePathBDAsGCForControlReadyTest(t)
	configureIsolatedRuntimeEnv(t)
	tmp := t.TempDir()
	argsPath := filepath.Join(tmp, "args")
	bdPath := filepath.Join(tmp, "bd")
	script := fmt.Sprintf(`#!/bin/sh
printf '%%s' "$*" > %q
	printf '%%s' '{"schema_version":"1","kind":"gc.bead_summary","verb":"ready","beads":[{"id":"gcw-summary","status":"open","type":"epic","created_at":"2026-08-12T08:40:00Z","assignee":"control","labels":["pool:worker"],"routing_metadata":{"gc.routed_to":"rig/control"}}]}'
`, argsPath)
	if err := os.WriteFile(bdPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake bd: %v", err)
	}
	t.Setenv("PATH", tmp+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("GC_BEADS", "bd")

	result, err := controlReadyFallbackReady(t.TempDir(), shimmedBdEnvForTest(t, tmp), false)
	if err != nil {
		t.Fatalf("controlReadyFallbackReady: %v", err)
	}
	if len(result) != 1 || result[0].ID != "gcw-summary" {
		t.Fatalf("result = %#v, want summary bead", result)
	}
	if result[0].Metadata[beadmeta.RoutedToMetadataKey] != "rig/control" {
		t.Fatalf("routing metadata = %#v", result[0].Metadata)
	}
	if got := filterReadyByAssignee(result, "control", workflowServeScanLimit); len(got) != 0 {
		t.Fatalf("filterReadyByAssignee(summary epic) = %#v, want empty", got)
	}
	args, err := os.ReadFile(argsPath)
	if err != nil {
		t.Fatalf("read bd args: %v", err)
	}
	if !strings.Contains(string(args), "--summary-json") {
		t.Fatalf("bd args = %q, want --summary-json", args)
	}
	if !strings.Contains(string(args), "--allow-unbounded") {
		t.Fatalf("bd args = %q, want --allow-unbounded for the control-plane read", args)
	}
}

func TestControlReadyFallbackReadyOmitsSummaryButKeepsUnboundedForPinnedNonCityScope(t *testing.T) {
	usePathBDAsGCForControlReadyTest(t)
	configureIsolatedRuntimeEnv(t)
	tmp := t.TempDir()
	argsPath := filepath.Join(tmp, "args")
	bdPath := filepath.Join(tmp, "bd")
	script := fmt.Sprintf("#!/bin/sh\nprintf '%%s' \"$*\" > %q\nprintf '[{\"id\":\"gcw-plain\"}]'\n", argsPath)
	if err := os.WriteFile(bdPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake bd: %v", err)
	}
	t.Setenv("PATH", tmp+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("GC_BEADS", "bd")

	env := shimmedBdEnvForTest(t, tmp)
	env["GC_STORE_SCOPE"] = "rig"
	result, err := controlReadyFallbackReady(t.TempDir(), env, false)
	if err != nil {
		t.Fatalf("controlReadyFallbackReady: %v", err)
	}
	if len(result) != 1 || result[0].ID != "gcw-plain" {
		t.Fatalf("result = %#v, want plain bead", result)
	}
	args, err := os.ReadFile(argsPath)
	if err != nil {
		t.Fatalf("read bd args: %v", err)
	}
	if strings.Contains(string(args), "--summary-json") {
		t.Fatalf("bd args = %q, must not request shim summary for pinned rig scope", args)
	}
	// A pinned rig scope is the control dispatcher's ALWAYS-case
	// (work_query_probe.go sets GC_STORE_SCOPE=rig for any rig-qualified
	// agent), and its ready set exceeds a megabyte. Without the exemption the
	// shim returns a gc.output_firewall envelope this caller cannot decode,
	// which is what took the dispatcher down in gcw-78nf4. The scope governs
	// --summary-json only; it must not withhold --allow-unbounded.
	if !strings.Contains(string(args), "--allow-unbounded") {
		t.Fatalf("bd args = %q, want --allow-unbounded: a rig-pinned control-plane read must not be firewall-bounded", args)
	}
}

func TestControlReadyFallbackReadyDegradesWhenSummaryQueryFails(t *testing.T) {
	usePathBDAsGCForControlReadyTest(t)
	configureIsolatedRuntimeEnv(t)
	tmp := t.TempDir()
	argsPath := filepath.Join(tmp, "args")
	bdPath := filepath.Join(tmp, "bd")
	script := fmt.Sprintf(`#!/bin/sh
printf '%%s\n' "$*" >> %q
case "$*" in
  *--summary-json*) exit 1 ;;
esac
printf '[{"id":"gcw-plain"}]'
`, argsPath)
	if err := os.WriteFile(bdPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake bd: %v", err)
	}
	t.Setenv("PATH", tmp+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("GC_BEADS", "bd")

	result, err := controlReadyFallbackReady(t.TempDir(), shimmedBdEnvForTest(t, tmp), false)
	if err != nil {
		t.Fatalf("controlReadyFallbackReady: %v", err)
	}
	if len(result) != 1 || result[0].ID != "gcw-plain" {
		t.Fatalf("result = %#v, want plain bead after degradation", result)
	}
	args, err := os.ReadFile(argsPath)
	if err != nil {
		t.Fatalf("read bd args: %v", err)
	}
	if got := strings.Count(string(args), "ready"); got != 2 {
		t.Fatalf("bd calls = %q, want summarized then unsummarized retry", args)
	}
}

func TestDecodeControlReadySummaryRejectsUnrecognizedEnvelope(t *testing.T) {
	if _, err := decodeControlReadySummary([]byte(`{"beads":[{"id":"gcw-unknown"}]}`)); err == nil {
		t.Fatal("decodeControlReadySummary accepted an unrecognized envelope")
	}
}

func TestNextWorkflowServeBeadsNonControlQueryUsesOriginalShellPath(t *testing.T) {
	tmp := t.TempDir()
	bdPath := filepath.Join(tmp, "bd")
	if err := os.WriteFile(bdPath, []byte(`#!/bin/sh
printf '[{"id":"ga-plain"}]'
`), 0o755); err != nil {
		t.Fatalf("write fake bd: %v", err)
	}
	t.Setenv("PATH", tmp+string(os.PathListSeparator)+os.Getenv("PATH"))

	got, err := nextWorkflowServeBeads("bd ready --json --limit=20", t.TempDir(), nil)
	if err != nil {
		t.Fatalf("nextWorkflowServeBeads: %v", err)
	}
	if len(got) != 1 || got[0].ID != "ga-plain" {
		t.Fatalf("nextWorkflowServeBeads = %#v, want [{ga-plain}]", got)
	}
}
