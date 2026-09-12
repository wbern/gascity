package usage

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

const maxAttributionFieldBytes = 256

// AttributionState reports whether a usage fact has an explicit, immutable
// work binding. It deliberately distinguishes an unbound persistent session
// from an incomplete bound record: callers must never infer a work bead from
// a session name, work directory, or time window.
type AttributionState string

const (
	// AttributionUnbound reports that no producer supplied a turn binding.
	AttributionUnbound AttributionState = "unbound"
	// AttributionBound reports that the producer persisted a complete work
	// binding before it started the provider turn.
	AttributionBound AttributionState = "bound"
)

// HeadState describes the repository state observed at a turn boundary.
// Empty SHAs are meaningful for non-repository, unborn, and missing paths.
type HeadState string

const (
	// HeadClean reports a repository with no tracked changes at the boundary.
	HeadClean HeadState = "clean"
	// HeadDirty reports a repository with tracked changes at the boundary.
	HeadDirty HeadState = "dirty"
	// HeadNonRepo reports that the work directory is present but is not a git
	// repository.
	HeadNonRepo HeadState = "nonrepo"
	// HeadUnborn reports a git repository without a HEAD commit.
	HeadUnborn HeadState = "unborn"
	// HeadMissing reports a work directory that did not exist at the boundary.
	HeadMissing HeadState = "missing"
)

// Head is a point-in-time repository observation. SHA is populated only when
// git had a resolved HEAD; State carries the explicit meaning when it did not.
type Head struct {
	State HeadState `json:"state"`
	SHA   string    `json:"sha,omitempty"`
}

// Valid reports whether h represents one of the explicit boundary states.
func (h Head) Valid() bool {
	switch h.State {
	case HeadClean, HeadDirty:
		return strings.TrimSpace(h.SHA) != ""
	case HeadNonRepo, HeadUnborn, HeadMissing:
		return strings.TrimSpace(h.SHA) == ""
	default:
		return false
	}
}

// InvocationAttribution is the additive v2 work binding carried by a usage
// fact. Bound records are immutable snapshots: StartHead is recorded before
// provider execution and CompletionHead is recorded when that same invocation
// completes. The model collector must use this persisted binding instead of
// looking up a session's current assignment.
type InvocationAttribution struct {
	State          AttributionState `json:"state"`
	City           string           `json:"city,omitempty"`
	Rig            string           `json:"rig,omitempty"`
	WorkBeadID     string           `json:"work_bead_id,omitempty"`
	Attempt        string           `json:"attempt,omitempty"`
	InvocationID   string           `json:"invocation_id,omitempty"`
	StartHead      Head             `json:"start_head,omitempty"`
	CompletionHead Head             `json:"completion_head,omitempty"`
}

// MarshalJSON omits absent boundary observations instead of serializing them
// as misleading {"state":""} objects. Bound start heads and observed
// completion heads remain explicit on the additive v2 wire format.
func (a InvocationAttribution) MarshalJSON() ([]byte, error) {
	type wireAttribution struct {
		State          AttributionState `json:"state"`
		City           string           `json:"city,omitempty"`
		Rig            string           `json:"rig,omitempty"`
		WorkBeadID     string           `json:"work_bead_id,omitempty"`
		Attempt        string           `json:"attempt,omitempty"`
		InvocationID   string           `json:"invocation_id,omitempty"`
		StartHead      *Head            `json:"start_head,omitempty"`
		CompletionHead *Head            `json:"completion_head,omitempty"`
	}
	w := wireAttribution{
		State:        a.State,
		City:         a.City,
		Rig:          a.Rig,
		WorkBeadID:   a.WorkBeadID,
		Attempt:      a.Attempt,
		InvocationID: a.InvocationID,
	}
	if a.StartHead != (Head{}) {
		start := a.StartHead
		w.StartHead = &start
	}
	if a.CompletionHead != (Head{}) {
		completion := a.CompletionHead
		w.CompletionHead = &completion
	}
	return json.Marshal(w)
}

// UnboundInvocationAttribution returns the explicit v2 value for a turn that
// did not receive a producer-side work binding.
func UnboundInvocationAttribution() InvocationAttribution {
	return InvocationAttribution{State: AttributionUnbound}
}

// NewBoundInvocationAttribution constructs an immutable pre-turn binding.
// Every identity field is required so a partially populated record cannot be
// mistaken for a reliable efficiency join.
func NewBoundInvocationAttribution(city, rig, workBeadID, attempt, invocationID string, startHead Head) (InvocationAttribution, error) {
	binding := InvocationAttribution{
		State:        AttributionBound,
		City:         strings.TrimSpace(city),
		Rig:          strings.TrimSpace(rig),
		WorkBeadID:   strings.TrimSpace(workBeadID),
		Attempt:      strings.TrimSpace(attempt),
		InvocationID: strings.TrimSpace(invocationID),
		StartHead:    startHead,
	}
	if err := binding.Validate(); err != nil {
		return InvocationAttribution{}, err
	}
	return binding, nil
}

// WithCompletionHead returns a new immutable snapshot with the completion
// repository state. It never reads a live session or current work assignment.
func (a InvocationAttribution) WithCompletionHead(completionHead Head) (InvocationAttribution, error) {
	if a.State != AttributionBound {
		return InvocationAttribution{}, fmt.Errorf("cannot complete %s invocation attribution", a.State)
	}
	if err := a.Validate(); err != nil {
		return InvocationAttribution{}, err
	}
	if !completionHead.Valid() {
		return InvocationAttribution{}, fmt.Errorf("invalid completion head state %q", completionHead.State)
	}
	a.CompletionHead = completionHead
	return a, nil
}

// Validate reports whether a is a well-formed explicit attribution value.
func (a InvocationAttribution) Validate() error {
	switch a.State {
	case AttributionUnbound:
		if a.City != "" || a.Rig != "" || a.WorkBeadID != "" || a.Attempt != "" || a.InvocationID != "" || a.StartHead != (Head{}) || a.CompletionHead != (Head{}) {
			return fmt.Errorf("unbound invocation attribution must not carry work identity")
		}
		return nil
	case AttributionBound:
		for _, field := range []struct {
			name  string
			value string
		}{
			{"city", a.City},
			{"rig", a.Rig},
			{"work bead id", a.WorkBeadID},
			{"attempt", a.Attempt},
			{"invocation id", a.InvocationID},
		} {
			if strings.TrimSpace(field.value) == "" {
				return fmt.Errorf("bound invocation attribution requires %s", field.name)
			}
			if len(field.value) > maxAttributionFieldBytes {
				return fmt.Errorf("bound invocation attribution %s exceeds %d bytes", field.name, maxAttributionFieldBytes)
			}
		}
		if !a.StartHead.Valid() {
			return fmt.Errorf("invalid start head state %q", a.StartHead.State)
		}
		if a.CompletionHead != (Head{}) && !a.CompletionHead.Valid() {
			return fmt.Errorf("invalid completion head state %q", a.CompletionHead.State)
		}
		return nil
	default:
		return fmt.Errorf("unknown invocation attribution state %q", a.State)
	}
}

// FactForCompletedInvocation attaches an immutable v2 attribution to a model
// fact after an adapter has supplied an exact terminal observation. The
// provider response id must match the persisted invocation record; this is the
// only supported correlation path for delayed transcript recovery.
func FactForCompletedInvocation(fact Fact, record InvocationRecord) (Fact, error) {
	if err := record.validateBound(); err != nil {
		return Fact{}, err
	}
	if !record.Complete() {
		return Fact{}, errors.New("invocation record is not completed")
	}
	if strings.TrimSpace(fact.UpstreamReqID) == "" || fact.UpstreamReqID != record.UpstreamRequestID {
		return Fact{}, errors.New("fact upstream request id does not match completed invocation")
	}
	attribution := record.Attribution
	fact.AttributionV2 = &attribution
	return fact, nil
}
