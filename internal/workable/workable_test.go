package workable_test

import (
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/workable"
)

func sampleContext() *workable.Context {
	configuredAgents := map[string]bool{
		"crm/gastown.polecat":     true,
		"crm/gastown.reviewer":    true,
		"crm/gastown.ux-reviewer": true,
		"crm/gastown.untangler":   true,
		"gascity/builder":         true,
	}

	workKinds := map[string]workable.WorkKind{
		"review": {
			RequireMetadata:    []string{"molecule_id", "review_context"},
			RequireRouteTarget: "agent",
			FreshnessBinding:   "head_matches_pr",
			MatchMetadata:      []string{"review_context"},
		},
	}

	return &workable.Context{
		AgentExists: func(target string) bool {
			return configuredAgents[target]
		},
		IsHeadCurrent: func(prNumber, head string) (bool, bool) {
			// Simulate PR 1720 where head moved to 53292b4 and older head 64ad199 has a newer verdict
			if prNumber == "1720" {
				if head == "53292b4" {
					return true, false
				}
				if head == "64ad199" {
					return false, true // moved and newer verdict published
				}
			}
			return true, false
		},
		WorkKinds: workKinds,
	}
}

// (1) Superseded head with a newer published verdict: unworkable
func TestCheck_SupersededHeadWithNewerPublishedVerdict(t *testing.T) {
	ctx := sampleContext()
	bead := beads.Bead{
		ID:     "crm-oiz2mkd",
		Type:   "task",
		Status: "open",
		Metadata: map[string]string{
			beadmeta.RoutedToMetadataKey: "crm/gastown.reviewer",
			"review_context":             "code-review",
			"pr_number":                  "1720",
			"head":                       "64ad199",
			"molecule_id":                "mol-123",
		},
	}

	res := workable.Check(bead, ctx)
	if res.Workable {
		t.Fatalf("Check(superseded head) = workable, want unworkable")
	}
	if res.Reason == "" {
		t.Errorf("Check(superseded head) reason is empty")
	}
}

// (2) Malformed custody with an open circuit: unworkable
func TestCheck_MalformedCustodyWithOpenCircuit(t *testing.T) {
	ctx := sampleContext()
	bead := beads.Bead{
		ID:     "crm-ektlgna",
		Type:   "task",
		Status: "open",
		Metadata: map[string]string{
			beadmeta.RoutedToMetadataKey:    "crm/gastown.ux-reviewer",
			"review_context":                "ux-review",
			"review_custody_circuit_open":   "true",
			"review_custody_circuit_reason": "malformed_custody",
		},
	}

	res := workable.Check(bead, ctx)
	if res.Workable {
		t.Fatalf("Check(circuit open) = workable, want unworkable")
	}
	if res.Reason == "" {
		t.Errorf("Check(circuit open) reason is empty")
	}
}

// (3) Unresolvable route target: unworkable
func TestCheck_UnresolvableRouteTarget(t *testing.T) {
	ctx := sampleContext()
	bead := beads.Bead{
		ID:     "b-unresolvable",
		Type:   "task",
		Status: "open",
		Metadata: map[string]string{
			beadmeta.RoutedToMetadataKey: "crm/nonexistent-agent",
			"review_context":             "code-review",
			"molecule_id":                "mol-123",
		},
	}

	res := workable.Check(bead, ctx)
	if res.Workable {
		t.Fatalf("Check(unresolvable route target) = workable, want unworkable")
	}
	if res.Reason == "" {
		t.Errorf("Check(unresolvable target) reason is empty")
	}
}

// (4) Gate routed to agent pool: unworkable (structurally unreachable by readiness)
func TestCheck_GateRoutedToAgentPool(t *testing.T) {
	ctx := sampleContext()
	bead := beads.Bead{
		ID:     "gate-123",
		Type:   "gate",
		Status: "open",
		Metadata: map[string]string{
			beadmeta.RoutedToMetadataKey: "crm/gastown.untangler",
		},
	}

	res := workable.Check(bead, ctx)
	if res.Workable {
		t.Fatalf("Check(gate routed to pool) = workable, want unworkable")
	}
}

// (5) Does NOT fire on human-routed beads
func TestCheck_DoesNotFireOnHumanRouted(t *testing.T) {
	ctx := sampleContext()
	bead := beads.Bead{
		ID:     "b-human",
		Type:   "task",
		Status: "open",
		Metadata: map[string]string{
			beadmeta.RoutedToMetadataKey: "human",
			"review_context":             "code-review",
			"molecule_id":                "mol-123",
		},
	}

	res := workable.Check(bead, ctx)
	if !res.Workable {
		t.Fatalf("Check(human-routed) = unworkable (%s), want workable", res.Reason)
	}
}

// (6) Does NOT fire on wedged-but-configured agent
func TestCheck_DoesNotFireOnWedgedButConfiguredAgent(t *testing.T) {
	ctx := sampleContext()
	bead := beads.Bead{
		ID:     "b-wedged",
		Type:   "task",
		Status: "open",
		Metadata: map[string]string{
			beadmeta.RoutedToMetadataKey: "crm/gastown.polecat",
			"molecule_id":                "mol-123",
			"review_context":             "code-review",
			"pr_number":                  "1720",
			"head":                       "53292b4", // current head
		},
	}

	res := workable.Check(bead, ctx)
	if !res.Workable {
		t.Fatalf("Check(configured agent) = unworkable (%s), want workable", res.Reason)
	}
}

// (7) Does NOT fire on deferred beads
func TestCheck_DoesNotFireOnDeferred(t *testing.T) {
	ctx := sampleContext()
	deferredUntil := time.Now().Add(10 * time.Minute)
	bead := beads.Bead{
		ID:         "b-deferred",
		Type:       "task",
		Status:     "open",
		DeferUntil: &deferredUntil,
		Metadata: map[string]string{
			beadmeta.RoutedToMetadataKey: "crm/nonexistent",
		},
	}

	res := workable.Check(bead, ctx)
	if !res.Workable {
		t.Fatalf("Check(deferred) = unworkable (%s), want workable", res.Reason)
	}
}

// (8) Does NOT fire on dependency-blocked beads
func TestCheck_DoesNotFireOnDependencyBlocked(t *testing.T) {
	ctx := sampleContext()
	bead := beads.Bead{
		ID:           "b-blocked",
		Type:         "task",
		Status:       "open",
		Dependencies: []beads.Dep{{IssueID: "dep-1"}}, // open blocking dep
		Metadata: map[string]string{
			beadmeta.RoutedToMetadataKey: "crm/nonexistent",
		},
	}

	res := workable.Check(bead, ctx)
	if !res.Workable {
		t.Fatalf("Check(dependency-blocked) = unworkable (%s), want workable", res.Reason)
	}
}

// (9) Does NOT fire on workability-exempt beads
func TestCheck_DoesNotFireOnExemptMarked(t *testing.T) {
	ctx := sampleContext()
	bead := beads.Bead{
		ID:     "gci-ns5qt",
		Type:   "task",
		Status: "open",
		Metadata: map[string]string{
			beadmeta.RoutedToMetadataKey:          "crm/nonexistent",
			beadmeta.WorkabilityExemptMetadataKey: "true",
		},
	}

	res := workable.Check(bead, ctx)
	if !res.Workable {
		t.Fatalf("Check(exempt) = unworkable (%s), want workable", res.Reason)
	}
}

// (10) Missing required metadata fires
func TestCheck_MissingRequiredMetadata(t *testing.T) {
	ctx := sampleContext()
	bead := beads.Bead{
		ID:     "b-no-mol",
		Type:   "task",
		Status: "open",
		Metadata: map[string]string{
			beadmeta.RoutedToMetadataKey: "crm/gastown.reviewer",
			"review_context":             "code-review",
			// missing molecule_id!
		},
	}

	res := workable.Check(bead, ctx)
	if res.Workable {
		t.Fatalf("Check(missing molecule_id) = workable, want unworkable")
	}
}

type fakeStore struct {
	metadataBatch map[string]string
	updateOpts    beads.UpdateOpts
	closed        bool
}

func (f *fakeStore) SetMetadataBatch(_ string, kvs map[string]string) error {
	f.metadataBatch = kvs
	return nil
}

func (f *fakeStore) Update(_ string, opts beads.UpdateOpts) error {
	f.updateOpts = opts
	return nil
}

type fakeRecorder struct {
	events []events.Event
}

func (r *fakeRecorder) Record(e events.Event) {
	r.events = append(r.events, e)
}

func TestParkBead_Containment(t *testing.T) {
	store := &fakeStore{}
	rec := &fakeRecorder{}

	bead := beads.Bead{
		ID:     "crm-ektlgna",
		Type:   "task",
		Status: "open",
		Metadata: map[string]string{
			beadmeta.RoutedToMetadataKey: "crm/gastown.ux-reviewer",
		},
	}

	err := workable.ParkBead(t.Context(), bead, "malformed_custody", 10*time.Minute, store, rec)
	if err != nil {
		t.Fatalf("ParkBead: %v", err)
	}

	// 1. gc.routed_to cleared, reason & timestamp stamped
	if store.metadataBatch[beadmeta.RoutedToMetadataKey] != "" {
		t.Errorf("gc.routed_to was not cleared, got %q", store.metadataBatch[beadmeta.RoutedToMetadataKey])
	}
	if store.metadataBatch[beadmeta.UnworkableReasonMetadataKey] != "malformed_custody" {
		t.Errorf("gc.unworkable_reason = %q, want 'malformed_custody'", store.metadataBatch[beadmeta.UnworkableReasonMetadataKey])
	}
	if store.metadataBatch[beadmeta.UnworkableAtMetadataKey] == "" {
		t.Errorf("gc.unworkable_at was not stamped")
	}

	// 2. Bead was deferred, NEVER closed
	if store.closed {
		t.Errorf("store closed the bead! ParkBead must never close")
	}
	if store.updateOpts.DeferUntil == nil {
		t.Errorf("store.updateOpts.DeferUntil is nil, want deferred")
	}

	// 3. Events emitted: bead.unworkable and bead.parked
	if len(rec.events) != 2 {
		t.Fatalf("emitted %d events, want 2", len(rec.events))
	}
	if rec.events[0].Type != events.BeadUnworkable {
		t.Errorf("first event type = %s, want %s", rec.events[0].Type, events.BeadUnworkable)
	}
	if rec.events[1].Type != events.BeadParked {
		t.Errorf("second event type = %s, want %s", rec.events[1].Type, events.BeadParked)
	}
}
