package session

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
)

// WorkStartedKey names the session-owned record of claims handed to its turns.
const WorkStartedKey = "work_started"

// ErrExecutionIncarnationChanged refuses execution evidence from a stale or
// unidentified runtime. It deliberately contains no incarnation capability.
var ErrExecutionIncarnationChanged = errors.New("session execution incarnation changed or unavailable")

// ErrClaimAlreadyStarted refuses release of work already handed to a turn.
var ErrClaimAlreadyStarted = errors.New("claim has started executing")

// ErrClaimReleasePending refuses execution while a captured release is pending.
var ErrClaimReleasePending = errors.New("claim release is pending")

// ErrExecutionRevisionMissing refuses a mutation whose row has no usable
// optimistic-concurrency token, independently of backend capability.
var ErrExecutionRevisionMissing = errors.New("execution fence requires a nonzero row revision")

// ErrExecutionDraining refuses work admission during a qualified drain census.
var ErrExecutionDraining = errors.New("session execution admission is draining")

// ErrDrainAdmissionChanged refuses a census that no longer owns admission.
var ErrDrainAdmissionChanged = errors.New("session drain operation was canceled or superseded")

// ErrDrainStopAccepted refuses reopening after the controller accepted a stop.
var ErrDrainStopAccepted = errors.New("session drain stop already accepted")

// ErrExecutionNotClaimable refuses execution after a dormant-state transition.
var ErrExecutionNotClaimable = errors.New("session state does not permit work admission")

// ExecutionClaimStateEligible preserves the session claim states, including
// startup states whose active commit may follow the worker's first claim.
func ExecutionClaimStateEligible(state State) bool {
	switch state {
	case StateNone, StateActive, StateAwake, StateCreating, StateStartPending:
		return true
	default:
		return false
	}
}

// ExecutionFence identifies the runtime writing execution evidence. Generation
// is optional for runtimes that export only their unique instance token.
type ExecutionFence struct {
	InstanceToken string
	Generation    string
}

// WorkStartedEntry records work a session has handed to a turn. Gen identifies
// the session incarnation, not a work claim generation or publication token.
type WorkStartedEntry struct {
	Store string    `json:"store"`
	Bead  string    `json:"bead"`
	Gen   int64     `json:"gen"`
	At    time.Time `json:"at"`
	Via   string    `json:"via"`
}

// WorkStarted is the durable execution record. Entries survive runtime restarts
// so a later drain cannot mistake interrupted work for unexecuted assignments.
type WorkStarted struct {
	V        int                `json:"v"`
	Claims   []WorkStartedEntry `json:"claims"`
	Releases []ClaimRelease     `json:"releases,omitempty"`
	Drain    *DrainAdmission    `json:"drain,omitempty"`
}

// DrainAdmission closes execution admission for one runtime incarnation. Attempt
// identifies a census operation, not a claim or ownership generation.
type DrainAdmission struct {
	Attempt     string `json:"attempt"`
	Incarnation string `json:"incarnation"`
	Generation  string `json:"generation"`
	Committed   bool   `json:"committed"`
	Accepted    bool   `json:"accepted,omitempty"`
}

func executionIncarnationDigest(token string) string {
	digest := sha256.Sum256([]byte(token))
	return hex.EncodeToString(digest[:])
}

func (d *DrainAdmission) matches(row beads.Bead) bool {
	return d != nil && d.Incarnation == executionIncarnationDigest(row.Metadata["instance_token"]) && d.Generation == row.Metadata["generation"]
}

// BeginDrainAdmission closes admission before collecting assigned work. A new
// census supersedes an abandoned uncommitted attempt without reopening admission.
// A committed fence remains terminal for the same runtime incarnation.
func (s *Store) BeginDrainAdmission(id string, fence ExecutionFence) (string, error) {
	return s.beginDrainAdmission(id, fence, false)
}

// BeginControllerDrainAdmission closes admission for a fresh controller intent.
// It does not attest a completed work-release census or release any work.
func (s *Store) BeginControllerDrainAdmission(id string, fence ExecutionFence) (string, error) {
	return s.beginDrainAdmission(id, fence, true)
}

func (s *Store) beginDrainAdmission(id string, fence ExecutionFence, controller bool) (string, error) {
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return "", fmt.Errorf("creating drain operation identity: %w", err)
	}
	attempt := hex.EncodeToString(nonce[:])
	err := s.mutateExecutionEvidence(id, fence, func(cell *WorkStarted, row beads.Bead) (bool, error) {
		if _, err := strconv.ParseInt(strings.TrimSpace(row.Metadata["generation"]), 10, 64); err != nil {
			return false, fmt.Errorf("reading drain generation: %w", err)
		}
		if cell.Drain.matches(row) {
			if !controller && cell.Drain.Committed {
				attempt = cell.Drain.Attempt
				return false, nil
			}
			if cell.Drain.Accepted {
				return false, ErrDrainStopAccepted
			}
		}
		cell.Drain = &DrainAdmission{Attempt: attempt, Incarnation: executionIncarnationDigest(row.Metadata["instance_token"]), Generation: row.Metadata["generation"]}
		return true, nil
	})
	return attempt, err
}

// FinishDrainAdmission commits a completed census or reopens an unsuccessful
// one. Only its exact active attempt may finish; a committed census cannot abort.
func (s *Store) FinishDrainAdmission(id string, fence ExecutionFence, attempt string, commit bool) error {
	return s.mutateExecutionEvidence(id, fence, func(cell *WorkStarted, row beads.Bead) (bool, error) {
		if !cell.Drain.matches(row) || cell.Drain.Attempt != attempt || attempt == "" {
			return false, ErrDrainAdmissionChanged
		}
		if cell.Drain.Accepted && !cell.Drain.Committed {
			return false, ErrDrainStopAccepted
		}
		if cell.Drain.Committed {
			if commit {
				return false, nil
			}
			return false, errors.New("committed drain census cannot reopen admission")
		}
		if commit {
			cell.Drain.Committed = true
		} else {
			cell.Drain = nil
		}
		return true, nil
	})
}

// ExecutionEvidence reads authoritative session execution evidence. The bool
// distinguishes an enrolled empty record from a legacy session with no record.
func (s *Store) ExecutionEvidence(id string) (Info, WorkStarted, bool, error) {
	row, err := s.executionBead(id)
	if err != nil {
		return Info{}, WorkStarted{}, false, err
	}
	raw := row.Metadata[WorkStartedKey]
	cell, err := DecodeWorkStarted(raw)
	return infoFromPersistedBead(row), cell, strings.TrimSpace(raw) != "", err
}

func (s *Store) executionBead(id string) (beads.Bead, error) {
	if s == nil || s.store.Store == nil {
		return beads.Bead{}, fmt.Errorf("loading session %q: %w", id, beads.ErrNotFound)
	}
	live := beads.HandlesFor(s.store.Store).Live
	if live == nil {
		return beads.Bead{}, beads.ErrConditionalWriteUnsupported
	}
	row, err := live.Get(id)
	if err != nil {
		return beads.Bead{}, fmt.Errorf("loading session %q: %w", id, err)
	}
	if strings.TrimSpace(row.ID) == "" || !IsSessionBeadOrRepairable(row) {
		return beads.Bead{}, fmt.Errorf("%w: %s", ErrSessionNotFound, id)
	}
	return row, nil
}

// InspectDrainAdmission CAS-validates and returns one exact census attempt.
// Success proves current admission ownership, not permission to stop a runtime.
func (s *Store) InspectDrainAdmission(id string, fence ExecutionFence, attempt string) (DrainAdmission, error) {
	var current DrainAdmission
	err := s.mutateExecutionEvidence(id, fence, func(cell *WorkStarted, row beads.Bead) (bool, error) {
		if !cell.Drain.matches(row) || cell.Drain.Attempt != attempt || attempt == "" {
			return false, ErrDrainAdmissionChanged
		}
		current = *cell.Drain
		return false, nil
	})
	return current, err
}

// CancelDrainAdmission reopens only the exact unaccepted census. Cancellation
// invalidates delayed publishers before admission can resume in this incarnation.
func (s *Store) CancelDrainAdmission(id string, fence ExecutionFence, attempt string) error {
	return s.mutateExecutionEvidence(id, fence, func(cell *WorkStarted, row beads.Bead) (bool, error) {
		if !cell.Drain.matches(row) {
			// No census belongs to this incarnation. Validate that absence
			// through CAS without removing historical evidence.
			return false, nil
		}
		if attempt == "" || cell.Drain.Attempt != attempt {
			return false, ErrDrainAdmissionChanged
		}
		if cell.Drain.Accepted {
			return false, ErrDrainStopAccepted
		}
		cell.Drain = nil
		return true, nil
	})
}

// AcceptDrainStop atomically accepts one committed census for stopping. A
// cancellation that wins this CAS excludes the stop; an accepted stop cannot
// subsequently reopen admission in the same incarnation.
func (s *Store) AcceptDrainStop(id string, fence ExecutionFence, attempt string) error {
	return s.acceptDrainStop(id, fence, attempt, true)
}

// AcceptControllerDrainStop accepts an exact controller intent without claiming
// that the agent completed a work-release census. It never releases work.
func (s *Store) AcceptControllerDrainStop(id string, fence ExecutionFence, attempt string) error {
	return s.acceptDrainStop(id, fence, attempt, false)
}

func (s *Store) acceptDrainStop(id string, fence ExecutionFence, attempt string, requireCensus bool) error {
	return s.mutateExecutionEvidence(id, fence, func(cell *WorkStarted, row beads.Bead) (bool, error) {
		if !cell.Drain.matches(row) || cell.Drain.Attempt != attempt || attempt == "" || (requireCensus && !cell.Drain.Committed) {
			return false, ErrDrainAdmissionChanged
		}
		cell.Drain.Accepted = true
		return true, nil
	})
}

// ClaimRelease reserves release of one exact work-row revision in one store.
// Revision is an optimistic-concurrency snapshot, never a claim-generation token.
type ClaimRelease struct {
	Store    string `json:"store"`
	Bead     string `json:"bead"`
	Revision int64  `json:"revision"`
}

// DecodeWorkStarted decodes execution evidence without discarding unreadable
// entries. Callers must retain claims when the record cannot be read.
func DecodeWorkStarted(raw string) (WorkStarted, error) {
	if strings.TrimSpace(raw) == "" {
		return WorkStarted{V: 1}, nil
	}
	var cell WorkStarted
	if err := json.Unmarshal([]byte(raw), &cell); err != nil {
		return WorkStarted{}, fmt.Errorf("decoding execution evidence: %w", err)
	}
	if cell.V != 1 {
		return WorkStarted{}, fmt.Errorf("unsupported execution evidence version %d", cell.V)
	}
	if cell.Drain != nil && (cell.Drain.Attempt == "" || cell.Drain.Incarnation == "" || cell.Drain.Generation == "") {
		return WorkStarted{}, errors.New("execution evidence contains an invalid drain admission")
	}
	for _, entry := range cell.Claims {
		if strings.TrimSpace(entry.Bead) == "" {
			return WorkStarted{}, errors.New("execution evidence contains an unnamed claim")
		}
	}
	for _, pending := range cell.Releases {
		if strings.TrimSpace(pending.Store) == "" || strings.TrimSpace(pending.Bead) == "" || pending.Revision == 0 {
			return WorkStarted{}, errors.New("execution evidence contains an invalid release reservation")
		}
	}
	return cell, nil
}

// Names reports whether this record names a bead in a store. An unknown store
// matches every store; uncertainty retains work rather than releasing it.
func (w WorkStarted) Names(store, bead string) bool {
	for _, entry := range w.Claims {
		if entry.Bead == bead && (entry.Store == "" || store == "" || entry.Store == store) {
			return true
		}
	}
	return false
}

// RecordStartedClaim records execution only while the exact runtime incarnation
// still owns the session row. It requires a real conditional writer and never
// falls back to an unfenced metadata write. This records session execution
// evidence; it does not issue or replace any work-claim ownership token.
func (s *Store) RecordStartedClaim(id string, fence ExecutionFence, entry WorkStartedEntry) error {
	entry.Bead, entry.Store = strings.TrimSpace(entry.Bead), strings.TrimSpace(entry.Store)
	if entry.Bead == "" {
		return errors.New("recording execution requires a claim id")
	}
	return s.mutateExecutionEvidence(id, fence, func(cell *WorkStarted, row beads.Bead) (bool, error) {
		for _, pending := range cell.Releases {
			if pending.Bead == entry.Bead && (pending.Store == entry.Store || entry.Store == "" || pending.Store == "") {
				return false, ErrClaimReleasePending
			}
		}
		if cell.Drain.matches(row) || row.Metadata["state"] == string(StateDraining) {
			return false, ErrExecutionDraining
		}
		if !ExecutionClaimStateEligible(State(strings.TrimSpace(row.Metadata["state"]))) {
			return false, ErrExecutionNotClaimable
		}
		var err error
		entry.Gen, err = strconv.ParseInt(strings.TrimSpace(row.Metadata["generation"]), 10, 64)
		if err != nil {
			return false, fmt.Errorf("reading execution generation: %w", err)
		}
		entry.At = time.Now().UTC()
		for i, old := range cell.Claims {
			if old.Store == entry.Store && old.Bead == entry.Bead {
				if old.Gen == entry.Gen {
					// Validate even idempotent admission against the current row CAS.
					return true, nil
				}
				cell.Claims[i] = entry
				return true, nil
			}
		}
		if len(cell.Claims) >= 64 {
			return false, errors.New("session execution evidence is full; retaining existing claims")
		}
		cell.Claims = append(cell.Claims, entry)
		return true, nil
	})
}

// ReserveClaimRelease serializes a release against RecordStartedClaim on the
// session row. A retry returns the ORIGINAL captured work revision; it cannot
// adopt a newer same-owner acquisition. Callers must fence the work mutation
// on that returned revision and retain the reservation on uncertain outcomes.
// Labels must come from the same captured work row: a canonical dispatch hold
// permits relinquishing that assignment, and the work-row CAS binds this fact.
func (s *Store) ReserveClaimRelease(id string, fence ExecutionFence, proposed ClaimRelease, labels ...string) (ClaimRelease, error) {
	return s.reserveClaimRelease(id, fence, proposed, "", labels...)
}

// ReserveDrainClaimRelease binds a captured release to the active census. A
// canceled or superseded census cannot reserve a newly acquired assignment.
func (s *Store) ReserveDrainClaimRelease(id string, fence ExecutionFence, attempt string, proposed ClaimRelease, labels ...string) (ClaimRelease, error) {
	if attempt == "" {
		return ClaimRelease{}, ErrDrainAdmissionChanged
	}
	return s.reserveClaimRelease(id, fence, proposed, attempt, labels...)
}

func (s *Store) reserveClaimRelease(id string, fence ExecutionFence, proposed ClaimRelease, attempt string, labels ...string) (ClaimRelease, error) {
	if proposed.Store == "" || proposed.Bead == "" {
		return ClaimRelease{}, errors.New("claim release requires an exact store, bead and revision")
	}
	if proposed.Revision == 0 {
		return ClaimRelease{}, ErrExecutionRevisionMissing
	}
	captured := proposed
	err := s.mutateExecutionEvidence(id, fence, func(cell *WorkStarted, row beads.Bead) (bool, error) {
		if attempt != "" && (!cell.Drain.matches(row) || cell.Drain.Attempt != attempt || cell.Drain.Committed || cell.Drain.Accepted) {
			return false, ErrDrainAdmissionChanged
		}
		held := slices.Contains(labels, beadmeta.HoldMayorLabel) || slices.Contains(labels, beadmeta.HoldExternalLabel)
		if !held && (cell.Names(proposed.Store, proposed.Bead) || row.Metadata[beadmeta.CurrentClaimBeadIDMetadataKey] == proposed.Bead || row.Metadata[CurrentBeadIDKey] == proposed.Bead) {
			return false, ErrClaimAlreadyStarted
		}
		for _, pending := range cell.Releases {
			if pending.Store == proposed.Store && pending.Bead == proposed.Bead {
				captured = pending
				return false, nil
			}
		}
		if len(cell.Releases) >= 64 {
			return false, errors.New("session release reservations are full")
		}
		captured = proposed
		cell.Releases = append(cell.Releases, proposed)
		return true, nil
	})
	return captured, err
}

// CompleteClaimRelease clears only the exact captured reservation. Callers may
// use it after a successful conditional release or a definitive revision
// conflict, never after a timeout or another uncertain backend outcome.
func (s *Store) CompleteClaimRelease(id string, fence ExecutionFence, captured ClaimRelease) error {
	return s.mutateExecutionEvidence(id, fence, func(cell *WorkStarted, _ beads.Bead) (bool, error) {
		for i, pending := range cell.Releases {
			if pending == captured {
				cell.Releases = append(cell.Releases[:i], cell.Releases[i+1:]...)
				return true, nil
			}
		}
		return false, nil
	})
}

// ApplyPatchWithExecutionFence writes metadata only while the captured runtime
// and admission attempt still match. An empty attempt requires open admission;
// a replacement attempt is never adopted by a conditional-write retry.
func (s *Store) ApplyPatchWithExecutionFence(id string, fence ExecutionFence, attempt string, patch MetadataPatch) error {
	for _, key := range []string{WorkStartedKey, "instance_token", "generation"} {
		if _, exists := patch[key]; exists {
			return fmt.Errorf("execution-fenced patch cannot replace %q", key)
		}
	}
	return s.mutateExecutionEvidenceWithPatch(id, fence, patch, func(cell *WorkStarted, row beads.Bead) (bool, error) {
		current := ""
		if cell.Drain.matches(row) {
			current = cell.Drain.Attempt
		}
		if current != attempt {
			return false, ErrDrainAdmissionChanged
		}
		return false, nil
	})
}

func (s *Store) mutateExecutionEvidence(id string, fence ExecutionFence, decide func(*WorkStarted, beads.Bead) (bool, error)) error {
	return s.mutateExecutionEvidenceWithPatch(id, fence, nil, decide)
}

func (s *Store) mutateExecutionEvidenceWithPatch(id string, fence ExecutionFence, patch MetadataPatch, decide func(*WorkStarted, beads.Bead) (bool, error)) error {
	if strings.TrimSpace(fence.InstanceToken) == "" {
		return ErrExecutionIncarnationChanged
	}
	writer, _, err := beads.ResolveConditionalWriter(s.store)
	if err != nil {
		return fmt.Errorf("resolving execution fence: %w", err)
	}
	if writer == nil {
		return beads.ErrConditionalWriteUnsupported
	}
	for attempt := 0; attempt < 5; attempt++ {
		row, err := s.executionBead(id)
		if err != nil {
			return err
		}
		if row.Status == "closed" || row.Metadata["instance_token"] != fence.InstanceToken ||
			fence.Generation != "" && row.Metadata["generation"] != fence.Generation {
			return ErrExecutionIncarnationChanged
		}
		if row.Revision == 0 {
			return ErrExecutionRevisionMissing
		}
		cell, err := DecodeWorkStarted(row.Metadata[WorkStartedKey])
		if err != nil {
			return err
		}
		_, err = decide(&cell, row)
		if err != nil {
			return err
		}
		// Even an unchanged retry must validate the row through CAS. A cached
		// committed census cannot authorize an ACK after another process cancels.

		raw, err := json.Marshal(cell)
		if err != nil {
			return fmt.Errorf("encoding execution evidence: %w", err)
		}
		metadata := make(map[string]string, len(patch)+1)
		for key, value := range patch {
			metadata[key] = value
		}
		metadata[WorkStartedKey] = string(raw)
		err = writer.UpdateIfMatch(id, row.Revision, beads.UpdateOpts{Metadata: metadata})
		if err == nil {
			return nil
		}
		if !beads.IsPreconditionFailed(err) {
			return fmt.Errorf("recording session execution: %w", err)
		}
	}
	return errors.New("session execution fence changed on every attempt; nothing recorded")
}
