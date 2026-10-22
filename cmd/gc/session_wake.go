package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/rollout/gate"
	"github.com/gastownhall/gascity/internal/runtime"
	sessions "github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/telemetry"
)

// errTokenMismatch indicates the running session's instance token
// doesn't match the expected one — the session was re-woken by a
// different incarnation and this drain/stop is stale.
var errTokenMismatch = errors.New("instance token mismatch")

// startPathWriteTimeout bounds a single start-path metadata write at the call
// site, below the bd-exec general timeout (~120s), so a write stalled by a
// saturated backing store fails fast. The reconciler then releases the
// per-session mutation lock and retries on the next tick instead of wedging
// new-session starts permanently. (gcw-7te)
const startPathWriteTimeout = 20 * time.Second

// errStartWriteTimedOut is returned by setMetadataBatchBounded when a
// start-path metadata write exceeds its deadline. It is transient: the start
// fails this tick and is retried fresh on the next.
var errStartWriteTimedOut = errors.New("start-path metadata write timed out")

// metadataBatchWriter is the minimal write surface setMetadataBatchBounded
// needs. Both the shared beads.Store and the session front-door's
// beads.SessionStore (sessFront.Store()) satisfy it.
type metadataBatchWriter interface {
	SetMetadataBatch(id string, kvs map[string]string) error
}

// setMetadataBatchBounded runs store.SetMetadataBatch with a wall-clock
// deadline. On timeout it returns errStartWriteTimedOut and stops waiting on
// the write goroutine; the underlying bd-exec carries its own timeout, so that
// goroutine drains rather than leaking unboundedly. This bounds only the start
// path — the shared store interface is intentionally left unchanged.
func setMetadataBatchBounded(store metadataBatchWriter, id string, kvs map[string]string, timeout time.Duration) error {
	done := make(chan error, 1)
	go func() { done <- store.SetMetadataBatch(id, kvs) }()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case err := <-done:
		return err
	case <-timer.C:
		return errStartWriteTimedOut
	}
}

// errTokenUnverifiable indicates the running session's instance token could
// not be read, so a kill by name cannot be proven to hit the intended
// incarnation. The stop is skipped and retried, never treated as stale.
var errTokenUnverifiable = errors.New("instance token unverifiable")

// tokenUnverifiableError is verifiedStop's errTokenUnverifiable. Its message
// omits the read error's text, which may match runtime.IsSessionGone (tmux's
// "no tmux server running") and so read "could not verify" as "gone". The
// sentinel and the cause stay reachable through errors.Is.
type tokenUnverifiableError struct {
	sessionID string
	cause     error
}

func (e *tokenUnverifiableError) Error() string {
	return fmt.Sprintf("%v for session %s (token_unverifiable)", errTokenUnverifiable, e.sessionID)
}

func (e *tokenUnverifiableError) Unwrap() []error { return []error{errTokenUnverifiable, e.cause} }

// preWakeCommit persists a new incarnation (generation + token) BEFORE
// starting the process. This is Phase 1 of the two-phase wake protocol.
// Returns the new generation, instance token, and the PreWakePatch batch it
// persisted so the caller can fold it onto its coherent typed snapshot
// (write-returns-Info) instead of re-projecting the bead. It reads the current
// persisted state off the caller's typed Info (session_name, generation,
// continuation epoch, sleep_reason, wake_mode, and the continuation-reset
// signals) — every field a verbatim raw mirror — so no raw bead crosses in.
func preWakeCommit(
	info sessions.Info,
	sessFront *sessions.Store,
	clk clock.Clock,
) (newGen int, token string, fold sessions.MetadataPatch, err error) {
	name := info.SessionNameMetadata
	if !sessions.IsSessionNameSyntaxValid(name) {
		return 0, "", nil, fmt.Errorf("invalid session_name %q", name)
	}

	gen, _ := strconv.Atoi(info.Generation)
	newGen = gen + 1
	token = sessions.NewInstanceToken()
	continuationEpoch, _ := strconv.Atoi(info.ContinuationEpoch)
	if continuationEpoch <= 0 {
		continuationEpoch = sessions.DefaultContinuationEpoch
	}
	if shouldBumpContinuationEpoch(info) {
		continuationEpoch++
	}

	sleepReason := ""
	if info.SleepReason == string(sessions.SleepReasonIdleTimeout) {
		// Preserve the idle-timeout wake override until the replacement
		// session has actually started. Failed starts must retry next tick.
		sleepReason = string(sessions.SleepReasonIdleTimeout)
	}

	freshWake := info.WakeMode == "fresh" || pendingContinuationResetNeedsFreshStart(info)
	batch := sessions.PreWakePatch(sessions.PreWakePatchInput{
		Generation:        newGen,
		InstanceToken:     token,
		ContinuationEpoch: continuationEpoch,
		Now:               clk.Now(),
		SleepReason:       sleepReason,
		FreshWake:         freshWake,
		// A retry of a claimed pending create continues its episode, so the
		// stale-create bound must keep measuring from the episode start.
		EpisodePendingCreateStartedAt: pendingCreateEpisodeStartedAt(info),
	})
	if writeErr := setMetadataBatchBounded(sessFront.Store(), info.ID, batch, startPathWriteTimeout); writeErr != nil {
		return 0, "", nil, fmt.Errorf("pre-wake metadata commit: %w", writeErr)
	}
	traceFreshWakeMetadataReset(name, freshWakeResetPriorValues(info), batch, freshWake)

	return newGen, token, batch, nil
}

// pendingCreateEpisodeStartedAt returns the pending_create_started_at a wake
// must carry forward, or "" when the wake opens a new episode and should stamp
// a fresh one.
//
// Only a held pending_create_claim continues an episode. Every site that sets
// the claim (bead creation, named-session reopen, wake requests) stamps a
// fresh marker in the same write, so while the claim is held the marker is the
// start of the current episode. Claimed rows are also the only ones whose
// stale-create rollback checks the configured start lease first, so keeping an
// old marker cannot make an in-flight start look stale. A claimless wake keeps
// the per-attempt stamp: the lifecycle projection ages a claimless creating
// row out on that marker alone, and an inherited one could project a healthy
// start asleep mid-spawn.
func pendingCreateEpisodeStartedAt(info sessions.Info) string {
	if !info.PendingCreateClaim {
		return ""
	}
	return info.PendingCreateStartedAt
}

// freshWakeResetPriorValues reconstructs the pre-reset values of the fresh-wake
// conversation-reset keys off the typed Info so traceFreshWakeMetadataReset can
// report which durable provider markers a fresh wake cleared without the raw
// bead. The keys mirror sessions.FreshWakeConversationResetKeys().
func freshWakeResetPriorValues(info sessions.Info) map[string]string {
	return map[string]string{
		"session_key":             info.SessionKey,
		"started_config_hash":     info.StartedConfigHash,
		"started_live_hash":       info.StartedLiveHash,
		"live_hash":               info.LiveHash,
		"startup_dialog_verified": info.StartupDialogVerified,
		// Priming markers share the fresh-wake reset (S19 Stage 2), so their prior
		// values come off the verbatim raw Info mirrors — otherwise the trace's
		// before[key] lookup reads "" and the cleared list omits them even though
		// FreshWakeConversationResetKeys() clears them. Written as raw string keys
		// to match the sibling entries.
		"primed_at":            info.PrimedAtMetadata,
		"priming_attempted_at": info.PrimingAttemptedAtMetadata,
		"prompt_hash":          info.PromptHashMetadata,
	}
}

func traceFreshWakeMetadataReset(name string, before map[string]string, batch sessions.MetadataPatch, freshWake bool) {
	if !freshWake || os.Getenv("GC_TMUX_TRACE") != "1" {
		return
	}
	cleared := make([]string, 0, len(sessions.FreshWakeConversationResetKeys()))
	for _, key := range sessions.FreshWakeConversationResetKeys() {
		if strings.TrimSpace(before[key]) == "" || batch[key] != "" {
			continue
		}
		cleared = append(cleared, key)
	}
	if len(cleared) == 0 {
		return
	}
	log.Printf(
		"[WAKE-TRACE] preWakeCommit session=%s wake_mode=fresh cleared_provider_metadata=%s",
		name,
		strings.Join(cleared, ","),
	)
}

func shouldBumpContinuationEpoch(info sessions.Info) bool {
	if info.ContinuationResetPending != "" {
		return true
	}
	return info.WakeMode == "fresh" && info.LastWokeAt != ""
}

func pendingContinuationResetNeedsFreshStart(info sessions.Info) bool {
	switch sessions.State(strings.TrimSpace(info.MetadataState)) {
	case sessions.StateStartPending, sessions.StateCreating:
		return false
	}
	return strings.TrimSpace(info.ContinuationResetPending) != "" &&
		strings.TrimSpace(info.StartedConfigHash) != ""
}

// validateWorkDir ensures the path is safe to use as a working directory.
func validateWorkDir(dir string) error {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	if abs != filepath.Clean(abs) {
		return fmt.Errorf("non-canonical path")
	}
	info, err := os.Stat(abs)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("not a directory")
	}
	return nil
}

// beginSessionDrainInfo initiates an async drain. Returns immediately.
// The drainTracker stores in-memory state; advanceSessionDrainsWithSessionsTraced progresses it.
//
// Returns true when this call enqueued a new drain (a state transition) and
// false when a drain was already enqueued for this session (no-op). Callers
// that emit user-visible log lines or convergence events tied to the drain
// MUST gate on the return value — otherwise those emissions fire every
// reconciler tick for the life of a stuck drain.
//
// The interrupt signal (Ctrl-C) is NOT sent immediately. It is deferred to
// the next reconciler tick via advanceSessionDrainsWithSessionsTraced. This gives the drain
// one full tick to be canceled (e.g., if the session was falsely orphaned
// due to a transient store failure) before any signal reaches the process.
// Without this, a single bad tick can interrupt a working agent mid-tool-call.
//
// It reads only session_name, generation, and id — all carried verbatim on Info.
func executionProtocolEnabled(front *sessions.Store, enrolled bool) bool {
	if enrolled {
		return true
	}
	mode := beads.InspectConditionalWrites(front.Store().Store).Mode
	return mode == gate.Auto || mode == gate.Require
}

func beginSessionDrainInfo(info sessions.Info, sp runtime.Provider, dt *drainTracker, reason string, clk clock.Clock, timeout time.Duration, fronts ...*sessions.Store) bool {
	begun, err := beginSessionDrainInfoResult(info, sp, dt, reason, clk, timeout, fronts...)
	if err != nil {
		log.Printf("session drain: issuing %s for %s: %v", reason, info.ID, err)
	}
	return begun
}

func beginSessionDrainInfoResult(
	info sessions.Info,
	_ runtime.Provider, // kept for caller compatibility; interrupt deferred to advanceSessionDrainsWithSessionsTraced
	dt *drainTracker,
	reason string,
	clk clock.Clock,
	timeout time.Duration,
	fronts ...*sessions.Store,
) (bool, error) {
	name := info.SessionNameMetadata
	if dt.get(info.ID) != nil {
		if os.Getenv("GC_TMUX_TRACE") == "1" {
			log.Printf("[DRAIN-TRACE] beginSessionDrain session=%s reason=%s noop=already-draining", name, reason)
		}
		return false, nil
	}
	attempt := ""
	if len(fronts) > 0 && fronts[0] != nil {
		front := fronts[0]
		current, _, enrolled, err := front.ExecutionEvidence(info.ID)
		if err != nil {
			return false, err
		}
		if current.InstanceToken != info.InstanceToken || current.Generation != info.Generation {
			return false, sessions.ErrExecutionIncarnationChanged
		}
		if executionProtocolEnabled(front, enrolled) {
			attempt, err = front.BeginControllerDrainAdmission(info.ID, sessions.ExecutionFence{InstanceToken: info.InstanceToken, Generation: info.Generation})
			if err != nil {
				return false, err
			}
		}
	}
	gen, _ := strconv.Atoi(info.Generation)

	dt.set(info.ID, &drainState{
		admissionAttempt: attempt,
		startedAt:        clk.Now(),
		deadline:         clk.Now().Add(timeout),
		reason:           reason,
		generation:       gen,
	})

	if os.Getenv("GC_TMUX_TRACE") == "1" {
		log.Printf("[DRAIN-TRACE] beginSessionDrain session=%s reason=%s", name, reason)
	}
	telemetry.RecordDrainTransition(context.Background(), name, reason, "begin")
	return true, nil
}

// executionStalledDrainReason drains a pool seat that claimed work and then
// never executed it, after the execution backstop spent its bounded nudges
// (execution_backstop.go).
//
// It is NOT cancelable, by any of the three cancel lenses, and that is the whole
// point of having its own reason. The session it drains is — by construction —
// alive, awake, and holding an in_progress claim, which is exactly the shape
// every keep-alive guard is built to protect: the assigned-work cancel would
// cancel it on the same claim that justified it, and the plain cancel would
// cancel it the moment any wake reason reappeared. A cancelable drain here is
// not a drain at all; the session stays wedged holding work no one else can
// take, which is the failure this whole lane exists to end.
//
// Convergence chain once it fires: tracked drain -> deferred interrupt -> stop ->
// session bead closed -> the claim released by the dead-assignee reopen lane ->
// the row is demand again -> a fresh seat claims it.
const executionStalledDrainReason = "execution-stalled"

func drainReasonCancelable(reason string) bool {
	return reason != "config-drift" && reason != "orphaned" && reason != "suspended" &&
		reason != executionStalledDrainReason && reason != idleRespawnDrainReason
}

func pendingDrainReasonCancelable(reason string) bool {
	return reason != "orphaned" && reason != "suspended" && reason != executionStalledDrainReason
}

// liveClaimDrainReasonCancelable is the live-claim cancel lens: the in-flight
// drain reasons that a live claim held by the session (sessionOwnsLiveClaim)
// may cancel. Only "orphaned": it is a demand-class verdict that a one-tick
// stale view (an out-of-process claim the cache has not seen) can produce, so
// it must be revisable once the claim is visible. Every other non-cancelable
// reason is operator or agent intent (suspended, config-drift,
// execution-stalled, idle-respawn) and stays final; the caller additionally
// requires liveClaimVetoApplies, so an orphaned drain of a removed or
// suspended agent is never canceled either.
func liveClaimDrainReasonCancelable(reason string) bool {
	return reason == "orphaned"
}

// orphanedDrainInFlightInfo reports whether the session has a reconciler-owned
// "orphaned" drain in flight, either tracked in memory or recovered from the
// runtime's reconciler drain-ack metadata (e.g. after a controller restart).
// Agent-sourced drain acks are never reported: those are the agent's intent.
func orphanedDrainInFlightInfo(info sessions.Info, sp runtime.Provider, dt *drainTracker, name string) bool {
	if dt != nil {
		if ds := dt.get(info.ID); ds != nil && liveClaimDrainReasonCancelable(ds.reason) {
			return true
		}
	}
	reason, ok := reconcilerDrainAckMatchesSessionInfo(info, sp, name)
	return ok && liveClaimDrainReasonCancelable(reason)
}

// cancelOrphanedDrainForLiveClaimInfo cancels an in-flight reconciler-owned
// "orphaned" drain — the tracked drain and/or its published drain ack — once the
// session is known to hold a live claim. It reports whether anything was
// canceled.
func cancelOrphanedDrainForLiveClaimInfo(info sessions.Info, sp runtime.Provider, dt *drainTracker, name string, fronts ...*sessions.Store) bool {
	canceled := dt != nil && cancelSessionDrainIfInfo(info, sp, dt, liveClaimDrainReasonCancelable, fronts...)
	recovered, err := cancelRecoveredSessionDrainInfoResult(info, sp, name, liveClaimDrainReasonCancelable, fronts...)
	if err != nil {
		log.Printf("session drain: recovering live-claim cancellation for %s: %v", info.ID, err)
	}
	return canceled || recovered && err == nil
}

const (
	reconcilerDrainAckSourceKey   = "GC_DRAIN_ACK_SOURCE"
	reconcilerDrainAckSourceValue = "reconciler"
	drainAckSourceAgentValue      = "agent"
	// drainAckRequesterInstanceTokenKey binds an agent acknowledgement to the
	// incarnation that wrote it. Pane environment is per-CHAIR state and pool
	// chairs are recycled under the same name, so without this an ack outlives
	// its author and the next occupant inherits it.
	drainAckRequesterInstanceTokenKey = "GC_DRAIN_ACK_REQUESTER_INSTANCE_TOKEN"
	reconcilerDrainAckReasonKey       = "GC_DRAIN_REASON"
	reconcilerDrainAckGenerationKey   = "GC_DRAIN_GENERATION"
)

const drainAckFrameKey = "GC_DRAIN_ADMISSION_FRAME"

// drainAckFrame is one provider value: policy reason and operation identity
// cannot be assembled from separate publications. It is committed last.
type drainAckFrame struct {
	V           int    `json:"v"`
	Source      string `json:"source"`
	Reason      string `json:"reason,omitempty"`
	Generation  string `json:"generation"`
	SessionID   string `json:"session_id"`
	RuntimeName string `json:"runtime_name"`
	Incarnation string `json:"incarnation"`
	Attempt     string `json:"attempt"`
}

func publishDrainAckFrame(sp runtime.Provider, name string, info sessions.Info, source, reason, attempt string) error {
	frame := drainAckFrame{V: 1, Source: source, Reason: reason, Generation: info.Generation, SessionID: info.ID, RuntimeName: name, Incarnation: drainAckInstanceTokenDigest(info.InstanceToken), Attempt: attempt}
	raw, err := json.Marshal(frame)
	if err != nil {
		return err
	}
	return sp.SetMeta(name, drainAckFrameKey, string(raw))
}

func readDrainAckFrame(info sessions.Info, sp runtime.Provider, name string) (drainAckFrame, error) {
	var frame drainAckFrame
	raw, err := sp.GetMeta(name, drainAckFrameKey)
	if err != nil {
		return frame, err
	}
	if raw == "" {
		return frame, sessions.ErrDrainAdmissionChanged
	}
	if len(raw) > 4096 {
		return frame, errors.New("drain acknowledgement frame exceeds limit")
	}
	if err := json.Unmarshal([]byte(raw), &frame); err != nil {
		return frame, fmt.Errorf("reading drain acknowledgement frame: %w", err)
	}
	if frame.V != 1 || frame.SessionID != info.ID || frame.RuntimeName != name || frame.RuntimeName != info.SessionNameMetadata || frame.Generation != info.Generation || frame.Incarnation != drainAckInstanceTokenDigest(info.InstanceToken) || frame.Attempt == "" || (frame.Source != reconcilerDrainAckSourceValue && frame.Source != drainAckSourceAgentValue) || (frame.Source == reconcilerDrainAckSourceValue && frame.Reason == "") {
		return frame, sessions.ErrDrainAdmissionChanged
	}
	runtimeID, err := sp.GetMeta(name, "GC_SESSION_ID")
	if err != nil {
		return frame, err
	}
	token, err := sp.GetMeta(name, "GC_INSTANCE_TOKEN")
	if err != nil {
		return frame, err
	}
	if runtimeID != info.ID || token != info.InstanceToken {
		return frame, sessions.ErrExecutionIncarnationChanged
	}
	return frame, nil
}

func setReconcilerDrainAckMetadata(sp runtime.Provider, name string, ds *drainState, fronts ...*sessions.Store) error {
	if ds == nil {
		return nil
	}
	var front *sessions.Store
	var info sessions.Info
	if len(fronts) > 0 && fronts[0] != nil {
		front = fronts[0]
		id, err := resolveSessionID(front.Store().Store, name)
		if err != nil {
			return err
		}
		current, _, enrolled, err := front.ExecutionEvidence(id)
		if err != nil {
			return err
		}
		info = current
		if executionProtocolEnabled(front, enrolled) {
			if ds.admissionAttempt == "" {
				return sessions.ErrDrainAdmissionChanged
			}
			if _, err := front.InspectDrainAdmission(id, sessions.ExecutionFence{InstanceToken: info.InstanceToken, Generation: info.Generation}, ds.admissionAttempt); err != nil {
				return err
			}
		} else {
			front = nil
		}
	}
	if err := sp.SetMeta(name, drainAdmissionAttemptKey, ds.admissionAttempt); err != nil {
		return err
	}
	if err := sp.SetMeta(name, reconcilerDrainAckSourceKey, reconcilerDrainAckSourceValue); err != nil {
		return err
	}
	if err := sp.SetMeta(name, reconcilerDrainAckReasonKey, ds.reason); err != nil {
		_ = clearReconcilerDrainAckMetadata(sp, name)
		return err
	}
	if err := sp.SetMeta(name, reconcilerDrainAckGenerationKey, strconv.Itoa(ds.generation)); err != nil {
		_ = clearReconcilerDrainAckMetadata(sp, name)
		return err
	}
	if err := sp.SetMeta(name, "GC_DRAIN_ACK", "1"); err != nil {
		_ = clearReconcilerDrainAckMetadata(sp, name)
		return err
	}
	if front != nil {
		if err := publishDrainAckFrame(sp, name, info, reconcilerDrainAckSourceValue, ds.reason, ds.admissionAttempt); err != nil {
			return err
		}
	} else if err := sp.RemoveMeta(name, drainAckFrameKey); err != nil {
		return err
	}
	if front != nil {
		_, err := front.InspectDrainAdmission(info.ID, sessions.ExecutionFence{InstanceToken: info.InstanceToken, Generation: info.Generation}, ds.admissionAttempt)
		return err
	}
	return nil
}

func clearReconcilerDrainAckMetadata(sp runtime.Provider, name string) error {
	if sp == nil {
		return fmt.Errorf("session provider is nil")
	}
	var errs []error
	for _, key := range []string{
		"GC_DRAIN_ACK",
		// Frames are bounded retained receipts. Native exact-attempt validation
		// makes old receipts inert; deleting here could erase a concurrent commit.
		drainAdmissionAttemptKey,
		reconcilerDrainAckSourceKey,
		// Cleared with the source it belongs to: a requester stamp that outlives
		// its acknowledgement is residue waiting for a later source to make it
		// look like evidence.
		drainAckRequesterInstanceTokenKey,
		reconcilerDrainAckReasonKey,
		reconcilerDrainAckGenerationKey,
	} {
		if err := sp.RemoveMeta(name, key); err != nil {
			log.Printf("session wake: clearing reconciler drain ack metadata %s for %s: %v", key, name, err)
			errs = append(errs, fmt.Errorf("removing %s: %w", key, err))
		}
	}
	return errors.Join(errs...)
}

// cancelSessionDrainInfo removes a cancelable drain if wake reasons reappeared
// for the same generation. If GC_DRAIN_ACK was already set by the reconciler
// (deferred drain signal), it is cleared so the Phase 1 drain-ack check doesn't
// kill the session. It reads the session id/generation/name off the Info snapshot.
func cancelSessionDrainInfo(info sessions.Info, sp runtime.Provider, dt *drainTracker, fronts ...*sessions.Store) bool {
	return cancelSessionDrainIfInfo(info, sp, dt, drainReasonCancelable, fronts...)
}

// cancelSessionDrainForPendingInfo cancels a pending-drain-cancelable drain for
// the reconciler's Phase-2 drain scan, working off the Info snapshot.
func cancelSessionDrainForPendingInfo(info sessions.Info, sp runtime.Provider, dt *drainTracker, fronts ...*sessions.Store) bool {
	return cancelSessionDrainIfInfo(info, sp, dt, pendingDrainReasonCancelable, fronts...)
}

// cancelSessionDrainForAssignedWorkInfo cancels an assigned-work-cancelable drain
// for the reconciler's Phase-2 drain scan, working off the Info snapshot.
func cancelSessionDrainForAssignedWorkInfo(info sessions.Info, sp runtime.Provider, dt *drainTracker, fronts ...*sessions.Store) bool {
	return cancelSessionDrainIfInfo(info, sp, dt, assignedWorkDrainReasonCancelable, fronts...)
}

func assignedWorkDrainReasonCancelable(reason string) bool {
	switch reason {
	case "orphaned", "no-wake-reason":
		return true
	default:
		return false
	}
}

// cancelSessionConfigDriftDrainInfo cancels a config-drift drain off the Info
// snapshot, threading Info straight into the typed drain-cancel core.
func cancelSessionConfigDriftDrainInfo(info sessions.Info, sp runtime.Provider, dt *drainTracker, fronts ...*sessions.Store) bool {
	canceled, err := cancelConfigDriftDrainInfoResult(info, sp, dt, fronts...)
	if err != nil {
		log.Printf("session drain: recovering config-drift cancellation for %s: %v", info.ID, err)
	}
	return canceled
}

func cancelConfigDriftDrainInfoResult(info sessions.Info, sp runtime.Provider, dt *drainTracker, fronts ...*sessions.Store) (bool, error) {
	return cancelConfigDriftDrainAttemptInfoResult(info, sp, dt, nil, fronts...)
}

func cancelConfigDriftDrainAttemptInfoResult(info sessions.Info, sp runtime.Provider, dt *drainTracker, expected *string, fronts ...*sessions.Store) (bool, error) {
	if dt != nil {
		if ds := dt.get(info.ID); ds != nil {
			if expected != nil && ds.admissionAttempt != *expected {
				return false, sessions.ErrDrainAdmissionChanged
			}
			return cancelSessionDrainIfInfoResult(info, sp, dt, func(reason string) bool { return reason == "config-drift" }, fronts...)
		}
	}
	return cancelRecoveredSessionDrainAttemptInfoResult(info, sp, info.SessionNameMetadata, func(reason string) bool { return reason == "config-drift" }, expected, fronts...)
}

// cancelSessionExecutionAdmission linearizes reopening before provider cleanup.

func cancelSessionExecutionAdmissionAttempt(info sessions.Info, expected string, fronts ...*sessions.Store) error {
	if len(fronts) == 0 || fronts[0] == nil {
		return nil
	}
	front := fronts[0]
	current, cell, enrolled, err := front.ExecutionEvidence(info.ID)
	if err != nil {
		return err
	}
	if !enrolled {
		return nil
	}
	if current.InstanceToken != info.InstanceToken || current.Generation != info.Generation {
		return sessions.ErrExecutionIncarnationChanged
	}
	attempt := ""
	if cell.Drain != nil {
		if cell.Drain.Generation == info.Generation && cell.Drain.Incarnation == drainAckInstanceTokenDigest(info.InstanceToken) && (expected == "" || cell.Drain.Attempt != expected) {
			return sessions.ErrDrainAdmissionChanged
		}
		attempt = cell.Drain.Attempt
	}
	return front.CancelDrainAdmission(info.ID, sessions.ExecutionFence{InstanceToken: info.InstanceToken, Generation: info.Generation}, attempt)
}

// cancelSessionDrainIfInfo is the typed core of the drain-cancel helpers. It
// preserves the existing reason/generation policy while canceling the exact
// durable intent before removing the tracker or provider acknowledgement.
func cancelSessionDrainIfInfo(info sessions.Info, sp runtime.Provider, dt *drainTracker, canCancel func(string) bool, fronts ...*sessions.Store) bool {
	ok, err := cancelSessionDrainIfInfoResult(info, sp, dt, canCancel, fronts...)
	if err != nil && !errors.Is(err, sessions.ErrDrainStopAccepted) && !errors.Is(err, sessions.ErrDrainAdmissionChanged) {
		log.Printf("session drain: canceling %s: %v", info.ID, err)
	}
	return ok
}

func cancelSessionDrainIfInfoResult(info sessions.Info, sp runtime.Provider, dt *drainTracker, canCancel func(string) bool, fronts ...*sessions.Store) (bool, error) {
	ds := dt.get(info.ID)
	if ds == nil {
		return false, nil
	}
	if !canCancel(ds.reason) {
		return false, nil
	}
	gen, _ := strconv.Atoi(info.Generation)
	if gen == ds.generation {
		if err := cancelSessionExecutionAdmissionAttempt(info, ds.admissionAttempt, fronts...); err != nil {
			return false, err
		}
		name := info.SessionNameMetadata
		// Clear GC_DRAIN_ACK if it was set — prevents stale ack from
		// killing the session on the next Phase 1 drain-ack check.
		if ds.ackSet {
			if err := clearReconcilerDrainAckMetadata(sp, name); err != nil {
				return false, err
			}
		}
		dt.clearIdleProbe(info.ID)
		dt.removeIfCurrent(info.ID, ds)
		telemetry.RecordDrainTransition(context.Background(), name, ds.reason, "cancel")
		return true, nil
	}
	return false, nil
}

// cancelReconcilerAckedDrainInfo cancels a reconciler-owned drain ack off the
// Info snapshot: it reads the session_name (Info.SessionNameMetadata), generation
// (via reconcilerDrainAckMatchesSessionInfo) and id (dt keying) — all carried
// verbatim on Info — and routes the cancel through the typed drain-cancel core.
func cancelReconcilerAckedDrainInfo(info sessions.Info, sp runtime.Provider, dt *drainTracker, fronts ...*sessions.Store) bool {
	if dt == nil {
		return false
	}
	name := strings.TrimSpace(info.SessionNameMetadata)
	reason, ok := reconcilerDrainAckMatchesSessionInfo(info, sp, name)
	if !ok || !pendingDrainReasonCancelable(reason) {
		return false
	}
	ds := dt.get(info.ID)
	if ds == nil || !ds.ackSet {
		return false
	}
	return cancelSessionDrainForPendingInfo(info, sp, dt, fronts...)
}

func reconcilerDrainAckMatchesSession(session beads.Bead, sp runtime.Provider, name string) (string, bool) {
	if sp == nil || name == "" {
		return "", false
	}
	source, err := sp.GetMeta(name, reconcilerDrainAckSourceKey)
	if err != nil || source != reconcilerDrainAckSourceValue {
		return "", false
	}
	reason, err := sp.GetMeta(name, reconcilerDrainAckReasonKey)
	if err != nil || reason == "" {
		return "", false
	}
	expectedGeneration, err := sp.GetMeta(name, reconcilerDrainAckGenerationKey)
	if err != nil || expectedGeneration == "" {
		return "", false
	}
	currentGeneration := strings.TrimSpace(session.Metadata["generation"])
	if currentGeneration == "" || currentGeneration != expectedGeneration {
		return "", false
	}
	return reason, true
}

// reconcilerDrainAckMatchesSessionInfo is the session.Info sibling of
// reconcilerDrainAckMatchesSession for the reconciler forward pass. The only
// session-bead read is the generation (Info.Generation); everything else is
// provider metadata (sp) and the caller-supplied name, shared verbatim with the
// raw form — so it is byte-identical, pinned by the sessionGeneration oracle row.
func reconcilerDrainAckMatchesSessionInfo(info sessions.Info, sp runtime.Provider, name string) (string, bool) {
	if sp == nil || name == "" {
		return "", false
	}
	raw, err := sp.GetMeta(name, drainAckFrameKey)
	if err != nil {
		return "", false
	}
	if raw != "" {
		frame, err := readDrainAckFrame(info, sp, name)
		return frame.Reason, err == nil && frame.Source == reconcilerDrainAckSourceValue
	}
	source, err := sp.GetMeta(name, reconcilerDrainAckSourceKey)
	if err != nil || source != reconcilerDrainAckSourceValue {
		return "", false
	}
	reason, err := sp.GetMeta(name, reconcilerDrainAckReasonKey)
	if err != nil || reason == "" {
		return "", false
	}
	expectedGeneration, err := sp.GetMeta(name, reconcilerDrainAckGenerationKey)
	if err != nil || expectedGeneration == "" {
		return "", false
	}
	currentGeneration := strings.TrimSpace(info.Generation)
	if currentGeneration == "" || currentGeneration != expectedGeneration {
		return "", false
	}
	return reason, true
}

func staleReconcilerDrainAck(session beads.Bead, sp runtime.Provider, name string) bool {
	if sp == nil || name == "" {
		return false
	}
	source, err := sp.GetMeta(name, reconcilerDrainAckSourceKey)
	if err != nil || source != reconcilerDrainAckSourceValue {
		return false
	}
	expectedGeneration, err := sp.GetMeta(name, reconcilerDrainAckGenerationKey)
	if err != nil || expectedGeneration == "" {
		return true
	}
	currentGeneration := strings.TrimSpace(session.Metadata["generation"])
	return currentGeneration == "" || currentGeneration != expectedGeneration
}

// staleReconcilerDrainAckInfo is the session.Info sibling of
// staleReconcilerDrainAck: the only session-bead read is the generation
// (Info.Generation), matching the raw form byte-for-byte (sessionGeneration
// oracle row).
func staleReconcilerDrainAckInfo(info sessions.Info, sp runtime.Provider, name string) bool {
	if sp == nil || name == "" {
		return false
	}
	source, err := sp.GetMeta(name, reconcilerDrainAckSourceKey)
	if err != nil || source != reconcilerDrainAckSourceValue {
		return false
	}
	expectedGeneration, err := sp.GetMeta(name, reconcilerDrainAckGenerationKey)
	if err != nil || expectedGeneration == "" {
		return true
	}
	currentGeneration := strings.TrimSpace(info.Generation)
	return currentGeneration == "" || currentGeneration != expectedGeneration
}

func staleOrLegacyDrainAckBeforeStart(session beads.Bead, sp runtime.Provider, name string) bool {
	if sp == nil || name == "" {
		return false
	}
	source, err := sp.GetMeta(name, reconcilerDrainAckSourceKey)
	if err == nil && source == drainAckSourceAgentValue {
		return false
	}
	if err == nil && source == reconcilerDrainAckSourceValue {
		return staleReconcilerDrainAck(session, sp, name)
	}
	acked, err := sp.GetMeta(name, "GC_DRAIN_ACK")
	return err == nil && acked == "1"
}

// staleOrLegacyDrainAckBeforeStartInfo is the session.Info sibling of
// staleOrLegacyDrainAckBeforeStart: it defers to staleReconcilerDrainAckInfo for
// the reconciler-owned branch (the only session-bead read, Info.Generation) and
// otherwise reads provider metadata only, so it is byte-identical to the raw form.
func staleOrLegacyDrainAckBeforeStartInfo(info sessions.Info, sp runtime.Provider, name string) bool {
	if sp == nil || name == "" {
		return false
	}
	source, err := sp.GetMeta(name, reconcilerDrainAckSourceKey)
	if err == nil && source == drainAckSourceAgentValue {
		return false
	}
	if err == nil && source == reconcilerDrainAckSourceValue {
		return staleReconcilerDrainAckInfo(info, sp, name)
	}
	acked, err := sp.GetMeta(name, "GC_DRAIN_ACK")
	return err == nil && acked == "1"
}

// cancelRecoveredReconcilerAckedDrainInfo clears a reconciler-owned drain ack
// whose in-memory tracker entry did not survive (recovered from provider
// metadata alone). Off the Info snapshot: the only session-bead read is the
// generation via reconcilerDrainAckMatchesSessionInfo.
func cancelRecoveredReconcilerAckedDrainInfo(info sessions.Info, sp runtime.Provider, name string, fronts ...*sessions.Store) bool {
	canceled, err := cancelRecoveredSessionDrainInfoResult(info, sp, name, pendingDrainReasonCancelable, fronts...)
	if err != nil {
		log.Printf("session drain: recovering pending cancellation for %s: %v", info.ID, err)
	}
	return canceled && err == nil
}

// cancelRecoveredDrainForAssignedWorkInfo binds recovered cancellation to the
// published intent rather than whichever newer attempt the native row contains.
func cancelRecoveredDrainForAssignedWorkInfo(info sessions.Info, sp runtime.Provider, name string, fronts ...*sessions.Store) bool {
	canceled, err := cancelRecoveredSessionDrainInfoResult(info, sp, name, assignedWorkDrainReasonCancelable, fronts...)
	if err != nil {
		log.Printf("session drain: recovering assigned-work cancellation for %s: %v", info.ID, err)
	}
	return canceled && err == nil
}

func cancelRecoveredSessionDrainInfoResult(info sessions.Info, sp runtime.Provider, name string, canCancel func(string) bool, fronts ...*sessions.Store) (bool, error) {
	return cancelRecoveredSessionDrainAttemptInfoResult(info, sp, name, canCancel, nil, fronts...)
}

func cancelRecoveredSessionDrainAttemptInfoResult(info sessions.Info, sp runtime.Provider, name string, canCancel func(string) bool, expected *string, fronts ...*sessions.Store) (bool, error) {
	strict, active := false, false
	captured := ""
	if len(fronts) > 0 && fronts[0] != nil {
		current, cell, enrolled, err := fronts[0].ExecutionEvidence(info.ID)
		if err != nil {
			return false, err
		}
		if current.InstanceToken != info.InstanceToken || current.Generation != info.Generation {
			return false, sessions.ErrExecutionIncarnationChanged
		}
		strict = executionProtocolEnabled(fronts[0], enrolled)
		active = cell.Drain != nil && cell.Drain.Generation == info.Generation && cell.Drain.Incarnation == drainAckInstanceTokenDigest(info.InstanceToken)
		if active {
			captured = cell.Drain.Attempt
		}
	}
	if sp == nil || name == "" {
		if strict && active {
			return false, sessions.ErrDrainAdmissionChanged
		}
		return false, nil
	}
	reason := ""
	if strict {
		frame, err := readDrainAckFrame(info, sp, name)
		if err != nil {
			if !active && errors.Is(err, sessions.ErrDrainAdmissionChanged) {
				return false, nil
			}
			return false, err
		}
		if frame.Source != reconcilerDrainAckSourceValue {
			return false, sessions.ErrDrainAdmissionChanged
		}
		if active && frame.Attempt != captured {
			return false, sessions.ErrDrainAdmissionChanged
		}
		if expected != nil && active && frame.Attempt != *expected {
			return false, sessions.ErrDrainAdmissionChanged
		}
		if !canCancel(frame.Reason) {
			return false, nil
		}
		reason = frame.Reason
		if err := cancelSessionExecutionAdmissionAttempt(info, frame.Attempt, fronts...); err != nil {
			return false, err
		}
	} else {
		var ok bool
		reason, ok = reconcilerDrainAckMatchesSessionInfo(info, sp, name)
		if !ok || !canCancel(reason) {
			return false, nil
		}
	}
	if err := clearReconcilerDrainAckMetadata(sp, name); err != nil {
		return true, err
	}
	telemetry.RecordDrainTransition(context.Background(), name, reason, "cancel")
	return true, nil
}

func advanceSessionDrainsWithSessionsTraced(
	dt *drainTracker,
	sp runtime.Provider,
	store beads.Store,
	infoLookup func(id string) (sessions.Info, bool),
	wakeEvals map[string]wakeEvaluation,
	cfg *config.City,
	clk clock.Clock,
	trace *sessionReconcilerTraceCycle,
	deferred ...map[string]bool,
) {
	// wakeEvals is required. The reconciler builds it from the coherent infoByID
	// snapshot via ComputeAwakeSet -> awakeSetToWakeEvals; tests supply explicit
	// wakeEvals encoding the premise they exercise. Step 5d dropped the raw-bead
	// wakeEvals==nil fallback and its now-unused sessionBeads/poolDesired/workSet/
	// readyWaitSet inputs from this prod core — the scan runs entirely off infoLookup.
	// Session front door constructed once from the same store; nil when store is
	// nil so completeDrain keeps its store==nil short-circuit.
	sessFront := sessionFrontDoor(store)
	if store == nil {
		sessFront = nil
	}
	for id, ds := range dt.all() {
		if len(deferred) > 0 && deferred[0][id] {
			continue
		}
		info, ok := infoLookup(id)
		if !ok {
			dt.clearIdleProbe(id)
			dt.removeIfCurrent(id, ds)
			continue
		}
		// The whole scan runs off the typed Info: decision reads (session_name,
		// generation, template), the drain-complete write (completeDrain → store),
		// the cancel checks (cancelSessionDrainFor*Info), verifiedStop, and the
		// process-running probe (by info.ID). Nothing reads the raw bead.
		name := info.SessionNameMetadata

		// Stale check: if session was re-woken (generation changed), cancel drain.
		gen, _ := strconv.Atoi(info.Generation)
		if gen != ds.generation {
			dt.clearIdleProbe(id)
			if ds.ackSet {
				_ = clearReconcilerDrainAckMetadata(sp, name)
			}
			dt.removeIfCurrent(id, ds)
			if trace != nil {
				trace.RecordDecision(TraceSiteDrainStale, TraceReasonStaleGeneration, TraceOutcomeCancel, normalizedSessionTemplateInfo(info, cfg), name, traceRecordPayload{
					"drain_reason":       ds.reason,
					"drain_generation":   ds.generation,
					"session_generation": gen,
				})
			}
			continue
		}

		// Check if process exited.
		running, err := workerSessionTargetRunningWithConfig("", store, sp, cfg, info.ID)
		if err != nil {
			if trace != nil {
				trace.RecordDecision(TraceSiteDrainComplete, TraceReasonCode(ds.reason), TraceOutcomeSkippedLivenessError, normalizedSessionTemplateInfo(info, cfg), name, traceRecordPayload{
					"liveness_error": err.Error(),
				})
			}
			continue
		}
		if !running {
			// Process exited — drain complete.
			completeDrain(info, sessFront, ds, clk)
			dt.clearIdleProbe(id)
			dt.removeIfCurrent(id, ds)
			telemetry.RecordDrainTransition(context.Background(), name, ds.reason, "complete")
			if trace != nil {
				trace.RecordDecision(TraceSiteDrainComplete, TraceReasonCode(ds.reason), TraceOutcomeComplete, normalizedSessionTemplateInfo(info, cfg), name, traceRecordPayload{
					"drain_started_at": ds.startedAt,
				})
			}
			continue
		}

		// Idle-respawn is a recovery action, not permission to interrupt a
		// session that resumed work after the probe completed. Revalidate both
		// the assigned-work premise and runtime activity immediately before the
		// reconciler publishes its drain acknowledgement. Observation failures
		// fail closed and leave the session running.
		if ds.reason == idleRespawnDrainReason {
			eval, exists := wakeEvals[info.ID]
			lastActivity, activityErr := workerSessionTargetLastActivityWithConfig("", store, sp, cfg, name)
			if !exists || !idleRespawnEligible(info, eval, clk.Now()) || activityErr != nil || lastActivity.After(ds.startedAt) {
				canceled, err := cancelSessionDrainIfInfoResult(info, sp, dt, func(reason string) bool { return reason == idleRespawnDrainReason }, sessFront)
				if err != nil && !errors.Is(err, sessions.ErrDrainStopAccepted) {
					continue
				}
				if canceled {
					if trace != nil {
						fields := traceRecordPayload{"activity_resumed": lastActivity.After(ds.startedAt)}
						if activityErr != nil {
							fields["activity_error"] = activityErr.Error()
						}
						trace.RecordDecision(TraceSiteDrainCancel, TraceReasonCode(ds.reason), TraceOutcomeCancel, normalizedSessionTemplateInfo(info, cfg), name, fields)
					}
					continue
				}
				if err == nil {
					continue
				}
			}
		}

		if eval, ok := wakeEvals[info.ID]; ok &&
			containsWakeReason(eval.Reasons, WakePending) &&
			pendingDrainReasonCancelable(ds.reason) {
			canceled, cancelErr := cancelSessionDrainIfInfoResult(info, sp, dt, pendingDrainReasonCancelable, sessFront)
			if cancelErr != nil && !errors.Is(cancelErr, sessions.ErrDrainStopAccepted) {
				continue
			}
			if !canceled && cancelErr == nil {
				continue
			}
			if canceled {
				if trace != nil {
					trace.RecordDecision(TraceSiteDrainCancel, TraceReasonCode(ds.reason), TraceOutcomeCancelPending, normalizedSessionTemplateInfo(info, cfg), name, nil)
				}
				continue
			}
		}

		if eval, ok := wakeEvals[info.ID]; ok &&
			eval.Reason == "assigned-work" &&
			containsWakeReason(eval.Reasons, WakeWork) &&
			assignedWorkDrainReasonCancelable(ds.reason) {
			canceled, cancelErr := cancelSessionDrainIfInfoResult(info, sp, dt, assignedWorkDrainReasonCancelable, sessFront)
			if cancelErr != nil && !errors.Is(cancelErr, sessions.ErrDrainStopAccepted) {
				continue
			}
			if !canceled && cancelErr == nil {
				continue
			}
			if canceled {
				if trace != nil {
					trace.RecordDecision(TraceSiteDrainCancel, TraceReasonCode(ds.reason), TraceOutcomeCancelAssignedWork, normalizedSessionTemplateInfo(info, cfg), name, nil)
				}
				continue
			}
		}

		// Cancellation check: if wake reasons reappeared, cancel the in-memory
		// drain. Orphaned, suspended, and ordinary config-drift drains are not
		// canceled here.
		if drainReasonCancelable(ds.reason) {
			if eval, ok := wakeEvals[info.ID]; ok && len(eval.Reasons) > 0 {
				canceled, err := cancelSessionDrainIfInfoResult(info, sp, dt, drainReasonCancelable, sessFront)
				if err != nil && !errors.Is(err, sessions.ErrDrainStopAccepted) {
					continue
				}
				if canceled {
					if trace != nil {
						trace.RecordDecision(TraceSiteDrainCancel, TraceReasonCode(ds.reason), TraceOutcomeCancel, normalizedSessionTemplateInfo(info, cfg), name, nil)
					}
					continue
				}
				if err == nil {
					continue
				}
			}
		}

		// Deferred drain signal: set GC_DRAIN_ACK after the drain has survived
		// at least one full tick without being canceled. This prevents a
		// single transient store failure from interrupting a working agent
		// — the false-orphan drain is canceled on the next tick when the
		// store recovers, before any signal is set.
		//
		// Uses the same GC_DRAIN_ACK env var that agents set via
		// `gc runtime drain-ack`. The reconciler's Phase 1 drain-ack check
		// sees it on the next tick and calls sp.Stop() for a clean
		// SIGTERM/SIGKILL — no Ctrl-C keystroke injection into the pane.
		if ds.ackSet && ds.admissionAttempt != "" {
			frame, err := readDrainAckFrame(info, sp, name)
			if err != nil || frame.Attempt != ds.admissionAttempt || frame.Source != reconcilerDrainAckSourceValue || frame.Reason != ds.reason {
				ds.ackSet = false
			}
		}
		if !ds.ackSet {
			if os.Getenv("GC_TMUX_TRACE") == "1" {
				log.Printf("[DRAIN-TRACE] advanceSessionDrainsWithSessionsTraced: setting GC_DRAIN_ACK session=%s reason=%s", name, ds.reason)
			}
			err := setReconcilerDrainAckMetadata(sp, name, ds, sessFront)
			if err == nil {
				ds.ackSet = true
				ds.followUp = true
			}
			if trace != nil {
				outcome := TraceOutcomeSuccess
				fields := traceRecordPayload{
					"reason":          ds.reason,
					"deferred_signal": true,
				}
				if err != nil {
					outcome = TraceOutcomeFailed
					fields["error"] = err.Error()
				}
				fields["template"] = normalizedSessionTemplateInfo(info, cfg)
				fields["before"] = ""
				fields["after"] = "1"
				fields["field"] = "GC_DRAIN_ACK"
				trace.RecordMutation(TraceSiteMutationRuntimeMeta, TraceReasonUnknown, outcome, "provider_meta", name, "GC_DRAIN_ACK", fields)
			}
		}

		if ds.admissionAttempt != "" && sessFront != nil {
			_, err := sessFront.InspectDrainAdmission(info.ID, sessions.ExecutionFence{InstanceToken: info.InstanceToken, Generation: info.Generation}, ds.admissionAttempt)
			if err != nil {
				if errors.Is(err, sessions.ErrDrainAdmissionChanged) {
					dt.removeIfCurrent(id, ds)
				}
				continue
			}
		}

		// Pending-interaction guards and wake-based cancellation run before this
		// timeout path. Preserve that ordering if this block is refactored.
		if clk.Now().After(ds.deadline) {
			// Drain timed out — force stop.
			if sessFront != nil {
				_, _, enrolled, err := sessFront.ExecutionEvidence(info.ID)
				if err != nil {
					continue
				}
				if executionProtocolEnabled(sessFront, enrolled) {
					if ds.admissionAttempt == "" {
						continue
					}
					if err := sessFront.AcceptControllerDrainStop(info.ID, sessions.ExecutionFence{InstanceToken: info.InstanceToken, Generation: info.Generation}, ds.admissionAttempt); err != nil {
						continue
					}
				}
			}
			if err := verifiedStop(info, store, sp, cfg); err != nil {
				if errors.Is(err, errTokenMismatch) {
					// Session was re-woken by a different incarnation.
					// This drain is stale — cancel it.
					dt.clearIdleProbe(id)
					dt.removeIfCurrent(id, ds)
				}
				// Other errors (transient stop failure, unverifiable token): keep drain
				// active for retry on next tick.
				if trace != nil {
					payload := traceRecordPayload{"error": err.Error()}
					if errors.Is(err, errTokenUnverifiable) {
						payload["token"] = "token_unverifiable"
					}
					trace.RecordDecision(TraceSiteDrainTimeout, TraceReasonCode(ds.reason), TraceOutcomeRetry, normalizedSessionTemplateInfo(info, cfg), name, payload)
				}
				continue
			}
			// Re-probe after stop to confirm process actually exited
			// before marking metadata as asleep.
			running, err := workerSessionTargetRunningWithConfig("", store, sp, cfg, info.ID)
			if err != nil {
				if trace != nil {
					trace.RecordDecision(TraceSiteDrainTimeout, TraceReasonCode(ds.reason), TraceOutcomeSkippedLivenessError, normalizedSessionTemplateInfo(info, cfg), name, traceRecordPayload{
						"liveness_error": err.Error(),
					})
				}
				continue
			}
			if !running {
				completeDrain(info, sessFront, ds, clk)
				dt.clearIdleProbe(id)
				dt.removeIfCurrent(id, ds)
				telemetry.RecordDrainTransition(context.Background(), name, ds.reason, "timeout")
				if trace != nil {
					trace.RecordDecision(TraceSiteDrainTimeout, TraceReasonCode(ds.reason), TraceOutcomeComplete, normalizedSessionTemplateInfo(info, cfg), name, nil)
				}
			}
			// If still running after stop, keep drain for next tick.
		}
		// Else: still draining, check again next tick.
	}
}

// completeDrain writes drain-complete metadata to the store for the drained
// session. It reads only the typed Info (id + raw wake_mode); the raw-bead
// mirror the reconciler used to keep is dropped. Nothing reads a drained
// session's metadata later in the tick — the awake scan runs before
// advanceSessionDrainsWithSessionsTraced, and completeDrain is always followed by dt.remove +
// continue — so the store write is the sole observable effect (all completeDrain
// tests assert on store.Get). With no store there is nothing to persist.
func completeDrain(info sessions.Info, sessFront *sessions.Store, ds *drainState, clk clock.Clock) {
	if sessFront == nil {
		return
	}
	batch := sessions.CompleteDrainPatch(clk.Now(), ds.reason, info.WakeMode == "fresh")
	_ = sessFront.ApplyPatch(info.ID, batch)
}

// verifiedStop stops a session after verifying the instance_token matches.
// Prevents stale drain operations from targeting a re-woken session.
// Returns errTokenMismatch if the running process has a different token, and
// errTokenUnverifiable if its token cannot be read.
//
// NOTE: On composite providers (auto/hybrid), GetMeta and Stop may route
// to different backends if the route table is stale. This is a pre-existing
// routing limitation — when the reconciler is wired in, consider a
// provider-level VerifiedStop that atomically verifies+stops on the same backend.
func verifiedStop(info sessions.Info, store beads.Store, sp runtime.Provider, cfg *config.City) error {
	name := info.SessionNameMetadata
	expectedToken := info.InstanceToken
	if expectedToken != "" {
		switch verdict, err := readRuntimeInstanceToken(sp, name, expectedToken); verdict {
		case runtimeTokenMismatch:
			return fmt.Errorf("%w for session %s", errTokenMismatch, info.ID)
		case runtimeTokenUnverifiable:
			return &tokenUnverifiableError{sessionID: info.ID, cause: err}
		}
	}
	handle, err := workerHandleForSessionWithConfig("", store, sp, cfg, info.ID)
	if err != nil {
		return err
	}
	observeAutonomousKillLiveSubagentsForHandle("session drain timeout", name, handle, log.Writer())
	return handle.Kill(context.Background())
}
