package events

import "encoding/json"

// Domain payload types shared across packages. Payloads specific to one
// package live with their emitter (see internal/api/event_payloads.go and
// internal/extmsg/events.go); this file holds payload shapes that are
// used by multiple callers — today, the supervisor's Dolt maintenance
// loop and its CLI/API projections (beads ga-e3s, ga-zn8, ga-p5n).

// StoreMaintenanceDonePayload is the typed payload for
// gc.store.maintenance.done events. Emitted after a successful
// maintenance cycle (backup snapshot + CALL DOLT_GC + smoke test).
type StoreMaintenanceDonePayload struct {
	DurationSeconds float64 `json:"duration_s"`
	BeforeBytes     int64   `json:"before_bytes"`
	AfterBytes      int64   `json:"after_bytes"`
	SnapshotPath    string  `json:"snapshot_path"`
}

// IsEventPayload marks StoreMaintenanceDonePayload as an events.Payload variant.
func (StoreMaintenanceDonePayload) IsEventPayload() {}

// StoreMaintenanceFailedPayload is the typed payload for
// gc.store.maintenance.failed events. Emitted when a maintenance stage
// returns an error. Stage names the failing phase ("backup" | "gc" |
// "smoke-test" | "prune"); ErrorMsg carries the human-readable cause;
// SnapshotPath is populated when the backup stage completed before a
// later stage failed (so operators can recover from the snapshot).
type StoreMaintenanceFailedPayload struct {
	Stage           string  `json:"stage"`
	ErrorMsg        string  `json:"error_msg"`
	SnapshotPath    string  `json:"snapshot_path,omitempty"`
	DurationSeconds float64 `json:"duration_s"`
}

// IsEventPayload marks StoreMaintenanceFailedPayload as an events.Payload variant.
func (StoreMaintenanceFailedPayload) IsEventPayload() {}

// BeadWorktreeReapedPayload is the typed payload for bead.worktree.reaped
// events. Emitted when the worktree reaper successfully removes a merged
// worktree and its branch after a bead is closed.
type BeadWorktreeReapedPayload struct {
	BeadID string `json:"bead_id"`
	Path   string `json:"path"`
	Rig    string `json:"rig"`
	Branch string `json:"branch"`
}

// IsEventPayload marks BeadWorktreeReapedPayload as an events.Payload variant.
func (BeadWorktreeReapedPayload) IsEventPayload() {}

// BeadWorktreeReapSkippedPayload is the typed payload for
// bead.worktree.reap_skipped events. Emitted when the worktree reaper
// decides not to remove a worktree (e.g., unmerged changes, open bead).
type BeadWorktreeReapSkippedPayload struct {
	BeadID string `json:"bead_id"`
	Path   string `json:"path"`
	Rig    string `json:"rig"`
	Reason string `json:"reason"`
}

// IsEventPayload marks BeadWorktreeReapSkippedPayload as an events.Payload variant.
func (BeadWorktreeReapSkippedPayload) IsEventPayload() {}

// BeadClaimRejectedPayload is the typed payload for bead.claim_rejected events
// (ADR-0009). Emitted when AttemptedClaimant tries to claim BeadID while it is
// already live-claimed by ExistingClaimant; the second claim is rejected as an
// idempotent no-op. The payload makes the lost-claim race observable for
// eval/audit (RCA gc-typpc: one bead concurrently claimed by four workers).
type BeadClaimRejectedPayload struct {
	BeadID            string `json:"bead_id"`
	ExistingClaimant  string `json:"existing_claimant"`
	AttemptedClaimant string `json:"attempted_claimant"`
}

// IsEventPayload marks BeadClaimRejectedPayload as an events.Payload variant.
func (BeadClaimRejectedPayload) IsEventPayload() {}

func init() {
	RegisterPayload(BeadWorktreeReaped, BeadWorktreeReapedPayload{})
	RegisterPayload(BeadWorktreeReapSkipped, BeadWorktreeReapSkippedPayload{})
	RegisterPayload(BeadClaimRejected, BeadClaimRejectedPayload{})
}

// StoreDiskWarnPayload is the typed payload for gc.store.disk_warn events.
// Emitted before CALL DOLT_GC when free space is below GC_DOLT_WARN_FREE_BYTES
// but above GC_DOLT_MIN_FREE_BYTES; the GC proceeds.
type StoreDiskWarnPayload struct {
	FreeBytes  int64  `json:"free_bytes"`
	WarnBytes  int64  `json:"warn_bytes"`
	FloorBytes int64  `json:"floor_bytes"`
	DataDir    string `json:"data_dir"`
}

// IsEventPayload marks StoreDiskWarnPayload as an events.Payload variant.
func (StoreDiskWarnPayload) IsEventPayload() {}

// StoreDiskCriticalPayload is the typed payload for gc.store.disk_critical
// events. Emitted before CALL DOLT_GC when free space is below
// GC_DOLT_MIN_FREE_BYTES; the GC is skipped to avoid growing the store.
type StoreDiskCriticalPayload struct {
	FreeBytes  int64  `json:"free_bytes"`
	FloorBytes int64  `json:"floor_bytes"`
	DataDir    string `json:"data_dir"`
}

// IsEventPayload marks StoreDiskCriticalPayload as an events.Payload variant.
func (StoreDiskCriticalPayload) IsEventPayload() {}

// SessionResetStalledPayload is the typed payload for
// session.reset_stalled events. It identifies the session whose reset
// completion has stalled and the reset timestamp used to compute the
// elapsed diagnostic threshold.
type SessionResetStalledPayload struct {
	SessionName      string `json:"session_name"`
	Template         string `json:"template"`
	ResetCommittedAt string `json:"reset_committed_at"`
	ElapsedSeconds   int    `json:"elapsed_s"`
}

// IsEventPayload marks SessionResetStalledPayload as an events.Payload variant.
func (SessionResetStalledPayload) IsEventPayload() {}

// SessionResetStalledPayloadJSON builds the JSON wire form for attachment to
// an Event.Payload field.
func SessionResetStalledPayloadJSON(sessionName, template, resetCommittedAt string, elapsedSeconds int) json.RawMessage {
	b, _ := json.Marshal(SessionResetStalledPayload{
		SessionName:      sessionName,
		Template:         template,
		ResetCommittedAt: resetCommittedAt,
		ElapsedSeconds:   elapsedSeconds,
	})
	return b
}

// SessionContinuationObservedPayload is the diagnostic payload for
// session.continuation_observed events. Correlation fields deliberately use
// the persisted string forms so an initial value such as generation "0" is
// distinguishable from an absent value. InstanceTokenFingerprint must contain
// only a one-way digest; raw instance tokens never belong in event data.
type SessionContinuationObservedPayload struct {
	SchemaVersion            string   `json:"schema_version" doc:"Payload schema version. Version 1 is emitted by this release."`
	Boundary                 string   `json:"boundary" doc:"Observed boundary: reset, runtime_stop, runtime_start, provider_hook, or mail_injection."`
	Source                   string   `json:"source" doc:"Code path that reached the boundary, such as explicit_reset, session_reconciler, or pre_compact."`
	Outcome                  string   `json:"outcome" doc:"Result observed at this boundary. This is diagnostic evidence, not an end-to-end delivery guarantee."`
	SessionName              string   `json:"session_name,omitempty" doc:"Stable runtime session name when known."`
	Template                 string   `json:"template,omitempty" doc:"Configured agent template when known."`
	Generation               string   `json:"generation,omitempty" doc:"Persisted session generation, kept as a string so generation 0 differs from an absent value."`
	ContinuationEpoch        string   `json:"continuation_epoch,omitempty" doc:"Persisted provider-continuation epoch when known."`
	InstanceTokenFingerprint string   `json:"instance_token_fingerprint,omitempty" doc:"SHA-256 fingerprint used to correlate an opaque session instance token without exposing the token."`
	HookEvent                string   `json:"hook_event,omitempty" doc:"Provider hook event, such as SessionStart, PreCompact, or UserPromptSubmit."`
	HookSource               string   `json:"hook_source,omitempty" doc:"Bounded provider hook-origin label when supplied by the runtime."`
	OldWorkID                string   `json:"old_work_id,omitempty" doc:"Work bead associated with the session before the boundary."`
	NewWorkID                string   `json:"new_work_id,omitempty" doc:"Work bead associated with the session after the boundary."`
	MailIDs                  []string `json:"mail_ids,omitempty" doc:"Mail bead IDs written to or injected by the hook boundary."`
	MessageCount             *int     `json:"message_count,omitempty" doc:"Number of messages handled by a mail hook. Zero is present only when the hook counted zero messages."`
	BodyBytes                *int     `json:"body_bytes,omitempty" doc:"Byte length of the provider hook context written; the context body itself is never recorded."`
	Route                    string   `json:"route,omitempty" doc:"Observed command or provider route, such as fallback, suspended, codex, or gemini."`
	ErrorCode                string   `json:"error_code,omitempty" doc:"Stable coarse failure code. Raw error strings are never recorded."`
}

// IsEventPayload marks SessionContinuationObservedPayload as an events.Payload
// variant.
func (SessionContinuationObservedPayload) IsEventPayload() {}

// SessionContinuationObservedPayloadJSON builds the JSON wire form for
// attachment to an Event.Payload field.
func SessionContinuationObservedPayloadJSON(p SessionContinuationObservedPayload) json.RawMessage {
	b, _ := json.Marshal(p)
	return b
}

func init() {
	RegisterPayload(SessionContinuationObserved, SessionContinuationObservedPayload{})
}
