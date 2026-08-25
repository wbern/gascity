package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/bddispatch"
	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/internal/events"
)

const hookClaimCommandName = "hook"

// Drain-action reasons for the gc hook --claim result contract
// (schemas/hook/result.schema.json). Every value here is a valid reason when
// action is "drain": an idle store, an operational claim-write failure, or a
// refused stale session.
const (
	hookClaimReasonNoWork        = "no_work"
	hookClaimReasonClaimsErrored = "claims_errored"
	hookClaimReasonStaleSession  = "stale_session"
)

var hookClaimMutationTimeout = 10 * time.Second

var hookClaimCommandRunnerWithEnvContext = managedHookClaimCommandRunnerWithEnvContext

// managedHookClaimCommandRunnerWithEnvContext makes hook-owned bd reads select
// the city bdshim from GC_BIN directly. T3/Codex preserves GC_BIN and
// GC_BD_REAL but intentionally rebuilds PATH, so PATH lookup can otherwise run
// raw bd while the hook enables shim-only flags such as --allow-unbounded.
func managedHookClaimCommandRunnerWithEnvContext(ctx context.Context, env map[string]string) beads.CommandRunner {
	runner := beads.ExecCommandRunnerWithEnvContext(ctx, env)
	return func(dir, name string, args ...string) ([]byte, error) {
		return runner(dir, hookClaimCommandPath(name, env), args...)
	}
}

func hookClaimCommandPath(name string, env map[string]string) string {
	if name != "bd" || strings.TrimSpace(env[citylayout.RealBdEnvVar]) == "" {
		return name
	}
	gcBin := strings.TrimSpace(env["GC_BIN"])
	shimDir := filepath.Dir(gcBin)
	if !filepath.IsAbs(gcBin) || filepath.Base(shimDir) != "shimbin" {
		return name
	}
	return filepath.Join(shimDir, "bd")
}

type hookClaimOptions struct {
	Context            context.Context
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
	// repo / detached HEAD) omits the branch key — the session back-reference is
	// still stamped.
	ResolveWorkBranch hookResolveWorkBranchFunc
	// StampWorkMeta writes the claim-time execution-identity metadata patch
	// (gc.work_branch and/or the durable session back-reference gc.session_id /
	// gc.session_name) onto the claimed bead in ONE update. Best-effort.
	StampWorkMeta hookStampWorkMetaFunc
	// PublishRunMap writes best-effort session-to-run correlation without
	// mutating the session bead after a successful work claim.
	PublishRunMap hookPublishRunMapFunc
	Now           func() time.Time
}

type (
	hookClaimFunc              func(context.Context, string, []string, string, string) (beads.Bead, bool, error)
	hookResolveBeadFunc        func(context.Context, string, []string, string) (beads.Bead, bool, error)
	hookListContinuationFunc   func(context.Context, string, []string, string, string) ([]beads.Bead, error)
	hookAssignContinuationFunc func(context.Context, string, []string, string, string) error
	hookDrainAckFunc           func(io.Writer) error
	hookEmitClaimRejectedFunc  func(beadID, existingClaimant, attemptedClaimant string)
	hookResolveWorkBranchFunc  func(dir string) string
	hookStampWorkMetaFunc      func(ctx context.Context, dir string, env []string, beadID, assignee string, patch map[string]string) error
	hookPublishRunMapFunc      func(runID, beadID string, sessionKeys ...string) error
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
	RootBeadID           string   `json:"root_bead_id,omitempty"`
	ContinuationGroup    string   `json:"continuation_group,omitempty"`
	ContinuationAssigned []string `json:"continuation_assigned,omitempty"`
	DrainAcknowledged    bool     `json:"drain_acknowledged,omitempty"`
}

// writeHookClaimJSON stages a managed hook result before publishing one JSON
// line. It intentionally runs after every claim-side mutation has completed so
// output admission cannot turn a successful claim into a retryable mutation.
func writeHookClaimJSON(ctx context.Context, stdout, stderr io.Writer, result hookClaimJSONResult) error {
	if !bddispatch.ManagedOutputFirewallActive("hook") {
		return writeCLIJSONLine(stdout, result)
	}
	if code := bddispatch.WriteManagedJSON(ctx, "managed_hook_claim", "hook", result, stdout, stderr); code != 0 {
		return errors.New("writing managed hook JSON")
	}
	return nil
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
	now := time.Now
	if ops.Now != nil {
		now = ops.Now
	}

	// The ready projection is fetched at most ONCE per invocation and shared by
	// the trigger readiness check below and the pool loop further down. The
	// trigger path runs first (it must keep its priority over pool work), so the
	// fetch is lazy rather than hoisted: a session with no trigger pays nothing,
	// and a session with one pays a single query, not two.
	ready := &hookReadyProjection{workQuery: workQuery, dir: dir, ops: ops, now: now}

	triggerErrored := false
	if triggerID := strings.TrimSpace(opts.TriggerBeadID); triggerID != "" {
		res := doHookTriggerClaim(triggerID, dir, *opts, *ops, stdout, stderr)
		if res.terminal {
			return res
		}
		// Trigger was not claimable. Do NOT drain: the pool query below is route
		// scoped and may hold work this session can legitimately take.
		triggerErrored = res.claimsErrored
	}
	if ops.Runner == nil {
		fmt.Fprintln(stderr, "gc hook --claim: missing work query runner") //nolint:errcheck
		return hookClaimResult{terminal: true, code: 1}
	}

	preFilter, normalized, err := ready.fetch()
	if err != nil {
		fmt.Fprintf(stderr, "gc hook --claim: %v\n", err) //nolint:errcheck
		return hookClaimResult{terminal: true, code: 1}
	}
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
		return hookClaimResult{claimsErrored: triggerErrored}
	}
	candidates, skipped, err := decodeHookClaimBeads(normalized)
	if err != nil {
		fmt.Fprintf(stderr, "gc hook --claim: requires JSON work_query output to identify claim candidates: %v\n", err) //nolint:errcheck
		return hookClaimResult{terminal: true, code: 1}
	}
	for _, skip := range skipped {
		fmt.Fprintf(stderr, "gc hook --claim: skipping undecodable bead %s: %v\n", skip.ID, skip.Err) //nolint:errcheck
	}
	if len(candidates) == 0 {
		return hookClaimResult{claimsErrored: triggerErrored}
	}

	if result, bead, ok := hookClaimExistingAssignment(candidates, *opts); ok {
		return hookClaimResult{terminal: true, code: writeHookClaimWorkResultForBead(result, bead, *opts, *ops, dir, stdout, stderr)}
	}

	readyResult := claimFirstReadyHookAssignment(candidates, *opts, *ops, dir, stdout, stderr)
	if readyResult.terminal {
		return readyResult
	}
	eligible := claimFirstEligibleHookCandidate(candidates, *opts, *ops, dir, stdout, stderr)
	if !eligible.terminal && triggerErrored {
		eligible.claimsErrored = true
	}
	return eligible
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
	if ops.StampWorkMeta == nil {
		ops.StampWorkMeta = hookStampWorkMetaWithBdStore
	}
	if ops.PublishRunMap == nil {
		ops.PublishRunMap = writeRunMap
	}
}

// claimFirstReadyHookAssignment atomically promotes the first open candidate
// already assigned to this session. Continuation preassignment deliberately
// leaves later group members open, so a resumed session must still run the
// store's idempotent claim mutation before it reports the bead as workable.
func claimFirstReadyHookAssignment(candidates []beads.Bead, opts hookClaimOptions, ops hookClaimOps, dir string, stdout, stderr io.Writer) hookClaimResult {
	ctx, cancel := context.WithTimeout(context.Background(), hookClaimMutationTimeout)
	defer cancel()
	for _, candidate := range candidates {
		if strings.TrimSpace(candidate.ID) == "" ||
			hookClaimCandidateIsMessage(candidate) ||
			!strings.EqualFold(strings.TrimSpace(candidate.Status), "open") ||
			!hookClaimHasIdentity(candidate.Assignee, opts.IdentityCandidates) {
			continue
		}
		if ctx.Err() != nil {
			fmt.Fprintf(stderr, "gc hook --claim: ready assignment %s claim deadline exhausted: %v\n", candidate.ID, ctx.Err()) //nolint:errcheck
			return hookClaimResult{terminal: true, code: 1}
		}
		// Use the bead's current own-identity assignee as the claim actor.
		// BEADS_ACTOR may be represented by the runtime name, session bead id,
		// or alias; bd's idempotent --claim path requires the actor to match the
		// existing assignee exactly.
		claimActor := strings.TrimSpace(candidate.Assignee)
		claimed, ok, err := ops.Claim(ctx, dir, opts.Env, candidate.ID, claimActor)
		if err != nil {
			if ok {
				fmt.Fprintf(stderr, "gc hook --claim: claimed %s but loading canonical bead failed: %v\n", candidate.ID, err) //nolint:errcheck
			} else {
				fmt.Fprintf(stderr, "gc hook --claim: promoting ready assignment %s: %v\n", candidate.ID, err) //nolint:errcheck
			}
			// This session already owns the bead. Do not skip it and claim
			// unrelated fresh work after an operational mutation failure.
			return hookClaimResult{terminal: true, code: 1}
		}
		// Deliberately unlike the err != nil branch above: a rejected claim is a
		// lost race, not an operational failure. Another claimant genuinely owns
		// the bead, so ownership is resolved and this session is free to fall
		// through to other routed work. A mutation failure leaves ownership
		// unresolved, so that branch fails closed instead.
		if !ok {
			reportHookClaimRejected(candidate, claimed, opts, ops)
			continue
		}
		if !strings.EqualFold(strings.TrimSpace(claimed.Status), "in_progress") ||
			strings.TrimSpace(claimed.Assignee) != claimActor {
			_, _ = fmt.Fprintf(
				stderr,
				"gc hook --claim: ready assignment %s claim readback remained status=%q assignee=%q; want in_progress owned by this session\n",
				candidate.ID,
				claimed.Status,
				claimed.Assignee,
			)
			return hookClaimResult{terminal: true, code: 1}
		}
		claimed = mergeHookClaimCandidateMetadata(candidate, claimed)
		result := hookClaimJSONResult{
			SchemaVersion: "1",
			OK:            true,
			Command:       hookClaimCommandName,
			Action:        "work",
			Reason:        "ready_assignment",
			BeadID:        claimed.ID,
			Assignee:      claimed.Assignee,
			Route:         hookClaimRoute(claimed),
		}
		if result.BeadID == "" {
			result.BeadID = candidate.ID
		}
		if result.Assignee == "" {
			result.Assignee = claimActor
		}
		return hookClaimResult{terminal: true, code: writeHookClaimWorkResultForBead(result, claimed, opts, ops, dir, stdout, stderr)}
	}
	return hookClaimResult{}
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
			if ok {
				// The atomic mutation committed, but its canonical readback failed.
				// Stop immediately: trying another candidate or draining would strand
				// the assignment while falsely reporting idle work.
				fmt.Fprintf(stderr, "gc hook --claim: claimed %s but loading canonical bead failed: %v\n", candidate.ID, err) //nolint:errcheck
				return hookClaimResult{terminal: true, code: 1}
			}
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
		claimed = mergeHookClaimCandidateMetadata(candidate, claimed)
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
// that bead.
//
// WHEN THE TRIGGER IS UNCLAIMABLE IT NOW FALLS THROUGH to the generic pool query
// instead of draining (gci-n1u2 / crm-72mmp2). It used to drain, which was safe in
// isolation and starved a queue in practice: the trigger id is materialized into
// the process env at SPAWN and never re-read, while the reconciler keeps
// re-pointing gc.trigger_bead_id on the session bead as work arrives. A worker
// whose trigger was legitimately parked therefore attempted one bead it could never
// claim, drained, and reported "nothing to do" on every wake — for 5h on GC3
// 2026-08-08, while two beads routed to it sat open and unassigned and the pool sat
// at its capacity of 1.
//
// Falling through is SAFE, and narrower than it looks: the generic path is already
// route-scoped (hookCandidateClaimable requires a gc.routed_to match) so a worker
// still cannot pick up unrelated work. The isolation this path was protecting
// (ga-80pen8) is about IdentityCandidates ADOPTING a named holder's in_progress
// bead, which is a different mechanism and is untouched.
//
// A terminal result means the trigger was handled (claimed, adopted, or a hard
// error). A non-terminal result means "not claimable — try the pool".
// hookReadyProjection memoizes the one work-query round trip per invocation.
// fetch returns the raw normalized output and the readiness-filtered output;
// both callers (trigger readiness, pool loop) get the same snapshot, so they
// cannot disagree about what was ready at this instant — which is the whole
// point of gci-pha.
type hookReadyProjection struct {
	workQuery string
	dir       string
	ops       *hookClaimOps
	now       func() time.Time

	done       bool
	preFilter  string
	normalized string
	err        error
}

func (r *hookReadyProjection) fetch() (string, string, error) {
	if r.done {
		return r.preFilter, r.normalized, r.err
	}
	r.done = true
	if r.ops == nil || r.ops.Runner == nil {
		r.err = errors.New("missing work query runner")
		return r.preFilter, r.normalized, r.err
	}
	output, err := r.ops.Runner(r.workQuery, r.dir)
	if err != nil {
		r.err = err
		return r.preFilter, r.normalized, r.err
	}
	r.preFilter = normalizeWorkQueryOutput(strings.TrimSpace(output))
	r.normalized = filterUnreadyHookCandidates(r.preFilter, r.now())
	return r.preFilter, r.normalized, nil
}

// hookBeadBlockedReason reports why a RESOLVED bead is not claimable, or "" when
// nothing blocks it. This is the typed half of the shared readiness predicate:
// isDepBlockedHookCandidate applies the same rule to the map-shaped ready
// projection, and both now agree that a non-closed blocking dependency parks its
// successor.
//
// Only ready-blocking dependency types count. A "tracks" or "relates-to" edge
// describes a bead, it does not gate it, and treating those as blockers would
// park work the graph never intended to serialize.
func hookBeadBlockedReason(bead beads.Bead) string {
	for _, dep := range bead.Dependencies {
		blocker := strings.TrimSpace(dep.DependsOnID)
		if blocker == "" {
			continue
		}
		if depType := strings.TrimSpace(dep.Type); depType != "" && !beads.IsReadyBlockingDependencyType(depType) {
			continue
		}
		// An UNKNOWN blocker status fails closed. bd's nested projection always
		// carries one; an absent status means we are looking at a shape we do not
		// understand, and claiming past a dependency we cannot evaluate is the
		// exact failure this bead exists to stop.
		if status := strings.TrimSpace(dep.Status); !strings.EqualFold(status, "closed") {
			if status == "" {
				status = "unknown"
			}
			return fmt.Sprintf("blocked by %s (status=%s)", blocker, status)
		}
	}
	return ""
}

func doHookTriggerClaim(triggerID, dir string, opts hookClaimOptions, ops hookClaimOps, stdout, stderr io.Writer) hookClaimResult {
	triggerDir := strings.TrimSpace(opts.TriggerStoreDir)
	if triggerDir == "" {
		triggerDir = dir
	}
	ctx, cancel := context.WithTimeout(context.Background(), hookClaimMutationTimeout)
	defer cancel()

	bead, found, err := ops.ResolveBead(ctx, triggerDir, opts.Env, triggerID)
	if err != nil {
		fmt.Fprintf(stderr, "gc hook --claim: resolving trigger bead %s: %v\n", triggerID, err) //nolint:errcheck
		return hookClaimResult{terminal: true, code: 1}
	}
	if !found {
		fmt.Fprintf(stderr, "gc hook --claim: trigger bead %s not found; falling through to the pool\n", triggerID) //nolint:errcheck
		return hookClaimResult{}
	}
	if beadHasDispatchHold(bead) {
		fmt.Fprintf(stderr, "gc hook --claim: trigger bead %s is held; falling through to the pool\n", triggerID) //nolint:errcheck
		return hookClaimResult{}
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
			return hookClaimResult{terminal: true, code: writeHookClaimWorkResultForBead(result, bead, opts, ops, triggerDir, stdout, stderr)}
		}
		fmt.Fprintf(stderr, "gc hook --claim: trigger bead %s already mine but status=%q; falling through to the pool\n", triggerID, status) //nolint:errcheck
		return hookClaimResult{}
	}

	if strings.TrimSpace(bead.Assignee) != "" || status != "open" {
		// A PEER HOLDING IT AND A PARK ARE NOT THE SAME EVENT, and only one of them
		// should free this session to take other routed work.
		//
		//   assignee set   -> another worker claimed it. The demand that spawned this
		//                     session is being served; going back to sleep is correct,
		//                     and taking other pool work here would let a per-demand
		//                     spawn drift onto work a different spawn exists for.
		//   assignee empty -> the bead is PARKED (First Line parks awaiting a human
		//                     with `--status blocked --assignee ""`). Nobody is working
		//                     it and nobody will until a human replies, so draining
		//                     pins this worker against a bead that cannot move while
		//                     route-matched work waits. That is the 5h GC3 stall.
		if strings.TrimSpace(bead.Assignee) != "" {
			fmt.Fprintf(stderr, "gc hook --claim: trigger bead %s not claimable (status=%q assignee=%q); draining\n", //nolint:errcheck
				triggerID, strings.TrimSpace(bead.Status), strings.TrimSpace(bead.Assignee))
			return hookClaimResult{terminal: true, code: writeHookClaimNoWork(opts, ops, false, stdout, stderr)}
		}
		fmt.Fprintf(stderr, "gc hook --claim: trigger bead %s is parked (status=%q, unassigned); falling through to the pool\n", //nolint:errcheck
			triggerID, strings.TrimSpace(bead.Status))
		return hookClaimResult{}
	}
	if !hookClaimMatchesRoute(bead, opts.RouteTargets) {
		fmt.Fprintf(stderr, "gc hook --claim: trigger bead %s routed to %q not in this session's targets; falling through to the pool\n", //nolint:errcheck
			triggerID, hookClaimRoute(bead))
		return hookClaimResult{}
	}

	// READINESS. Until gci-pha this path ran exactly four checks — exists,
	// already-mine, unassigned+open, route — and then claimed. None of them
	// looked at dependencies, so candidate SELECTION and the store's close gate
	// disagreed: on GC3 2026-08-12T00:25Z a session claimed the graph.v2
	// self-review step crm-q6zfr9 while its Investigate blocker crm-e9jahp was
	// still open, and the close was refused with "blocked by open issues". The
	// generic discovery path had filtered that same bead out all along.
	//
	// Falling through rather than draining is deliberate and costs no liveness:
	// the pool query below is route-scoped, so a session refused its own trigger
	// still takes the next genuinely ready bead on its route.
	if reason := hookBeadBlockedReason(bead); reason != "" {
		fmt.Fprintf(stderr, "gc hook --claim: trigger bead %s is not ready (%s); falling through to the pool\n", //nolint:errcheck
			triggerID, reason)
		return hookClaimResult{}
	}
	// NO READY-PROJECTION CROSS-CHECK HERE, deliberately. Consulting it would be
	// a useful second opinion — the projection reads blocked_by/is_blocked, which
	// `bd show` does not emit at all — but it costs a work query on EVERY trigger
	// claim, and the trigger path is contractually cheap: a trigger-scoped claim
	// must resolve and claim one bead without running discovery at all
	// (TestClaimHookWorkTargetsTriggerBeforeFederatedDiscovery fails the runner
	// outright if it is called). The typed predicate above is the fix; this would
	// only have been belt and braces, and not at the price of a query per wake.

	claimed, ok, err := ops.Claim(ctx, triggerDir, opts.Env, triggerID, opts.Assignee)
	if err != nil {
		fmt.Fprintf(stderr, "gc hook --claim: claiming trigger %s: %v; falling through to the pool\n", triggerID, err) //nolint:errcheck
		return hookClaimResult{claimsErrored: true}
	}
	if !ok {
		reportHookClaimRejected(bead, claimed, opts, ops)
		fmt.Fprintf(stderr, "gc hook --claim: trigger bead %s claimed by another session; falling through to the pool\n", triggerID) //nolint:errcheck
		return hookClaimResult{}
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
	return hookClaimResult{terminal: true, code: writeHookClaimWorkResultForBead(result, claimed, opts, ops, triggerDir, stdout, stderr)}
}

// mergeHookClaimCandidateMetadata retains work-query metadata when bd update
// --claim returns only a partial projection, while preferring canonical values
// returned by the mutation.
func mergeHookClaimCandidateMetadata(candidate, claimed beads.Bead) beads.Bead {
	if len(candidate.Metadata) == 0 {
		return claimed
	}
	metadata := maps.Clone(candidate.Metadata)
	maps.Copy(metadata, claimed.Metadata)
	claimed.Metadata = metadata
	return claimed
}

// hookCandidateClaimable reports whether a work-query candidate is eligible for a
// fresh claim: it has an id, is currently unassigned, and matches one of this
// session's route targets.
func hookCandidateClaimable(candidate beads.Bead, routeTargets []string) bool {
	return strings.TrimSpace(candidate.ID) != "" &&
		strings.TrimSpace(candidate.Assignee) == "" &&
		!beadHasDispatchHold(candidate) &&
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

func hookClaimExistingAssignment(candidates []beads.Bead, opts hookClaimOptions) (hookClaimJSONResult, beads.Bead, bool) {
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
	return hookClaimJSONResult{}, beads.Bead{}, false
}

// hookClaimCandidateIsMessage reports whether candidate is a mail message
// bead (issue_type="message"). Mail is read, not claimed as work: a message
// bead addressed to this session's identity has the same
// assignee-matches-identity shape as a real existing/ready assignment, so
// without this check it was returned by the existing/ready-assignment paths as work
// ahead of any real routed work waiting in the same batch (#4419) -- not by
// race, by construction, since this function runs before
// claimFirstEligibleHookCandidate ever sees the routed candidates.
func hookClaimCandidateIsMessage(candidate beads.Bead) bool {
	return strings.EqualFold(strings.TrimSpace(candidate.Type), "message")
}

func writeHookClaimWorkResultForBead(result hookClaimJSONResult, bead beads.Bead, opts hookClaimOptions, ops hookClaimOps, dir string, stdout, stderr io.Writer) int {
	result.RootBeadID = strings.TrimSpace(bead.Metadata[beadmeta.RootBeadIDMetadataKey])
	result.ContinuationGroup = strings.TrimSpace(bead.Metadata[beadmeta.ContinuationGroupMetadataKey])
	stampHookClaimIdentity(bead, opts, ops, dir, stderr)
	publishHookClaimRunMap(bead, opts, ops, stderr)
	assigned, err := preassignHookContinuationGroup(bead, opts, ops, dir)
	if err != nil {
		fmt.Fprintf(stderr, "gc hook --claim: preassigning continuation group for %s: %v\n", bead.ID, err) //nolint:errcheck
		return 1
	}
	result.ContinuationAssigned = assigned
	if opts.JSON {
		ctx := opts.Context
		if ctx == nil {
			ctx = context.Background()
		}
		if err := writeHookClaimJSON(ctx, stdout, stderr, result); err != nil {
			fmt.Fprintf(stderr, "gc hook --claim: writing JSON after completed claim: %v\n", err) //nolint:errcheck
			return 0
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
	reason := hookClaimReasonNoWork
	if claimsErrored {
		reason = hookClaimReasonClaimsErrored
	}
	return writeHookClaimDrain(reason, opts.JSON, opts.DrainAck, ops.DrainAck, stdout, stderr)
}

// writeHookClaimStaleSessionDrain emits the terminal result for a refused stale
// session (closed, superseded instance token, or a dormant/terminal state) that
// must stop instead of claiming. It preserves the gc hook --claim result
// contract: a --json caller gets a schema-backed drain record (action "drain",
// reason "stale_session"), and --drain-ack is honored, so a startup wrapper
// acknowledges drain and exits cleanly rather than seeing a bare exit 1 and
// retrying the refusal forever.
func writeHookClaimStaleSessionDrain(opts hookCommandOptions, stdout, stderr io.Writer) int {
	return writeHookClaimDrain(hookClaimReasonStaleSession, opts.JSON, opts.DrainAck, hookRuntimeDrainAck, stdout, stderr)
}

// writeHookClaimDrain writes the single structured drain result shared by every
// terminal no-claim outcome: an idle no-work store, a claims-errored store, and a
// refused stale session. For a --json caller it emits the schema-backed drain
// line; when drainAck is set it first runs drainAckFn and marks the result
// acknowledged. The exit code mirrors the historical contract — 0 once drain is
// acknowledged, else 1 — so a non-drain-ack caller still reports action=drain
// (a completed drain) rather than a bare failure.
func writeHookClaimDrain(reason string, jsonOut, drainAck bool, drainAckFn hookDrainAckFunc, stdout, stderr io.Writer) int {
	result := hookClaimJSONResult{
		SchemaVersion: "1",
		OK:            true,
		Command:       hookClaimCommandName,
		Action:        "drain",
		Reason:        reason,
	}
	if drainAck {
		if err := drainAckFn(stderr); err != nil {
			fmt.Fprintf(stderr, "gc hook --claim: drain-ack failed: %v\n", err) //nolint:errcheck
			return 1
		}
		result.DrainAcknowledged = true
	}
	if jsonOut {
		if err := writeHookClaimJSON(context.Background(), stdout, stderr, result); err != nil {
			fmt.Fprintf(stderr, "gc hook --claim: writing JSON: %v\n", err) //nolint:errcheck
			return 1
		}
	}
	if drainAck {
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

func hookClaimWithBdStore(ctx context.Context, dir string, env []string, beadID, assignee string) (beads.Bead, bool, error) {
	store := hookClaimBdStoreContext(ctx, dir, env, assignee)
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
	canonical, err := store.Get(beadID)
	if err != nil {
		// A withheld re-read says nothing about the bead: the claim above
		// already committed and its identity is verified, so report it rather
		// than stranding it in_progress behind a failure the caller must treat
		// as terminal. Every other read failure stays fatal (gcw-qap3.16).
		if errors.Is(err, beads.ErrOutputTruncated) {
			return claimed, true, nil
		}
		return claimed, true, fmt.Errorf("reloading claimed bead %q: %w", beadID, err)
	}
	if !hookClaimHasIdentity(canonical.Assignee, []string{assignee}) {
		return canonical, false, nil
	}
	return canonical, true, nil
}

// hookResolveBeadWithBdStore reads one bead by id from the store at dir. A
// missing bead is reported as found=false rather than an error, so the
// trigger-claim path can drain on a vanished trigger instead of failing.
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

// stampHookClaimIdentity records the claiming worker's execution identity on the
// claimed bead in ONE metadata write: gc.work_branch (the durable handle from the
// bead to its work that the close gate later reads, ADR-0009) plus the durable
// session back-reference gc.session_id / gc.session_name (#2843) so the dashboard
// run-detail can resolve which session executed a pool step after the transient
// Assignee is cleared on close. graphroute leaves pool steps unbound at route time,
// deferring the session binding to this claim (graphroute.go:200-203).
//
// The patch is compare-and-skipped against the bead's current metadata and the
// write is issued only when at least one key actually changes: this runs again on
// every hook tick via the existing_assignment / ready_assignment adoption paths, so
// an unconditional write would emit a bead.updated per tick per in-progress bead
// (the cache-reconcile flood class). Best-effort: a missing repo, detached HEAD,
// absent session, or write error never blocks the claim.
func stampHookClaimIdentity(bead beads.Bead, opts hookClaimOptions, ops hookClaimOps, dir string, stderr io.Writer) {
	patch := hookClaimIdentityPatch(bead, opts, ops, dir)
	if len(patch) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), hookClaimMutationTimeout)
	defer cancel()
	if err := ops.StampWorkMeta(ctx, dir, opts.Env, bead.ID, opts.Assignee, patch); err != nil {
		fmt.Fprintf(stderr, "gc hook --claim: stamping execution identity on %s: %v\n", bead.ID, err) //nolint:errcheck
	}
}

// hookClaimIdentityPatch builds the compare-and-skipped claim-time metadata patch.
// It carries gc.work_branch when the worktree resolves a branch that differs from
// the bead's, and the session back-reference gc.session_id / gc.session_name when
// this is a session-run claim (GC_SESSION_ID present) of a non-control bead and the
// values differ. Session identity is stamped even when the branch is empty — a
// session with no worktree still needs its back-reference — but never on control
// beads, which stay session-free by graphroute's design
// (ApplyGraphControlRouteBinding), even when a control-dispatcher session claims one
// through this same hook path. An empty result means every key is already current,
// so the caller issues no write.
func hookClaimIdentityPatch(bead beads.Bead, opts hookClaimOptions, ops hookClaimOps, dir string) map[string]string {
	patch := map[string]string{}
	if branch := strings.TrimSpace(ops.ResolveWorkBranch(dir)); branch != "" &&
		strings.TrimSpace(bead.Metadata[beadmeta.WorkBranchMetadataKey]) != branch {
		patch[beadmeta.WorkBranchMetadataKey] = branch
	}
	if sessionID := hookClaimSessionID(opts.Env); sessionID != "" &&
		!beadmeta.IsControlKind(strings.TrimSpace(bead.Metadata[beadmeta.KindMetadataKey])) {
		if strings.TrimSpace(bead.Metadata[beadmeta.SessionIDMetadataKey]) != sessionID {
			patch[beadmeta.SessionIDMetadataKey] = sessionID
		}
		if sessionName := hookClaimSessionName(opts.Env); sessionName != "" &&
			strings.TrimSpace(bead.Metadata[beadmeta.SessionNameMetadataKey]) != sessionName {
			patch[beadmeta.SessionNameMetadataKey] = sessionName
		}
	}
	return patch
}

func hookStampWorkMetaWithBdStore(_ context.Context, dir string, env []string, beadID, assignee string, patch map[string]string) error {
	store := hookClaimBdStore(dir, env, assignee)
	return store.Update(beadID, beads.UpdateOpts{Metadata: patch})
}

// publishHookClaimRunMap publishes the claimed bead's resolved run ID for the
// external proxy correlation path. It deliberately does not decorate the
// session bead: bd's fuzzy ID resolver can redirect a post-claim update to a
// prefix-colliding session if the intended session disappears concurrently.
// The run map is independent, best-effort telemetry and preserves useful
// correlation without issuing that unsafe second store mutation.
func publishHookClaimRunMap(bead beads.Bead, opts hookClaimOptions, ops hookClaimOps, stderr io.Writer) {
	sessionBeadID := hookClaimSessionID(opts.Env)
	if sessionBeadID == "" {
		return
	}
	runID := beadmeta.ResolveRunID(bead.Metadata, bead.ID, sessionBeadID)
	if err := ops.PublishRunMap(runID, bead.ID,
		hookClaimEnvValue(opts.Env, "GC_SESSION_NAME"),
		sessionBeadID,
		hookClaimEnvValue(opts.Env, "BEADS_ACTOR")); err != nil {
		fmt.Fprintf(stderr, "gc hook --claim: publishing run-map for session %s: %v\n", sessionBeadID, err) //nolint:errcheck
	}
}

// hookClaimSessionID returns the session bead id (GC_SESSION_ID) from the claim
// env, the override-sanitized value the rest of the claim path uses; it is empty
// for a non-session run (cmd_hook.go blanks GC_SESSION_ID outside a session).
func hookClaimSessionID(env []string) string {
	return hookClaimEnvValue(env, "GC_SESSION_ID")
}

// hookClaimEnvValue returns the last value of key in the claim env (trimmed),
// the same KEY=VALUE scan the rest of the claim path uses.
func hookClaimEnvValue(env []string, key string) string {
	val := ""
	for _, entry := range env {
		if k, v, ok := strings.Cut(entry, "="); ok && k == key {
			val = v
		}
	}
	return strings.TrimSpace(val)
}

// sanitizeRunMapKey maps a session key to its run-map filename stem: keep
// [A-Za-z0-9._-], replace every other rune with '_'. It is byte-identical to
// the manifold proxy's sanitizeSession (gc-manifold-proxy.go) — the
// cross-process contract: runMapFileName appends ".json" to this stem and the
// proxy opens exactly that name. The stem is intentionally lossy (distinct keys
// such as "a/b" and "a_b" share it), which is safe because the proxy resolves a
// session by a single structured key — the x-manifold-affinity gc session name
// — whose realistic collision surface is nil, not by the wider key set the
// writer also publishes.
func sanitizeRunMapKey(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
			return r
		default:
			return '_'
		}
	}, s)
}

// runMapFileName is the filename (no directory) a session key publishes under.
// It is the cross-process contract with the manifold proxy: the proxy reads
// sanitizeSession(affinity)+".json" (gc-manifold-proxy.go), so this MUST be
// sanitizeRunMapKey(key)+".json" byte-for-byte or the proxy's ReadFile misses
// and X-Gc-Run-Id is never stamped. The proxy resolves exactly one key per
// request — the x-manifold-affinity header, i.e. the gc session name — so the
// only collision that could clobber run attribution is two live sessions whose
// names sanitize identically, which does not happen for real structured session
// names. A consumer that looks a session up by key MUST apply this identical
// transform.
func runMapFileName(key string) string {
	return sanitizeRunMapKey(key) + ".json"
}

// runMapEntry is the session→run-id mapping payload published per session key.
// It is a cross-process contract: the external manifold proxy decodes the same
// JSON shape and consumes run_id to stamp X-Gc-Run-Id. Keep the field tags
// (run_id/bead_id/ts) in lock-step with that reader (GC_PROXY_RUNMAP_DIR in
// gc-manifold-proxy.go).
type runMapEntry struct {
	RunID  string `json:"run_id"`
	BeadID string `json:"bead_id"`
	TS     int64  `json:"ts"`
}

// runMapProxyDefaultDir is the zero-config run-map directory, kept
// byte-identical to the manifold proxy's own default (gc-manifold-proxy.go's
// runmapDir). The two sides MUST share a directory or the proxy never finds the
// mapping and X-Gc-Run-Id is never stamped. The proxy runs as root and
// provisions this path sticky 0o1777 before any agent cell starts, so a
// non-root worker's os.MkdirAll(0o755) no-ops on it and CreateTemp succeeds
// there; runMapDirSafeToPublish trusts that sticky root-owned handoff. Both
// sides override in lock-step via GC_RUNMAP_DIR / GC_PROXY_RUNMAP_DIR.
const runMapProxyDefaultDir = "/run/gc-manifold-runmap"

// defaultRunMapDir returns the zero-config run-map directory used when
// GC_RUNMAP_DIR is unset. It is the proxy-aligned default: with the proxy
// present the dir already exists sticky 0o1777 and is worker-writable; with no
// proxy present a non-root worker cannot create it, and writeRunMap treats that
// absent, uncreatable default as a silent no-proxy no-op — there is no proxy
// reading the map in that case, so nothing is lost and the hot claim path stays
// quiet. Only an explicit GC_RUNMAP_DIR that cannot be created is surfaced.
func defaultRunMapDir() string {
	return runMapProxyDefaultDir
}

// runMapDirSafeToPublish reports whether the resolved run-map dir is safe to
// publish a proxy-trusted <session>.json into. os.MkdirAll self-provisions an
// owner-only 0o755 dir but is a no-op on a pre-existing one, so an externally
// provisioned dir keeps its own mode and must be re-checked here.
//
// A dir writable by neither group nor other is always safe: owner-only (0o755),
// or a read-only shared-group dir (0o750), where non-owners cannot create or
// replace entries. Directory write permission is what lets a non-owner create,
// rename, or delete entries, so group-write is gated exactly like other-write:
// a group- or other-writable dir is trusted only as a sticky handoff owned by
// root or this user — the manifold proxy's deliberate multi-user contract, where
// root provisions /run/gc-manifold-runmap as 0o1777 so each agent cell drops its
// own <session>.json and the proxy reads them (the /tmp trust model). A
// non-sticky group- or other-writable dir, or a sticky one owned by another
// user, is refused (CWE-732).
//
// This gate bounds the DIRECTORY's provisioner; it does NOT by itself make the
// shared handoff forgery-proof. The sticky bit only stops a non-owner from
// deleting or renaming over an EXISTING file — it does not stop first-writer
// squatting of a not-yet-existing, predictable <session>.json. So in the shared
// 0o1777 handoff a hostile co-uid can pre-plant a victim's file, and per-file
// run-map authenticity is therefore the READER's responsibility: the manifold
// proxy MUST authenticate each <session>.json by owner (st_uid), mode, and link
// state before trusting run_id — a hard precondition of using a shared handoff.
// writeRunMap additionally refuses to publish over a symlink or foreign-owned
// target (see publishRunMapKey) so the writer never silently blesses a squat, but
// a world-writable handoff cannot be made forgery-proof by the writer alone.
//
// Deployment trust model (verified against the deployed gc-manifold-proxy, which
// reads <session>.json with an unauthenticated os.ReadFile and provisions the dir
// 0o1777): the handoff lives inside a single fleet uid — every agent cell writes
// as that uid and root reads — so the residual forgery is intra-trust-domain.
// Exploiting it needs an already-compromised same-uid cell, and the asset is only
// a best-effort spend-correlation header that degrades safely (a forged or missing
// mapping mis-stamps or omits X-Gc-Run-Id; it never affects code, data, or
// privilege). Because every cell shares that uid, even reader-side owner
// authentication (st_uid == fleet uid) cannot separate a genuine publish from a
// forged one; real per-session anti-forgery needs a proxy-side control the writer
// cannot supply alone — an unguessable per-cell filename/nonce or a
// root-authenticated private channel. That out-of-repo reader/deploy hardening is
// tracked in ga-zzvsuls.
func runMapDirSafeToPublish(dir string) bool {
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		return false
	}
	// Group- and other-write are gated identically: either bit lets a non-owner
	// create, rename, or delete the <session>.json the proxy trusts, so a
	// group-writable dir is no safer than a world-writable one (CWE-732).
	if info.Mode().Perm()&0o022 == 0 {
		return true
	}
	if info.Mode()&os.ModeSticky == 0 {
		return false
	}
	return runMapDirOwnedByTrustedUser(info)
}

// writeRunMap publishes a session→run-id map file,
// ${GC_RUNMAP_DIR:-<defaultRunMapDir>}/<runMapFileName(session)> =
// {"run_id":...,"bead_id":...,"ts":...}, so an external tool can correlate a
// session's activity to the run it is working. One file is written per distinct
// non-empty session key (the session may be addressed as GC_SESSION_NAME,
// GC_SESSION_ID, or BEADS_ACTOR). Best-effort and atomic (tmp + rename): a
// per-key write failure is skipped and never blocks the claim.
//
// SECURITY CONTRACT — the run-map, and the X-Gc-Run-Id header the manifold proxy
// stamps from it, are UNAUTHENTICATED best-effort telemetry: a spend-correlation
// hint, never an authoritative signal. Downstream systems MUST NOT feed
// X-Gc-Run-Id or the run-map into billing, authorization, audit, or any other
// trust decision. The handoff is a single-fleet-uid, /tmp-style trust domain, so
// a compromised same-uid cell can pre-plant a predictable <session>.json that the
// proxy reads unauthenticated; because every cell shares the uid, neither this
// writer nor reader-side owner authentication can distinguish a forgery from a
// genuine publish (see runMapDirSafeToPublish). It degrades safely — a forged or
// missing mapping only mis-stamps or omits the header and never affects code,
// data, privilege, or routing. Real per-session anti-forgery needs a proxy-side
// nonce or private channel this writer cannot supply alone, tracked in
// ga-zzvsuls. TestRunMapEntryIsUnauthenticatedBestEffortTelemetry pins this.
//
// It returns a non-nil error whenever run attribution is compromised or the
// whole map is dropped, so the caller can surface an otherwise silent symptom:
// an unsafe directory, every attempted per-key publish failing, or a squatted
// target (a symlink or foreign-owned <session>.json the proxy would trust). A
// per-key hiccup that still leaves at least one file published is not reported —
// except a squat, which is surfaced even when other keys published, because the
// squatted session's run attribution is forged.
//
// When GC_RUNMAP_DIR is unset (zero-config default) and the default proxy dir is
// absent and uncreatable by a non-root worker, no proxy is reading the map, so
// publication is a silent no-op rather than a per-claim stderr diagnostic on the
// hottest control-plane operation.
func writeRunMap(runID, beadID string, sessionKeys ...string) error {
	dir := strings.TrimSpace(os.Getenv("GC_RUNMAP_DIR"))
	explicit := dir != ""
	if dir == "" {
		dir = defaultRunMapDir()
	}
	return writeRunMapTo(dir, explicit, runID, beadID, sessionKeys...)
}

// writeRunMapTo is writeRunMap with the directory resolution lifted out so both
// the explicit-override and zero-config-default branches are testable. explicit
// is true when the operator set GC_RUNMAP_DIR: only then is an uncreatable dir
// surfaced as an error; a zero-config default that cannot be created means no
// proxy is present and the publish is a silent no-op.
func writeRunMapTo(dir string, explicit bool, runID, beadID string, sessionKeys ...string) error {
	if strings.TrimSpace(runID) == "" {
		return nil
	}
	// Self-provision the dir owned by this session user (0o755). This creates a
	// GC_RUNMAP_DIR override under a writable parent and no-ops on any
	// pre-existing dir — including the default /run/gc-manifold-runmap, which
	// the root proxy provisions sticky 0o1777 before workers run. A world-writable
	// dir is deliberately NOT self-provisioned here — it would let any local user
	// plant a forged <session>.json the proxy would trust — and 0o1777 could not
	// be produced anyway (os.FileMode drops the sticky bit and umask strips
	// other-write, so a self-provisioned dir is 0o755 regardless). The shared
	// multi-uid handoff is the proxy's / systemd-tmpfiles' job.
	if err := os.MkdirAll(dir, 0o755); err != nil {
		// Zero-config default with no proxy present: the default dir does not
		// exist and a non-root worker cannot create it. Nothing reads the map in
		// that case, so stay silent instead of emitting an error-shaped
		// diagnostic on every claim. Surface the failure only when the operator
		// explicitly opted in via GC_RUNMAP_DIR.
		if !explicit {
			return nil
		}
		return fmt.Errorf("creating run-map dir %q: %w", dir, err)
	}
	// MkdirAll no-ops on a pre-existing dir, so gate publish on the resolved
	// dir's safety: a non-sticky group- or other-writable handoff would let any
	// local process forge or clobber run attribution.
	if !runMapDirSafeToPublish(dir) {
		return fmt.Errorf("run-map dir %q is group/other-writable without a sticky trusted-owner handoff; refusing to publish (CWE-732)", dir)
	}
	body, err := json.Marshal(runMapEntry{RunID: runID, BeadID: beadID, TS: time.Now().Unix()})
	if err != nil {
		return fmt.Errorf("marshaling run-map entry: %w", err)
	}
	seen := map[string]bool{}
	// Track publish outcomes so a dropped map or a squatted target is surfaced,
	// not silent: per-key hiccups are best-effort as long as one file lands, but
	// an all-keys-failed run returns its first failure, and a squat (symlink or
	// foreign-owned target) is surfaced even when other keys published.
	var firstErr error
	attempted, published, squats := 0, 0, 0
	for _, k := range sessionKeys {
		k = strings.TrimSpace(k)
		if k == "" || seen[k] {
			continue
		}
		seen[k] = true
		attempted++
		ok, squat, err := publishRunMapKey(dir, k, body)
		if err != nil && firstErr == nil {
			firstErr = err
		}
		if squat {
			squats++
		}
		if ok {
			published++
		}
	}
	// Reap dead sessions' entries so a writer-owned dir doesn't leak one stale
	// file per ended session on a long-uptime box (tmpfs clears /run only on
	// reboot). pruneRunMap self-limits — it skips a shared proxy handoff and
	// bounds its scan — so this stays cheap on the claim hot path.
	pruneRunMap(dir, time.Now(), runMapTTL())
	// A squatted proxy-read target forges the session's run attribution, so it is
	// surfaced even when other keys published, never folded into best-effort nil.
	if squats > 0 {
		return firstErr
	}
	if published == 0 && attempted > 0 {
		return firstErr
	}
	return nil
}

// publishRunMapKey atomically publishes body at <dir>/<runMapFileName(key)> via a
// unique temp + rename. It returns published=true only when the file landed, and
// squat=true when the target is a pre-existing symlink or foreign-owned file — a
// run-attribution squat the proxy's ReadFile would trust — which the writer
// refuses rather than following or reporting as best-effort success. In the
// sticky handoff the writer cannot overwrite a foreign file anyway (sticky yields
// EPERM); refusing here turns a would-be silent forgery into a surfaced error.
func publishRunMapKey(dir, key string, body []byte) (published, squat bool, err error) {
	fileName := runMapFileName(key)
	finalPath := filepath.Join(dir, fileName)
	// Lstat (does NOT follow the link) before writing: a pre-planted symlink (the
	// proxy's os.ReadFile would follow it to an attacker-controlled file) or a
	// foreign-owned file at the predictable name is a squat. Refuse it — surfacing
	// a distinct error — rather than renaming over the name and reporting success.
	// This catches the documented pre-plant attack; a squat that races in after
	// this Lstat instead fails the sticky rename below and is reported per-key.
	if li, lerr := os.Lstat(finalPath); lerr == nil {
		if li.Mode()&os.ModeSymlink != 0 || !runMapExistingFileIsOurs(li) {
			return false, true, fmt.Errorf("run-map target for %q (%s) is a symlink or foreign-owned; refusing to publish (possible run-attribution squat)", key, finalPath)
		}
	}
	// Unique temp name (not a predictable "<file>.tmp"): os.CreateTemp opens
	// O_CREATE|O_EXCL on an unpredictable name, so a pre-planted symlink at the
	// temp path can't be followed on write. The "*" expands before the trailing
	// ".tmp", so any leftover still ends in ".tmp" and pruneRunMap reaps it by age.
	f, err := os.CreateTemp(dir, fileName+".*.tmp")
	if err != nil {
		return false, false, fmt.Errorf("creating run-map temp for %q: %w", key, err)
	}
	tmp := f.Name()
	_, werr := f.Write(body)
	cerr := f.Close()
	if werr != nil || cerr != nil {
		_ = os.Remove(tmp)
		if werr != nil {
			return false, false, fmt.Errorf("writing run-map for %q: %w", key, werr)
		}
		return false, false, fmt.Errorf("closing run-map for %q: %w", key, cerr)
	}
	_ = os.Chmod(tmp, 0o644)
	if err := os.Rename(tmp, finalPath); err != nil {
		_ = os.Remove(tmp)
		return false, false, fmt.Errorf("publishing run-map for %q: %w", key, err)
	}
	return true, false, nil
}

// runMapTTL bounds how long a run-map file survives without a refreshing claim
// before pruneRunMap reaps it. The file's mtime is refreshed on every claim, so
// only sessions that have STOPPED claiming go stale; the default is generous
// enough to exceed the longest a live session goes between claims (one
// long-running work bead) so a working session is never pruned out from under
// the proxy. The in-process reap only bounds a writer-owned dir; a shared
// multi-uid proxy handoff is cleaned by its provisioner (systemd-tmpfiles / the
// root proxy / tmpfs reboot), not by pruneRunMap. Overridable via GC_RUNMAP_TTL
// (Go duration).
func runMapTTL() time.Duration {
	if v := strings.TrimSpace(os.Getenv("GC_RUNMAP_TTL")); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
	}
	return 48 * time.Hour
}

// runMapPruneScanBudget caps how many directory entries a single claim-path
// prune scans and stats, so the reap cost is bounded regardless of how many
// files sit in the run-map dir. Removing reaped entries frees their slots, so a
// dir holding more than one budget of stale files drains opportunistically over
// consecutive claims rather than stalling any single claim. Sized well above the
// live-session count a single host realistically reaches.
const runMapPruneScanBudget = 256

// runMapDirPrunable reports whether the writer may safely reap stale files from
// dir on the claim hot path: true only for a dir writable by neither group nor
// other — one this user provisioned (0o755) or a read-only shared-group dir
// (0o750), where every entry is created by this uid (or root) and os.Remove can
// actually unlink it. It is false for the shared manifold-proxy handoff (a
// group- or other-writable sticky dir, canonically root-owned 0o1777): a
// non-root writer cannot unlink another uid's <session>.json there (the sticky
// bit yields EPERM), so an in-process reap is a no-op — and scanning an
// attacker-fillable directory on every claim would let a local co-tenant inflate
// claim latency by filling it (CWE-400). Cleanup of the shared handoff is the
// provisioner's job (systemd-tmpfiles / the root proxy / tmpfs reboot).
func runMapDirPrunable(dir string) bool {
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		return false
	}
	return info.Mode().Perm()&0o022 == 0
}

// runMapFileIsOwnedEntry reports whether the .json file at path is one this
// writer published: it decodes as a runMapEntry carrying a non-empty run_id AND
// bead_id. pruneRunMap uses it so a reap only ever unlinks the writer's own
// <session>.json files. publishHookClaimRunMap always publishes both
// fields (the resolved run id and the claimed bead id are both non-empty), so a
// genuine entry is never mistaken for foreign; an unrelated config.json an
// operator's explicit GC_RUNMAP_DIR happens to share a directory with fails to
// decode or lacks the fields and is left untouched. Published files are written
// atomically (CreateTemp + rename), so the read here always sees a complete old
// or new entry, never a partial write.
func runMapFileIsOwnedEntry(path string) bool {
	raw, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	var entry runMapEntry
	if err := json.Unmarshal(raw, &entry); err != nil {
		return false
	}
	return strings.TrimSpace(entry.RunID) != "" && strings.TrimSpace(entry.BeadID) != ""
}

// runMapTempOrphanName reports whether name has the shape publishRunMapKey's
// os.CreateTemp produces — "<stem>.json.<random>.tmp" — so a prune reaps only
// this writer's own crash-left temp orphans, never an unrelated cache.tmp an
// explicit GC_RUNMAP_DIR happens to share a directory with. CreateTemp expands
// the "*" in runMapFileName+".*.tmp", so a genuine orphan is the writer's
// "<stem>.json" filename followed by a ".<random>.tmp" suffix.
func runMapTempOrphanName(name string) bool {
	rest, ok := strings.CutSuffix(name, ".tmp")
	if !ok {
		return false
	}
	// Drop the ".<random>" CreateTemp inserted before ".tmp"; what precedes it
	// must be the "<stem>.json" run-map filename the writer built the pattern from.
	i := strings.LastIndex(rest, ".")
	if i < 0 {
		return false
	}
	return strings.HasSuffix(rest[:i], ".json")
}

// pruneRunMap best-effort removes run-map files not refreshed within ttl — the
// files of sessions that have stopped claiming — so a writer-owned dir stays
// bounded by the live session set rather than growing one file per session ever
// seen. It also reaps crash-left temp orphans older than ttl: a live writer's
// temp exists only between CreateTemp and rename, so one that old is a dead-write
// orphan (a process killed mid-publish) the .json-only match used to leak forever.
//
// It reaps ONLY files this writer provably owns, never an unrelated file an
// operator's explicit GC_RUNMAP_DIR happens to share a directory with: a stale
// .json must decode as a runMapEntry with a non-empty run_id and bead_id
// (runMapFileIsOwnedEntry) and a stale .tmp must have the writer's
// "<stem>.json.<rand>.tmp" temp shape (runMapTempOrphanName). Without this an
// owner-only GC_RUNMAP_DIR pointed at a directory that also holds a stale
// config.json or cache.tmp would silently delete it on the claim hot path.
//
// It runs on the claim hot path, so it is deliberately self-limiting: it skips
// the shared group/other-writable proxy handoff entirely (see runMapDirPrunable)
// and, in a writer-owned dir, scans at most runMapPruneScanBudget entries per
// call so a claim's cost never scales with the directory size. Never blocks or
// fails the claim.
func pruneRunMap(dir string, now time.Time, ttl time.Duration) {
	if !runMapDirPrunable(dir) {
		return
	}
	f, err := os.Open(dir)
	if err != nil {
		return
	}
	defer f.Close() //nolint:errcheck // read-only dir handle; close error is irrelevant
	// ReadDir(n>0) returns at most n entries from the directory stream, so the
	// scan+stat cost is capped at the budget rather than the directory size; an
	// empty dir reports io.EOF, which is not a failure.
	entries, err := f.ReadDir(runMapPruneScanBudget)
	if err != nil && !errors.Is(err, io.EOF) {
		return
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		isJSON := strings.HasSuffix(name, ".json")
		isTmp := strings.HasSuffix(name, ".tmp")
		if !isJSON && !isTmp {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		// Only stale files are reap candidates: a live session's .json is
		// refreshed on every claim and a live writer's temp exists for
		// microseconds, so neither is ever this old.
		if now.Sub(info.ModTime()) <= ttl {
			continue
		}
		// Reap only the writer's own files. The ownership check is last so the
		// .json read is paid only for the few stale candidates, not every entry.
		path := filepath.Join(dir, name)
		switch {
		case isTmp:
			if !runMapTempOrphanName(name) {
				continue
			}
		case isJSON:
			if !runMapFileIsOwnedEntry(path) {
				continue
			}
		}
		_ = os.Remove(path)
	}
}

// hookClaimSessionName returns the session display name (GC_SESSION_NAME) from the
// claim env — the pool slot's session/tmux name (e.g. "gc__role-mc-xxxxx") — stamped
// onto the work bead as the durable gc.session_name back-reference so the dashboard's
// byName index can resolve the step's session even when the raw id fails the
// resolver's prefix gate. Empty when the env carries no session name.
func hookClaimSessionName(env []string) string {
	sessionName := ""
	for _, entry := range env {
		if k, v, ok := strings.Cut(entry, "="); ok && k == "GC_SESSION_NAME" {
			sessionName = v
		}
	}
	return strings.TrimSpace(sessionName)
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
//
// Its reads are exempt from the managed output firewall wherever bdshim can
// honor the exemption: they are control-plane reads whose result gc consumes
// and re-renders under its own bounded output (writeHookClaimJSON), never raw
// transcript. Bounding them a second time made an existing assignment read as
// absent and armed a claim on unrelated work (gcw-qap3.16). The gate mirrors
// controlReadyShimmed — raw bd does not know --allow-unbounded, and the shim
// strips it before any passthrough.
func hookClaimBdStoreContext(ctx context.Context, dir string, env []string, actor string) *beads.BdStore {
	envMap := hookClaimEnvMap(env, dir, actor)
	var opts []beads.BdStoreOption
	if controlReadyShimmed(envMap) {
		opts = append(opts, beads.WithBdStoreAllowUnboundedReads())
	}
	return beads.NewBdStore(dir, hookClaimCommandRunnerWithEnvContext(ctx, envMap), opts...)
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

// hookClaimSkip records a work_query element that parsed as JSON but could not
// be unmarshaled into a claim candidate — e.g. a bead a buggy filer wrote with
// a value whose type does not match beads.Bead (a numeric "status", say). The
// scan reports these so the caller can log and skip them instead of failing
// wholesale: one malformed bead must not halt dispatch city-wide.
type hookClaimSkip struct {
	ID  string
	Err error
}

// decodeHookClaimBeads parses work_query output into claim candidates. It is
// resilient to individual malformed beads: the array is split into raw elements
// first, then each is typed-decoded independently, so a single undecodable bead
// is collected into skipped rather than failing the whole scan. A top-level
// value that is not a JSON array still returns an error, preserving the
// "requires JSON work_query output" contract for non-JSON command output.
//
// Non-string *metadata* values are already tolerated one layer down:
// beads.Bead.Metadata is a StringMap that coerces them to their JSON text form,
// so a nested-object or boolean metadata value decodes fine and is never
// skipped. The per-element split guards the batch against type errors OUTSIDE
// metadata (e.g. a numeric "status"), which that coercion does not repair and
// which would otherwise fail the whole-slice unmarshal and drop every bead.
func decodeHookClaimBeads(output string) ([]beads.Bead, []hookClaimSkip, error) {
	output = strings.TrimSpace(output)
	if output == "" {
		return nil, nil, nil
	}
	if !json.Valid([]byte(output)) {
		extracted, ok := firstHookJSONValue(output)
		if !ok {
			return nil, nil, errors.New("output is not JSON")
		}
		output = extracted
	}
	// Reject a withheld payload before normalizeWorkQueryOutput wraps the lone
	// manifest object into a one-element array, which yields a phantom
	// candidate with an empty ID instead of a reported failure (gcw-qap3.16).
	if err := beads.OutputFirewallTruncation([]byte(output)); err != nil {
		return nil, nil, err
	}
	output = normalizeWorkQueryOutput(output)
	// Split into raw elements before typed decoding so one malformed bead
	// cannot fail the whole batch. json.RawMessage accepts any valid JSON
	// value, so the array split never trips on a bead that a direct
	// []beads.Bead unmarshal would reject.
	var raws []json.RawMessage
	if err := json.Unmarshal([]byte(output), &raws); err != nil {
		return nil, nil, err
	}
	candidates := make([]beads.Bead, 0, len(raws))
	var skipped []hookClaimSkip
	for _, raw := range raws {
		var bead beads.Bead
		if err := json.Unmarshal(raw, &bead); err != nil {
			skipped = append(skipped, hookClaimSkip{ID: hookClaimBeadIDForLog(raw), Err: err})
			continue
		}
		candidates = append(candidates, bead)
	}
	return candidates, skipped, nil
}

// hookClaimBeadIDForLog best-effort extracts a bead id from a raw work_query
// element for skip diagnostics. A malformed bead is typically malformed only in
// one field; its id remains a decodable string, so logging it keeps the skip
// actionable (the offending bead can be traced to fix the upstream filer).
// Returns "<unknown>" when even the id cannot be read.
func hookClaimBeadIDForLog(raw json.RawMessage) string {
	var probe struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(raw, &probe); err == nil {
		if id := strings.TrimSpace(probe.ID); id != "" {
			return id
		}
	}
	return "<unknown>"
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
