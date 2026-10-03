package main

import (
	"fmt"
	"log"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gastownhall/gascity/internal/agentutil"
	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/worktree"
)

// poolNewDemandLoadAverageFn is the host load probe the new-demand veto
// reads, indirected so it is stubbable without a process (mirrors
// reapLoadAverageFn in bead_worktree_reaper.go).
var poolNewDemandLoadAverageFn = oneMinuteLoadAverage

// poolNewDemandLoadVeto is the outcome of the PoolNewDemandMaxLoadPercent
// guard for one tick, resolved once by resolvePoolNewDemandLoadVeto and
// threaded through computePoolDesiredStates explicitly — never re-probed
// inside it. The host's 1-minute load average is a per-tick-varying input
// exactly like the interleave rotation seed: when a tick calls
// computePoolDesiredStates more than once on the same inputs, both calls
// must see the identical decision, or they can disagree about which demand
// exists this tick (the same class of bug the seed threading exists to
// prevent — gcw-tuwx8.4 PR #126 review cycle 3; this finding on this guard,
// PR #131 review cycle 2).
type poolNewDemandLoadVeto struct {
	Vetoed  bool
	Load    float64
	Ceiling float64
}

// resolvePoolNewDemandLoadVeto probes the host load once and decides whether
// this tick's anonymous new pool demand is vetoed. Callers that compute pool
// desired state more than once on one tick's inputs must call this ONCE and
// thread the result through every call (typically via a field on
// DesiredStateResult, mirroring PoolNewDemandInterleaveSeed) rather than
// letting computePoolDesiredStates probe internally.
func resolvePoolNewDemandLoadVeto(cfg *config.City, scaleCheckCounts map[string]int) poolNewDemandLoadVeto {
	if pct := cfg.Daemon.PoolNewDemandMaxLoadPercent(); pct > 0 && len(scaleCheckCounts) > 0 {
		if load, err := poolNewDemandLoadAverageFn(); err == nil {
			ceiling := float64(runtime.NumCPU()) * float64(pct) / 100
			if load > ceiling {
				return poolNewDemandLoadVeto{Vetoed: true, Load: load, Ceiling: ceiling}
			}
		}
	}
	return poolNewDemandLoadVeto{}
}

// SessionRequest represents a single session the reconciler should start.
type SessionRequest struct {
	Template     string // agent template qualified name (e.g., "gascity/claude")
	BeadPriority int    // priority of the driving work bead
	// Tier is "resume" for in-progress work with a live session,
	// "wake-known-identity" for in-progress work whose session exited but
	// template is configured, or "new" for ready unassigned work.
	Tier          string
	SessionBeadID string // concrete session to preserve for resume or in-flight new demand
	WorkBeadID    string // the work bead driving this request
	WorkBeadTitle string // title of the work bead driving this request, when known
	WorkPack      string // pack route key from the work bead, when known
	WorkWorkspace string // explicit pack workspace route key from the work bead, when known
	WorkStoreRef  string // city or rig:<name> store reference for WorkBeadID when known
	// WorktreeSpec is the single-owner evidence published by the provisioner.
	// When present, the pool path verifies it before publishing work_dir on a
	// session bead; a mismatch fails the whole atomic metadata update.
	WorktreeSpec *worktree.Spec
	// WorktreeError carries incomplete/conflicting worktree evidence discovered
	// while building demand. The realization path fails closed before creating
	// or updating a session bead.
	WorktreeError string
	// BrainParentSID is gc.brain_parent_sid from the driving work bead, when
	// set: the parent session to fork this launch off of (warm-arm fork-launch).
	BrainParentSID string
	// FloorGuarantee marks a "new" request that satisfies an agent's
	// min_active_sessions floor, whether reserveNestedCapFloors minted it for
	// the floor or it was an elastic scale-check request the floor absorbed.
	// The per-tick create-budget allocator reserves a token for each
	// floor-bearing template before round-robining the remainder, so a cold
	// pool's floor spawn cannot be starved by a warm pool's large elastic
	// demand (follow-up to #2893).
	FloorGuarantee bool
}

func beadPriority(b beads.Bead) int {
	if b.Priority != nil {
		return *b.Priority
	}
	// nil Priority round-trips through native_dolt_store as bd's documented
	// mid default (P2), not "highest" — match that semantics here so an
	// unset-priority bead cannot out-schedule an explicitly-labeled P1 bead.
	return 2
}

// beadPriorityRankOffset is the base of the scheduler's descending rank scale.
// It sits above every bd priority (0-4) so a rank-bearing request always sorts
// ahead of the "new"/floor-guarantee requests that leave SessionRequest.
// BeadPriority at its zero value.
const beadPriorityRankOffset = 10

// beadPriorityRank converts a bd priority — ascending-urgent, where P0 is the
// most urgent — into the descending rank SessionRequest.BeadPriority is sorted
// by, where a larger value is scheduled first. Without this inversion a P4 bead
// would out-schedule a P0 one.
func beadPriorityRank(p int) int {
	return beadPriorityRankOffset - p
}

// legacyWorkDirNoticeSeen deduplicates the unmanaged-workspace notice keyed by
// work bead ID. worktreeSpecForBead runs for every ready bead on every
// scale-check tick and nothing backfills ownership metadata onto beads that
// never published it, so an unguarded log line would repeat for the life of
// the process.
var legacyWorkDirNoticeSeen sync.Map // work bead ID -> struct{}

// worktreeSpecForBead reconstructs the exact single-owner verification input
// from metadata published after gc worktree ensure succeeds. A work_dir with
// incomplete or conflicting evidence is an error, never permission to launch
// a session into an unverified directory.
func worktreeSpecForBead(bead beads.Bead, storeRef string) (*worktree.Spec, error) {
	canonicalPath := strings.TrimSpace(bead.Metadata[beadmeta.WorkDirMetadataKey])
	legacyPath := strings.TrimSpace(bead.Metadata[beadmeta.LegacyWorkDirMetadataKey])
	if canonicalPath != "" && legacyPath != "" && canonicalPath != legacyPath {
		return nil, fmt.Errorf("work bead %s has conflicting %s=%q and %s=%q",
			bead.ID, beadmeta.WorkDirMetadataKey, canonicalPath, beadmeta.LegacyWorkDirMetadataKey, legacyPath)
	}
	path := canonicalPath
	pathKey := beadmeta.WorkDirMetadataKey
	if path == "" {
		path = legacyPath
		pathKey = beadmeta.LegacyWorkDirMetadataKey
	}
	if path == "" {
		return nil, nil
	}
	values := []struct {
		key   string
		value string
	}{
		{beadmeta.WorktreeRepoMetadataKey, bead.Metadata[beadmeta.WorktreeRepoMetadataKey]},
		{beadmeta.WorktreeRootMetadataKey, bead.Metadata[beadmeta.WorktreeRootMetadataKey]},
		{beadmeta.WorkBranchMetadataKey, bead.Metadata[beadmeta.WorkBranchMetadataKey]},
		{beadmeta.WorktreeBaseRefMetadataKey, bead.Metadata[beadmeta.WorktreeBaseRefMetadataKey]},
		{beadmeta.WorktreeBaseSHAMetadataKey, bead.Metadata[beadmeta.WorktreeBaseSHAMetadataKey]},
		{beadmeta.WorktreeCreatorMetadataKey, bead.Metadata[beadmeta.WorktreeCreatorMetadataKey]},
		{beadmeta.WorktreeOwnerMetadataKey, bead.Metadata[beadmeta.WorktreeOwnerMetadataKey]},
		{beadmeta.WorktreeGenerationMetadataKey, bead.Metadata[beadmeta.WorktreeGenerationMetadataKey]},
		{beadmeta.WorktreeLifecycleMetadataKey, bead.Metadata[beadmeta.WorktreeLifecycleMetadataKey]},
	}
	missing := make([]string, 0, len(values))
	present := make([]string, 0, len(values))
	for _, item := range values {
		if strings.TrimSpace(item.value) == "" {
			missing = append(missing, item.key)
		} else {
			present = append(present, item.key)
		}
	}
	if len(missing) == len(values) {
		// A bead carrying work_dir without any ownership evidence is not
		// incomplete evidence -- it never claimed to publish any. Recipe steps
		// are still minted this way (stampDrainItemRecipe in
		// internal/dispatch/drain.go copies both work_dir spellings and none
		// of the nine ownership keys), so treat it as an unmanaged workspace
		// and let the seat spawn as it always did, instead of erroring it into
		// permanent starvation.
		if _, dup := legacyWorkDirNoticeSeen.LoadOrStore(bead.ID, struct{}{}); !dup {
			log.Printf("worktreeSpecForBead: work bead %s has %s=%q with no worktree ownership metadata; treating as unmanaged",
				bead.ID, pathKey, path)
		}
		return nil, nil
	}
	if len(present) == 1 && present[0] == beadmeta.WorkBranchMetadataKey {
		// gc.work_branch alone is not incomplete evidence either -- it is the
		// shape a hook claim's ambient branch stamp produces on an otherwise
		// unmanaged bead (hookClaimIdentityPatch stamps gc.work_branch onto
		// any bead with a resolvable branch, independent of whether the bead
		// ever published worktree ownership evidence). Treat it the same as
		// the zero-key case above instead of hard-erroring the seat into
		// permanent starvation the first time a hook claim touches it.
		if _, dup := legacyWorkDirNoticeSeen.LoadOrStore(bead.ID, struct{}{}); !dup {
			log.Printf("worktreeSpecForBead: work bead %s has %s=%q with only %s published; treating as unmanaged",
				bead.ID, pathKey, path, beadmeta.WorkBranchMetadataKey)
		}
		return nil, nil
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("work bead %s has %s=%q but is missing %s",
			bead.ID, beadmeta.WorkDirMetadataKey, path, missing[0])
	}
	// The bead's own gc.root_store_ref is the canonical spelling
	// ("city:<name>", "rig:<name>") and is what the creating side recorded in
	// the workspace provenance. The caller's storeRef is a probe shorthand
	// ("city"), so verification against durable provenance compares unequal
	// strings and refuses a workspace that is in fact ours. Prefer the bead's
	// value and keep the shorthand only as a fallback for beads that carry no
	// canonical ref.
	resolvedStoreRef := strings.TrimSpace(bead.Metadata[beadmeta.RootStoreRefMetadataKey])
	if resolvedStoreRef == "" {
		resolvedStoreRef = strings.TrimSpace(storeRef)
	}
	if resolvedStoreRef == "" {
		return nil, fmt.Errorf("work bead %s has %s=%q but no store reference", bead.ID, beadmeta.WorkDirMetadataKey, path)
	}
	return &worktree.Spec{
		RepoDir:    strings.TrimSpace(bead.Metadata[beadmeta.WorktreeRepoMetadataKey]),
		Root:       strings.TrimSpace(bead.Metadata[beadmeta.WorktreeRootMetadataKey]),
		Path:       path,
		Branch:     strings.TrimSpace(bead.Metadata[beadmeta.WorkBranchMetadataKey]),
		Base:       strings.TrimSpace(bead.Metadata[beadmeta.WorktreeBaseRefMetadataKey]),
		BaseSHA:    strings.TrimSpace(bead.Metadata[beadmeta.WorktreeBaseSHAMetadataKey]),
		BeadID:     strings.TrimSpace(bead.ID),
		StoreRef:   resolvedStoreRef,
		Creator:    strings.TrimSpace(bead.Metadata[beadmeta.WorktreeCreatorMetadataKey]),
		Owner:      strings.TrimSpace(bead.Metadata[beadmeta.WorktreeOwnerMetadataKey]),
		Generation: strings.TrimSpace(bead.Metadata[beadmeta.WorktreeGenerationMetadataKey]),
		Lifecycle:  strings.TrimSpace(bead.Metadata[beadmeta.WorktreeLifecycleMetadataKey]),
	}, nil
}

// PoolDesiredState holds the desired state for a single agent template.
type PoolDesiredState struct {
	Template string
	Requests []SessionRequest // accepted requests (within all caps)
}

// ReconcileDecision is the output of the nested cap enforcement.
type ReconcileDecision struct {
	Start []SessionRequest // sessions to start
	// Stop is computed by the reconciler by comparing Start against running sessions.
}

func PoolDesiredCounts(states []PoolDesiredState) map[string]int {
	if len(states) == 0 {
		return nil
	}
	counts := make(map[string]int, len(states))
	for _, state := range states {
		counts[state.Template] = len(state.Requests)
	}
	return counts
}

// ComputePoolDesiredStates computes the desired state for all pool agents.
// assignedWorkBeads contains actionable assigned work beads only: in-progress
// work and open work that was already proven ready upstream. Routed but
// unassigned pool queue work must not be passed here; new-session demand comes
// from scale_check, while this function only preserves sessions that already
// own actionable work.
// Each bead's gc.routed_to determines which agent template it belongs to.
// scaleCheckCounts maps agent template → new session demand from scale_check.
// Pass nil for either when unavailable.
//
// The convenience wrappers below that take no seed (ComputePoolDesiredStates,
// ComputePoolDesiredStatesAt, ComputePoolDesiredStatesTraced, ...) each draw a
// fresh rotation seed from poolNewDemandInterleaveCounter and probe the host
// load once for a single, standalone call. When a tick calls this computation
// MORE THAN ONCE on the same inputs — the reconciler builds desired state once
// to materialize sessions and again to compute the wake-demand count
// (city_runtime.go's paired buildDesiredState + PoolDesiredCounts calls) —
// callers MUST use the "WithSeed" variants and thread the same seed, load-veto
// decision, and decision time through both calls (typically stored once on
// DesiredStateResult). Otherwise the two calls can round-robin-rotate to
// different splits on one tick and disagree about which sessions exist versus
// which should be awake (gcw-tuwx8.4 PR #126 review cycle 3).
func ComputePoolDesiredStates(
	cfg *config.City,
	assignedWorkBeads []beads.Bead,
	sessionInfos []sessionpkg.Info,
	scaleCheckCounts map[string]int,
) []PoolDesiredState {
	return computePoolDesiredStates(cfg, assignedWorkBeads, nil, sessionInfos, scaleCheckCounts, nil, nextPoolNewDemandInterleaveSeed(), resolvePoolNewDemandLoadVeto(cfg, scaleCheckCounts), nil, nil)
}

// ComputePoolDesiredStatesWithAdmission is ComputePoolDesiredStates with explicit admission verdicts.
func ComputePoolDesiredStatesWithAdmission(
	cfg *config.City,
	assignedWorkBeads []beads.Bead,
	sessionInfos []sessionpkg.Info,
	scaleCheckCounts map[string]int,
	admissionVerdicts map[string]AdmissionVerdict,
) []PoolDesiredState {
	return computePoolDesiredStates(cfg, assignedWorkBeads, nil, sessionInfos, scaleCheckCounts, nil, nextPoolNewDemandInterleaveSeed(), resolvePoolNewDemandLoadVeto(cfg, scaleCheckCounts), admissionVerdicts, nil)
}

// ComputePoolDesiredStatesWithSeed is ComputePoolDesiredStates for a caller
// that must agree with another call on the same tick's rotation and load-veto
// decision (see the package doc above ComputePoolDesiredStates).
func ComputePoolDesiredStatesWithSeed(
	cfg *config.City,
	assignedWorkBeads []beads.Bead,
	sessionInfos []sessionpkg.Info,
	scaleCheckCounts map[string]int,
	seed uint64,
	loadVeto poolNewDemandLoadVeto,
) []PoolDesiredState {
	return computePoolDesiredStates(cfg, assignedWorkBeads, nil, sessionInfos, scaleCheckCounts, nil, seed, loadVeto, nil, nil)
}

// ComputePoolDesiredStatesWithSeedAt is ComputePoolDesiredStatesWithSeed at a
// caller-supplied decision time, so post-create retention agrees with the
// paired call that used the same time.
func ComputePoolDesiredStatesWithSeedAt(
	cfg *config.City,
	assignedWorkBeads []beads.Bead,
	sessionInfos []sessionpkg.Info,
	scaleCheckCounts map[string]int,
	seed uint64,
	loadVeto poolNewDemandLoadVeto,
	decisionTime time.Time,
) []PoolDesiredState {
	return computePoolDesiredStatesWithOptions(cfg, assignedWorkBeads, nil, sessionInfos, scaleCheckCounts, nil, seed, loadVeto, nil, decisionTime, nil)
}

// ComputePoolDesiredStatesAt computes pool demand at a caller-supplied
// decision time so post-create retention is consistent with its session
// snapshot.
func ComputePoolDesiredStatesAt(
	cfg *config.City,
	assignedWorkBeads []beads.Bead,
	sessionInfos []sessionpkg.Info,
	scaleCheckCounts map[string]int,
	decisionTime time.Time,
) []PoolDesiredState {
	return computePoolDesiredStatesAt(cfg, assignedWorkBeads, sessionInfos, scaleCheckCounts, nil, decisionTime, nil)
}

func ComputePoolDesiredStatesTraced(
	cfg *config.City,
	assignedWorkBeads []beads.Bead,
	sessionInfos []sessionpkg.Info,
	scaleCheckCounts map[string]int,
	trace *sessionReconcilerTraceCycle,
) []PoolDesiredState {
	return computePoolDesiredStates(cfg, assignedWorkBeads, nil, sessionInfos, scaleCheckCounts, nil, nextPoolNewDemandInterleaveSeed(), resolvePoolNewDemandLoadVeto(cfg, scaleCheckCounts), nil, trace)
}

// ComputePoolDesiredStatesTracedWithSeed is ComputePoolDesiredStatesTraced
// for a caller that must agree with another call on the same tick's
// rotation and load-veto decision (see the package doc above
// ComputePoolDesiredStates).
func ComputePoolDesiredStatesTracedWithSeed(
	cfg *config.City,
	assignedWorkBeads []beads.Bead,
	sessionInfos []sessionpkg.Info,
	scaleCheckCounts map[string]int,
	seed uint64,
	loadVeto poolNewDemandLoadVeto,
	admissionVerdicts map[string]AdmissionVerdict,
	trace *sessionReconcilerTraceCycle,
) []PoolDesiredState {
	return computePoolDesiredStates(cfg, assignedWorkBeads, nil, sessionInfos, scaleCheckCounts, nil, seed, loadVeto, admissionVerdicts, trace)
}

// ComputePoolDesiredStatesTracedWithSeedAt is
// ComputePoolDesiredStatesTracedWithSeed at a caller-supplied decision time.
// The paired wake-count call should pass the same decision time the build
// call used so post-create protection and pending-create floors agree.
func ComputePoolDesiredStatesTracedWithSeedAt(
	cfg *config.City,
	assignedWorkBeads []beads.Bead,
	sessionInfos []sessionpkg.Info,
	scaleCheckCounts map[string]int,
	seed uint64,
	loadVeto poolNewDemandLoadVeto,
	admissionVerdicts map[string]AdmissionVerdict,
	decisionTime time.Time,
	trace *sessionReconcilerTraceCycle,
) []PoolDesiredState {
	return computePoolDesiredStatesWithOptions(cfg, assignedWorkBeads, nil, sessionInfos, scaleCheckCounts, nil, seed, loadVeto, admissionVerdicts, decisionTime, trace)
}

// ComputePoolDesiredStatesTracedAt is ComputePoolDesiredStatesAt with
// reconciler decision tracing.
func ComputePoolDesiredStatesTracedAt(
	cfg *config.City,
	assignedWorkBeads []beads.Bead,
	sessionInfos []sessionpkg.Info,
	scaleCheckCounts map[string]int,
	decisionTime time.Time,
	trace *sessionReconcilerTraceCycle,
) []PoolDesiredState {
	return computePoolDesiredStatesAt(cfg, assignedWorkBeads, sessionInfos, scaleCheckCounts, nil, decisionTime, trace)
}

func ComputePoolDesiredStatesWithDemandTraced(
	cfg *config.City,
	assignedWorkBeads []beads.Bead,
	sessionInfos []sessionpkg.Info,
	scaleCheckCounts map[string]int,
	scaleCheckDemand map[string]scaleCheckDemand,
	trace *sessionReconcilerTraceCycle,
) []PoolDesiredState {
	return computePoolDesiredStates(cfg, assignedWorkBeads, nil, sessionInfos, scaleCheckCounts, scaleCheckDemand, nextPoolNewDemandInterleaveSeed(), resolvePoolNewDemandLoadVeto(cfg, scaleCheckCounts), nil, trace)
}

// ComputePoolDesiredStatesWithDemandTracedWithSeed is
// ComputePoolDesiredStatesWithDemandTraced for a caller that must agree with
// another call on the same tick's rotation and load-veto decision (see the
// package doc above ComputePoolDesiredStates). assignedWorkStoreRefs, when
// non-nil, carries the city or rig store provenance of each assigned work
// bead, aligned by index.
func ComputePoolDesiredStatesWithDemandTracedWithSeed(
	cfg *config.City,
	assignedWorkBeads []beads.Bead,
	assignedWorkStoreRefs []string,
	sessionInfos []sessionpkg.Info,
	scaleCheckCounts map[string]int,
	scaleCheckDemand map[string]scaleCheckDemand,
	seed uint64,
	loadVeto poolNewDemandLoadVeto,
	admissionVerdicts map[string]AdmissionVerdict,
	trace *sessionReconcilerTraceCycle,
) []PoolDesiredState {
	return computePoolDesiredStates(cfg, assignedWorkBeads, assignedWorkStoreRefs, sessionInfos, scaleCheckCounts, scaleCheckDemand, seed, loadVeto, admissionVerdicts, trace)
}

// ComputePoolDesiredStatesWithDemandTracedWithSeedAt is
// ComputePoolDesiredStatesWithDemandTracedWithSeed at a caller-supplied
// decision time. This is the variant buildDesiredState uses: it draws the
// seed and resolves the load veto itself, passes both here with its pool
// decision time, and records them on DesiredStateResult so the paired
// wake-count call later in the same tick can reuse them.
func ComputePoolDesiredStatesWithDemandTracedWithSeedAt(
	cfg *config.City,
	assignedWorkBeads []beads.Bead,
	assignedWorkStoreRefs []string,
	sessionInfos []sessionpkg.Info,
	scaleCheckCounts map[string]int,
	scaleCheckDemand map[string]scaleCheckDemand,
	seed uint64,
	loadVeto poolNewDemandLoadVeto,
	admissionVerdicts map[string]AdmissionVerdict,
	decisionTime time.Time,
	trace *sessionReconcilerTraceCycle,
) []PoolDesiredState {
	return computePoolDesiredStatesWithOptions(cfg, assignedWorkBeads, assignedWorkStoreRefs, sessionInfos, scaleCheckCounts, scaleCheckDemand, seed, loadVeto, admissionVerdicts, decisionTime, trace)
}

// ComputePoolDesiredStatesWithDemandTracedAt computes traced pool demand at a
// caller-supplied decision time while preserving per-work demand provenance.
func ComputePoolDesiredStatesWithDemandTracedAt(
	cfg *config.City,
	assignedWorkBeads []beads.Bead,
	sessionInfos []sessionpkg.Info,
	scaleCheckCounts map[string]int,
	scaleCheckDemand map[string]scaleCheckDemand,
	decisionTime time.Time,
	trace *sessionReconcilerTraceCycle,
) []PoolDesiredState {
	return computePoolDesiredStatesAt(cfg, assignedWorkBeads, sessionInfos, scaleCheckCounts, scaleCheckDemand, decisionTime, trace)
}

// nextPoolNewDemandInterleaveSeed draws the next rotation seed for a
// standalone (unpaired) call. Advancing here, one layer above
// computePoolDesiredStates, mirrors poolSessionCreateFairShareCounter's
// placement (advanced in configurePoolSessionCreateFairShare — after desired
// state is computed, not inside it) and keeps computePoolDesiredStates itself
// a pure function of a given seed.
func nextPoolNewDemandInterleaveSeed() uint64 {
	return poolNewDemandInterleaveCounter.Add(1) - 1
}

func computePoolDesiredStates(
	cfg *config.City,
	assignedWorkBeads []beads.Bead,
	assignedWorkStoreRefs []string,
	sessionInfos []sessionpkg.Info,
	scaleCheckCounts map[string]int,
	scaleCheckDemand map[string]scaleCheckDemand,
	newDemandInterleaveSeed uint64,
	loadVeto poolNewDemandLoadVeto,
	admissionVerdicts map[string]AdmissionVerdict,
	trace *sessionReconcilerTraceCycle,
) []PoolDesiredState {
	return computePoolDesiredStatesWithOptions(cfg, assignedWorkBeads, assignedWorkStoreRefs, sessionInfos, scaleCheckCounts, scaleCheckDemand, newDemandInterleaveSeed, loadVeto, admissionVerdicts, time.Time{}, trace)
}

// computePoolDesiredStatesAt is the standalone decision-time entry point: it
// draws a fresh rotation seed and resolves the load veto for this one call.
func computePoolDesiredStatesAt(
	cfg *config.City,
	assignedWorkBeads []beads.Bead,
	sessionInfos []sessionpkg.Info,
	scaleCheckCounts map[string]int,
	scaleCheckDemand map[string]scaleCheckDemand,
	decisionTime time.Time,
	trace *sessionReconcilerTraceCycle,
) []PoolDesiredState {
	return computePoolDesiredStatesWithOptions(cfg, assignedWorkBeads, nil, sessionInfos, scaleCheckCounts, scaleCheckDemand, nextPoolNewDemandInterleaveSeed(), resolvePoolNewDemandLoadVeto(cfg, scaleCheckCounts), nil, decisionTime, trace)
}

// poolTemplateDemand is one template's sizing inputs for the fair-share new
// demand interleave in computePoolDesiredStatesWithOptions.
type poolTemplateDemand struct {
	agent       *config.Agent
	template    string
	scaleCount  int
	demandCount int
	newCount    int
	protected   int
	inFlight    int
	concrete    []SessionRequest
	anonymous   int
	residualIDs []string
	demand      scaleCheckDemand
}

func computePoolDesiredStatesWithOptions(
	cfg *config.City,
	assignedWorkBeads []beads.Bead,
	assignedWorkStoreRefs []string,
	sessionInfos []sessionpkg.Info,
	scaleCheckCounts map[string]int,
	scaleCheckDemand map[string]scaleCheckDemand,
	newDemandInterleaveSeed uint64,
	loadVeto poolNewDemandLoadVeto,
	admissionVerdicts map[string]AdmissionVerdict,
	decisionTime time.Time,
	trace *sessionReconcilerTraceCycle,
) []PoolDesiredState {
	if len(assignedWorkStoreRefs) > 0 && len(assignedWorkStoreRefs) != len(assignedWorkBeads) {
		// Store refs are provenance, not optional positional hints. A malformed
		// non-empty slice could bind a session to another rig, so drop this
		// assigned-work snapshot rather than guessing. Nil remains the legacy
		// single-store fallback and intentionally continues to work.
		assignedWorkBeads = nil
		assignedWorkStoreRefs = nil
	}
	// Build reverse lookup: any identifier → session bead ID.
	// Assignee on work beads may be a bead ID, session name, alias, or
	// a prior alias preserved in alias_history. Resume-tier dispatch
	// drops in-progress work whose owning session can't be resolved
	// from this map, so missing identities cause live sessions to look
	// orphaned and let a duplicate spawn for the same bead.
	assigneeToSessionBeadID := make(map[string]string)
	sessionBeadTemplate := make(map[string]string)
	namedSessionBeadIDs := make(map[string]bool)
	// asleepSessionBeadIDs marks non-closed sessions whose runtime state is
	// asleep (normalizeInfoState also folds "drained" into StateAsleep). A
	// wake_mode="fresh" agent must not resume one of these stale rows — see the
	// resume-tier guard below.
	asleepSessionBeadIDs := make(map[string]bool)
	for _, sb := range sessionInfos {
		if sb.Closed {
			continue
		}
		if sessionHasProviderTerminalErrorInfo(sb) {
			continue
		}
		template := strings.TrimSpace(normalizedSessionTemplateInfo(sb, cfg))
		if template != "" {
			sessionBeadTemplate[sb.ID] = template
		}
		for _, id := range sessionBeadAssigneeIdentitiesInfo(sb) {
			assigneeToSessionBeadID[id] = sb.ID
		}
		if isNamedSessionInfo(sb) {
			namedSessionBeadIDs[sb.ID] = true
		}
		if sb.State == sessionpkg.StateAsleep {
			asleepSessionBeadIDs[sb.ID] = true
		}
	}

	aliasHeldTemplates := canonicalSingletonAliasHeldTemplates(cfg, sessionInfos)

	// now anchors the defer_until readiness gate below. An OPEN assigned bead
	// hidden from claim by a future defer_until is not actionable, so waking a
	// session for it would only drain-and-re-wake every tick (gcw-ehvg P1a).
	// It shares the caller's decision time when one was supplied.
	now := decisionTime
	if now.IsZero() {
		now = time.Now()
	}

	var resumeRequests []SessionRequest
	wakeRequestedTemplates := make(map[string]struct{})

	for i := range cfg.Agents {
		agent := &cfg.Agents[i]
		if agent.Suspended {
			continue
		}
		if !agent.SupportsGenericEphemeralSessions() {
			continue
		}
		template := agent.QualifiedName()

		// Resume tier: actionable assigned work beads whose assignee resolves
		// to a non-closed session bead. These sessions must stay alive.
		for workIndex, wb := range assignedWorkBeads {
			workStoreRef := ""
			if len(assignedWorkStoreRefs) == len(assignedWorkBeads) && len(assignedWorkStoreRefs) > 0 {
				canonical, valid := canonicalContinuationClaimStoreRef(cfg.Workspace.Name, assignedWorkStoreRefs[workIndex])
				switch {
				case valid:
					workStoreRef = canonical
				case strings.TrimSpace(assignedWorkStoreRefs[workIndex]) != "":
					// A non-empty aligned provenance value that cannot name a
					// canonical city or rig store must not be propagated into a
					// session request. Treating it as a generic fallback could
					// claim work from the wrong store.
					continue
				}
				// An empty aligned value carries no provenance at all — the
				// single-store shape of a city with no workspace name — so it
				// keeps the legacy no-store-ref fallback rather than dropping
				// the work.
			}
			routedTo := routedToOrLegacyWorkflowTarget(wb)
			if wb.Status != "in_progress" && wb.Status != "open" {
				continue
			}
			// An OPEN bead deferred into the future is not claimable; waking a
			// session for it just drains and re-wakes each tick (gcw-ehvg P1a).
			// In-progress work resumes regardless (defer hides from claim, not
			// from an already-running session).
			if wb.Status == "open" && beads.IsDeferred(wb, now) {
				continue
			}
			assignee := strings.TrimSpace(wb.Assignee)
			if assignee == "" {
				continue
			}
			sessionBeadID := assigneeToSessionBeadID[assignee]
			if routedTo == "" && sessionBeadID != "" {
				routedTo = sessionBeadTemplate[sessionBeadID]
				if routedTo == "" && len(cfg.Agents) == 1 {
					routedTo = cfg.Agents[0].QualifiedName()
				}
			}
			routedTo = normalizeAgentTemplateIdentity(cfg, agentutil.NormalizePoolRouteTarget(cfg, routedTo))
			if sessionBeadID != "" {
				sessionTemplate := strings.TrimSpace(sessionBeadTemplate[sessionBeadID])
				if sessionTemplate != "" && routedTo != "" && !agentTemplateIdentitiesEquivalent(cfg, routedTo, sessionTemplate) {
					continue
				}
			}
			if routedTo != template {
				continue
			}
			if sessionBeadID != "" {
				// Named-session beads are materialized by the named-session
				// loop in buildDesiredState, not by the pool path. Skipping
				// here prevents realizePoolDesiredSessions from renaming the
				// canonical named identity to a phantom "{name}-1" pool
				// instance — which would create two desired sessions for the
				// same agent even when max_active_sessions=1.
				if namedSessionBeadIDs[sessionBeadID] {
					continue
				}
				// A wake_mode="fresh" pool agent must not inherit a stale
				// *asleep* session bead: by configuration it never wants the
				// old session's state back, so resuming that row leaves the
				// pool bound to a session it will not reuse and the assigned
				// work stranded. Plan a clean session bound to the same work
				// instead — the wake-known-identity tier with an empty
				// SessionBeadID, the same shape the closed-session case
				// already uses. A live (non-asleep) session still resumes;
				// agents with unset or wake_mode="resume" are unaffected.
				// (gastownhall/gascity#4849)
				if agent.EffectiveWakeMode() == "fresh" && asleepSessionBeadIDs[sessionBeadID] {
					if _, ok := wakeRequestedTemplates[template]; ok {
						continue
					}
					wakeRequestedTemplates[template] = struct{}{}
					resumeRequests = append(resumeRequests, SessionRequest{
						Template:     template,
						BeadPriority: beadPriorityRank(beadPriority(wb)),
						Tier:         "wake-known-identity",
						// SessionBeadID intentionally empty: the stale asleep
						// bead must not be reused, so realizePoolDesiredSessions
						// plans a clean create.
						WorkBeadID:     wb.ID,
						WorkBeadTitle:  strings.TrimSpace(wb.Title),
						WorkPack:       strings.TrimSpace(wb.Metadata[beadmeta.PackMetadataKey]),
						WorkWorkspace:  strings.TrimSpace(wb.Metadata[beadmeta.PackWorkspaceMetadataKey]),
						WorkStoreRef:   workStoreRef,
						BrainParentSID: strings.TrimSpace(wb.Metadata[beadmeta.BrainParentSIDMetadataKey]),
					})
					if trace != nil {
						trace.RecordDecision(TraceSitePoolWakeKnownIdentity, TraceReasonAssignedWork, TraceOutcomeScheduled, template, "", traceRecordPayload{
							"tier":                   "wake-known-identity",
							"work_bead":              wb.ID,
							"skipped_asleep_session": sessionBeadID,
						})
					}
					continue
				}
				resumeRequests = append(resumeRequests, SessionRequest{
					Template:       template,
					BeadPriority:   beadPriorityRank(beadPriority(wb)),
					Tier:           "resume",
					SessionBeadID:  sessionBeadID,
					WorkBeadID:     wb.ID,
					WorkBeadTitle:  strings.TrimSpace(wb.Title),
					WorkPack:       strings.TrimSpace(wb.Metadata[beadmeta.PackMetadataKey]),
					WorkWorkspace:  strings.TrimSpace(wb.Metadata[beadmeta.PackWorkspaceMetadataKey]),
					WorkStoreRef:   workStoreRef,
					BrainParentSID: strings.TrimSpace(wb.Metadata[beadmeta.BrainParentSIDMetadataKey]),
				})
				continue
			}
			if isConfiguredNamedSessionIdentity(cfg, assignee) {
				// A configured named session's own bare identity never
				// generates pool demand — namedWorkReady recovers it
				// instead (ga-i1d0tr Candidate B). Mirrors the resume
				// tier's namedSessionBeadIDs skip above, extended to the
				// case where no live session bead resolves the assignee
				// at all.
				continue
			}
			if !agentTemplateIdentitiesEquivalent(cfg, assignee, template) || !isKnownPoolTemplate(assignee, cfg) {
				// Assignee set but session closed/unknown and not a configured
				// pool template — orphaned work, not our job to respawn. The
				// identity-equivalence compare keeps work assigned under a
				// legacy bound form of this template eligible for the
				// wake-known-identity tier; the emitted request carries the
				// canonical template.
				continue
			}
			if _, ok := wakeRequestedTemplates[template]; ok {
				continue
			}
			wakeRequestedTemplates[template] = struct{}{}
			resumeRequests = append(resumeRequests, SessionRequest{
				Template:       template,
				BeadPriority:   beadPriorityRank(beadPriority(wb)),
				Tier:           "wake-known-identity",
				WorkBeadID:     wb.ID,
				WorkBeadTitle:  strings.TrimSpace(wb.Title),
				WorkPack:       strings.TrimSpace(wb.Metadata[beadmeta.PackMetadataKey]),
				WorkWorkspace:  strings.TrimSpace(wb.Metadata[beadmeta.PackWorkspaceMetadataKey]),
				WorkStoreRef:   workStoreRef,
				BrainParentSID: strings.TrimSpace(wb.Metadata[beadmeta.BrainParentSIDMetadataKey]),
			})
			if trace != nil {
				trace.RecordDecision(TraceSitePoolWakeKnownIdentity, TraceReasonAssignedWork, TraceOutcomeScheduled, template, "", traceRecordPayload{
					"tier":      "wake-known-identity",
					"work_bead": wb.ID,
				})
			}
		}
	}

	limits := newNestedCapLimits(cfg)
	resumeSessionBeadIDs := make(map[string]struct{}, len(resumeRequests))
	for _, req := range resumeRequests {
		if req.SessionBeadID != "" {
			resumeSessionBeadIDs[req.SessionBeadID] = struct{}{}
		}
	}
	protectedNewRequests, inFlightNewRequests := poolNewDemandRequests(cfg, sessionInfos, resumeSessionBeadIDs, decisionTime)
	sessionInfoByID := make(map[string]sessionpkg.Info, len(sessionInfos))
	for _, info := range sessionInfos {
		if info.ID == "" {
			continue
		}
		sessionInfoByID[info.ID] = info
	}
	// A wake-known request has assigned work but no surviving concrete session
	// identity. Freshly completed unassigned pool sessions are valid concrete
	// capacity for that request. Bind them before calculating residual protected
	// demand so wake-known + protection cannot materialize the same capacity
	// twice.
	for i := range resumeRequests {
		req := &resumeRequests[i]
		if req.Tier != "wake-known-identity" || req.SessionBeadID != "" {
			continue
		}
		candidates := protectedNewRequests[req.Template]
		if len(candidates) == 0 {
			continue
		}
		req.SessionBeadID = candidates[0].SessionBeadID
		protectedNewRequests[req.Template] = candidates[1:]
	}
	usage := acceptedNestedCapUsage(limits, resumeRequests)
	floorUsage := acceptedNestedCapUsage(limits, concreteNestedCapRequests(cfg, resumeRequests, protectedNewRequests, inFlightNewRequests))
	floorReservations := newNestedCapFloorReservations(cfg, aliasHeldTemplates, limits, floorUsage)
	allRequests := append([]SessionRequest(nil), resumeRequests...)

	// New-demand load veto: while the host's 1-minute load average exceeded
	// the configured ceiling AT THE TIME THE CALLER RESOLVED loadVeto,
	// decline every template's *anonymous* new demand for this tick. This
	// clamps the built-in demand signal in place rather than recomputing it,
	// and applies only to demand that would add load — the resume tier,
	// wake-known-identity tier, and already-in-flight new-tier requests
	// (sessions already created and mid-start, tracked in
	// inFlightNewRequests below) are none of them vetoed as requests. This
	// does have an indirect effect on wake for anonymous-only templates via
	// PoolDesiredCounts going to 0 (see the field doc on
	// PoolNewDemandMaxLoadPercent). An unreadable load proceeds (fail-open),
	// matching the worktree reaper's load guard: a probe failure must not
	// freeze new-session creation.
	//
	// The decision itself is resolved ONCE by the caller (see
	// resolvePoolNewDemandLoadVeto) and passed in, never re-probed here: the
	// host load is a per-tick-varying input exactly like the interleave
	// rotation seed, and this function can run twice on one tick's inputs
	// (build the create plan, then recompute wake demand). Probing internally
	// let the two calls independently reach opposite verdicts on the same
	// tick (PR #131 review cycle 2).
	newDemandVetoed := loadVeto.Vetoed
	if newDemandVetoed && trace != nil {
		trace.RecordControllerDecision(TraceSitePoolNewDemandLoadVeto, TraceReasonHostLoadVeto, TraceOutcomeSkipped, traceRecordPayload{
			"load":              loadVeto.Load,
			"max_load_percent":  cfg.Daemon.PoolNewDemandMaxLoadPercent(),
			"num_cpu":           runtime.NumCPU(),
			"ceiling":           loadVeto.Ceiling,
			"templates_at_risk": len(scaleCheckCounts),
		})
	}

	// Merge scale_check demand. In bead-backed reconciliation, scale_check is
	// the authoritative signal for new unassigned demand only; resume requests
	// are calculated independently from assigned work and must not be deducted
	// from that count. Pool-created sessions that have not claimed work yet
	// represent already-spent new demand, so they occupy the first new-demand
	// slots explicitly before anonymous creates are materialized. Freshly
	// completed sessions also establish a short-lived demand floor so a sibling
	// startup is not drained merely because another worker claimed first.
	//
	// Each template's candidates are sized against the static resume-only
	// baseline (usage is never mutated in this loop), not against capacity
	// accumulated across templates in declaration order, so a shared
	// workspace or rig cap is divided across contention by the interleave
	// below instead of exhausted by whichever template is declared first
	// (gcw-tuwx8.4). applyNestedCaps remains the authoritative admission.
	var demands []poolTemplateDemand
	for i := range cfg.Agents {
		agent := &cfg.Agents[i]
		if agent.Suspended {
			continue
		}
		template := agent.QualifiedName()
		scaleCount, hasScaleCount := scaleCheckCounts[template]
		protected := protectedNewRequests[template]
		if !hasScaleCount && len(protected) == 0 {
			// A template with no demand never becomes a key in
			// scaleCheckCounts, so without this record the skip is silent and
			// a pool that correctly wants zero sessions reads exactly like a
			// pool the controller never evaluated. That ambiguity has already
			// cost days of misdiagnosis; the decision below is the evidence
			// that the template was reached and found idle on purpose.
			if trace != nil {
				trace.RecordDecision(TraceSitePoolDemandCompute, TraceReasonNoDemand, TraceOutcomeSkipped, template, "", traceRecordPayload{
					"scale_check": scaleCount,
					"protected":   len(protected),
					"in_flight":   len(inFlightNewRequests[template]),
				})
			}
			continue
		}
		inFlight := inFlightNewRequests[template]
		templateAdmissionVetoed := false
		if av, ok := admissionVerdicts[template]; ok && !av.Allowed {
			templateAdmissionVetoed = true
		}
		if newDemandVetoed || templateAdmissionVetoed {
			// Anonymous new demand is declined under load pressure or admission veto, but
			// in-flight requests represent sessions already created and
			// mid-start — already-spent capacity, not new load — so they
			// remain admissible up to their own count. Protected sessions are
			// concrete capacity too and keep their own floor below.
			scaleCount = minInt(scaleCount, len(inFlight))
		}
		if templateAdmissionVetoed && trace != nil {
			trace.RecordDecision(TraceSiteAdmissionCheckExec, TraceReasonAdmissionGate, TraceOutcomeDeny, template, "", traceRecordPayload{
				"reason":             admissionVerdicts[template].Reason,
				"demand_vetoed":      scaleCheckCounts[template],
				"in_flight_retained": len(inFlight),
			})
		}
		if _, ok := aliasHeldTemplates[template]; ok {
			continue
		}
		inFlightFloor := 0
		for _, req := range inFlight {
			if info, ok := sessionInfoByID[req.SessionBeadID]; ok && poolSessionWithinPendingCreateLease(info, cfg, decisionTime) {
				inFlightFloor++
			}
		}
		effectiveDemand := max(scaleCount, len(protected), inFlightFloor)
		newCount := capNewDemandCount(limits, usage, floorReservations, agent, effectiveDemand)
		concreteLimit := concreteNestedCapLimit(limits, usage, template, newCount, effectiveDemand, len(protected)+len(inFlight))
		protectedCount := minInt(len(protected), concreteLimit)
		inFlightCount := minInt(len(inFlight), concreteLimit-protectedCount)
		reusedCount := protectedCount + inFlightCount
		selectedConcrete := make([]SessionRequest, 0, reusedCount)
		selectedConcrete = append(selectedConcrete, protected[:protectedCount]...)
		selectedConcrete = append(selectedConcrete, inFlight[:inFlightCount]...)
		demand := scaleCheckDemand[template]
		selectedConcrete, residualWorkBeadIDs := allocateScaleDemandToConcrete(demand, selectedConcrete)
		demands = append(demands, poolTemplateDemand{
			agent:       agent,
			template:    template,
			scaleCount:  scaleCount,
			demandCount: effectiveDemand,
			newCount:    newCount,
			protected:   len(protected),
			inFlight:    len(inFlight),
			concrete:    selectedConcrete,
			anonymous:   max(0, newCount-reusedCount),
			residualIDs: residualWorkBeadIDs,
			demand:      demand,
		})
	}

	if len(demands) > 0 {
		start := int(newDemandInterleaveSeed % uint64(len(demands)))
		demands = append(append([]poolTemplateDemand(nil), demands[start:]...), demands[:start]...)

		// Emit every contending template's concrete (protected and in-flight)
		// requests before any template's anonymous ones. A concrete request
		// carries SessionBeadID and stands for a session that already exists;
		// sweepUndesiredPoolSessionBeads protects a still-leased
		// pending_create_claim or non-stale creating session regardless of
		// whether it appears in this tick's desired state, so dropping it
		// from desired does not free its slot — the session survives the
		// sweep either way. Interleaving a concrete request against another
		// template's anonymous one on equal terms would let the anonymous
		// request win a shared cap and add a session on top of the concrete
		// one that the sweep keeps regardless, over-subscribing the cap
		// (gcw-tuwx8.4 PR #126 review cycle 3). This mirrors
		// build_desired_state.go, which excludes SessionBeadID != "" from
		// fresh-create fair-share budget for the same reason.
		for _, td := range demands {
			allRequests = append(allRequests, td.concrete...)
		}

		// Interleave only the anonymous remainder, one slot per template per
		// round, so a shared cap (workspace or rig) is divided across
		// contention instead of exhausted by whichever template is
		// processed first (gcw-tuwx8.4).
		for round := 0; ; round++ {
			progressed := false
			for _, td := range demands {
				if round >= td.anonymous {
					continue
				}
				progressed = true
				workBeadID := ""
				if round < len(td.residualIDs) {
					workBeadID = td.residualIDs[round]
				}
				allRequests = append(allRequests, newTierSessionRequest(td.template, td.demand, workBeadID))
			}
			if !progressed {
				break
			}
		}

		// applyNestedCaps below performs the real, authoritative admission;
		// replay the accept/reject algorithm here purely to explain it per
		// template — generating the trace from the same final allRequests set
		// this pass produced keeps the reported cap/current values accurate
		// even when a shared cap (not a per-template one) is what actually
		// binds.
		finalUsage := acceptedNestedCapUsage(limits, allRequests)
		acceptedNewByTemplate := make(map[string]int, len(demands))
		for _, req := range finalUsage.requests {
			if req.Tier == "new" {
				acceptedNewByTemplate[req.Template]++
			}
		}
		for _, td := range demands {
			acceptedNew := minInt(acceptedNewByTemplate[td.template], td.newCount)
			// newDemandBlockingScope's predicates (capMax - usage.count <=
			// newCount) assume usage has not yet absorbed newCount — feeding
			// it finalUsage, which already counts this template's own
			// acceptedNew, double-counts and can blame the wrong cap.
			// Exclude this template's own accepted new-tier contribution
			// before handing it to the trace.
			preAcceptUsage := usageExcludingTemplateNew(finalUsage, td.template, limits.agentRig[td.template], acceptedNewByTemplate[td.template])
			recordNewDemandCapTrace(trace, td.template, td.agent, limits, preAcceptUsage, td.demandCount, acceptedNew)
			if td.scaleCount > 0 && td.protected+td.inFlight > 0 && trace != nil {
				reused := minInt(len(td.concrete), acceptedNew)
				trace.RecordDecision(TraceSitePoolInFlightReuse, TraceReasonInFlightReuse, TraceOutcomeAccepted, td.template, "", traceRecordPayload{
					"scale_check":   td.scaleCount,
					"in_flight":     td.inFlight,
					"protected":     td.protected,
					"reused":        reused,
					"anonymous_new": acceptedNew - reused,
				})
			}
		}
	}

	return applyNestedCaps(cfg, allRequests, aliasHeldTemplates, trace)
}

// poolNewDemandInterleaveCounter backs nextPoolNewDemandInterleaveSeed, used
// only by the standalone convenience wrappers above. computePoolDesiredStates
// itself never reads this counter directly — it takes the seed as an
// explicit parameter, so a caller that must call it twice on one tick (see
// the doc comment on ComputePoolDesiredStates) can hold the rotation fixed
// across both calls instead of each call silently advancing it independently.
var poolNewDemandInterleaveCounter atomic.Uint64

// newTierSessionRequest builds an anonymous "new" tier SessionRequest for
// template, threading the driving work bead's identity and worktree evidence
// from scaleCheckDemand when workBeadID names residual demand. BeadPriority
// stays at its zero value, which ranks below every beadPriorityRank-bearing
// resume or wake request (see beadPriorityRankOffset).
func newTierSessionRequest(template string, demand scaleCheckDemand, workBeadID string) SessionRequest {
	request := SessionRequest{Template: template, Tier: "new"}
	if strings.TrimSpace(workBeadID) == "" {
		return request
	}
	return requestWithScaleDemandProvenance(request, demand, workBeadID)
}

func concreteNestedCapRequests(
	cfg *config.City,
	resumeRequests []SessionRequest,
	protectedRequests map[string][]SessionRequest,
	inFlightRequests map[string][]SessionRequest,
) []SessionRequest {
	requests := append([]SessionRequest(nil), resumeRequests...)
	for i := range cfg.Agents {
		template := cfg.Agents[i].QualifiedName()
		requests = append(requests, protectedRequests[template]...)
		requests = append(requests, inFlightRequests[template]...)
	}
	return requests
}

// allocateScaleDemandToConcrete matches concrete reused capacity against scale
// demand by identity, then rebinds unmatched concrete capacity to the earliest
// unclaimed demand. A concrete request carrying B covers B even when demand is
// ordered [A, B]; a stale X (or blank trigger) is rebound to A rather than
// silently consuming A while remaining pointed at X. Surplus concrete capacity
// with no residual demand keeps its prior trigger because its protection floor
// is independent of scale demand.
func allocateScaleDemandToConcrete(demand scaleCheckDemand, concrete []SessionRequest) ([]SessionRequest, []string) {
	allocated := append([]SessionRequest(nil), concrete...)
	uniqueDemand := make([]string, 0, len(demand.WorkBeadIDs))
	demandSet := make(map[string]struct{}, len(demand.WorkBeadIDs))
	for _, rawID := range demand.WorkBeadIDs {
		id := strings.TrimSpace(rawID)
		if id == "" {
			continue
		}
		if _, seen := demandSet[id]; seen {
			continue
		}
		demandSet[id] = struct{}{}
		uniqueDemand = append(uniqueDemand, id)
	}

	matched := make(map[string]struct{}, len(allocated))
	unmatchedConcrete := make([]int, 0, len(allocated))
	for i, request := range allocated {
		id := strings.TrimSpace(request.WorkBeadID)
		if _, demanded := demandSet[id]; demanded {
			if _, alreadyMatched := matched[id]; !alreadyMatched {
				matched[id] = struct{}{}
				allocated[i] = requestWithScaleDemandProvenance(request, demand, id)
				continue
			}
		}
		unmatchedConcrete = append(unmatchedConcrete, i)
	}

	remaining := make([]string, 0, len(uniqueDemand)-len(matched))
	for _, id := range uniqueDemand {
		if _, covered := matched[id]; !covered {
			remaining = append(remaining, id)
		}
	}
	rebound := minInt(len(unmatchedConcrete), len(remaining))
	for i := 0; i < rebound; i++ {
		idx := unmatchedConcrete[i]
		allocated[idx] = requestWithScaleDemandProvenance(allocated[idx], demand, remaining[i])
	}
	return allocated, remaining[rebound:]
}

func requestWithScaleDemandProvenance(request SessionRequest, demand scaleCheckDemand, workBeadID string) SessionRequest {
	workBeadID = strings.TrimSpace(workBeadID)
	request.WorkBeadID = workBeadID
	request.WorkBeadTitle = strings.TrimSpace(demand.Titles[workBeadID])
	request.WorkPack = strings.TrimSpace(demand.Packs[workBeadID])
	request.WorkWorkspace = strings.TrimSpace(demand.Workspaces[workBeadID])
	request.WorkStoreRef = strings.TrimSpace(demand.StoreRefs[workBeadID])
	request.BrainParentSID = strings.TrimSpace(demand.ParentSIDs[workBeadID])
	// Worktree evidence is per-bead like every field above it. Carrying the
	// previous bead's spec into a rebound request would hand the session a
	// workspace verified for other work.
	request.WorktreeSpec = nil
	request.WorktreeError = ""
	if demand.WorktreeSpecs != nil {
		request.WorktreeSpec = demand.WorktreeSpecs[workBeadID]
	}
	if demand.WorktreeErrors != nil {
		request.WorktreeError = strings.TrimSpace(demand.WorktreeErrors[workBeadID])
	}
	return request
}

func canonicalSingletonAliasHeldTemplates(cfg *config.City, sessionInfos []sessionpkg.Info) map[string]struct{} {
	held := make(map[string]struct{})
	if cfg == nil {
		return held
	}
	for i := range cfg.Agents {
		agent := &cfg.Agents[i]
		if agent.Suspended || !agent.UsesCanonicalSingletonPoolIdentity() {
			continue
		}
		template := agent.QualifiedName()
		for _, sb := range sessionInfos {
			// None of these own the canonical alias: a closed or drained named
			// session released it at close via the retire path, a pool-managed bead
			// never held it, and a failed-create bead released it via
			// failedCreateIdentityReleased (names.go). Counting any as a live holder
			// would suppress demand while the alias is actually free, hanging routed
			// work.
			if sb.Closed || isPoolManagedSessionInfo(sb) || isDrainedSessionInfo(sb) || isFailedCreateSessionInfo(sb) {
				continue
			}
			if strings.TrimSpace(sb.Alias) == template {
				held[template] = struct{}{}
				break
			}
			// A named session's Alias holds its own configured identity, not
			// the backing template (build_desired_state.go sets tp.Alias =
			// identity for every named session, e.g. "primary" bound to
			// template "worker"). When identity != template, the Alias check
			// above never matches even though this bead is the singleton
			// slot's sole occupant. Its Template field is always the backing
			// template's qualified name, so use that as the named-session
			// match instead of Alias.
			if isNamedSessionInfo(sb) && strings.TrimSpace(sb.Template) == template {
				held[template] = struct{}{}
				break
			}
		}
	}
	return held
}

func poolNewDemandRequests(
	cfg *config.City,
	sessionInfos []sessionpkg.Info,
	resumeSessionBeadIDs map[string]struct{},
	decisionTime time.Time,
) (map[string][]SessionRequest, map[string][]SessionRequest) {
	protected := make(map[string][]SessionRequest)
	inFlight := make(map[string][]SessionRequest)
	sortedSessionInfos := append([]sessionpkg.Info(nil), sessionInfos...)
	sort.SliceStable(sortedSessionInfos, func(i, j int) bool {
		if !sortedSessionInfos[i].CreatedAt.Equal(sortedSessionInfos[j].CreatedAt) {
			return sortedSessionInfos[i].CreatedAt.Before(sortedSessionInfos[j].CreatedAt)
		}
		return sortedSessionInfos[i].ID < sortedSessionInfos[j].ID
	})
	for i := range cfg.Agents {
		agent := &cfg.Agents[i]
		if agent.Suspended || !agent.SupportsGenericEphemeralSessions() {
			continue
		}
		template := agent.QualifiedName()
		for _, sb := range sortedSessionInfos {
			if sb.ID == "" || sb.Closed {
				continue
			}
			if sessionHasProviderTerminalErrorInfo(sb) {
				continue
			}
			if _, ok := resumeSessionBeadIDs[sb.ID]; ok {
				continue
			}
			if !isEphemeralSessionInfoForAgent(sb, agent) || !isPoolManagedSessionInfo(sb) {
				continue
			}
			if normalizedSessionTemplateInfo(sb, cfg) != template {
				continue
			}
			req := SessionRequest{
				Template:       template,
				Tier:           "new",
				SessionBeadID:  sb.ID,
				WorkBeadID:     strings.TrimSpace(sb.TriggerBeadID),
				WorkPack:       strings.TrimSpace(sb.Pack),
				WorkWorkspace:  strings.TrimSpace(sb.PackWorkspace),
				WorkStoreRef:   strings.TrimSpace(sb.TriggerBeadStoreRef),
				BrainParentSID: strings.TrimSpace(sb.BrainParentSID),
			}
			if poolSessionEligibleForProtectedDemand(sb, decisionTime) {
				protected[template] = append(protected[template], req)
				continue
			}
			if poolSessionConsumesNewDemandInfo(sb) {
				inFlight[template] = append(inFlight[template], req)
			}
		}
	}
	return protected, inFlight
}

// poolSessionWithinPostCreateProtection reports whether a successfully-created,
// reusable pool session is still in the same grace window used by the
// steady-state sweep. decisionTime is injected by the desired-state build so all
// decisions in a tick share one clock observation. A future marker fails
// closed: the writer and reader share a host clock, so negative age indicates
// corrupt metadata rather than a legitimate grace window.
func poolSessionWithinPostCreateProtection(info sessionpkg.Info, decisionTime time.Time) bool {
	if decisionTime.IsZero() {
		return false
	}
	if info.Closed || isDrainedSessionInfo(info) || isFailedCreateSessionInfo(info) ||
		sessionHasProviderTerminalErrorInfo(info) {
		return false
	}
	state := strings.TrimSpace(info.MetadataState)
	if state != "active" && state != "awake" {
		return false
	}
	if strings.TrimSpace(info.StateReason) != "creation_complete" {
		return false
	}
	creationCompleteAt, ok := parseRFC3339Metadata(info.CreationCompleteAt)
	if !ok {
		return false
	}
	age := decisionTime.Sub(creationCompleteAt)
	return age >= 0 && age < postCreateProtectionTimeout
}

// poolSessionEligibleForProtectedDemand is intentionally narrower than the
// sweep-retention predicate above. A fresh dependency-only or lifecycle-blocked
// session should survive the post-create sweep window, but it is not runnable
// generic capacity and therefore must neither establish a demand floor nor bind
// a wake-known request.
func poolSessionEligibleForProtectedDemand(info sessionpkg.Info, decisionTime time.Time) bool {
	return poolSessionWithinPostCreateProtection(info, decisionTime) &&
		!info.DependencyOnly &&
		strings.TrimSpace(info.WaitHold) == "" &&
		!metadataTimeInFuture(info.HeldUntil, decisionTime) &&
		!metadataTimeInFuture(info.QuarantinedUntil, decisionTime)
}

// poolSessionWithinPendingCreateLease reports whether an in-flight
// pending-create pool session is still within its own lease window and should
// therefore establish a demand floor of its own, mirroring the protected-
// session floor above. decisionTime is injected the same way as
// poolSessionWithinPostCreateProtection so every decision in a tick shares one
// clock observation; a zero decisionTime fails closed for the same reason.
//
// The lease arithmetic itself is not reimplemented here: it delegates to
// pendingCreateLeaseExpiredForRollbackInfo (session_reconciler.go), the exact
// predicate the reconciler's own rollback path uses, so a bead genuinely stuck
// past its lease is never propped up by this floor — it still rolls back on
// schedule. That function only reasons about beads in a rollback-eligible
// state (start-pending/creating/asleep, via pendingCreateRollbackState) and
// returns false (not expired) for any other state as a no-op default, so the
// state gate is re-checked here first: a pending_create_claim left set on an
// already-stopped or failed-create session is not a live in-flight attempt
// and must not establish a floor, regardless of how recently it was created.
func poolSessionWithinPendingCreateLease(info sessionpkg.Info, cfg *config.City, decisionTime time.Time) bool {
	if decisionTime.IsZero() {
		return false
	}
	if !info.PendingCreateClaim {
		return false
	}
	if !pendingCreateRollbackState(info.MetadataState) {
		return false
	}
	var startupTimeout time.Duration
	if cfg != nil {
		startupTimeout = cfg.Session.StartupTimeoutDuration()
	}
	return !pendingCreateLeaseExpiredForRollbackInfo(info, &clock.Fake{Time: decisionTime}, startupTimeout)
}

// poolSessionConsumesNewDemandInfo reports whether a pool session already
// represents spent "new" demand: it holds an active pending_create_claim, or
// its raw state is creating/start-pending. It reads PendingCreateClaim and the
// raw MetadataState. This pure desired-state pass has no reconciler clock:
// creating sessions still represent already-spent new demand; lifecycle code
// owns stale-creating recovery with its clock-aware predicate.
func poolSessionConsumesNewDemandInfo(info sessionpkg.Info) bool {
	if info.PendingCreateClaim {
		return true
	}
	state := strings.TrimSpace(info.MetadataState)
	return state == "creating" || state == string(sessionpkg.StateStartPending)
}

// applyNestedCaps enforces workspace, rig, and agent max_active_sessions caps.
//
// Admission runs in three phases, and the phase order — not BeadPriority — is
// the outer precedence:
//
//  1. Concrete requests (resume-like tier, or any request carrying a
//     SessionBeadID) are admitted first, so among the requests this function
//     is given they outrank *all* new demand regardless of BeadPriority: at a
//     saturated cap a low-priority live session keeps the last slot and a
//     higher-priority new request is rejected. That is deliberate.
//     Reconciliation already owns those sessions, so preempting one to start a
//     new request would trade work in progress for work not yet begun, and no
//     cap this pass enforces is violated by keeping it. The guarantee stops at
//     this function's inputs: computePoolDesiredStatesAt's demand pre-pass
//     selects each template's concrete candidates under newCount (see
//     concreteNestedCapLimit), so a template declared later in cfg.Agents can
//     lose a live session before phase 1 ever sees the request (ga-2c8ll).
//  2. Agent floors reserve capacity out of what phase 1 left. When floors
//     exceed a cap, qualified template name order decides which floor receives
//     the scarce capacity; rejected floor traces identify the loser.
//  3. Demand not consumed by phase 1 or 2 competes by BeadPriority. Priority is
//     therefore the precedence *within* new demand, not across phases.
//
// TestApplyNestedCaps_NoFloorsPreservesDemandPriority pins both regimes.
func applyNestedCaps(cfg *config.City, requests []SessionRequest, aliasHeldTemplates map[string]struct{}, trace *sessionReconcilerTraceCycle) []PoolDesiredState {
	// Order the phase-3 priority walk: priority DESC, resume tier first within
	// same priority. BeadPriority is a descending rank (beadPriorityRank
	// inverts bd's ascending-urgent priority, so a P0 bead outranks a P4 one).
	// Phase 1 walks this same slice but does not honor the order
	// as an admission ranking against new demand — it admits every concrete
	// request that fits — so this sort does not decide concrete-versus-new
	// precedence. Among concrete requests competing for a binding cap the order
	// is still the tiebreaker: the earlier one in this order wins.
	sort.SliceStable(requests, func(i, j int) bool {
		if requests[i].BeadPriority != requests[j].BeadPriority {
			return requests[i].BeadPriority > requests[j].BeadPriority
		}
		// Resume-like tiers before new tier at same priority.
		if requests[i].Tier != requests[j].Tier {
			return isResumeLikeTier(requests[i].Tier) && !isResumeLikeTier(requests[j].Tier)
		}
		return false
	})

	limits := newNestedCapLimits(cfg)
	usage := newNestedCapUsage()
	accepted := make(map[string][]SessionRequest) // template → accepted requests
	consumed := acceptConcreteNestedCapRequests(requests, limits, &usage, accepted, trace)
	reserveNestedCapFloors(cfg, requests, aliasHeldTemplates, limits, &usage, accepted, consumed, trace)

	// Walk demand not already used by a floor, accepting by priority.
	for i, req := range requests {
		if consumed[i] {
			continue
		}
		template := req.Template
		if usage.isDuplicateSessionRequest(req) {
			continue
		}
		if site, reason, payload, rejected := usage.rejection(req, limits); rejected {
			if trace != nil {
				trace.RecordDecision(site, reason, TraceOutcomeRejected, template, "", payload)
			}
			continue
		}

		// Accept.
		accepted[template] = append(accepted[template], req)
		if trace != nil {
			trace.RecordDecision(TraceSitePoolAccept, TraceReasonCap, TraceOutcomeAccepted, template, "", traceRecordPayload{
				"tier": req.Tier,
			})
		}
		usage.accept(req, limits)
	}

	// Build output.
	var result []PoolDesiredState
	for template, reqs := range accepted {
		result = append(result, PoolDesiredState{
			Template: template,
			Requests: reqs,
		})
	}
	// Stable output order.
	sort.Slice(result, func(i, j int) bool {
		return result[i].Template < result[j].Template
	})
	return result
}

// acceptConcreteNestedCapRequests reserves capacity for sessions that already
// exist, are being resumed, or are in flight. Floors may preempt anonymous new
// demand, but must not displace concrete capacity that reconciliation is
// already responsible for preserving.
func acceptConcreteNestedCapRequests(
	requests []SessionRequest,
	limits nestedCapLimits,
	usage *nestedCapUsage,
	accepted map[string][]SessionRequest,
	trace *sessionReconcilerTraceCycle,
) []bool {
	consumed := make([]bool, len(requests))
	for i, request := range requests {
		if !isResumeLikeTier(request.Tier) && request.SessionBeadID == "" {
			continue
		}
		if usage.isDuplicateSessionRequest(request) {
			consumed[i] = true
			continue
		}
		if site, reason, payload, rejected := usage.rejection(request, limits); rejected {
			if trace != nil {
				trace.RecordDecision(site, reason, TraceOutcomeRejected, request.Template, "", payload)
			}
			consumed[i] = true
			continue
		}
		accepted[request.Template] = append(accepted[request.Template], request)
		usage.accept(request, limits)
		consumed[i] = true
	}
	return consumed
}

type nestedCapFloor struct {
	template string
	minimum  int
}

func reserveNestedCapFloors(
	cfg *config.City,
	requests []SessionRequest,
	aliasHeldTemplates map[string]struct{},
	limits nestedCapLimits,
	usage *nestedCapUsage,
	accepted map[string][]SessionRequest,
	consumed []bool,
	trace *sessionReconcilerTraceCycle,
) {
	floors := make([]nestedCapFloor, 0, len(cfg.Agents))
	for i := range cfg.Agents {
		agent := &cfg.Agents[i]
		template := agent.QualifiedName()
		if agent.Suspended || agent.EffectiveMinActiveSessions() == 0 {
			continue
		}
		if _, held := aliasHeldTemplates[template]; held {
			continue
		}
		floors = append(floors, nestedCapFloor{template: template, minimum: agent.EffectiveMinActiveSessions()})
	}
	sort.Slice(floors, func(i, j int) bool { return floors[i].template < floors[j].template })

	for _, floor := range floors {
		if limits.agentRigUnresolved[floor.template] {
			log.Printf("pool desired state: template %q rig_resolution_error: refusing min_active_sessions floor %d for non-city agent",
				floor.template, floor.minimum)
			if trace != nil {
				trace.RecordDecision(TraceSitePoolRigCap, TraceReasonRigCap, TraceOutcomeRejected, floor.template, "", traceRecordPayload{
					"floor_template":       floor.template,
					"floor_min":            floor.minimum,
					"rig_resolution_error": true,
				})
			}
			continue
		}
		for i, request := range requests {
			if usage.agentCount[floor.template] >= floor.minimum {
				break
			}
			if request.Template != floor.template || consumed[i] {
				continue
			}
			consumed[i] = true
			request.FloorGuarantee = request.Tier == "new"
			if !acceptNestedCapFloor(request, floor, limits, usage, accepted, trace) {
				break
			}
		}
		for usage.agentCount[floor.template] < floor.minimum {
			request := SessionRequest{Template: floor.template, Tier: "new", FloorGuarantee: true}
			if !acceptNestedCapFloor(request, floor, limits, usage, accepted, trace) {
				break
			}
		}
	}
}

func acceptNestedCapFloor(
	request SessionRequest,
	floor nestedCapFloor,
	limits nestedCapLimits,
	usage *nestedCapUsage,
	accepted map[string][]SessionRequest,
	trace *sessionReconcilerTraceCycle,
) bool {
	if usage.isDuplicateSessionRequest(request) {
		return true
	}
	if site, reason, payload, rejected := usage.rejection(request, limits); rejected {
		if trace != nil {
			payload["floor_template"] = floor.template
			payload["floor_min"] = floor.minimum
			trace.RecordDecision(site, reason, TraceOutcomeRejected, floor.template, "", payload)
		}
		return false
	}
	accepted[floor.template] = append(accepted[floor.template], request)
	if trace != nil {
		trace.RecordDecision(TraceSitePoolMinFill, TraceReasonMinFill, TraceOutcomeAccepted, floor.template, "", traceRecordPayload{
			"min":     floor.minimum,
			"current": usage.agentCount[floor.template],
			"tier":    request.Tier,
		})
	}
	usage.accept(request, limits)
	return true
}

type nestedCapLimits struct {
	workspaceMax       int
	rigMax             map[string]int
	agentMax           map[string]int
	agentRig           map[string]string
	agentRigUnresolved map[string]bool
}

type nestedCapUsage struct {
	agentCount      map[string]int
	rigCount        map[string]int
	workspaceCount  int
	seenSessionBead map[string]bool
	requests        []SessionRequest
}

type nestedCapFloorReservations map[string]int

func newNestedCapLimits(cfg *config.City) nestedCapLimits {
	limits := nestedCapLimits{
		workspaceMax:       -1,
		rigMax:             make(map[string]int),
		agentMax:           make(map[string]int),
		agentRig:           make(map[string]string),
		agentRigUnresolved: make(map[string]bool),
	}
	if cfg.Workspace.MaxActiveSessions != nil {
		limits.workspaceMax = *cfg.Workspace.MaxActiveSessions
	}
	for _, rig := range cfg.Rigs {
		if rig.MaxActiveSessions != nil {
			limits.rigMax[rig.Name] = *rig.MaxActiveSessions
		} else {
			limits.rigMax[rig.Name] = -1
		}
	}
	for i := range cfg.Agents {
		agent := &cfg.Agents[i]
		template := agent.QualifiedName()
		limits.agentRig[template], limits.agentRigUnresolved[template] = nestedCapAgentRigName(agent, cfg.Rigs)
		resolved := agent.ResolvedMaxActiveSessions(cfg)
		if resolved != nil {
			limits.agentMax[template] = *resolved
		} else {
			limits.agentMax[template] = -1
		}
	}
	return limits
}

// nestedCapAgentRigName distinguishes no rig cap ("", false), a resolved rig
// (name, false), and an explicitly rig-scoped agent with no matching rig ("", true).
func nestedCapAgentRigName(agent *config.Agent, rigs []config.Rig) (string, bool) {
	if rigName := agentRigScopeName(agent, rigs); rigName != "" {
		return rigName, false
	}
	if agent == nil {
		return "", false
	}
	scope := strings.TrimSpace(agent.Scope)
	if scope == "city" {
		return "", false
	}
	agentDir := filepath.Clean(strings.TrimSpace(agent.Dir))
	if agentDir == "." {
		return "", scope == "rig"
	}
	for _, rig := range rigs {
		if rig.Path != "" && filepath.Clean(rig.Path) == agentDir {
			return rig.Name, false
		}
	}
	return "", scope == "rig"
}

func newNestedCapUsage() nestedCapUsage {
	return nestedCapUsage{
		agentCount:      make(map[string]int),
		rigCount:        make(map[string]int),
		seenSessionBead: make(map[string]bool),
	}
}

// usageExcludingTemplateNew returns base with template's own acceptedNew
// "new" tier contribution removed from the agent, rig, and workspace counts,
// and those requests filtered out of requests. Callers that need to evaluate
// "would accepting newCount more requests for template hit a cap" (as
// newDemandBlockingScope does) must pass a usage that has not yet absorbed
// that template's own newCount, or the predicate double-counts and can
// blame the wrong cap.
//
// The returned value is base itself, unmodified, when acceptedNew <= 0
// (nothing to exclude), and a value with freshly cloned agentCount,
// rigCount, and requests otherwise. seenSessionBead is never read by
// newDemandBlockingScope, so it is intentionally left aliased with base in
// both cases rather than cloned.
func usageExcludingTemplateNew(base nestedCapUsage, template string, rig string, acceptedNew int) nestedCapUsage {
	if acceptedNew <= 0 {
		return base
	}
	adjusted := base
	adjusted.agentCount = make(map[string]int, len(base.agentCount))
	for k, v := range base.agentCount {
		adjusted.agentCount[k] = v
	}
	adjusted.agentCount[template] -= acceptedNew
	adjusted.rigCount = make(map[string]int, len(base.rigCount))
	for k, v := range base.rigCount {
		adjusted.rigCount[k] = v
	}
	if rig != "" {
		adjusted.rigCount[rig] -= acceptedNew
	}
	adjusted.workspaceCount -= acceptedNew
	adjusted.requests = make([]SessionRequest, 0, len(base.requests))
	for _, req := range base.requests {
		if req.Template == template && req.Tier == "new" {
			continue
		}
		adjusted.requests = append(adjusted.requests, req)
	}
	return adjusted
}

func acceptedNestedCapUsage(limits nestedCapLimits, requests []SessionRequest) nestedCapUsage {
	usage := newNestedCapUsage()
	sorted := append([]SessionRequest(nil), requests...)
	// Same descending-rank ordering as applyNestedCaps — this mirror sort must
	// agree with the one that produced requests, or usage simulated here
	// diverges from what was actually accepted.
	sort.SliceStable(sorted, func(i, j int) bool {
		if sorted[i].BeadPriority != sorted[j].BeadPriority {
			return sorted[i].BeadPriority > sorted[j].BeadPriority
		}
		if sorted[i].Tier != sorted[j].Tier {
			return isResumeLikeTier(sorted[i].Tier) && !isResumeLikeTier(sorted[j].Tier)
		}
		return false
	})
	for _, req := range sorted {
		if usage.canAccept(req, limits) {
			usage.accept(req, limits)
		}
	}
	return usage
}

func newNestedCapFloorReservations(
	cfg *config.City,
	aliasHeldTemplates map[string]struct{},
	limits nestedCapLimits,
	usage nestedCapUsage,
) nestedCapFloorReservations {
	reservations := make(nestedCapFloorReservations)
	floors := make([]nestedCapFloor, 0, len(cfg.Agents))
	for i := range cfg.Agents {
		agent := &cfg.Agents[i]
		template := agent.QualifiedName()
		if agent.Suspended {
			continue
		}
		if _, held := aliasHeldTemplates[template]; held {
			continue
		}
		minimum := agent.EffectiveMinActiveSessions()
		if agentMax := limits.agentMax[template]; agentMax >= 0 {
			minimum = minInt(minimum, agentMax)
		}
		if minimum > usage.agentCount[template] {
			floors = append(floors, nestedCapFloor{template: template, minimum: minimum})
		}
	}
	sort.Slice(floors, func(i, j int) bool { return floors[i].template < floors[j].template })
	rigReserved := make(map[string]int)
	workspaceReserved := 0
	for _, floor := range floors {
		if limits.agentRigUnresolved[floor.template] {
			continue
		}
		grant := floor.minimum - usage.agentCount[floor.template]
		if rig := limits.agentRig[floor.template]; rig != "" {
			if rigMax := limits.rigMax[rig]; rigMax >= 0 {
				grant = minInt(grant, rigMax-usage.rigCount[rig]-rigReserved[rig])
			}
		}
		if limits.workspaceMax >= 0 {
			grant = minInt(grant, limits.workspaceMax-usage.workspaceCount-workspaceReserved)
		}
		if grant <= 0 {
			continue
		}
		reservations[floor.template] = usage.agentCount[floor.template] + grant
		workspaceReserved += grant
		if rig := limits.agentRig[floor.template]; rig != "" {
			rigReserved[rig] += grant
		}
	}
	return reservations
}

// concreteNestedCapLimit bounds how many already-concrete requests (protected
// plus in-flight) the demand pre-pass selects for a template and charges to the
// shared cap usage. For a normally-capped template newCount already carries
// every applicable bound. capNewDemandCount instead fails closed at 0 for an
// explicitly rig-scoped agent whose rig does not resolve, which must not also
// drop the concrete sessions reconciliation already owns, so the bound is
// rebuilt here from the template's own agent max. Of the three caps, that is
// the one applyNestedCaps still enforces per-template on such a request: the
// rig cap drops out because an unresolved rig contributes no rig counter
// (nestedCapAgentRigName leaves agentRig empty), and the workspace cap is
// shared rather than per-template (see below). Floor reservations are
// deliberately not subtracted — a floor must never displace concrete capacity.
//
// Without the agent-max bound the pre-pass charged usage for concrete requests
// the authoritative applyNestedCaps pass rejects anyway, and because that pass
// can only reject candidates and never mint them, the over-charge permanently
// cost every later template in the cfg.Agents loop that much new-demand
// headroom. The shared workspace max is deliberately still not applied here: it
// would make a live session's survival depend on cfg.Agents order, dropping a
// late template's protected session before applyNestedCaps — whose
// concrete-first phase is the authority on that contest — ever sees it. Leaving
// it out only over-generates concrete candidates for that pass to reject, and
// the workspace over-charge that can remain is inert: once usage reaches the
// workspace cap, later templates have no new-demand headroom either way.
func concreteNestedCapLimit(limits nestedCapLimits, usage nestedCapUsage, template string, newCount, effectiveDemand, concreteCount int) int {
	// The normal path leaves the whole bound to newCount. The demand pre-pass
	// sizes newCount against a static concrete baseline rather than capacity
	// accumulated across earlier cfg.Agents templates (gcw-tuwx8.4), so a
	// later-declared template's live session is not dropped merely for its
	// declaration order; applyNestedCaps' concrete-first phase decides any
	// genuine contest. (Upstream tracks the accumulated-usage variant of this
	// asymmetry in ga-2c8ll.)
	if !limits.agentRigUnresolved[template] {
		return newCount
	}
	limit := minInt(effectiveDemand, concreteCount)
	if agentMax := limits.agentMax[template]; agentMax >= 0 {
		limit = minInt(limit, max(0, agentMax-usage.agentCount[template]))
	}
	return limit
}

// capNewDemandCount bounds demand by agent, rig, and workspace headroom
// remaining against usage, net of capacity floors reserve for other templates.
// Callers evaluating multiple templates that may share a rig or workspace cap
// must pass a static, unmutated usage snapshot per template (not one
// accumulated across templates in declaration order) so the result reflects
// this template's own headroom rather than whatever an earlier template
// already consumed (gcw-tuwx8.4).
func capNewDemandCount(limits nestedCapLimits, usage nestedCapUsage, floors nestedCapFloorReservations, agent *config.Agent, demand int) int {
	if demand <= 0 {
		return 0
	}
	template := agent.QualifiedName()
	if limits.agentRigUnresolved[template] {
		return 0
	}
	remaining := demand
	if agentMax := limits.agentMax[template]; agentMax >= 0 {
		remaining = minInt(remaining, agentMax-usage.agentCount[template])
	}
	if rig := limits.agentRig[template]; rig != "" {
		rigMax, ok := limits.rigMax[rig]
		if !ok {
			rigMax = -1
		}
		if rigMax >= 0 {
			headroom := rigMax - usage.rigCount[rig] - floors.reservedForOthers(usage, template, rig, limits)
			remaining = minInt(remaining, headroom)
		}
	}
	if limits.workspaceMax >= 0 {
		headroom := limits.workspaceMax - usage.workspaceCount - floors.reservedForOthers(usage, template, "", limits)
		remaining = minInt(remaining, headroom)
	}
	if remaining < 0 {
		return 0
	}
	return remaining
}

func (r nestedCapFloorReservations) reservedForOthers(usage nestedCapUsage, template, rig string, limits nestedCapLimits) int {
	reserved := 0
	for floorTemplate, minimum := range r {
		if floorTemplate == template || (rig != "" && limits.agentRig[floorTemplate] != rig) {
			continue
		}
		if unmet := minimum - usage.agentCount[floorTemplate]; unmet > 0 {
			reserved += unmet
		}
	}
	return reserved
}

func (u nestedCapUsage) canAccept(req SessionRequest, limits nestedCapLimits) bool {
	if u.isDuplicateSessionRequest(req) {
		return false
	}
	_, _, _, rejected := u.rejection(req, limits)
	return !rejected
}

func (u nestedCapUsage) isDuplicateSessionRequest(req SessionRequest) bool {
	return req.SessionBeadID != "" && u.seenSessionBead[req.SessionBeadID]
}

func (u nestedCapUsage) rejection(req SessionRequest, limits nestedCapLimits) (TraceSiteCode, TraceReasonCode, traceRecordPayload, bool) {
	template := req.Template
	if agentMax := limits.agentMax[template]; agentMax >= 0 && u.agentCount[template] >= agentMax {
		return TraceSitePoolAgentCap, TraceReasonAgentCap, traceRecordPayload{
			"agent_max": agentMax,
			"current":   u.agentCount[template],
			"tier":      req.Tier,
		}, true
	}
	rig := limits.agentRig[template]
	if rig != "" {
		rigMax, ok := limits.rigMax[rig]
		if !ok {
			rigMax = -1
		}
		if rigMax >= 0 && u.rigCount[rig] >= rigMax {
			return TraceSitePoolRigCap, TraceReasonRigCap, traceRecordPayload{
				"rig":     rig,
				"rig_max": rigMax,
				"current": u.rigCount[rig],
				"tier":    req.Tier,
			}, true
		}
	}
	if limits.workspaceMax >= 0 && u.workspaceCount >= limits.workspaceMax {
		return TraceSitePoolWorkspaceCap, TraceReasonWorkspaceCap, traceRecordPayload{
			"workspace_max": limits.workspaceMax,
			"current":       u.workspaceCount,
			"tier":          req.Tier,
		}, true
	}
	return "", "", nil, false
}

func (u *nestedCapUsage) accept(req SessionRequest, limits nestedCapLimits) {
	u.agentCount[req.Template]++
	if rig := limits.agentRig[req.Template]; rig != "" {
		u.rigCount[rig]++
	}
	u.workspaceCount++
	if req.SessionBeadID != "" {
		u.seenSessionBead[req.SessionBeadID] = true
	}
	u.requests = append(u.requests, req)
}

func recordNewDemandCapTrace(
	trace *sessionReconcilerTraceCycle,
	template string,
	agent *config.Agent,
	limits nestedCapLimits,
	usage nestedCapUsage,
	scaleCount int,
	newCount int,
) {
	if scaleCount <= 0 || newCount >= scaleCount {
		return
	}
	if limits.agentRigUnresolved[template] {
		log.Printf("pool desired state: template %q rig_resolution_error: refusing %d new session(s) for non-city agent scope=%q dir=%q",
			template, scaleCount-newCount, strings.TrimSpace(agent.Scope), strings.TrimSpace(agent.Dir))
		if trace != nil {
			trace.RecordDecision(TraceSitePoolNewDemandCap, TraceReasonRigCap, TraceOutcomeRejected, template, "", traceRecordPayload{
				"scale_check":          scaleCount,
				"accepted_new":         newCount,
				"blocked_new":          scaleCount - newCount,
				"current":              usage.agentCount[template],
				"max":                  0,
				"blocking_sessions":    []string{},
				"blocking_work_beads":  []string{},
				"active_capacity_kind": "rig_resolution_error",
				"rig_resolution_error": true,
			})
		}
		return
	}
	if trace == nil {
		return
	}
	site, reason, capMax, current, blockers := newDemandBlockingScope(template, agent, limits, usage, newCount)
	if site == "" {
		return
	}
	blockingSessions := make([]string, 0, len(blockers))
	blockingWork := make([]string, 0, len(blockers))
	for _, req := range blockers {
		if req.SessionBeadID != "" {
			blockingSessions = append(blockingSessions, req.SessionBeadID)
		}
		if req.WorkBeadID != "" {
			blockingWork = append(blockingWork, req.WorkBeadID)
		}
	}
	trace.RecordDecision(site, reason, TraceOutcomeRejected, template, "", traceRecordPayload{
		"scale_check":          scaleCount,
		"accepted_new":         newCount,
		"blocked_new":          scaleCount - newCount,
		"current":              current,
		"max":                  capMax,
		"blocking_sessions":    blockingSessions,
		"blocking_work_beads":  blockingWork,
		"active_capacity_kind": string(reason),
	})
}

func newDemandBlockingScope(
	template string,
	agent *config.Agent,
	limits nestedCapLimits,
	usage nestedCapUsage,
	newCount int,
) (TraceSiteCode, TraceReasonCode, int, int, []SessionRequest) {
	if agentMax := limits.agentMax[template]; agentMax >= 0 && agentMax-usage.agentCount[template] <= newCount {
		return TraceSitePoolNewDemandCap, TraceReasonAgentCap, agentMax, usage.agentCount[template], filterCapBlockers(usage.requests, func(req SessionRequest) bool {
			return req.Template == template
		})
	}
	if agent != nil {
		if rig := limits.agentRig[template]; rig != "" {
			rigMax, ok := limits.rigMax[rig]
			if !ok {
				rigMax = -1
			}
			if rigMax >= 0 && rigMax-usage.rigCount[rig] <= newCount {
				return TraceSitePoolNewDemandCap, TraceReasonRigCap, rigMax, usage.rigCount[rig], filterCapBlockers(usage.requests, func(req SessionRequest) bool {
					return limits.agentRig[req.Template] == rig
				})
			}
		}
	}
	if limits.workspaceMax >= 0 && limits.workspaceMax-usage.workspaceCount <= newCount {
		return TraceSitePoolNewDemandCap, TraceReasonWorkspaceCap, limits.workspaceMax, usage.workspaceCount, usage.requests
	}
	return "", "", 0, 0, nil
}

func filterCapBlockers(requests []SessionRequest, keep func(SessionRequest) bool) []SessionRequest {
	out := make([]SessionRequest, 0, len(requests))
	for _, req := range requests {
		if keep(req) {
			out = append(out, req)
		}
	}
	return out
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func isKnownPoolTemplate(assignee string, cfg *config.City) bool {
	assignee = strings.TrimSpace(assignee)
	if assignee == "" || cfg == nil {
		return false
	}
	for i := range cfg.Agents {
		agent := &cfg.Agents[i]
		if agent.Suspended || !agent.SupportsGenericEphemeralSessions() {
			continue
		}
		if agentTemplateIdentitiesEquivalent(cfg, assignee, agent.QualifiedName()) {
			return true
		}
	}
	return false
}

func isResumeLikeTier(tier string) bool {
	return tier == "resume" || tier == "wake-known-identity"
}

// isConfiguredNamedSessionIdentity reports whether assignee names a
// configured [[named_session]]'s own identity — checked structurally via
// cfg.NamedSessions, with no live-session/store lookup, so it holds even
// when the named session has no live session bead at all. A named session
// whose backing agent is suspended is excluded: the named-session tier
// never claims work for a suspended agent (mirrors the namedSpecs filter in
// build_desired_state.go), so exempting it here too would orphan the bead
// with neither side picking it up.
func isConfiguredNamedSessionIdentity(cfg *config.City, assignee string) bool {
	assignee = strings.TrimSpace(assignee)
	if assignee == "" || cfg == nil {
		return false
	}
	spec, ok := findNamedSessionSpec(cfg, "", assignee)
	if !ok || spec.Agent == nil {
		return false
	}
	return !spec.Agent.Suspended
}
