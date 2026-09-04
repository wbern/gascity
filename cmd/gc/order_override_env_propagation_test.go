package main

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/orders"
)

// This file characterizes the resolved-order override chain end to end: a
// city.toml [[orders.overrides]] env entry must reach the environment of the
// process an exec order actually spawns, on every path that can spawn one.
//
// It exists because a GC3 field report (gcw-hh9an, PR #547) claimed order
// execution silently lost PR_REVIEW_CORPUS_ROOT. Source tracing disproved the
// claim -- the chain is intact -- so these tests lock it against regression
// instead, so the same report cannot resurface without a failing test to point
// at. The env key below is deliberately the one from that report.
//
// The chain under test is:
//
//	city.toml [[orders.overrides]].env
//	  -> orderdiscovery.ScanAll (the ONLY non-test caller of orders.ApplyOverrides)
//	  -> orders.applyOverride merges ov.Env into a.Env key-by-key
//	  -> orderExecEnvWithError overlays a.Env into the process env
//	  -> shellExecRunner hands that env to the spawned `sh -c`
//
// Both spawning paths run a real subprocess here rather than asserting on an
// intercepted ExecRunner, so what is observed is the child's actual
// environment, not the argument we intended to pass it.

// overrideEnvProbeKey is the env key from the original GC3 report.
const overrideEnvProbeKey = "PR_REVIEW_CORPUS_ROOT"

// TestOverrideEnvReachesManualOrderRunProcess covers the manual
// `gc order run` path (cmd_order.go doOrderRunExecResult), whose order comes
// from loadAllOrders -> orderdiscovery.ScanAll.
func TestOverrideEnvReachesManualOrderRunProcess(t *testing.T) {
	cityDir, cfg, envDump := newOrderOverrideEnvCity(t, map[string]string{
		overrideEnvProbeKey: "/override/corpus",
	})

	a := resolveProbeOrder(t, cityDir, cfg)

	var stderr bytes.Buffer
	result := doOrderRunExecResult(a, cityDir, cfg, nil, io.Discard, &stderr)
	if result.code != 0 {
		t.Fatalf("doOrderRunExecResult = %d (%s); stderr: %s", result.code, result.failureLabel, stderr.String())
	}

	childEnv := readChildEnv(t, envDump)
	if got := childEnv[overrideEnvProbeKey]; got != "/override/corpus" {
		t.Fatalf("manual `gc order run` child env[%s] = %q, want %q -- the resolved override did not reach the spawned process",
			overrideEnvProbeKey, got, "/override/corpus")
	}
	// The order's own [order.env] keys must still survive the override merge:
	// applyOverride merges key-by-key rather than replacing the map.
	if got := childEnv["ORDER_OWN_KEY"]; got != "from-order" {
		t.Fatalf("child env[ORDER_OWN_KEY] = %q, want %q -- overriding one key dropped the order's other [order.env] keys",
			got, "from-order")
	}
}

// TestOverrideEnvReachesScheduledDispatchProcess covers the scheduled
// controller path (order_dispatch.go dispatchExec), whose order comes from
// memoryOrderDispatcher.aa -- populated only by the constructor, which
// buildOrderDispatcher feeds from orderdiscovery.ScanAll.
func TestOverrideEnvReachesScheduledDispatchProcess(t *testing.T) {
	cityDir, cfg, envDump := newOrderOverrideEnvCity(t, map[string]string{
		overrideEnvProbeKey: "/override/corpus",
	})

	var stderr bytes.Buffer
	ad := buildOrderDispatcher(cityDir, cfg, events.Discard, &stderr)
	if ad == nil {
		t.Fatalf("buildOrderDispatcher returned nil; stderr: %s", stderr.String())
	}
	mad, ok := ad.(*memoryOrderDispatcher)
	if !ok {
		t.Fatalf("buildOrderDispatcher returned %T, want *memoryOrderDispatcher", ad)
	}

	// Take the order from the dispatcher's own resolved set, not from a second
	// lookup -- that set is what the tick loop dispatches from.
	a := requireProbeOrder(t, mad.aa)

	target, err := resolveOrderExecTarget(cityDir, cfg, a)
	if err != nil {
		t.Fatalf("resolveOrderExecTarget: %v", err)
	}
	front := orders.NewStore(beads.OrdersStore{Store: beads.NewMemStore()})
	run, err := front.CreateRun(a.ScopedName(), orders.RunOpts{})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}

	mad.dispatchExec(context.Background(), front, target, a, cityDir, run.ID, nil)

	childEnv := readChildEnv(t, envDump)
	if got := childEnv[overrideEnvProbeKey]; got != "/override/corpus" {
		t.Fatalf("scheduled dispatch child env[%s] = %q, want %q -- the resolved override did not reach the spawned process; stderr: %s",
			overrideEnvProbeKey, got, "/override/corpus", stderr.String())
	}
}

// TestOverrideEnvPrecedenceUnderDispatchVars locks the precedence order that
// orderExecEnvWithError implements: the resolved order Env overlays the
// controller-built env, and only dispatch-time vars (a webhook rule's args or
// `gc order run --var`) overlay after it.
func TestOverrideEnvPrecedenceUnderDispatchVars(t *testing.T) {
	cityDir, cfg, envDump := newOrderOverrideEnvCity(t, map[string]string{
		overrideEnvProbeKey: "/override/corpus",
	})
	a := resolveProbeOrder(t, cityDir, cfg)

	// The order declares /pack/default; the override must beat it.
	var stderr bytes.Buffer
	if result := doOrderRunExecResult(a, cityDir, cfg, nil, io.Discard, &stderr); result.code != 0 {
		t.Fatalf("doOrderRunExecResult (no vars) = %d; stderr: %s", result.code, stderr.String())
	}
	if got := readChildEnv(t, envDump)[overrideEnvProbeKey]; got != "/override/corpus" {
		t.Fatalf("with no dispatch vars, child env[%s] = %q, want the override value %q",
			overrideEnvProbeKey, got, "/override/corpus")
	}

	// A dispatch-time var must in turn beat the override.
	stderr.Reset()
	vars := map[string]string{overrideEnvProbeKey: "/var/corpus"}
	if result := doOrderRunExecResult(a, cityDir, cfg, vars, io.Discard, &stderr); result.code != 0 {
		t.Fatalf("doOrderRunExecResult (with vars) = %d; stderr: %s", result.code, stderr.String())
	}
	if got := readChildEnv(t, envDump)[overrideEnvProbeKey]; got != "/var/corpus" {
		t.Fatalf("with a dispatch var, child env[%s] = %q, want the dispatch-time value %q -- [order.env] must overlay ahead only of dispatch vars",
			overrideEnvProbeKey, got, "/var/corpus")
	}
}

// TestOverrideEnvValueShapesSurviveIntact covers the value shapes most likely
// to be mangled between TOML, the env map, and `sh -c`: whitespace, quotes, an
// embedded '=', and the empty string. An empty VALUE is a real assignment; an
// empty override MAP is a no-op (applyOverride guards on len(ov.Env) > 0).
func TestOverrideEnvValueShapesSurviveIntact(t *testing.T) {
	want := map[string]string{
		"PROBE_SPACES": "two words",
		"PROBE_QUOTES": `he said "hi"`,
		"PROBE_EQUALS": "a=b=c",
		"PROBE_EMPTY":  "",
	}
	cityDir, cfg, envDump := newOrderOverrideEnvCity(t, want)
	a := resolveProbeOrder(t, cityDir, cfg)

	var stderr bytes.Buffer
	if result := doOrderRunExecResult(a, cityDir, cfg, nil, io.Discard, &stderr); result.code != 0 {
		t.Fatalf("doOrderRunExecResult = %d; stderr: %s", result.code, stderr.String())
	}

	childEnv := readChildEnv(t, envDump)
	for key, wantValue := range want {
		got, present := childEnv[key]
		if !present {
			t.Fatalf("child env is missing override key %s entirely", key)
		}
		if got != wantValue {
			t.Fatalf("child env[%s] = %q, want %q", key, got, wantValue)
		}
	}
}

// TestEmptyOverrideEnvLeavesOrderEnvIntact pins the no-op case: an override
// entry carrying no env keys must not clear or replace the order's own
// [order.env].
func TestEmptyOverrideEnvLeavesOrderEnvIntact(t *testing.T) {
	cityDir, cfg, envDump := newOrderOverrideEnvCity(t, map[string]string{})
	a := resolveProbeOrder(t, cityDir, cfg)

	var stderr bytes.Buffer
	if result := doOrderRunExecResult(a, cityDir, cfg, nil, io.Discard, &stderr); result.code != 0 {
		t.Fatalf("doOrderRunExecResult = %d; stderr: %s", result.code, stderr.String())
	}

	childEnv := readChildEnv(t, envDump)
	if got := childEnv[overrideEnvProbeKey]; got != "/pack/default" {
		t.Fatalf("child env[%s] = %q, want the order's own %q -- an empty override env must be a no-op",
			overrideEnvProbeKey, got, "/pack/default")
	}
}

// TestOrderExecEnvIsCityScopedAndDropsSensitiveAmbient characterizes what the
// child does and does not inherit, so "the override arrived" is not confused
// with "everything arrived".
//
// It records the CURRENT contract truthfully: shellExecRunner merges the
// controller's own environ under the order env (mergeOrderExecEnv), so benign
// ambient vars ARE inherited; execenv.FilterInherited strips only keys whose
// name marks them sensitive. Controller-owned scope keys are set from the
// resolved target, not inherited.
func TestOrderExecEnvIsCityScopedAndDropsSensitiveAmbient(t *testing.T) {
	t.Setenv("PROBE_AMBIENT_BENIGN", "inherited")
	t.Setenv("PROBE_AMBIENT_TOKEN", "must-not-leak")

	cityDir, cfg, envDump := newOrderOverrideEnvCity(t, map[string]string{
		overrideEnvProbeKey: "/override/corpus",
	})
	a := resolveProbeOrder(t, cityDir, cfg)

	var stderr bytes.Buffer
	if result := doOrderRunExecResult(a, cityDir, cfg, nil, io.Discard, &stderr); result.code != 0 {
		t.Fatalf("doOrderRunExecResult = %d; stderr: %s", result.code, stderr.String())
	}
	childEnv := readChildEnv(t, envDump)

	if _, leaked := childEnv["PROBE_AMBIENT_TOKEN"]; leaked {
		t.Fatal("child env carries PROBE_AMBIENT_TOKEN -- a sensitive-marked ambient key leaked into an exec order")
	}
	if got := childEnv["PROBE_AMBIENT_BENIGN"]; got != "inherited" {
		t.Fatalf("child env[PROBE_AMBIENT_BENIGN] = %q, want %q -- benign ambient inheritance is the current contract; if this changed deliberately, update this test with the reason",
			got, "inherited")
	}

	// A city-scoped order resolves to the city scope, and the controller-owned
	// scope keys are authored from the target rather than inherited.
	if got := childEnv["GC_STORE_SCOPE"]; got != "city" {
		t.Fatalf("child env[GC_STORE_SCOPE] = %q, want %q for a city-scoped order", got, "city")
	}
	if got := childEnv["GC_RIG"]; got != "" {
		t.Fatalf("child env[GC_RIG] = %q, want empty for a city-scoped order", got)
	}
	if got := childEnv["GC_STORE_ROOT"]; got != cityDir {
		t.Fatalf("child env[GC_STORE_ROOT] = %q, want the city root %q", got, cityDir)
	}
}

// TestOverrideEnvCannotSetReservedControllerKey pins that an override reaching
// a.Env is held to the same reserved-key guard as a hand-authored [order.env]:
// the override path routes through the same validateOrderExecEnvOverrides, so
// a config override is not a way around the guard.
func TestOverrideEnvCannotSetReservedControllerKey(t *testing.T) {
	cityDir, cfg, _ := newOrderOverrideEnvCity(t, map[string]string{"GC_CITY": "hijacked"})

	// ScanAll validates with validateOrderExecEnvOverrides, so the order is
	// dropped at discovery rather than dispatched with a hijacked env.
	var stderr bytes.Buffer
	aa, code := loadAllOrders(cityDir, cfg, &stderr, "gc order run")
	if code != 0 {
		t.Fatalf("loadAllOrders = %d; stderr: %s", code, stderr.String())
	}
	if _, found := findOrder(aa, "probe", ""); found {
		t.Fatal("an order whose override env sets the reserved key GC_CITY was still discovered as runnable; the reserved-key guard does not cover the override path")
	}
	if !strings.Contains(stderr.String(), "GC_CITY") {
		t.Fatalf("stderr does not name the rejected reserved key, so the drop is silent:\n%s", stderr.String())
	}
}

// newOrderOverrideEnvCity builds a city whose single city-scoped exec order
// dumps its own environment to a file, plus a config carrying overrideEnv as a
// [[orders.overrides]] env block for that order. It returns the city root, the
// config, and the path the child writes its environment to.
func newOrderOverrideEnvCity(t *testing.T, overrideEnv map[string]string) (string, *config.City, string) {
	t.Helper()

	cityDir := t.TempDir()
	formulasDir := filepath.Join(cityDir, "formulas")
	if err := os.MkdirAll(formulasDir, 0o755); err != nil {
		t.Fatalf("mkdir formulas: %v", err)
	}
	ordersDir := filepath.Join(cityDir, "orders")
	if err := os.MkdirAll(ordersDir, 0o755); err != nil {
		t.Fatalf("mkdir orders: %v", err)
	}

	// The dump lands outside the city dir so nothing in the city scan can see
	// it. printenv is addressed absolutely so the assertion never depends on
	// the child's PATH -- PATH itself is part of what is under test.
	envDump := filepath.Join(t.TempDir(), "child-env")
	order := `[order]
exec = "/usr/bin/printenv > '` + envDump + `'"
trigger = "cooldown"
interval = "1m"

[order.env]
` + overrideEnvProbeKey + ` = "/pack/default"
ORDER_OWN_KEY = "from-order"
`
	if err := os.WriteFile(filepath.Join(ordersDir, "probe.toml"), []byte(order), 0o644); err != nil {
		t.Fatalf("write probe order: %v", err)
	}

	cfg := &config.City{
		Workspace:     config.Workspace{Name: "test-city"},
		FormulaLayers: config.FormulaLayers{City: []string{formulasDir}},
		Orders: config.OrdersConfig{
			Overrides: []config.OrderOverride{{Name: "probe", Env: overrideEnv}},
		},
	}
	return cityDir, cfg, envDump
}

// resolveProbeOrder returns the probe order exactly as the manual
// `gc order run` path obtains it: through loadAllOrders (orderdiscovery.ScanAll,
// the sole applier of overrides) and findOrder.
func resolveProbeOrder(t *testing.T, cityDir string, cfg *config.City) orders.Order {
	t.Helper()

	var stderr bytes.Buffer
	aa, code := loadAllOrders(cityDir, cfg, &stderr, "gc order run")
	if code != 0 {
		t.Fatalf("loadAllOrders = %d; stderr: %s", code, stderr.String())
	}
	a, found := findOrder(aa, "probe", "")
	if !found {
		t.Fatalf("probe order not found in discovered set %#v; stderr: %s", aa, stderr.String())
	}
	return a
}

// requireProbeOrder picks the probe order out of an already-resolved set.
func requireProbeOrder(t *testing.T, aa []orders.Order) orders.Order {
	t.Helper()

	for _, a := range aa {
		if a.Name == "probe" {
			return a
		}
	}
	t.Fatalf("probe order absent from resolved order set %#v", aa)
	return orders.Order{}
}

// readChildEnv parses the `printenv` dump the spawned order wrote. Values may
// contain '=', so only the first separator splits. A multi-line value would be
// misparsed, which is why no test above asserts one.
func readChildEnv(t *testing.T, path string) map[string]string {
	t.Helper()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading child env dump %s: %v -- the order process did not run or could not write it", path, err)
	}
	env := make(map[string]string)
	for _, line := range strings.Split(strings.TrimSuffix(string(data), "\n"), "\n") {
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		env[key] = value
	}
	if len(env) == 0 {
		t.Fatalf("child env dump %s is empty; the assertions below would pass vacuously", path)
	}
	return env
}
