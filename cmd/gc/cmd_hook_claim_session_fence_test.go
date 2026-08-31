package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/session"
)

// writeFenceTestCity writes a minimal single-worker city and returns its dir.
func writeFenceTestCity(t *testing.T) string {
	t.Helper()
	cityDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityDir, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(`[workspace]
name = "test-city"

[[agent]]
name = "worker"
`), 0o644); err != nil {
		t.Fatal(err)
	}
	return cityDir
}

// newFenceSessionBead creates a session bead in the city store with the given
// runtime state and instance token, returning its id.
func newFenceSessionBead(t *testing.T, cityDir string, state session.State, instanceToken string) string {
	return newFenceSessionBeadWithMetadata(t, cityDir, state, instanceToken, nil)
}

func newFenceSessionBeadWithMetadata(t *testing.T, cityDir string, state session.State, instanceToken string, metadata map[string]string) string {
	t.Helper()
	store, err := openCityStoreAt(cityDir)
	if err != nil {
		t.Fatalf("openCityStoreAt: %v", err)
	}
	beadMetadata := map[string]string{
		"session_name":   "worker-1",
		"template":       "worker",
		"state":          string(state),
		"instance_token": instanceToken,
	}
	for key, value := range metadata {
		beadMetadata[key] = value
	}
	bead, err := store.Create(beads.Bead{
		Title:    "worker-1",
		Type:     session.BeadType,
		Labels:   []string{"gc:session", "agent:worker-1"},
		Metadata: beadMetadata,
	})
	if err != nil {
		t.Fatalf("create session bead: %v", err)
	}
	return bead.ID
}

// TestHookCommandClaimReadsCurrentSessionTrigger verifies that a pooled reviewer
// claims the trigger currently bound to its session bead. Its process environment
// is only startup-time state, so a warm rebind must not let an old trigger/store
// pair redirect the claim into the prior rig.
func TestHookCommandClaimReadsCurrentSessionTrigger(t *testing.T) {
	clearGCEnv(t)
	disableManagedDoltRecoveryForTest(t)
	t.Setenv("GC_BEADS", "file")
	cityDir := t.TempDir()
	rigA := filepath.Join(cityDir, "rig-a")
	rigB := filepath.Join(cityDir, "rig-b")
	fakeBin := t.TempDir()
	logPath := filepath.Join(t.TempDir(), "bd.log")
	for _, dir := range []string{filepath.Join(cityDir, ".gc"), rigA, rigB} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	cityTOML := fmt.Sprintf(`[workspace]
name = "test-city"

[[rigs]]
name = "old"
path = %q

[[rigs]]
name = "new"
path = %q

[[agent]]
name = "pr-reviewer"
scope = "city"
`, rigA, rigB)
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(cityTOML), 0o644); err != nil {
		t.Fatal(err)
	}
	triggerStore, err := openStoreAtForCity(rigB, cityDir)
	if err != nil {
		t.Fatalf("open rig-b trigger store: %v", err)
	}
	trigger, err := triggerStore.Create(beads.Bead{
		Title:    "current trigger",
		Type:     "task",
		Metadata: map[string]string{"gc.routed_to": "pr-reviewer"},
	})
	if err != nil {
		t.Fatalf("create rig-b trigger: %v", err)
	}
	sessionID := newFenceSessionBeadWithMetadata(t, cityDir, session.StateActive, "current-token", map[string]string{
		"template":                              "pr-reviewer",
		beadmeta.TriggerBeadIDMetadataKey:       trigger.ID,
		beadmeta.TriggerBeadStoreRefMetadataKey: "rig:new",
	})
	store, err := openCityStoreAt(cityDir)
	if err != nil {
		t.Fatalf("open city store: %v", err)
	}
	cfg, err := loadCityConfig(cityDir, io.Discard)
	if err != nil {
		t.Fatalf("load city config: %v", err)
	}
	resolveRigPaths(cityDir, cfg.Rigs)
	stored, err := cliSessionFrontDoor(store, cfg, cityDir).Get(sessionID)
	if err != nil {
		t.Fatalf("load current session: %v", err)
	}
	if stored.TriggerBeadID != trigger.ID || stored.TriggerBeadStoreRef != "rig:new" {
		t.Fatalf("stored trigger = (%q, %q), want (%q, rig:new)", stored.TriggerBeadID, stored.TriggerBeadStoreRef, trigger.ID)
	}
	fakeBD := filepath.Join(fakeBin, "bd")
	script := fmt.Sprintf(`#!/bin/sh
printf 'pwd=%%s args=%%s\n' "$(pwd)" "$*" >> %q
case "$*" in
  *"show --json %s"*)
    printf '[{"id":%q,"status":"open","metadata":{"gc.routed_to":"pr-reviewer"}}]'
    ;;
  *"update %s --claim --json"*)
    printf '[{"id":%q,"status":"in_progress","assignee":"reviewer-1","metadata":{"gc.routed_to":"pr-reviewer"}}]'
    ;;
  *) printf '[]' ;;
esac
`, logPath, trigger.ID, trigger.ID, trigger.ID, trigger.ID)
	if err := os.WriteFile(fakeBD, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("GC_CITY", cityDir)
	t.Setenv("GC_TEMPLATE", "pr-reviewer")
	t.Setenv("GC_ALIAS", "")
	t.Setenv("GC_AGENT", "")
	t.Setenv("GC_SESSION_NAME", "reviewer-1")
	t.Setenv("GC_SESSION_ID", sessionID)
	t.Setenv("GC_SESSION_ORIGIN", "pool")
	t.Setenv("GC_INSTANCE_TOKEN", "current-token")
	// These are the stale values inherited when this warm slot first started.
	t.Setenv("GC_TRIGGER_WORK_BEAD_ID", "old-trigger")
	t.Setenv("GC_TRIGGER_WORK_STORE_REF", "rig:old")
	originalRunner := hookClaimCommandRunnerWithEnvContext
	t.Cleanup(func() { hookClaimCommandRunnerWithEnvContext = originalRunner })
	hookClaimCommandRunnerWithEnvContext = func(_ context.Context, env map[string]string) beads.CommandRunner {
		return func(dir, name string, args ...string) ([]byte, error) {
			if dir != rigB {
				t.Fatalf("trigger store dir = %q, want %q", dir, rigB)
			}
			if name != "bd" {
				t.Fatalf("trigger command name = %q, want bd", name)
			}
			switch {
			case len(args) == 3 && args[0] == "show" && args[1] == "--json" && args[2] == trigger.ID:
				bead, err := triggerStore.Get(trigger.ID)
				if err != nil {
					return nil, err
				}
				return json.Marshal([]beads.Bead{bead})
			case len(args) == 4 && args[0] == "update" && args[1] == trigger.ID && args[2] == "--claim" && args[3] == "--json":
				actor := env["BEADS_ACTOR"]
				if actor != "reviewer-1" {
					t.Fatalf("BEADS_ACTOR = %q, want reviewer-1", actor)
				}
				status := "in_progress"
				if err := triggerStore.Update(trigger.ID, beads.UpdateOpts{Status: &status, Assignee: &actor}); err != nil {
					return nil, err
				}
				bead, err := triggerStore.Get(trigger.ID)
				if err != nil {
					return nil, err
				}
				return json.Marshal([]beads.Bead{bead})
			case len(args) == 7 && args[0] == "update" && args[1] == "--json" && args[2] == trigger.ID &&
				args[3] == "--set-metadata" && args[4] == "gc.session_id="+sessionID &&
				args[5] == "--set-metadata" && args[6] == "gc.session_name=reviewer-1":
				if err := triggerStore.Update(trigger.ID, beads.UpdateOpts{Metadata: map[string]string{
					"gc.session_id":   sessionID,
					"gc.session_name": "reviewer-1",
				}}); err != nil {
					return nil, err
				}
				bead, err := triggerStore.Get(trigger.ID)
				if err != nil {
					return nil, err
				}
				return json.Marshal([]beads.Bead{bead})
			default:
				t.Fatalf("unexpected trigger command: %v", args)
				return nil, nil
			}
		}
	}
	triggerID, triggerRef, _, handled := currentHookClaimTrigger(cityDir, cfg, sessionID, hookCommandOptions{Claim: true, JSON: true}, io.Discard, io.Discard)
	if handled || triggerID != trigger.ID || triggerRef != "rig:new" {
		t.Fatalf("current session trigger = (%q, %q, handled=%t), want (%q, rig:new, false)", triggerID, triggerRef, handled, trigger.ID)
	}

	var stdout, stderr bytes.Buffer
	code := cmdHookWithOptions(nil, hookCommandOptions{Claim: true, DrainAck: true, JSON: true}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("cmdHookWithOptions(--claim) = %d, want 0; stdout=%q stderr=%s bd log=%s", code, stdout.String(), stderr.String(), readFileString(t, logPath))
	}
	var result hookClaimJSONResult
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, stdout.String())
	}
	if result.BeadID != trigger.ID || result.Reason != "claimed_trigger" {
		t.Fatalf("claim result = %+v, want current %s", result, trigger.ID)
	}
	if _, err := os.Stat(logPath); !os.IsNotExist(err) {
		t.Fatalf("generic discovery ran after the terminal trigger claim; stat error = %v", err)
	}
}

// TestHookCommandClaimQueryTargetUsesConfiguredWorkQuery proves that a tokened
// warm session can select a template's strict query while retaining its own
// concrete runtime identity as claim owner and bypassing a stale trigger.
func TestHookCommandClaimQueryTargetUsesConfiguredWorkQuery(t *testing.T) {
	clearGCEnv(t)
	disableManagedDoltRecoveryForTest(t)
	t.Setenv("GC_BEADS", "file")
	cityDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityDir, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	queryMarker := filepath.Join(t.TempDir(), "work-query-ran")
	wrongQueryMarker := filepath.Join(t.TempDir(), "wrong-work-query-ran")
	cityTOML := fmt.Sprintf(`[workspace]
name = "test-city"

[[agent]]
name = "reviewer"
work_query = "touch %s; printf '[{\"id\":\"review-work\",\"status\":\"open\",\"metadata\":{\"gc.routed_to\":\"reviewer\"}}]'"

[[agent]]
name = "other"
work_query = "touch %s; printf '[]'"
`, queryMarker, wrongQueryMarker)
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(cityTOML), 0o644); err != nil {
		t.Fatal(err)
	}
	loaded, err := loadCityConfig(cityDir, io.Discard)
	if err != nil {
		t.Fatalf("load city config: %v", err)
	}
	if got := loaded.Agents[0].WorkQuery; !strings.Contains(got, "review-work") {
		t.Fatalf("work_query = %q, want review-work", got)
	}
	sessionID := newFenceSessionBeadWithMetadata(t, cityDir, session.StateActive, "current-token", map[string]string{
		"template":                              "other",
		beadmeta.TriggerBeadIDMetadataKey:       "authoritative-trigger",
		beadmeta.TriggerBeadStoreRefMetadataKey: "city:test-city",
	})

	fakeBin := t.TempDir()
	triggerMarker := filepath.Join(t.TempDir(), "trigger-claimed")
	reviewMarker := filepath.Join(t.TempDir(), "review-claimed")
	bdLog := filepath.Join(t.TempDir(), "bd.log")
	script := fmt.Sprintf(`#!/bin/sh
printf '%%s\n' "$*" >> %q
case "$*" in
  *"show --json authoritative-trigger"*) printf '[{"id":"authoritative-trigger","status":"open","metadata":{"gc.routed_to":"reviewer"}}]' ;;
  *"update authoritative-trigger --claim --json"*) : > %q; printf '[{"id":"authoritative-trigger","status":"in_progress","assignee":"reviewer-1","metadata":{"gc.routed_to":"reviewer"}}]' ;;
  *"show --json review-work"*) if [ -f %q ]; then printf '[{"id":"review-work","status":"in_progress","assignee":"reviewer-1","metadata":{"gc.routed_to":"reviewer"}}]'; else printf '[{"id":"review-work","status":"open","metadata":{"gc.routed_to":"reviewer"}}]'; fi ;;
  *"update review-work --claim --json"*) : > %q; printf '[{"id":"review-work","status":"in_progress","assignee":"reviewer-1","metadata":{"gc.routed_to":"reviewer"}}]' ;;
  *) printf '[]' ;;
esac
`, bdLog, triggerMarker, reviewMarker, reviewMarker)
	if err := os.WriteFile(filepath.Join(fakeBin, "bd"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"))
	setFenceClaimEnv(t, cityDir, sessionID, "current-token")
	t.Setenv("GC_TEMPLATE", "other")
	t.Setenv("GC_ALIAS", "reviewer-1")
	t.Setenv("GC_SESSION_NAME", "reviewer-1")
	t.Setenv("GC_TRIGGER_WORK_BEAD_ID", "stale-startup-trigger")
	t.Setenv("GC_TRIGGER_WORK_STORE_REF", "city:test-city")

	var stdout, stderr bytes.Buffer
	code := run([]string{"--city", cityDir, "hook", "--query-target", "reviewer", "--claim", "--skip-trigger", "--json"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("gc hook --query-target reviewer --claim --skip-trigger = %d, want 0; stdout=%q stderr=%s bd log=%s", code, stdout.String(), stderr.String(), readFileString(t, bdLog))
	}
	var result hookClaimJSONResult
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, stdout.String())
	}
	if result.BeadID != "review-work" || result.Reason != "claimed" {
		t.Fatalf("result = %+v, want configured-query claim of review-work", result)
	}
	if _, err := os.Stat(queryMarker); err != nil {
		t.Fatalf("configured work_query did not run: %v; bd log=%s", err, readFileString(t, bdLog))
	}
	if _, err := os.Stat(wrongQueryMarker); !os.IsNotExist(err) {
		t.Fatalf("runtime template query ran instead of --query-target reviewer; stat error = %v", err)
	}
	if _, err := os.Stat(triggerMarker); !os.IsNotExist(err) {
		t.Fatalf("authoritative trigger was claimed despite --skip-trigger; stat error = %v", err)
	}
}

func TestHookCommandClaimNamedSessionClaimsLegacyGraphV2RoutedStep(t *testing.T) {
	clearGCEnv(t)
	disableManagedDoltRecoveryForTest(t)
	t.Setenv("GC_BEADS", "file")
	cityDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityDir, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(`[workspace]
name = "test-city"

[[agent]]
name = "worker"
`), 0o644); err != nil {
		t.Fatal(err)
	}

	fakeBin := t.TempDir()
	bdLog := filepath.Join(t.TempDir(), "bd.log")
	script := fmt.Sprintf(`#!/bin/sh
printf '%%s\n' "$*" >> %q
case "$*" in
  *"ready"*"gc.formula_contract=graph.v2"*) printf '[]' ;;
  *"show --json graph-root"*) printf '[{"id":"graph-root","metadata":{"gc.formula_contract":"graph.v2"}}]' ;;
  *"ready"*"gc.routed_to=worker"*) printf '[{"id":"graph-initial-step","status":"open","metadata":{"gc.routed_to":"worker","gc.root_bead_id":"graph-root","gc.step_ref":"workflow.load-context"}}]' ;;
  *"update graph-initial-step --claim --json"*) printf '[{"id":"graph-initial-step","status":"in_progress","assignee":"named-worker","metadata":{"gc.routed_to":"worker","gc.root_bead_id":"graph-root","gc.step_ref":"workflow.load-context"}}]' ;;
  *"show --json graph-initial-step"*) printf '[{"id":"graph-initial-step","status":"in_progress","assignee":"named-worker","metadata":{"gc.routed_to":"worker","gc.root_bead_id":"graph-root","gc.step_ref":"workflow.load-context"}}]' ;;
  *) printf '[]' ;;
esac
`, bdLog)
	if err := os.WriteFile(filepath.Join(fakeBin, "bd"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("GC_CITY", cityDir)
	t.Setenv("GC_TEMPLATE", "worker")
	t.Setenv("GC_ALIAS", "worker")
	t.Setenv("GC_SESSION_ID", "named-session")
	t.Setenv("GC_SESSION_NAME", "named-worker")
	t.Setenv("GC_SESSION_ORIGIN", "named")

	var stdout, stderr bytes.Buffer
	code := run([]string{"--city", cityDir, "hook", "--query-target", "worker", "--claim", "--skip-trigger", "--json"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("gc hook --claim = %d, want 0; stdout=%q stderr=%s bd log=%s", code, stdout.String(), stderr.String(), readFileString(t, bdLog))
	}
	var result hookClaimJSONResult
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, stdout.String())
	}
	if result.BeadID != "graph-initial-step" || result.Reason != "claimed" {
		t.Fatalf("result = %+v, want named claim of graph-initial-step", result)
	}
	if !strings.Contains(readFileString(t, bdLog), "update graph-initial-step --claim --json") {
		t.Fatalf("named session did not claim graph step; bd log=%s", readFileString(t, bdLog))
	}
}

// TestHookCommandClaimSkipTriggerIgnoresTokenlessEnvironmentTrigger preserves
// the same query-only behavior for legacy runtimes, which obtain trigger data
// directly from GC_TRIGGER_WORK_* rather than a session projection.
func TestHookCommandClaimSkipTriggerIgnoresTokenlessEnvironmentTrigger(t *testing.T) {
	clearGCEnv(t)
	disableManagedDoltRecoveryForTest(t)
	cityDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityDir, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	queryMarker := filepath.Join(t.TempDir(), "work-query-ran")
	cityTOML := fmt.Sprintf(`[workspace]
name = "test-city"

[[agent]]
name = "worker"
work_query = "touch %s; printf '[{\"id\":\"query-work\",\"status\":\"open\",\"metadata\":{\"gc.routed_to\":\"worker\"}}]'"
`, queryMarker)
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(cityTOML), 0o644); err != nil {
		t.Fatal(err)
	}
	fakeBin := t.TempDir()
	triggerMarker := filepath.Join(t.TempDir(), "trigger-claimed")
	queryClaimMarker := filepath.Join(t.TempDir(), "query-claimed")
	script := fmt.Sprintf(`#!/bin/sh
case "$*" in
  *"show --json env-trigger"*) printf '[{"id":"env-trigger","status":"open","metadata":{"gc.routed_to":"worker"}}]' ;;
  *"update env-trigger --claim --json"*) : > %q; printf '[{"id":"env-trigger","status":"in_progress","assignee":"worker-1","metadata":{"gc.routed_to":"worker"}}]' ;;
  *"show --json query-work"*) if [ -f %q ]; then printf '[{"id":"query-work","status":"in_progress","assignee":"worker-1","metadata":{"gc.routed_to":"worker"}}]'; else printf '[{"id":"query-work","status":"open","metadata":{"gc.routed_to":"worker"}}]'; fi ;;
  *"update query-work --claim --json"*) : > %q; printf '[{"id":"query-work","status":"in_progress","assignee":"worker-1","metadata":{"gc.routed_to":"worker"}}]' ;;
  *) printf '[]' ;;
esac
`, triggerMarker, queryClaimMarker, queryClaimMarker)
	if err := os.WriteFile(filepath.Join(fakeBin, "bd"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("GC_CITY", cityDir)
	t.Setenv("GC_TEMPLATE", "worker")
	t.Setenv("GC_ALIAS", "worker-1")
	t.Setenv("GC_SESSION_NAME", "worker-1")
	t.Setenv("GC_SESSION_ID", "legacy-session")
	t.Setenv("GC_TRIGGER_WORK_BEAD_ID", "env-trigger")
	t.Setenv("GC_TRIGGER_WORK_STORE_REF", "city:test-city")

	var stdout, stderr bytes.Buffer
	code := run([]string{"--city", cityDir, "hook", "--claim", "--skip-trigger", "--json"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("gc hook --claim --skip-trigger = %d, want 0; stdout=%q stderr=%s", code, stdout.String(), stderr.String())
	}
	var result hookClaimJSONResult
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, stdout.String())
	}
	if result.BeadID != "query-work" || result.Reason != "claimed" {
		t.Fatalf("result = %+v, want configured-query claim of query-work", result)
	}
	if _, err := os.Stat(queryMarker); err != nil {
		t.Fatalf("configured work_query did not run: %v", err)
	}
	if _, err := os.Stat(triggerMarker); !os.IsNotExist(err) {
		t.Fatalf("environment trigger was claimed despite --skip-trigger; stat error = %v", err)
	}
}

// installFenceWorkQueryProbe puts a fake `bd` on PATH that records each
// invocation by touching the returned marker path and prints an empty
// work-query result, so a test can assert whether the claim fence reached the
// work query. Call it AFTER creating any session beads so bead setup uses the
// real store, not the probe.
func installFenceWorkQueryProbe(t *testing.T) string {
	t.Helper()
	fakeBin := t.TempDir()
	queryMarker := filepath.Join(t.TempDir(), "query-ran")
	fakeBD := filepath.Join(fakeBin, "bd")
	if err := os.WriteFile(fakeBD, []byte("#!/bin/sh\ntouch \"$QUERY_MARKER\"\nprintf '[]'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("QUERY_MARKER", queryMarker)
	return queryMarker
}

// setFenceClaimEnv points cmdHookWithOptions at the given session identity.
func setFenceClaimEnv(t *testing.T, cityDir, sessionID, instanceToken string) {
	t.Helper()
	t.Setenv("GC_CITY", cityDir)
	t.Setenv("GC_TEMPLATE", "worker")
	t.Setenv("GC_ALIAS", "worker-1")
	t.Setenv("GC_SESSION_ID", sessionID)
	t.Setenv("GC_SESSION_NAME", "worker-1")
	t.Setenv("GC_SESSION_ORIGIN", "ephemeral")
	t.Setenv("GC_INSTANCE_TOKEN", instanceToken)
}

// TestHookCommandClaimStaleSessionDrainsBeforeWorkQuery proves a definitively
// stale session (here failed-create) is refused before the work query AND that
// the refusal now honors the gc hook --claim result contract: a --json caller
// gets a structured terminal drain record (action=drain, reason=stale_session)
// instead of empty stdout, so a startup wrapper can distinguish a definitive
// stale-session refusal from a transient command failure and stop retrying.
func TestHookCommandClaimStaleSessionDrainsBeforeWorkQuery(t *testing.T) {
	clearGCEnv(t)
	disableManagedDoltRecoveryForTest(t)
	t.Setenv("GC_BEADS", "file")
	cityDir := writeFenceTestCity(t)
	sessionID := newFenceSessionBead(t, cityDir, session.StateFailedCreate, "failed-token")
	queryMarker := installFenceWorkQueryProbe(t)
	setFenceClaimEnv(t, cityDir, sessionID, "failed-token")

	var stdout, stderr bytes.Buffer
	code := cmdHookWithOptions(nil, hookCommandOptions{Claim: true, JSON: true}, &stdout, &stderr)

	// Without --drain-ack the refusal is still terminal (exit 1) but now carries a
	// schema-backed drain record instead of empty stdout.
	if code != 1 {
		t.Fatalf("code = %d, want 1; stdout=%q stderr=%s", code, stdout.String(), stderr.String())
	}
	var result hookClaimJSONResult
	if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &result); err != nil {
		t.Fatalf("stdout is not a JSON drain result: %v\n%s", err, stdout.String())
	}
	if result.Action != "drain" || result.Reason != hookClaimReasonStaleSession {
		t.Fatalf("result = %+v, want action=drain reason=stale_session", result)
	}
	if result.DrainAcknowledged {
		t.Fatalf("result.DrainAcknowledged = true without --drain-ack")
	}
	if !strings.Contains(stderr.String(), "refusing stale session") ||
		!strings.Contains(stderr.String(), "failed-create") {
		t.Fatalf("stderr = %q, want failed-session refusal naming the state", stderr.String())
	}
	if _, err := os.Stat(queryMarker); !os.IsNotExist(err) {
		t.Fatalf("work query ran for stale session; stat error = %v", err)
	}
}

// TestHookCommandClaimEligibleStatesReachWorkQuery proves the fence lets the
// states a live worker legitimately claims in — active/awake plus the in-startup
// states creating/start-pending the deferred-start path passes through before
// its async active commit lands — through to the work query, rather than
// refusing a healthy first claim as stale.
func TestHookCommandClaimEligibleStatesReachWorkQuery(t *testing.T) {
	for _, state := range []session.State{
		session.StateActive,
		session.StateAwake,
		session.StateCreating,
		session.StateStartPending,
	} {
		t.Run(string(state), func(t *testing.T) {
			clearGCEnv(t)
			disableManagedDoltRecoveryForTest(t)
			t.Setenv("GC_BEADS", "file")
			cityDir := writeFenceTestCity(t)
			sessionID := newFenceSessionBead(t, cityDir, state, "current-token")
			queryMarker := installFenceWorkQueryProbe(t)
			setFenceClaimEnv(t, cityDir, sessionID, "current-token")

			var stdout, stderr bytes.Buffer
			code := cmdHookWithOptions(nil, hookCommandOptions{Claim: true, JSON: true}, &stdout, &stderr)

			// The probe bd returns no work, so the claim drains with no_work; the
			// point is that the fence let an eligible session THROUGH to the work
			// query (marker created) instead of refusing it as stale.
			if _, err := os.Stat(queryMarker); err != nil {
				t.Fatalf("work query did not run for eligible %s session: %v; stderr=%s", state, err, stderr.String())
			}
			if strings.Contains(stderr.String(), "refusing stale session") {
				t.Fatalf("eligible %s session was refused as stale: %s", state, stderr.String())
			}
			if code != 1 {
				t.Fatalf("code = %d, want 1 (JSON no-work drain without --drain-ack); stderr=%s", code, stderr.String())
			}
			var result hookClaimJSONResult
			if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &result); err != nil {
				t.Fatalf("stdout is not a JSON result: %v\n%s", err, stdout.String())
			}
			if result.Action != "drain" || result.Reason != hookClaimReasonNoWork {
				t.Fatalf("result = %+v, want action=drain reason=no_work (probe returns no work)", result)
			}
		})
	}
}

// TestHookCommandClaimEmptyLegacyStateReachesWorkQuery proves a pre-metadata legacy
// session bead — one persisted with an empty state during upgrade — reaches the work
// query instead of being refused as stale. With Closed=false and a matching instance
// token the runtime is the live current incarnation, and the session lifecycle
// canonicalizes empty state to active, so draining it here would starve a healthy
// upgraded legacy worker of its routed work.
func TestHookCommandClaimEmptyLegacyStateReachesWorkQuery(t *testing.T) {
	clearGCEnv(t)
	disableManagedDoltRecoveryForTest(t)
	t.Setenv("GC_BEADS", "file")
	cityDir := writeFenceTestCity(t)
	sessionID := newFenceSessionBead(t, cityDir, session.StateNone, "current-token")
	queryMarker := installFenceWorkQueryProbe(t)
	setFenceClaimEnv(t, cityDir, sessionID, "current-token")

	var stdout, stderr bytes.Buffer
	code := cmdHookWithOptions(nil, hookCommandOptions{Claim: true, JSON: true}, &stdout, &stderr)

	if _, err := os.Stat(queryMarker); err != nil {
		t.Fatalf("work query did not run for empty-legacy-state session: %v; stderr=%s", err, stderr.String())
	}
	if strings.Contains(stderr.String(), "refusing stale session") {
		t.Fatalf("empty-legacy-state session was refused as stale: %s", stderr.String())
	}
	if code != 1 {
		t.Fatalf("code = %d, want 1 (JSON no-work drain without --drain-ack); stderr=%s", code, stderr.String())
	}
	var result hookClaimJSONResult
	if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &result); err != nil {
		t.Fatalf("stdout is not a JSON result: %v\n%s", err, stdout.String())
	}
	if result.Action != "drain" || result.Reason != hookClaimReasonNoWork {
		t.Fatalf("result = %+v, want action=drain reason=no_work (probe returns no work)", result)
	}
}

// TestHookCommandClaimTokenlessRuntimeSkipsFence proves the fence's empty-token
// guard keeps a token-less (legacy/unmanaged) runtime out of the identity check:
// with GC_INSTANCE_TOKEN unset the fence is skipped entirely and the work query
// runs, even when the session bead is in a state the fence would otherwise refuse
// as stale. This pins the deliberate compatibility escape hatch so a future
// refactor cannot silently start fencing — and refusing — healthy legacy workers.
func TestHookCommandClaimTokenlessRuntimeSkipsFence(t *testing.T) {
	clearGCEnv(t)
	disableManagedDoltRecoveryForTest(t)
	t.Setenv("GC_BEADS", "file")
	cityDir := writeFenceTestCity(t)
	// A failed-create bead would drain stale if the fence ran; the point is that a
	// token-less runtime never reaches that classification.
	sessionID := newFenceSessionBead(t, cityDir, session.StateFailedCreate, "legacy-token")
	queryMarker := installFenceWorkQueryProbe(t)
	setFenceClaimEnv(t, cityDir, sessionID, "")

	var stdout, stderr bytes.Buffer
	code := cmdHookWithOptions(nil, hookCommandOptions{Claim: true, JSON: true}, &stdout, &stderr)

	if _, err := os.Stat(queryMarker); err != nil {
		t.Fatalf("work query did not run for token-less runtime (fence should be skipped): %v; stderr=%s", err, stderr.String())
	}
	if strings.Contains(stderr.String(), "refusing stale session") {
		t.Fatalf("token-less runtime was refused by the fence: %s", stderr.String())
	}
	if code != 1 {
		t.Fatalf("code = %d, want 1 (JSON no-work drain without --drain-ack); stderr=%s", code, stderr.String())
	}
	var result hookClaimJSONResult
	if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &result); err != nil {
		t.Fatalf("stdout is not a JSON result: %v\n%s", err, stdout.String())
	}
	if result.Action != "drain" || result.Reason != hookClaimReasonNoWork {
		t.Fatalf("result = %+v, want action=drain reason=no_work (probe returns no work)", result)
	}
}

// TestHookCommandClaimSkipTriggerStillFencesStaleSession proves query-only
// claim mode bypasses priority selection only; an obsolete instance must still
// drain before it can run a configured query or attempt a CAS claim.
func TestHookCommandClaimSkipTriggerStillFencesStaleSession(t *testing.T) {
	clearGCEnv(t)
	disableManagedDoltRecoveryForTest(t)
	t.Setenv("GC_BEADS", "file")
	cityDir := writeFenceTestCity(t)
	sessionID := newFenceSessionBead(t, cityDir, session.StateFailedCreate, "failed-token")
	queryMarker := installFenceWorkQueryProbe(t)
	setFenceClaimEnv(t, cityDir, sessionID, "failed-token")

	var stdout, stderr bytes.Buffer
	code := cmdHookWithOptions(nil, hookCommandOptions{Claim: true, SkipTrigger: true, JSON: true}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("query-only stale claim code = %d, want 1; stdout=%q stderr=%s", code, stdout.String(), stderr.String())
	}
	if _, err := os.Stat(queryMarker); !os.IsNotExist(err) {
		t.Fatalf("work_query ran for stale query-only session; stat error = %v", err)
	}
	var result hookClaimJSONResult
	if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &result); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, stdout.String())
	}
	if result.Action != "drain" || result.Reason != hookClaimReasonStaleSession {
		t.Fatalf("result = %+v, want stale-session drain", result)
	}
}

// TestHookCommandClaimAbsentSessionBeadDrainsStale proves a runtime whose session
// bead is confirmed absent — GC_SESSION_ID names no bead in the store — is refused
// as stale before the work query, not failed open into the claim path. A vanished
// session bead is a definitive identity failure: the incarnation can no longer
// prove it is the current one, so it must drain (action=drain,
// reason=stale_session) and stop rather than adopt routed work ahead of the
// reconciler terminating it.
func TestHookCommandClaimAbsentSessionBeadDrainsStale(t *testing.T) {
	clearGCEnv(t)
	disableManagedDoltRecoveryForTest(t)
	t.Setenv("GC_BEADS", "file")
	cityDir := writeFenceTestCity(t)
	queryMarker := installFenceWorkQueryProbe(t)
	setFenceClaimEnv(t, cityDir, "worker-1-vanished", "any-token")

	var stdout, stderr bytes.Buffer
	code := cmdHookWithOptions(nil, hookCommandOptions{Claim: true, JSON: true}, &stdout, &stderr)

	if code != 1 {
		t.Fatalf("code = %d, want 1; stdout=%q stderr=%s", code, stdout.String(), stderr.String())
	}
	var result hookClaimJSONResult
	if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &result); err != nil {
		t.Fatalf("stdout is not a JSON drain result: %v\n%s", err, stdout.String())
	}
	if result.Action != "drain" || result.Reason != hookClaimReasonStaleSession {
		t.Fatalf("result = %+v, want action=drain reason=stale_session", result)
	}
	if !strings.Contains(stderr.String(), "refusing stale session") ||
		!strings.Contains(stderr.String(), "not found") {
		t.Fatalf("stderr = %q, want stale refusal naming the missing bead", stderr.String())
	}
	if _, err := os.Stat(queryMarker); !os.IsNotExist(err) {
		t.Fatalf("work query ran for a session with no bead; stat error = %v", err)
	}
}

// TestHookCommandClaimFailsOpenOnSessionStoreError proves a GENUINE session-store
// fault — here a corrupt/unreadable store file, so the fence's store open itself
// fails — is NOT mislabeled as a stale session: the fence fails open and lets the
// normal claim path run, which surfaces and escalates its own store errors. This
// is the counterpart to the absent-bead case above: a confirmed-missing bead
// drains stale, but an infrastructure fault must never refuse a possibly-healthy
// worker.
func TestHookCommandClaimFailsOpenOnSessionStoreError(t *testing.T) {
	clearGCEnv(t)
	disableManagedDoltRecoveryForTest(t)
	t.Setenv("GC_BEADS", "file")
	cityDir := writeFenceTestCity(t)
	queryMarker := installFenceWorkQueryProbe(t)
	// Corrupt the file store so openCityStoreAt fails to parse it: a genuine
	// store-open fault, distinct from an absent bead (a confirmed identity
	// failure that drains stale).
	if err := os.WriteFile(filepath.Join(cityDir, ".gc", "beads.json"), []byte("{ this is not valid json"), 0o644); err != nil {
		t.Fatal(err)
	}
	setFenceClaimEnv(t, cityDir, "worker-1", "any-token")

	var stdout, stderr bytes.Buffer
	code := cmdHookWithOptions(nil, hookCommandOptions{Claim: true, JSON: true}, &stdout, &stderr)

	if _, err := os.Stat(queryMarker); err != nil {
		t.Fatalf("fail-open did not reach the work query: %v; stderr=%s", err, stderr.String())
	}
	if strings.Contains(stderr.String(), "refusing stale session") {
		t.Fatalf("store fault was mislabeled as a stale session: %s", stderr.String())
	}
	if !strings.Contains(stderr.String(), "session fence unavailable") {
		t.Fatalf("stderr = %q, want fence-unavailable diagnostic", stderr.String())
	}
	if code != 1 {
		t.Fatalf("code = %d, want 1 (JSON no-work drain without --drain-ack)", code)
	}
}

// TestClassifyHookClaimSessionLookupError exercises the error taxonomy that
// decides whether a failed session lookup is a definitive identity failure
// (stale, drain) or a transient store fault (unavailable, fail open). The two
// confirmed-identity errors mirror the documented session.Store.Get contract: a
// confirmed-absent id wraps beads.ErrNotFound, a present-but-non-session id is
// session.ErrSessionNotFound.
func TestClassifyHookClaimSessionLookupError(t *testing.T) {
	cases := []struct {
		name    string
		err     error
		want    hookClaimSessionVerdict
		wantMsg string
	}{
		{
			name:    "confirmed absent bead is stale",
			err:     fmt.Errorf("loading session %q: %w", "s", beads.ErrNotFound),
			want:    hookClaimSessionStale,
			wantMsg: "not found",
		},
		{
			name:    "present but non-session bead is stale",
			err:     fmt.Errorf("%w: %s", session.ErrSessionNotFound, "s"),
			want:    hookClaimSessionStale,
			wantMsg: "non-session",
		},
		{
			name:    "genuine store read fault fails open",
			err:     fmt.Errorf("loading session %q: %w", "s", errors.New("dial tcp: connection refused")),
			want:    hookClaimSessionStoreUnavailable,
			wantMsg: "loading session bead",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			verdict, msg := classifyHookClaimSessionLookupError(tc.err)
			if verdict != tc.want {
				t.Fatalf("verdict = %d, want %d (msg=%q)", verdict, tc.want, msg)
			}
			if !strings.Contains(msg, tc.wantMsg) {
				t.Fatalf("msg = %q, want substring %q", msg, tc.wantMsg)
			}
		})
	}
}

// TestHookClaimSessionEligibility exercises the pure eligibility decision over a
// session Info snapshot for every branch of the fence.
func TestHookClaimSessionEligibility(t *testing.T) {
	const token = "current-token"
	cases := []struct {
		name    string
		info    session.Info
		token   string
		want    hookClaimSessionVerdict
		wantMsg string
	}{
		{
			name:    "closed",
			info:    session.Info{ID: "s", Closed: true, InstanceToken: token},
			token:   token,
			want:    hookClaimSessionStale,
			wantMsg: "closed",
		},
		{
			name:    "superseded token",
			info:    session.Info{ID: "s", MetadataState: string(session.StateActive), InstanceToken: "replacement-token"},
			token:   "stale-runtime-token",
			want:    hookClaimSessionStale,
			wantMsg: "token",
		},
		{
			name:    "empty stored token",
			info:    session.Info{ID: "s", MetadataState: string(session.StateActive), InstanceToken: ""},
			token:   token,
			want:    hookClaimSessionStale,
			wantMsg: "token",
		},
		{
			name:    "failed-create",
			info:    session.Info{ID: "s", MetadataState: string(session.StateFailedCreate), InstanceToken: token},
			token:   token,
			want:    hookClaimSessionStale,
			wantMsg: "failed-create",
		},
		{
			name:    "drained",
			info:    session.Info{ID: "s", MetadataState: string(session.StateDrained), InstanceToken: token},
			token:   token,
			want:    hookClaimSessionStale,
			wantMsg: "drained",
		},
		{
			name:  "active",
			info:  session.Info{ID: "s", MetadataState: string(session.StateActive), InstanceToken: token},
			token: token,
			want:  hookClaimSessionEligible,
		},
		{
			name:  "awake",
			info:  session.Info{ID: "s", MetadataState: string(session.StateAwake), InstanceToken: token},
			token: token,
			want:  hookClaimSessionEligible,
		},
		{
			name:  "creating",
			info:  session.Info{ID: "s", MetadataState: string(session.StateCreating), InstanceToken: token},
			token: token,
			want:  hookClaimSessionEligible,
		},
		{
			name:  "start-pending",
			info:  session.Info{ID: "s", MetadataState: string(session.StateStartPending), InstanceToken: token},
			token: token,
			want:  hookClaimSessionEligible,
		},
		{
			// A pre-metadata legacy bead mid-upgrade carries an empty MetadataState
			// (session.StateNone). With Closed=false and a matching instance token it
			// is the live current incarnation, and the session lifecycle canonicalizes
			// empty state to active, so the fence must admit it rather than drain a
			// healthy upgraded legacy worker before it claims its routed work.
			name:  "empty legacy state admitted as active",
			info:  session.Info{ID: "s", MetadataState: string(session.StateNone), InstanceToken: token},
			token: token,
			want:  hookClaimSessionEligible,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			verdict, reason := hookClaimSessionEligibility(tc.info, tc.token)
			if verdict != tc.want {
				t.Fatalf("verdict = %d, want %d (reason=%q)", verdict, tc.want, reason)
			}
			if tc.want == hookClaimSessionEligible && reason != "" {
				t.Fatalf("eligible verdict carried reason %q, want empty", reason)
			}
			if tc.wantMsg != "" && !strings.Contains(reason, tc.wantMsg) {
				t.Fatalf("reason = %q, want substring %q", reason, tc.wantMsg)
			}
		})
	}
}

// TestWriteHookClaimDrainStaleSessionWithDrainAck proves the stale-session drain
// honors --drain-ack: it runs the drain-ack, marks the record acknowledged, and
// exits 0 so a startup wrapper treats the refusal as a completed drain.
func TestWriteHookClaimDrainStaleSessionWithDrainAck(t *testing.T) {
	acked := false
	fakeAck := func(io.Writer) error { acked = true; return nil }

	var stdout, stderr bytes.Buffer
	code := writeHookClaimDrain(hookClaimReasonStaleSession, true, true, fakeAck, &stdout, &stderr)

	if code != 0 {
		t.Fatalf("code = %d, want 0 for an acknowledged drain; stderr=%s", code, stderr.String())
	}
	if !acked {
		t.Fatalf("drain-ack function was not called")
	}
	var result hookClaimJSONResult
	if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &result); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, stdout.String())
	}
	if result.Action != "drain" || result.Reason != hookClaimReasonStaleSession || !result.DrainAcknowledged {
		t.Fatalf("result = %+v, want drain/stale_session/acknowledged", result)
	}
}

// TestWriteHookClaimDrainDoesNotAckWhenNotRequested proves the drain path never
// runs drain-ack unless --drain-ack was requested, and returns the historical
// exit 1 for an unacknowledged drain.
func TestWriteHookClaimDrainDoesNotAckWhenNotRequested(t *testing.T) {
	fakeAck := func(io.Writer) error {
		t.Fatalf("drain-ack must not run without --drain-ack")
		return nil
	}
	var stdout, stderr bytes.Buffer
	code := writeHookClaimDrain(hookClaimReasonStaleSession, true, false, fakeAck, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("code = %d, want 1 when drain is not acknowledged", code)
	}
	var result hookClaimJSONResult
	if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &result); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, stdout.String())
	}
	if result.DrainAcknowledged {
		t.Fatalf("result.DrainAcknowledged = true without --drain-ack")
	}
}

// writeFenceTestCityAgents writes a minimal city with a caller-supplied agent
// block, so a test can exercise the claim fence against a config that does NOT
// contain the runtime's template, or contains it in a suspended state.
func writeFenceTestCityAgents(t *testing.T, agentsTOML string) string {
	t.Helper()
	cityDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityDir, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	body := "[workspace]\nname = \"test-city\"\n\n" + agentsTOML
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return cityDir
}

// TestHookCommandClaimStaleSessionMissingTemplateDrainsBeforeAgentResolution
// proves a stale runtime whose template was removed from config is refused as
// stale by the claim fence BEFORE the "agent not found in config" early return.
// The startup wrapper invokes gc hook --claim --json --drain-ack; before the
// fence was hoisted ahead of agent resolution, a stale session whose template
// had been dropped hit the bare `return 1` with "not found in config" and its
// wrapper retried that plain failure forever instead of seeing the terminal
// stale-session drain result and exiting. This pins the ordering so the fence
// always pre-empts the missing-template early return for a stale session.
func TestHookCommandClaimStaleSessionMissingTemplateDrainsBeforeAgentResolution(t *testing.T) {
	clearGCEnv(t)
	disableManagedDoltRecoveryForTest(t)
	t.Setenv("GC_BEADS", "file")
	// The city has no "worker" agent, so resolveAgentIdentity(template "worker")
	// fails — the early return the fence must now pre-empt for a stale session.
	cityDir := writeFenceTestCityAgents(t, "[[agent]]\nname = \"other\"\n")
	sessionID := newFenceSessionBead(t, cityDir, session.StateFailedCreate, "failed-token")
	queryMarker := installFenceWorkQueryProbe(t)
	setFenceClaimEnv(t, cityDir, sessionID, "failed-token")

	var stdout, stderr bytes.Buffer
	code := cmdHookWithOptions(nil, hookCommandOptions{Claim: true, JSON: true}, &stdout, &stderr)

	if code != 1 {
		t.Fatalf("code = %d, want 1; stdout=%q stderr=%s", code, stdout.String(), stderr.String())
	}
	var result hookClaimJSONResult
	if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &result); err != nil {
		t.Fatalf("stdout is not a JSON drain result: %v\n%s", err, stdout.String())
	}
	if result.Action != "drain" || result.Reason != hookClaimReasonStaleSession {
		t.Fatalf("result = %+v, want action=drain reason=stale_session", result)
	}
	if !strings.Contains(stderr.String(), "refusing stale session") {
		t.Fatalf("stderr = %q, want stale-session refusal", stderr.String())
	}
	// The reorder's whole point: the fence must pre-empt the missing-template
	// early return, so its "not found in config" failure must NOT be reached.
	if strings.Contains(stderr.String(), "not found in config") {
		t.Fatalf("stale session hit the agent-not-found early return before the fence: %s", stderr.String())
	}
	if _, err := os.Stat(queryMarker); !os.IsNotExist(err) {
		t.Fatalf("work query ran for a stale session with a missing template; stat error = %v", err)
	}
}

// TestHookCommandClaimStaleSessionSuspendedAgentDrainsBeforeSuspensionCheck
// proves a stale runtime whose agent is suspended is refused as stale by the
// claim fence BEFORE the "agent is suspended" early return. As with the
// missing-template case, the startup wrapper's gc hook --claim --json
// --drain-ack must see the terminal stale-session drain instead of the bare
// suspension failure it would otherwise retry forever.
func TestHookCommandClaimStaleSessionSuspendedAgentDrainsBeforeSuspensionCheck(t *testing.T) {
	clearGCEnv(t)
	disableManagedDoltRecoveryForTest(t)
	t.Setenv("GC_BEADS", "file")
	// The "worker" agent exists but is suspended, so isAgentEffectivelySuspendedWith
	// returns true — the early return the fence must now pre-empt for a stale session.
	cityDir := writeFenceTestCityAgents(t, "[[agent]]\nname = \"worker\"\nsuspended = true\n")
	sessionID := newFenceSessionBead(t, cityDir, session.StateFailedCreate, "failed-token")
	queryMarker := installFenceWorkQueryProbe(t)
	setFenceClaimEnv(t, cityDir, sessionID, "failed-token")

	var stdout, stderr bytes.Buffer
	code := cmdHookWithOptions(nil, hookCommandOptions{Claim: true, JSON: true}, &stdout, &stderr)

	if code != 1 {
		t.Fatalf("code = %d, want 1; stdout=%q stderr=%s", code, stdout.String(), stderr.String())
	}
	var result hookClaimJSONResult
	if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &result); err != nil {
		t.Fatalf("stdout is not a JSON drain result: %v\n%s", err, stdout.String())
	}
	if result.Action != "drain" || result.Reason != hookClaimReasonStaleSession {
		t.Fatalf("result = %+v, want action=drain reason=stale_session", result)
	}
	if !strings.Contains(stderr.String(), "refusing stale session") {
		t.Fatalf("stderr = %q, want stale-session refusal", stderr.String())
	}
	// The reorder's whole point: the fence must pre-empt the suspension early
	// return, so its "is suspended" failure must NOT be reached.
	if strings.Contains(stderr.String(), "is suspended") {
		t.Fatalf("stale session hit the agent-suspended early return before the fence: %s", stderr.String())
	}
	if _, err := os.Stat(queryMarker); !os.IsNotExist(err) {
		t.Fatalf("work query ran for a stale session with a suspended agent; stat error = %v", err)
	}
}

// TestHookCommandClaimStaleSessionSuspendedCityDrainsBeforeSuspensionCheck
// proves a stale runtime in a SUSPENDED CITY is refused as stale by the claim
// fence BEFORE the bare "gc hook: city is suspended" early return. Before the
// fence was hoisted ahead of the city-suspension check, a stale session in a
// suspended city hit that bare `return 1` and its startup wrapper retried the
// plain city-suspended failure forever instead of seeing the terminal
// stale-session drain result and exiting. This pins the ordering so the fence
// pre-empts the city-suspension early return for a stale session too.
func TestHookCommandClaimStaleSessionSuspendedCityDrainsBeforeSuspensionCheck(t *testing.T) {
	clearGCEnv(t)
	disableManagedDoltRecoveryForTest(t)
	t.Setenv("GC_BEADS", "file")
	// A normal, resolvable, non-suspended "worker" agent, so the only early
	// return in play is the city-suspension one the fence must pre-empt.
	cityDir := writeFenceTestCity(t)
	sessionID := newFenceSessionBead(t, cityDir, session.StateFailedCreate, "failed-token")
	queryMarker := installFenceWorkQueryProbe(t)
	setFenceClaimEnv(t, cityDir, sessionID, "failed-token")
	// Suspend the whole city via the documented GC_SUSPENDED escape hatch so
	// citySuspendedWithState fires the "gc hook: city is suspended" early return.
	t.Setenv("GC_SUSPENDED", "1")

	var stdout, stderr bytes.Buffer
	code := cmdHookWithOptions(nil, hookCommandOptions{Claim: true, JSON: true}, &stdout, &stderr)

	if code != 1 {
		t.Fatalf("code = %d, want 1; stdout=%q stderr=%s", code, stdout.String(), stderr.String())
	}
	var result hookClaimJSONResult
	if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &result); err != nil {
		t.Fatalf("stdout is not a JSON drain result: %v\n%s", err, stdout.String())
	}
	if result.Action != "drain" || result.Reason != hookClaimReasonStaleSession {
		t.Fatalf("result = %+v, want action=drain reason=stale_session", result)
	}
	if !strings.Contains(stderr.String(), "refusing stale session") {
		t.Fatalf("stderr = %q, want stale-session refusal", stderr.String())
	}
	// The reorder's whole point: the fence must pre-empt the city-suspended early
	// return, so its "city is suspended" failure must NOT be reached.
	if strings.Contains(stderr.String(), "city is suspended") {
		t.Fatalf("stale session hit the city-suspended early return before the fence: %s", stderr.String())
	}
	if _, err := os.Stat(queryMarker); !os.IsNotExist(err) {
		t.Fatalf("work query ran for a stale session in a suspended city; stat error = %v", err)
	}
}
