package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/internal/events"
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
	// TriggerBeadID, when set, scopes the claim to the exact bead a demand
	// spawn was created for. Triggered sessions never fall through to generic
	// discovery, because a federated work query can surface unrelated work.
	TriggerBeadID string
	// TriggerStoreDir is the working dir for the trigger bead's owning store.
	// Empty falls back to the store dir already being queried.
	TriggerStoreDir string
}

type hookClaimOps struct {
	Runner             WorkQueryRunner
	Claim              hookClaimFunc
	ResolveBead        hookResolveBeadFunc
	ListContinuation   hookListContinuationFunc
	AssignContinuation hookAssignContinuationFunc
	DrainAck           hookDrainAckFunc
	// EmitClaimRejected publishes a bead.claim_rejected event when a claim is
	// lost to a different live claimant (ADR-0009). Best-effort.
	EmitClaimRejected hookEmitClaimRejectedFunc
	// ResolveWorkBranch returns the git branch of the worker's worktree (dir),
	// stamped onto the bead as gc.work_branch at claim time. Empty result (no
	// repo / detached HEAD) skips the stamp.
	ResolveWorkBranch hookResolveWorkBranchFunc
	// StampWorkBranch writes gc.work_branch onto the claimed bead. Best-effort.
	StampWorkBranch hookStampWorkBranchFunc
	// RecordSessionPointers writes the session bead's current-pointers — gc.current_run_id
	// AND gc.active_work_bead (the claimed work bead's gc.step_id) — in ONE update, so
	// the (run, step) tuple stays atomically consistent. Best-effort.
	RecordSessionPointers hookRecordSessionPointersFunc
	Now                   func() time.Time
}

type (
	hookClaimFunc                 func(context.Context, string, []string, string, string) (beads.Bead, bool, error)
	hookResolveBeadFunc           func(context.Context, string, []string, string) (beads.Bead, bool, error)
	hookListContinuationFunc      func(context.Context, string, []string, string, string) ([]beads.Bead, error)
	hookAssignContinuationFunc    func(context.Context, string, []string, string, string) error
	hookDrainAckFunc              func(io.Writer) error
	hookEmitClaimRejectedFunc     func(beadID, existingClaimant, attemptedClaimant string)
	hookResolveWorkBranchFunc     func(dir string) string
	hookStampWorkBranchFunc       func(ctx context.Context, dir string, env []string, beadID, assignee, branch string) error
	hookRecordSessionPointersFunc func(ctx context.Context, dir string, env []string, assignee, sessionBeadID, runID, stepID string) error
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

// hookClaimResult is the outcome of attempting a claim against one store's
// captured work-query output. A terminal result has already written its final
// output — a claim, an existing assignment, or a hard error — and the caller
// must return code as-is. A non-terminal result means the store yielded no
// claimable work (it was empty/unready, every claimable candidate was lost to
// another claimant, or every claimable candidate's claim mutation errored and was
// skipped) and NO terminal output was written, so a federated caller may try a
// later store before writing the single no-work drain.
type hookClaimResult struct {
	terminal bool
	code     int
	// claimsErrored is set on a NON-terminal result when one or more eligible
	// candidates' claim mutations errored and nothing was ultimately claimed. It
	// lets the shared no-work drain report a distinct "claims_errored" reason
	// instead of a healthy "no_work", so an operational write failure (store
	// contention or a controller-socket flap in the read→write window) is not
	// laundered into an idle signal. Meaningless on a terminal result.
	claimsErrored bool
}

func doHookClaim(workQuery, dir string, opts hookClaimOptions, ops hookClaimOps, stdout, stderr io.Writer) int {
	res := tryHookClaim(workQuery, dir, &opts, &ops, stdout, stderr)
	if res.terminal {
		return res.code
	}
	return writeHookClaimNoWork(opts, ops, res.claimsErrored, stdout, stderr)
}

// tryHookClaim runs the work query for one store (dir, via ops.Runner) and
// attempts to claim a ready candidate. It returns a terminal result once a
// claim, existing assignment, or hard error has been written, or a non-terminal
// result — with NO output written — when the store yielded no claimable work, so
// a federated caller can try a later store before draining. opts and ops are
// normalized in place so a non-terminal caller can reuse the normalized ops
// (defaults applied) for the shared drain.
func tryHookClaim(workQuery, dir string, opts *hookClaimOptions, ops *hookClaimOps, stdout, stderr io.Writer) hookClaimResult {
	opts.Assignee = strings.TrimSpace(opts.Assignee)
	opts.IdentityCandidates = hookClaimIdentityCandidates(append([]string{opts.Assignee}, opts.IdentityCandidates...)...)
	opts.RouteTargets = hookClaimRouteTargets(opts.RouteTargets...)
	if opts.Assignee == "" {
		fmt.Fprintln(stderr, "gc hook --claim: assignee not specified (set $GC_SESSION_NAME or $GC_SESSION_ID)") //nolint:errcheck
		return hookClaimResult{terminal: true, code: 1}
	}
	ops.applyDefaults()
	if triggerID := strings.TrimSpace(opts.TriggerBeadID); triggerID != "" {
		return hookClaimResult{terminal: true, code: doHookTriggerClaim(triggerID, dir, *opts, *ops, stdout, stderr)}
	}
	if ops.Runner == nil {
		fmt.Fprintln(stderr, "gc hook --claim: missing work query runner") //nolint:errcheck
		return hookClaimResult{terminal: true, code: 1}
	}
	now := time.Now
	if ops.Now != nil {
		now = ops.Now
	}

	output, err := ops.Runner(workQuery, dir)
	if err != nil {
		fmt.Fprintf(stderr, "gc hook --claim: %v\n", err) //nolint:errcheck
		return hookClaimResult{terminal: true, code: 1}
	}

	preFilter := normalizeWorkQueryOutput(strings.TrimSpace(output))
	normalized := filterUnreadyHookCandidates(preFilter, now())
	if stripped := hookClaimStripDiagnostic(preFilter, now()); len(stripped) > 0 {
		// Emit len(before) and len(after) so a recurrence self-classifies the
		// mechanism (gci-x8zo): before>0,after=0 == the worker stripped rows the
		// reconciler's demand count still counted (readiness divergence). A
		// separate "demand counted but bd ready returned before=0" case is
		// bd-ready non-determinism (gci-8qm3) and needs store determinism.
		beforeN := hookCandidateCount(preFilter)
		afterN := hookCandidateCount(normalized)
		fmt.Fprintf(stderr, "gc hook --claim: bd ready returned %d routed candidate(s), %d claimable after readiness filter; stripped: %s\n", beforeN, afterN, strings.Join(stripped, ", ")) //nolint:errcheck
	}
	if !workQueryHasReadyWork(normalized) {
		return hookClaimResult{}
	}
	candidates, err := decodeHookClaimBeads(normalized)
	if err != nil {
		fmt.Fprintf(stderr, "gc hook --claim: requires JSON work_query output to identify claim candidates: %v\n", err) //nolint:errcheck
		return hookClaimResult{terminal: true, code: 1}
	}
	if len(candidates) == 0 {
		return hookClaimResult{}
	}

	if result, bead, ok := hookClaimExistingOrAssigned(candidates, *opts); ok {
		return hookClaimResult{terminal: true, code: writeHookClaimWorkResultForBead(result, bead, *opts, *ops, dir, stdout, stderr)}
	}

	return claimFirstEligibleHookCandidate(candidates, *opts, *ops, dir, stdout, stderr)
}

// applyDefaults fills any unset op seam with its production implementation, so
// callers (and tests) only override the seams they care about. Runner has no
// default — a missing work-query runner is a caller error handled in doHookClaim.
func (ops *hookClaimOps) applyDefaults() {
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
	if ops.EmitClaimRejected == nil {
		ops.EmitClaimRejected = hookEmitClaimRejected
	}
	if ops.ResolveWorkBranch == nil {
		ops.ResolveWorkBranch = hookResolveWorkBranch
	}
	if ops.StampWorkBranch == nil {
		ops.StampWorkBranch = hookStampWorkBranchWithBdStore
	}
	if ops.RecordSessionPointers == nil {
		ops.RecordSessionPointers = hookRecordSessionPointersWithBdStore
	}
}

// claimFirstEligibleHookCandidate claims the first unassigned, route-matched
// candidate and returns a terminal result carrying the exit code of the
// work-result write. A claim lost to a different live claimant is surfaced as a
// bead.claim_rejected event before moving on. A candidate whose claim mutation
// errors is logged and skipped so one unclaimable id cannot wedge the hook. When
// no candidate can be claimed — none match this session, every claimable one was
// lost to another claimant, or every claimable one errored — it returns a
// non-terminal result (no output written) so a federated caller can try a later
// store before the shared no-work drain; the result's claimsErrored flag records
// whether any skip was an error so that drain stays distinguishable from idle.
func claimFirstEligibleHookCandidate(candidates []beads.Bead, opts hookClaimOptions, ops hookClaimOps, dir string, stdout, stderr io.Writer) hookClaimResult {
	ctx, cancel := context.WithTimeout(context.Background(), hookClaimMutationTimeout)
	defer cancel()
	claimsErrored := false
	for _, candidate := range candidates {
		if !hookCandidateClaimable(candidate, opts.RouteTargets) {
			continue
		}
		if ctx.Err() != nil {
			// The shared claim budget is spent (an earlier slow-failing claim
			// consumed it). Stop rather than attempting the remaining candidates
			// with an already-expired context, which would only manufacture
			// deadline-exceeded skips on ids never really tried; they are reclaimed
			// next tick (NDI).
			break
		}
		claimed, ok, err := ops.Claim(ctx, dir, opts.Env, candidate.ID, opts.Assignee)
		if err != nil {
			// A single unclaimable candidate (a routed id whose bead was deleted,
			// one that no longer resolves in the store this context can reach, or a
			// transient write failure) must not wedge the whole hook. Record it and
			// try the next candidate. If none claim, claimsErrored makes the shared
			// drain report claims_errored instead of a healthy no_work so the write
			// failure stays visible; the work is reclaimed next tick (NDI) either way.
			fmt.Fprintf(stderr, "gc hook --claim: skipping %s: %v\n", candidate.ID, err) //nolint:errcheck
			claimsErrored = true
			continue
		}
		if !ok {
			reportHookClaimRejected(candidate, claimed, opts, ops)
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
		return hookClaimResult{terminal: true, code: writeHookClaimWorkResultForBead(result, claimed, opts, ops, dir, stdout, stderr)}
	}

	return hookClaimResult{claimsErrored: claimsErrored}
}

// doHookTriggerClaim resolves the exact trigger bead in its owning store, runs
// the same ownership/route checks as the generic claim path, and claims only
// that bead. If the trigger is gone, taken, misrouted, or lost to a concurrent
// claimant, the session drains instead of claiming unrelated generic work.
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
		fmt.Fprintf(stderr, "gc hook --claim: trigger bead %s not found; draining\n", triggerID) //nolint:errcheck
		return writeHookClaimNoWork(opts, ops, false, stdout, stderr)
	}

	status := strings.ToLower(strings.TrimSpace(bead.Status))
	if hookClaimHasIdentity(bead.Assignee, opts.IdentityCandidates) {
		reason := ""
		switch status {
		case "in_progress":
			reason = "existing_assignment"
		case "open":
			reason = "ready_assignment"
		}
		if reason != "" {
			result := hookClaimJSONResult{
				SchemaVersion: "1",
				OK:            true,
				Command:       hookClaimCommandName,
				Action:        "work",
				Reason:        reason,
				BeadID:        bead.ID,
				Assignee:      bead.Assignee,
				Route:         hookClaimRoute(bead),
			}
			return writeHookClaimWorkResultForBead(result, bead, opts, ops, triggerDir, stdout, stderr)
		}
		fmt.Fprintf(stderr, "gc hook --claim: trigger bead %s already mine but status=%q; draining\n", triggerID, status) //nolint:errcheck
		return writeHookClaimNoWork(opts, ops, false, stdout, stderr)
	}

	if strings.TrimSpace(bead.Assignee) != "" || status != "open" {
		fmt.Fprintf(stderr, "gc hook --claim: trigger bead %s not claimable (status=%q assignee=%q); draining\n", //nolint:errcheck
			triggerID, strings.TrimSpace(bead.Status), strings.TrimSpace(bead.Assignee))
		return writeHookClaimNoWork(opts, ops, false, stdout, stderr)
	}
	if !hookClaimMatchesRoute(bead, opts.RouteTargets) {
		fmt.Fprintf(stderr, "gc hook --claim: trigger bead %s routed to %q not in this session's targets; draining\n", //nolint:errcheck
			triggerID, hookClaimRoute(bead))
		return writeHookClaimNoWork(opts, ops, false, stdout, stderr)
	}

	claimed, ok, err := ops.Claim(ctx, triggerDir, opts.Env, triggerID, opts.Assignee)
	if err != nil {
		fmt.Fprintf(stderr, "gc hook --claim: claiming trigger %s: %v\n", triggerID, err) //nolint:errcheck
		return 1
	}
	if !ok {
		reportHookClaimRejected(bead, claimed, opts, ops)
		fmt.Fprintf(stderr, "gc hook --claim: trigger bead %s claimed by another session; draining\n", triggerID) //nolint:errcheck
		return writeHookClaimNoWork(opts, ops, false, stdout, stderr)
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

// hookCandidateClaimable reports whether a work-query candidate is eligible for a
// fresh claim: it has an id, is currently unassigned, and matches one of this
// session's route targets.
func hookCandidateClaimable(candidate beads.Bead, routeTargets []string) bool {
	return strings.TrimSpace(candidate.ID) != "" &&
		strings.TrimSpace(candidate.Assignee) == "" &&
		hookClaimMatchesRoute(candidate, routeTargets)
}

// reportHookClaimRejected publishes a bead.claim_rejected event (ADR-0009) when a
// claim was lost to a *different* live claimant. An empty or own-identity assignee
// means the winner is unknown or is us, so there is no rejection to report.
func reportHookClaimRejected(candidate, claimed beads.Bead, opts hookClaimOptions, ops hookClaimOps) {
	existing := strings.TrimSpace(claimed.Assignee)
	if existing == "" || hookClaimHasIdentity(claimed.Assignee, opts.IdentityCandidates) {
		return
	}
	ops.EmitClaimRejected(candidate.ID, existing, opts.Assignee)
}

func hookClaimExistingOrAssigned(candidates []beads.Bead, opts hookClaimOptions) (hookClaimJSONResult, beads.Bead, bool) {
	for _, candidate := range candidates {
		if hookClaimCandidateIsMessage(candidate) {
			continue
		}
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
		if hookClaimCandidateIsMessage(candidate) {
			continue
		}
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

func hookClaimCandidateIsMessage(candidate beads.Bead) bool {
	return strings.EqualFold(strings.TrimSpace(candidate.Type), "message")
}

func writeHookClaimWorkResultForBead(result hookClaimJSONResult, bead beads.Bead, opts hookClaimOptions, ops hookClaimOps, dir string, stdout, stderr io.Writer) int {
	stampHookWorkBranch(bead, opts, ops, dir, stderr)
	recordHookClaimSessionPointers(bead, opts, ops, dir, stderr)
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

// writeHookClaimNoWork writes the single drain result for a hook that claimed
// nothing. The reason is "no_work" for a genuinely idle store; it is
// "claims_errored" when claimsErrored is set — ready work existed but every
// eligible claim mutation errored — so an operational write failure stays
// distinguishable from idle even though both still drain and reclaim next tick.
func writeHookClaimNoWork(opts hookClaimOptions, ops hookClaimOps, claimsErrored bool, stdout, stderr io.Writer) int {
	reason := "no_work"
	if claimsErrored {
		reason = "claims_errored"
	}
	result := hookClaimJSONResult{
		SchemaVersion: "1",
		OK:            true,
		Command:       hookClaimCommandName,
		Action:        "drain",
		Reason:        reason,
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
	rootID := strings.TrimSpace(bead.Metadata[beadmeta.RootBeadIDMetadataKey])
	group := strings.TrimSpace(bead.Metadata[beadmeta.ContinuationGroupMetadataKey])
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
		// Claim conflict: re-read the bead so the caller can surface who won
		// the race in the bead.claim_rejected event (ADR-0009). Best-effort —
		// a read error degrades to a silent no-op (empty bead, no event).
		current, getErr := store.Get(beadID)
		if getErr != nil {
			return beads.Bead{}, false, nil
		}
		return current, false, nil
	}
	if !hookClaimHasIdentity(claimed.Assignee, []string{assignee}) {
		// bd reported a successful mutation but the bead is owned by another
		// claimant (stale projection / lost race). Return it as a non-claim so
		// the caller can report the rejection rather than treat it as ours.
		return claimed, false, nil
	}
	return claimed, true, nil
}

func hookResolveBeadWithBdStore(ctx context.Context, dir string, env []string, beadID string) (beads.Bead, bool, error) {
	store := hookClaimBdStoreContext(ctx, dir, env, "")
	bead, err := store.Get(beadID)
	if err != nil {
		if errors.Is(err, beads.ErrNotFound) {
			return beads.Bead{}, false, nil
		}
		return beads.Bead{}, false, err
	}
	return bead, true, nil
}

// stampHookWorkBranch records the claiming worker's git branch on the bead as
// gc.work_branch — the durable handle from the bead to its work that the close
// gate later reads (ADR-0009). Idempotent (skips when already current) and
// best-effort: a missing repo, detached HEAD, or write error never blocks the
// claim.
func stampHookWorkBranch(bead beads.Bead, opts hookClaimOptions, ops hookClaimOps, dir string, stderr io.Writer) {
	branch := strings.TrimSpace(ops.ResolveWorkBranch(dir))
	if branch == "" {
		return
	}
	if strings.TrimSpace(bead.Metadata[beadmeta.WorkBranchMetadataKey]) == branch {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), hookClaimMutationTimeout)
	defer cancel()
	if err := ops.StampWorkBranch(ctx, dir, opts.Env, bead.ID, opts.Assignee, branch); err != nil {
		fmt.Fprintf(stderr, "gc hook --claim: stamping work_branch on %s: %v\n", bead.ID, err) //nolint:errcheck
	}
}

func hookStampWorkBranchWithBdStore(_ context.Context, dir string, env []string, beadID, assignee, branch string) error {
	store := hookClaimBdStore(dir, env, assignee)
	return store.Update(beadID, beads.UpdateOpts{Metadata: map[string]string{beadmeta.WorkBranchMetadataKey: branch}})
}

// recordHookClaimRunID records, on the session bead named by GC_SESSION_ID, the
// run this session is now working: beadmeta.ResolveRunID of the just-claimed
// bead, the same resolver the usage-fact emitters use (internal/worker). Those
// emitters still resolve the run id from the session bead's own chain today;
// once the deferred reader (ga-2m8abf) consumes gc.current_run_id, a per-request
// reader of the session bead will yield the same run id the model and compute
// facts carry. A bead with no run chain resolves to its own id, so a
// standalone unit is its own run and is never misattributed to a previous run on
// this reused session bead. The write is unconditional on every claim by design:
// the run id is a current-pointer that must follow a reused pool session onto its
// new run, and the prior value isn't in hand here to guard against (only the work
// bead and session id are). The only in-process idempotence guard available to
// this subprocess is a pre-write read of the session bead — the controller's
// CachingStore value-match guard is unreachable from here — so on a reused
// session that re-stamps the same run id the cost is one redundant bd update and
// its bead.updated event per claim. That is an accepted cost: claims are far less
// frequent than the per-second no-op writes the CachingStore guard targets, and a
// guard here would only trade the write for an equally unconditional read. The
// write reuses the claiming assignee as the bd actor for parity with the
// work_branch stamp, so both claim-time stamps attribute identically. Best-effort:
// the bd write is bound to ctx, so a slow or stuck update cannot outlast
// hookClaimMutationTimeout, and a non-session run (no GC_SESSION_ID), a timeout,
// or a write error never blocks the claim.
func recordHookClaimSessionPointers(bead beads.Bead, opts hookClaimOptions, ops hookClaimOps, dir string, stderr io.Writer) {
	sessionBeadID := hookClaimSessionID(opts.Env)
	if sessionBeadID == "" {
		return
	}
	// Both pointers are derived from the SAME just-claimed work bead so the (run, step)
	// tuple is consistent: run_id is the bead's resolved run root; step_id is its bare
	// gc.step_id (the cross-plane join key the events plane also uses), empty when the
	// work has no formula step (ad-hoc/manual) — which clears any prior step.
	runID := beadmeta.ResolveRunID(bead.Metadata, bead.ID, sessionBeadID)
	stepID := strings.TrimSpace(bead.Metadata[beadmeta.StepIDMetadataKey])
	ctx, cancel := context.WithTimeout(context.Background(), hookClaimMutationTimeout)
	defer cancel()
	if err := ops.RecordSessionPointers(ctx, dir, opts.Env, opts.Assignee, sessionBeadID, runID, stepID); err != nil {
		fmt.Fprintf(stderr, "gc hook --claim: recording session pointers on session bead %s: %v\n", sessionBeadID, err) //nolint:errcheck
	}
}

func hookRecordSessionPointersWithBdStore(ctx context.Context, dir string, _ []string, assignee, sessionBeadID, runID, stepID string) error {
	cityDir, cityEnv, err := sessionPointerStoreEnv(ctx, dir, assignee)
	if err != nil {
		return fmt.Errorf("resolving city store env for session-pointer write: %w", err)
	}
	if cityEnv == nil {
		// The city store could not be confidently resolved, or ctx expired while
		// building the env. Session beads live only in the city store, so a write
		// to an unresolved/rig store would miss the bead; skipping this
		// best-effort bookkeeping is safe and avoids a park-triggering warning.
		return nil
	}
	store := beads.NewBdStore(cityDir, beads.ExecCommandRunnerWithEnvContext(ctx, cityEnv))
	return store.Update(sessionBeadID, beads.UpdateOpts{Metadata: map[string]string{
		beadmeta.CurrentRunIDMetadataKey:   runID,
		beadmeta.ActiveWorkBeadMetadataKey: stepID,
	}})
}

// sessionPointerStoreEnv resolves the CITY store dir and a CITY-scoped bd env
// for a session-pointer write, from the work bead's (possibly rig-scoped) store
// dir. Session beads live only in the city store, so the write must target the
// city store's env — not the rig env the hook runs under. The prior fix
// repointed only the dir (sessionPointerStoreDir) but reused the rig env, so
// BEADS_DIR/GC_RIG still pointed at the rig store and bd resolved the update
// there, missing the session bead ("bead not found"). bdRuntimeEnvWithError
// sets BEADS_DIR to the city .beads and clears GC_RIG/GC_RIG_ROOT; the exec
// runner merges it over the process env, so PATH/HOME and other ambient vars
// are preserved.
//
// It returns a nil env (a skip signal, with err == nil) in two guarded cases,
// both safe for this best-effort write:
//   - the city cannot be confirmed — sessionPointerStoreDir falls back to its
//     input when GC_CITY is unset and the store sits outside the city tree, and
//     writing the pointer there would silently target the wrong store; and
//   - ctx expires while building the env — bdRuntimeEnvWithError is synchronous
//     and can descend into a managed-Dolt recovery path during an outage, so
//     the write must not outlive the claim ctx on the hot claim path.
func sessionPointerStoreEnv(ctx context.Context, workBeadStoreDir, actor string) (string, map[string]string, error) {
	cityDir := sessionPointerStoreDir(workBeadStoreDir)
	if !isResolvedCityRoot(cityDir) {
		return cityDir, nil, nil
	}
	env, err := cityStoreEnvBounded(ctx, cityDir)
	if err != nil {
		return "", nil, err
	}
	if env == nil {
		return cityDir, nil, nil
	}
	if strings.TrimSpace(actor) != "" {
		env["BEADS_ACTOR"] = actor
	}
	return cityDir, env, nil
}

// isResolvedCityRoot reports whether dir is a real city root, using the SAME
// markers as the resolver (findCity / validateCityPath): the canonical city.toml
// OR the .gc/ runtime root. sessionPointerStoreDir returns its input unchanged
// when it cannot resolve a city, so this distinguishes a genuine city from that
// fallback — and matching the resolver's predicate avoids skipping a valid
// .gc/-only city (city roots are not required to carry city.toml).
func isResolvedCityRoot(dir string) bool {
	if strings.TrimSpace(dir) == "" {
		return false
	}
	return citylayout.HasCityConfig(dir) || citylayout.HasRuntimeRoot(dir)
}

// cityStoreEnvBounded builds the city bd env, abandoning it (nil, nil) if ctx
// expires first. bdRuntimeEnvWithError is synchronous and can descend into a
// managed-Dolt recovery path during an outage; this write is best-effort, so it
// must respect the claim ctx rather than block the hot claim path. The build
// runs in a goroutine with a buffered result channel, so an abandoned build
// completes and is garbage-collected without leaking.
func cityStoreEnvBounded(ctx context.Context, cityDir string) (map[string]string, error) {
	type envResult struct {
		env map[string]string
		err error
	}
	ch := make(chan envResult, 1)
	go func() {
		env, err := bdRuntimeEnvWithError(cityDir)
		ch <- envResult{env: env, err: err}
	}()
	select {
	case <-ctx.Done():
		return nil, nil
	case r := <-ch:
		return r.env, r.err
	}
}

// sessionPointerStoreDir resolves the store that holds SESSION beads (the city
// store) from the work bead's store dir. Session beads are created only in the
// city store, so a session-pointer update issued against a rig-scoped work
// bead's store misses the session bead entirely — surfacing as
// "recording session pointers on session bead <id>: bead not found" — whenever
// a pool worker claims a rig-scoped bead (the common case). Resolving the city
// store makes the pointer land where the session bead actually lives. City-scoped
// work claims are unaffected: cityForStoreDir is idempotent on a city dir.
func sessionPointerStoreDir(workBeadStoreDir string) string {
	return cityForStoreDir(workBeadStoreDir)
}

// hookClaimSessionID returns the session bead id (GC_SESSION_ID) from the claim
// env, the override-sanitized value the rest of the claim path uses; it is empty
// for a non-session run (cmd_hook.go blanks GC_SESSION_ID outside a session).
func hookClaimSessionID(env []string) string {
	sessionID := ""
	for _, entry := range env {
		if k, v, ok := strings.Cut(entry, "="); ok && k == "GC_SESSION_ID" {
			sessionID = v
		}
	}
	return strings.TrimSpace(sessionID)
}

// hookResolveWorkBranch returns the current git branch of dir, or "" when dir
// is not a worktree or HEAD is detached (no meaningful branch to stamp).
func hookResolveWorkBranch(dir string) string {
	if strings.TrimSpace(dir) == "" {
		return ""
	}
	out, err := exec.Command("git", "-C", dir, "rev-parse", "--abbrev-ref", "HEAD").Output()
	if err != nil {
		return ""
	}
	branch := strings.TrimSpace(string(out))
	if branch == "HEAD" { // detached HEAD
		return ""
	}
	return branch
}

// hookEmitClaimRejected publishes a best-effort bead.claim_rejected event to the
// city event log so a lost-claim race is observable for eval/audit (ADR-0009).
func hookEmitClaimRejected(beadID, existingClaimant, attemptedClaimant string) {
	payload, err := json.Marshal(events.BeadClaimRejectedPayload{
		BeadID:            beadID,
		ExistingClaimant:  existingClaimant,
		AttemptedClaimant: attemptedClaimant,
	})
	if err != nil {
		return
	}
	rec := openCityRecorder(io.Discard)
	rec.Record(events.Event{
		Type:    events.BeadClaimRejected,
		Actor:   attemptedClaimant,
		Subject: beadID,
		Payload: payload,
	})
	if closer, ok := rec.(io.Closer); ok {
		_ = closer.Close()
	}
}

func hookListContinuationWithBdStore(_ context.Context, dir string, env []string, rootID, group string) ([]beads.Bead, error) {
	store := hookClaimBdStore(dir, env, "")
	return store.List(beads.ListQuery{
		Status: "open",
		Metadata: map[string]string{
			beadmeta.RootBeadIDMetadataKey:        rootID,
			beadmeta.ContinuationGroupMetadataKey: group,
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
	return hookClaimBdStoreContext(context.Background(), dir, env, actor)
}

// hookClaimBdStoreContext is hookClaimBdStore with its bd commands bound to ctx,
// so a best-effort claim-time write cannot outlast the caller's deadline even if
// the underlying bd update stalls.
func hookClaimBdStoreContext(ctx context.Context, dir string, env []string, actor string) *beads.BdStore {
	return beads.NewBdStore(dir, beads.ExecCommandRunnerWithEnvContext(ctx, hookClaimEnvMap(env, dir, actor)))
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
	routedTo := strings.TrimSpace(candidate.Metadata[beadmeta.RoutedToMetadataKey])
	runTarget := strings.TrimSpace(candidate.Metadata[beadmeta.RunTargetMetadataKey])
	kind := strings.TrimSpace(candidate.Metadata[beadmeta.KindMetadataKey])
	for _, target := range routeTargets {
		target = strings.TrimSpace(target)
		if target == "" {
			continue
		}
		if routedTo == target {
			return true
		}
		if routedTo == "" && kind == beadmeta.KindWorkflow && runTarget == target {
			return true
		}
	}
	return false
}

func hookClaimRoute(candidate beads.Bead) string {
	if routedTo := strings.TrimSpace(candidate.Metadata[beadmeta.RoutedToMetadataKey]); routedTo != "" {
		return routedTo
	}
	if strings.TrimSpace(candidate.Metadata[beadmeta.KindMetadataKey]) == beadmeta.KindWorkflow {
		return strings.TrimSpace(candidate.Metadata[beadmeta.RunTargetMetadataKey])
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
