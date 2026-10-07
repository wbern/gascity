package prwatchdog

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const watchdogWorkflowFile = "pr-evidence-watchdog.yml"

func loadWatchdogWorkflow(t *testing.T) map[string]any {
	t.Helper()
	path := filepath.Join("..", "..", ".github", "workflows", watchdogWorkflowFile)
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal(body, &doc); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	return doc
}

func TestWatchdogWorkflow_TriggerSetIsExact(t *testing.T) {
	doc := loadWatchdogWorkflow(t)
	on, ok := doc["on"].(map[string]any)
	if !ok {
		t.Fatalf("workflow 'on' must be a mapping, got %T", doc["on"])
	}
	if len(on) != 1 {
		t.Fatalf("workflow must trigger on pull_request_target only, got triggers: %v", on)
	}
	prTarget, ok := on["pull_request_target"].(map[string]any)
	if !ok {
		t.Fatalf("workflow must trigger on pull_request_target, got: %v", on)
	}
	types, ok := prTarget["types"].([]any)
	if !ok {
		t.Fatalf("pull_request_target must declare explicit types, got: %v", prTarget["types"])
	}
	want := []string{"opened", "reopened", "synchronize", "ready_for_review"}
	if len(types) != len(want) {
		t.Fatalf("pull_request_target.types = %v, want exactly %v", types, want)
	}
	for i, w := range want {
		if types[i] != w {
			t.Fatalf("pull_request_target.types[%d] = %v, want %q", i, types[i], w)
		}
	}
}

func TestWatchdogWorkflow_MinimumReadOnlyPermissions(t *testing.T) {
	doc := loadWatchdogWorkflow(t)
	perms, ok := doc["permissions"].(map[string]any)
	if !ok {
		t.Fatalf("workflow must declare explicit top-level permissions, got: %v", doc["permissions"])
	}
	want := map[string]string{
		"checks":        "read",
		"pull-requests": "read",
		"contents":      "read",
	}
	if len(perms) != len(want) {
		t.Fatalf("permissions = %v, want exactly %v", perms, want)
	}
	for key, level := range want {
		got, ok := perms[key].(string)
		if !ok || got != level {
			t.Fatalf("permissions[%q] = %v, want %q", key, perms[key], level)
		}
	}
}

func TestWatchdogWorkflow_NoWriteOrSecretsScopes(t *testing.T) {
	doc := loadWatchdogWorkflow(t)
	body, err := yaml.Marshal(doc)
	if err != nil {
		t.Fatalf("re-marshal workflow: %v", err)
	}
	text := string(body)
	if strings.Contains(text, "secrets") {
		t.Fatalf("workflow must not reference secrets:\n%s", text)
	}
	if strings.Contains(text, ": write") {
		t.Fatalf("workflow must not grant any write permission:\n%s", text)
	}
}

func TestWatchdogWorkflow_RequiredCheckJobExists(t *testing.T) {
	doc := loadWatchdogWorkflow(t)
	jobs, ok := doc["jobs"].(map[string]any)
	if !ok {
		t.Fatalf("workflow jobs must be a mapping")
	}
	var found map[string]any
	for _, raw := range jobs {
		job, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if job["name"] == RequiredCheckName {
			found = job
			break
		}
	}
	if found == nil {
		t.Fatalf("no job has name %q (the required check name)", RequiredCheckName)
	}

	timeout, ok := found["timeout-minutes"].(int)
	if !ok || timeout <= 0 || timeout > 35 {
		t.Fatalf("job %q timeout-minutes = %v, want a bounded value > 0 and <= 35 (slightly above the 25m observation deadline)", RequiredCheckName, found["timeout-minutes"])
	}
}

func TestWatchdogWorkflow_ConcurrencyKeyedByPRCancelsObsoleteHead(t *testing.T) {
	doc := loadWatchdogWorkflow(t)
	concurrency, ok := doc["concurrency"].(map[string]any)
	if !ok {
		t.Fatalf("workflow must declare top-level concurrency, got: %v", doc["concurrency"])
	}
	group, ok := concurrency["group"].(string)
	if !ok || !strings.Contains(group, "pull_request.number") {
		t.Fatalf("concurrency.group = %v, want it to key on the PR number", concurrency["group"])
	}
	if strings.Contains(group, "head") {
		t.Fatalf("concurrency.group = %q must not key on head SHA, or a new push would never cancel the obsolete-head observation", group)
	}
	cancel, ok := concurrency["cancel-in-progress"].(bool)
	if !ok || !cancel {
		t.Fatalf("concurrency.cancel-in-progress = %v, want true", concurrency["cancel-in-progress"])
	}
}

func TestWatchdogWorkflow_CheckoutIsTrustedBaseOnlyOrAbsent(t *testing.T) {
	doc := loadWatchdogWorkflow(t)
	jobs, _ := doc["jobs"].(map[string]any)
	for jobName, raw := range jobs {
		job, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		steps, ok := job["steps"].([]any)
		if !ok {
			continue
		}
		for _, rawStep := range steps {
			step, ok := rawStep.(map[string]any)
			if !ok {
				continue
			}
			uses, _ := step["uses"].(string)
			if !strings.HasPrefix(uses, "actions/checkout@") {
				continue
			}
			with, _ := step["with"].(map[string]any)
			ref, _ := with["ref"].(string)
			if ref != "${{ github.event.pull_request.base.sha }}" {
				t.Fatalf("job %q checkout must pin ref to the PR base SHA only, got ref=%q", jobName, ref)
			}
			persist, hasPersist := with["persist-credentials"].(bool)
			if !hasPersist || persist {
				t.Fatalf("job %q checkout must set persist-credentials: false, got %v", jobName, with["persist-credentials"])
			}
		}
	}
}

func TestWatchdogWorkflow_InvokesTheWatchdogProgram(t *testing.T) {
	doc := loadWatchdogWorkflow(t)
	body, err := yaml.Marshal(doc)
	if err != nil {
		t.Fatalf("re-marshal workflow: %v", err)
	}
	text := string(body)
	if !strings.Contains(text, "scripts/prwatchdog/cmd/watchdog") {
		t.Fatalf("expected the workflow to invoke the watchdog program at scripts/prwatchdog/cmd/watchdog, got:\n%s", text)
	}
}

func TestWatchdogWorkflow_UsesExplicitPRHeadSHANotBareGithubSHA(t *testing.T) {
	doc := loadWatchdogWorkflow(t)
	body, err := yaml.Marshal(doc)
	if err != nil {
		t.Fatalf("re-marshal workflow: %v", err)
	}
	text := string(body)
	if !strings.Contains(text, "github.event.pull_request.head.sha") {
		t.Fatalf("workflow must read the PR head SHA explicitly from github.event.pull_request.head.sha for API lookups, got:\n%s", text)
	}
	// Under pull_request_target, github.sha resolves to the BASE branch
	// commit, not the PR head -- using it here would silently evaluate the
	// wrong commit's evidence. This is precisely the class of bug this
	// watchdog exists to catch (observed on fork PR #4967).
	if strings.Contains(text, "${{ github.sha }}") {
		t.Fatalf("workflow must not use the bare github.sha context (resolves to the base ref under pull_request_target, not the PR head):\n%s", text)
	}
}

func TestForkContractWorkflowProducerAgreement(t *testing.T) {
	watchdog := loadWatchdogWorkflow(t)
	jobs := watchdog["jobs"].(map[string]any)
	steps := jobs["evidence"].(map[string]any)["steps"].([]any)
	want := "${{ github.repository == 'wbern/gascity' && 'fork' || 'upstream' }}"
	found := false
	for _, raw := range steps {
		step := raw.(map[string]any)
		if env, ok := step["env"].(map[string]any); ok && env["EVIDENCE_CONTRACT"] == want {
			found = true
		}
	}
	if !found {
		t.Fatal("trusted workflow must explicitly select fork only for wbern/gascity")
	}
	body, err := os.ReadFile(filepath.Join("..", "..", ".github", "workflows", "fork-verify.yml"))
	if err != nil {
		t.Fatal(err)
	}
	var fork map[string]any
	if err := yaml.Unmarshal(body, &fork); err != nil {
		t.Fatal(err)
	}
	producers := fork["jobs"].(map[string]any)
	for _, name := range []string{ForkVerifyName, ForkLintName} {
		if _, ok := producers[name]; !ok {
			t.Fatalf("missing producer %q", name)
		}
	}
	proof := producers["handoff-config-proof"].(map[string]any)
	if proof["name"] != ForkProofName || proof["needs"] != "verify" {
		t.Fatalf("required proof producer miswired: %v", proof)
	}
	verify := producers["verify"].(map[string]any)
	outputs := verify["outputs"].(map[string]any)
	if outputs["handoff-config-proof"] != "${{ steps.handoff-config-proof.outputs.passed }}" {
		t.Fatal("proof output disconnected")
	}
	found = false
	for _, raw := range verify["steps"].([]any) {
		step := raw.(map[string]any)
		if step["id"] == "handoff-config-proof" {
			run, _ := step["run"].(string)
			if step["if"] != nil || !strings.Contains(run, "python3 scripts/prwatchdog/handoff_proof.py --head") || !strings.Contains(run, "passed=true") {
				t.Fatal("proof must execute unconditionally before success output")
			}
			found = true
		}
	}
	if !found {
		t.Fatal("missing executed proof")
	}
	for _, name := range []string{ForkVerifyName, ForkLintName} {
		for _, raw := range producers[name].(map[string]any)["steps"].([]any) {
			step := raw.(map[string]any)
			if uses, _ := step["uses"].(string); strings.HasPrefix(uses, "actions/checkout@") {
				with := step["with"].(map[string]any)
				if with["ref"] != "${{ github.event.pull_request.head.sha || github.sha }}" {
					t.Fatalf("%s must test exact head", name)
				}
			}
		}
	}
}
