package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

const hookClaimCommandName = "hook"

var hookClaimMutationTimeout = 10 * time.Second

type hookClaimOptions struct {
	Assignee           string
	IdentityCandidates []string
	RouteTargets       []string
	Env                []string
	DrainAck           bool
	JSON               bool
	// TriggerBeadID, when set, scopes the claim to that exact bead — the
	// target a demand/trigger spawn was created for (GC_TRIGGER_WORK_BEAD_ID).
	// A triggered session claims its target by ID and never falls back to the
	// generic work_query, so a federated bd store that surfaces unrelated
	// global work cannot make the session claim the wrong bead or spawn-loop.
	TriggerBeadID string
	// TriggerStoreDir, when set, is the working dir whose bead store owns the
	// trigger bead (resolved from GC_TRIGGER_WORK_STORE_REF). Empty falls back
	// to the agent's own work dir.
	TriggerStoreDir string
}

type hookClaimOps struct {
	Runner             WorkQueryRunner
	Claim              hookClaimFunc
	ResolveBead        hookResolveBeadFunc
	ListContinuation   hookListContinuationFunc
	AssignContinuation hookAssignContinuationFunc
	DrainAck           hookDrainAckFunc
	Now                func() time.Time
}

type (
	hookClaimFunc              func(context.Context, string, []string, string, string) (beads.Bead, bool, error)
	hookResolveBeadFunc        func(context.Context, string, []string, string) (beads.Bead, bool, error)
	hookListContinuationFunc   func(context.Context, string, []string, string, string) ([]beads.Bead, error)
	hookAssignContinuationFunc func(context.Context, string, []string, string, string) error
	hookDrainAckFunc           func(io.Writer) error
)

type hookClaimJSONResult struct {
	SchemaVersion        string   `json:"schema_version"`
	OK                   bool     `json:"ok"`
	Command              string   `json:"command"`
	Action               string   `json:"action"`
	Reason               string   `json:"reason,omitempty"`
	BeadID               string   `json:"bead_id,omitempty"`
	Assignee             string   `json:"assignee,omitempty"`
	Route                string   `json:"route,omitempty"`
	ContinuationAssigned []string `json:"continuation_assigned,omitempty"`
	DrainAcknowledged    bool     `json:"drain_acknowledged,omitempty"`
}

func doHookClaim(workQuery, dir string, opts hookClaimOptions, ops hookClaimOps, stdout, stderr io.Writer) int {
	opts.Assignee = strings.TrimSpace(opts.Assignee)
	opts.IdentityCandidates = hookClaimIdentityCandidates(append([]string{opts.Assignee}, opts.IdentityCandidates...)...)
	opts.RouteTargets = hookClaimRouteTargets(opts.RouteTargets...)
	if opts.Assignee == "" {
		fmt.Fprintln(stderr, "gc hook --claim: assignee not specified (set $GC_SESSION_NAME or $GC_SESSION_ID)") //nolint:errcheck
		return 1
	}
	if ops.Runner == nil {
		fmt.Fprintln(stderr, "gc hook --claim: missing work query runner") //nolint:errcheck
		return 1
	}
	if ops.Claim == nil {
		ops.Claim = hookClaimWithBdStore
	}
	if ops.ResolveBead == nil {
		ops.ResolveBead = hookResolveBeadWithBdStore
	}
	if ops.ListContinuation == nil {
		ops.ListContinuation = hookListContinuationWithBdStore
	}
	if ops.AssignContinuation == nil {
		ops.AssignContinuation = hookAssignContinuationWithBdStore
	}
	if ops.DrainAck == nil {
		ops.DrainAck = hookRuntimeDrainAck
	}
	now := time.Now
	if ops.Now != nil {
		now = ops.Now
	}

	// A demand/trigger spawn is created FOR one exact bead. Claim that bead
	// by ID and never fall back to generic work_query selection: a federated
	// bd store can surface unrelated global work from `bd ready`, and taking
	// it would either claim the wrong bead or (when the route filter rejects
	// it) drain-and-respawn forever. Resolving + claiming by ID is immune to
	// which store `bd ready` reads.
	if triggerID := strings.TrimSpace(opts.TriggerBeadID); triggerID != "" {
		return doHookTriggerClaim(triggerID, dir, opts, ops, stdout, stderr)
	}

	output, err := ops.Runner(workQuery, dir)
	if err != nil {
		fmt.Fprintf(stderr, "gc hook --claim: %v\n", err) //nolint:errcheck
		return 1
	}

	normalized := normalizeWorkQueryOutput(strings.TrimSpace(output))
	normalized = filterUnreadyHookCandidates(normalized, now())
	if !workQueryHasReadyWork(normalized) {
		return writeHookClaimNoWork(opts, ops, stdout, stderr)
	}
	candidates, err := decodeHookClaimBeads(normalized)
	if err != nil {
		fmt.Fprintf(stderr, "gc hook --claim: requires JSON work_query output to identify claim candidates: %v\n", err) //nolint:errcheck
		return 1
	}
	if len(candidates) == 0 {
		return writeHookClaimNoWork(opts, ops, stdout, stderr)
	}

	if result, bead, ok := hookClaimExistingOrAssigned(candidates, opts); ok {
		return writeHookClaimWorkResultForBead(result, bead, opts, ops, dir, stdout, stderr)
	}

	ctx, cancel := context.WithTimeout(context.Background(), hookClaimMutationTimeout)
	defer cancel()
	for _, candidate := range candidates {
		if strings.TrimSpace(candidate.ID) == "" ||
			strings.TrimSpace(candidate.Assignee) != "" ||
			!hookClaimMatchesRoute(candidate, opts.RouteTargets) {
			continue
		}
		claimed, ok, err := ops.Claim(ctx, dir, opts.Env, candidate.ID, opts.Assignee)
		if err != nil {
			fmt.Fprintf(stderr, "gc hook --claim: claiming %s: %v\n", candidate.ID, err) //nolint:errcheck
			return 1
		}
		if !ok {
			continue
		}
		if claimed.Metadata == nil {
			claimed.Metadata = candidate.Metadata
		}
		result := hookClaimJSONResult{
			SchemaVersion: "1",
			OK:            true,
			Command:       hookClaimCommandName,
			Action:        "work",
			Reason:        "claimed",
			BeadID:        claimed.ID,
			Assignee:      claimed.Assignee,
			Route:         hookClaimRoute(claimed),
		}
		if result.BeadID == "" {
			result.BeadID = candidate.ID
		}
		if result.Assignee == "" {
			result.Assignee = opts.Assignee
		}
		return writeHookClaimWorkResultForBead(result, claimed, opts, ops, dir, stdout, stderr)
	}

	return writeHookClaimNoWork(opts, ops, stdout, stderr)
}

// doHookTriggerClaim resolves the exact trigger bead in its own store, runs the
// same readiness/route/ownership checks the generic path applies, and claims it
// through the existing atomic claim primitive. A triggered session NEVER falls
// through to generic selection: if the trigger is gone, taken, misrouted, or
// lost to a race, it drains rather than grabbing unrelated work.
func doHookTriggerClaim(triggerID, dir string, opts hookClaimOptions, ops hookClaimOps, stdout, stderr io.Writer) int {
	triggerDir := strings.TrimSpace(opts.TriggerStoreDir)
	if triggerDir == "" {
		triggerDir = dir
	}
	ctx, cancel := context.WithTimeout(context.Background(), hookClaimMutationTimeout)
	defer cancel()

	bead, found, err := ops.ResolveBead(ctx, triggerDir, opts.Env, triggerID)
	if err != nil {
		fmt.Fprintf(stderr, "gc hook --claim: resolving trigger bead %s: %v\n", triggerID, err) //nolint:errcheck
		return 1
	}
	if !found {
		// The trigger was completed or removed — nothing to do; drain.
		fmt.Fprintf(stderr, "gc hook --claim: trigger bead %s not found; draining\n", triggerID) //nolint:errcheck
		return writeHookClaimNoWork(opts, ops, stdout, stderr)
	}

	// Already mine and in progress: report the existing assignment (resume).
	if strings.EqualFold(strings.TrimSpace(bead.Status), "in_progress") &&
		hookClaimHasIdentity(bead.Assignee, opts.IdentityCandidates) {
		result := hookClaimJSONResult{
			SchemaVersion: "1",
			OK:            true,
			Command:       hookClaimCommandName,
			Action:        "work",
			Reason:        "existing_assignment",
			BeadID:        bead.ID,
			Assignee:      bead.Assignee,
			Route:         hookClaimRoute(bead),
		}
		return writeHookClaimWorkResultForBead(result, bead, opts, ops, triggerDir, stdout, stderr)
	}

	// Taken by another session or no longer open: drain, do NOT grab other work.
	if strings.TrimSpace(bead.Assignee) != "" || !strings.EqualFold(strings.TrimSpace(bead.Status), "open") {
		fmt.Fprintf(stderr, "gc hook --claim: trigger bead %s not claimable (status=%q assignee=%q); draining\n", //nolint:errcheck
			triggerID, strings.TrimSpace(bead.Status), strings.TrimSpace(bead.Assignee))
		return writeHookClaimNoWork(opts, ops, stdout, stderr)
	}

	// Route must match this session's targets — a misroute is not ours to take.
	if !hookClaimMatchesRoute(bead, opts.RouteTargets) {
		fmt.Fprintf(stderr, "gc hook --claim: trigger bead %s routed to %q not in this session's targets; draining\n", //nolint:errcheck
			triggerID, hookClaimRoute(bead))
		return writeHookClaimNoWork(opts, ops, stdout, stderr)
	}

	claimed, ok, err := ops.Claim(ctx, triggerDir, opts.Env, triggerID, opts.Assignee)
	if err != nil {
		fmt.Fprintf(stderr, "gc hook --claim: claiming trigger %s: %v\n", triggerID, err) //nolint:errcheck
		return 1
	}
	if !ok {
		// Lost the race — another session claimed it first. Drain; never
		// fall through to claim unrelated work.
		fmt.Fprintf(stderr, "gc hook --claim: trigger bead %s claimed by another session; draining\n", triggerID) //nolint:errcheck
		return writeHookClaimNoWork(opts, ops, stdout, stderr)
	}
	if claimed.Metadata == nil {
		claimed.Metadata = bead.Metadata
	}
	result := hookClaimJSONResult{
		SchemaVersion: "1",
		OK:            true,
		Command:       hookClaimCommandName,
		Action:        "work",
		Reason:        "claimed_trigger",
		BeadID:        claimed.ID,
		Assignee:      claimed.Assignee,
		Route:         hookClaimRoute(claimed),
	}
	if result.BeadID == "" {
		result.BeadID = triggerID
	}
	if result.Assignee == "" {
		result.Assignee = opts.Assignee
	}
	return writeHookClaimWorkResultForBead(result, claimed, opts, ops, triggerDir, stdout, stderr)
}

func hookClaimExistingOrAssigned(candidates []beads.Bead, opts hookClaimOptions) (hookClaimJSONResult, beads.Bead, bool) {
	for _, candidate := range candidates {
		if strings.EqualFold(strings.TrimSpace(candidate.Status), "in_progress") &&
			hookClaimHasIdentity(candidate.Assignee, opts.IdentityCandidates) {
			result := hookClaimJSONResult{
				SchemaVersion: "1",
				OK:            true,
				Command:       hookClaimCommandName,
				Action:        "work",
				Reason:        "existing_assignment",
				BeadID:        candidate.ID,
				Assignee:      candidate.Assignee,
				Route:         hookClaimRoute(candidate),
			}
			return result, candidate, true
		}
	}
	for _, candidate := range candidates {
		if strings.EqualFold(strings.TrimSpace(candidate.Status), "open") &&
			hookClaimHasIdentity(candidate.Assignee, opts.IdentityCandidates) {
			result := hookClaimJSONResult{
				SchemaVersion: "1",
				OK:            true,
				Command:       hookClaimCommandName,
				Action:        "work",
				Reason:        "ready_assignment",
				BeadID:        candidate.ID,
				Assignee:      candidate.Assignee,
				Route:         hookClaimRoute(candidate),
			}
			return result, candidate, true
		}
	}
	return hookClaimJSONResult{}, beads.Bead{}, false
}

func writeHookClaimWorkResultForBead(result hookClaimJSONResult, bead beads.Bead, opts hookClaimOptions, ops hookClaimOps, dir string, stdout, stderr io.Writer) int {
	assigned, err := preassignHookContinuationGroup(bead, opts, ops, dir)
	if err != nil {
		fmt.Fprintf(stderr, "gc hook --claim: preassigning continuation group for %s: %v\n", bead.ID, err) //nolint:errcheck
		return 1
	}
	result.ContinuationAssigned = assigned
	if opts.JSON {
		if err := writeCLIJSONLine(stdout, result); err != nil {
			fmt.Fprintf(stderr, "gc hook --claim: writing JSON: %v\n", err) //nolint:errcheck
			return 1
		}
		return 0
	}
	fmt.Fprintln(stdout, result.BeadID) //nolint:errcheck
	return 0
}

func writeHookClaimNoWork(opts hookClaimOptions, ops hookClaimOps, stdout, stderr io.Writer) int {
	result := hookClaimJSONResult{
		SchemaVersion: "1",
		OK:            true,
		Command:       hookClaimCommandName,
		Action:        "drain",
		Reason:        "no_work",
	}
	if opts.DrainAck {
		if err := ops.DrainAck(stderr); err != nil {
			fmt.Fprintf(stderr, "gc hook --claim: drain-ack failed: %v\n", err) //nolint:errcheck
			return 1
		}
		result.DrainAcknowledged = true
	}
	if opts.JSON {
		if err := writeCLIJSONLine(stdout, result); err != nil {
			fmt.Fprintf(stderr, "gc hook --claim: writing JSON: %v\n", err) //nolint:errcheck
			return 1
		}
	}
	if opts.DrainAck {
		return 0
	}
	return 1
}

func preassignHookContinuationGroup(bead beads.Bead, opts hookClaimOptions, ops hookClaimOps, dir string) ([]string, error) {
	rootID := strings.TrimSpace(bead.Metadata["gc.root_bead_id"])
	group := strings.TrimSpace(bead.Metadata["gc.continuation_group"])
	if rootID == "" || group == "" {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), hookClaimMutationTimeout)
	defer cancel()
	siblings, err := ops.ListContinuation(ctx, dir, opts.Env, rootID, group)
	if err != nil {
		return nil, err
	}
	assigned := make([]string, 0, len(siblings))
	for _, sibling := range siblings {
		if strings.TrimSpace(sibling.ID) == "" ||
			sibling.ID == bead.ID ||
			strings.TrimSpace(sibling.Assignee) != "" ||
			!strings.EqualFold(strings.TrimSpace(sibling.Status), "open") ||
			!hookClaimMatchesRoute(sibling, opts.RouteTargets) {
			continue
		}
		if err := ops.AssignContinuation(ctx, dir, opts.Env, sibling.ID, opts.Assignee); err != nil {
			return assigned, fmt.Errorf("assigning %s: %w", sibling.ID, err)
		}
		assigned = append(assigned, sibling.ID)
	}
	return assigned, nil
}

func hookClaimWithBdStore(_ context.Context, dir string, env []string, beadID, assignee string) (beads.Bead, bool, error) {
	store := hookClaimBdStore(dir, env, assignee)
	claimed, ok, err := store.Claim(beadID)
	if err != nil {
		return beads.Bead{}, false, err
	}
	if !ok {
		return beads.Bead{}, false, nil
	}
	if !hookClaimHasIdentity(claimed.Assignee, []string{assignee}) {
		return beads.Bead{}, false, nil
	}
	return claimed, true, nil
}

// hookResolveBeadWithBdStore fetches one bead by ID from the store rooted at
// dir. A missing bead is reported as (zero, false, nil) so a triggered claim
// treats an already-completed trigger as "drain", not an error. Resolution is
// by exact ID, so it is unaffected by which store a federated `bd ready` reads.
func hookResolveBeadWithBdStore(_ context.Context, dir string, env []string, beadID string) (beads.Bead, bool, error) {
	store := hookClaimBdStore(dir, env, "")
	bead, err := store.Get(beadID)
	if err != nil {
		if errors.Is(err, beads.ErrNotFound) {
			return beads.Bead{}, false, nil
		}
		return beads.Bead{}, false, err
	}
	return bead, true, nil
}

func hookListContinuationWithBdStore(_ context.Context, dir string, env []string, rootID, group string) ([]beads.Bead, error) {
	store := hookClaimBdStore(dir, env, "")
	return store.List(beads.ListQuery{
		Status: "open",
		Metadata: map[string]string{
			"gc.root_bead_id":       rootID,
			"gc.continuation_group": group,
		},
		TierMode: beads.TierBoth,
	})
}

func hookAssignContinuationWithBdStore(_ context.Context, dir string, env []string, beadID, assignee string) error {
	store := hookClaimBdStore(dir, env, assignee)
	return store.Update(beadID, beads.UpdateOpts{Assignee: &assignee})
}

func hookRuntimeDrainAck(stderr io.Writer) error {
	if code := cmdRuntimeDrainAck(nil, false, io.Discard, stderr); code != 0 {
		return errors.New("runtime drain-ack returned non-zero")
	}
	return nil
}

func hookClaimBdStore(dir string, env []string, actor string) *beads.BdStore {
	return beads.NewBdStore(dir, beads.ExecCommandRunnerWithEnv(hookClaimEnvMap(env, dir, actor)))
}

func hookClaimEnvMap(env []string, dir string, actor string) map[string]string {
	env = workQueryEnvForDir(env, dir)
	out := make(map[string]string, len(env)+1)
	for _, entry := range env {
		key, value, ok := strings.Cut(entry, "=")
		if !ok || key == "" {
			continue
		}
		out[key] = value
	}
	if strings.TrimSpace(actor) != "" {
		out["BEADS_ACTOR"] = actor
	}
	return out
}

func decodeHookClaimBeads(output string) ([]beads.Bead, error) {
	output = strings.TrimSpace(output)
	if output == "" {
		return nil, nil
	}
	if !json.Valid([]byte(output)) {
		extracted, ok := firstHookJSONValue(output)
		if !ok {
			return nil, errors.New("output is not JSON")
		}
		output = extracted
	}
	output = normalizeWorkQueryOutput(output)
	var candidates []beads.Bead
	if err := json.Unmarshal([]byte(output), &candidates); err != nil {
		return nil, err
	}
	return candidates, nil
}

func firstHookJSONValue(output string) (string, bool) {
	for idx, r := range output {
		if r != '[' && r != '{' {
			continue
		}
		dec := json.NewDecoder(strings.NewReader(output[idx:]))
		var raw json.RawMessage
		if err := dec.Decode(&raw); err == nil {
			return string(raw), true
		}
	}
	return "", false
}

func hookClaimHasIdentity(assignee string, identities []string) bool {
	assignee = strings.TrimSpace(assignee)
	if assignee == "" {
		return false
	}
	for _, identity := range identities {
		if assignee == strings.TrimSpace(identity) {
			return true
		}
	}
	return false
}

func hookClaimMatchesRoute(candidate beads.Bead, routeTargets []string) bool {
	if len(routeTargets) == 0 {
		return false
	}
	routedTo := strings.TrimSpace(candidate.Metadata["gc.routed_to"])
	runTarget := strings.TrimSpace(candidate.Metadata["gc.run_target"])
	kind := strings.TrimSpace(candidate.Metadata["gc.kind"])
	for _, target := range routeTargets {
		target = strings.TrimSpace(target)
		if target == "" {
			continue
		}
		if routedTo == target {
			return true
		}
		if routedTo == "" && kind == "workflow" && runTarget == target {
			return true
		}
	}
	return false
}

func hookClaimRoute(candidate beads.Bead) string {
	if routedTo := strings.TrimSpace(candidate.Metadata["gc.routed_to"]); routedTo != "" {
		return routedTo
	}
	if strings.TrimSpace(candidate.Metadata["gc.kind"]) == "workflow" {
		return strings.TrimSpace(candidate.Metadata["gc.run_target"])
	}
	return ""
}

func hookClaimIdentityCandidates(values ...string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
		if legacy := hookLegacyWorkflowControlName(value); legacy != "" && !seen[legacy] {
			seen[legacy] = true
			out = append(out, legacy)
		}
	}
	return out
}

func hookClaimRouteTargets(values ...string) []string {
	return hookClaimIdentityCandidates(values...)
}

func hookLegacyWorkflowControlName(value string) string {
	value = strings.TrimSpace(value)
	const suffix = "control-dispatcher"
	if !strings.HasSuffix(value, suffix) {
		return ""
	}
	return strings.TrimSuffix(value, suffix) + "workflow-control"
}
