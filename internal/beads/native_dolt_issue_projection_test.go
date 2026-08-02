package beads

import (
	"testing"
	"time"

	beadslib "github.com/steveyegge/beads"
)

// TestBeadFromNativeIssueCarriesEveryMappableField is the drift guard
// TestEveryBeadFieldSurvivesJSONRoundTrip could not be: that test pins the JSON
// round trip of Bead itself and says in as many words that it cannot reach the
// four hand-written mappings. beadFromNativeIssue is one of those four AND it
// lives in this package, so it can be pinned directly — and it was not.
//
// The gap was not theoretical. beadFromNativeIssue silently dropped UpdatedAt
// for every bead a native-Dolt-backed store served. Measured against the live
// controller before the fix: of 500 beads returned by GET /beads, 500 carried no
// updated_at at all, while priority, owner, notes and created_by all came
// through. Nothing failed, because Bead.UpdatedAt is documented as "zero for
// legacy beads" and UpdatedBefore falls back to CreatedAt — so every bead on
// this backend silently impersonated a legacy bead and every staleness
// comparison quietly used creation time instead of modification time.
//
// This asserts field by field rather than by count so a future addition names
// itself in the failure.
func TestBeadFromNativeIssueCarriesEveryMappableField(t *testing.T) {
	created := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	updated := time.Date(2026, 6, 7, 8, 9, 10, 0, time.UTC)
	deferUntil := time.Date(2026, 9, 9, 9, 9, 9, 0, time.UTC)

	issue := &beadslib.Issue{
		ID:          "gcw-proj1",
		Title:       "projection probe",
		Status:      beadslib.StatusOpen,
		IssueType:   beadslib.IssueType("task"),
		Priority:    0, // not 2: P2 maps to nil by design, see nativePriorityFromIssue.
		CreatedAt:   created,
		UpdatedAt:   updated,
		Assignee:    "assignee@example",
		Sender:      "sender@example",
		Description: "description",
		Labels:      []string{"label-a"},
		Ephemeral:   true,
		NoHistory:   true,
		DeferUntil:  &deferUntil,
		AwaitType:   "human",
		CreatedBy:   "creator@example",
		Owner:       "owner@example",
		Notes:       "notes",
	}

	bead, err := beadFromNativeIssue(issue)
	if err != nil {
		t.Fatalf("beadFromNativeIssue: %v", err)
	}

	if bead.ID != issue.ID {
		t.Errorf("ID = %q, want %q", bead.ID, issue.ID)
	}
	if bead.Title != issue.Title {
		t.Errorf("Title = %q, want %q", bead.Title, issue.Title)
	}
	if bead.Type != string(issue.IssueType) {
		t.Errorf("Type = %q, want %q", bead.Type, issue.IssueType)
	}
	if bead.Priority == nil || *bead.Priority != issue.Priority {
		t.Errorf("Priority = %v, want %d", bead.Priority, issue.Priority)
	}
	if !bead.CreatedAt.Equal(created) {
		t.Errorf("CreatedAt = %v, want %v", bead.CreatedAt, created)
	}
	// The field this guard exists for.
	if !bead.UpdatedAt.Equal(updated) {
		t.Errorf("UpdatedAt = %v, want %v — a bead whose UpdatedAt is zero "+
			"impersonates a legacy bead and silently degrades every staleness "+
			"comparison to CreatedAt", bead.UpdatedAt, updated)
	}
	if bead.Assignee != issue.Assignee {
		t.Errorf("Assignee = %q, want %q", bead.Assignee, issue.Assignee)
	}
	if bead.From != issue.Sender {
		t.Errorf("From = %q, want %q", bead.From, issue.Sender)
	}
	if bead.Description != issue.Description {
		t.Errorf("Description = %q, want %q", bead.Description, issue.Description)
	}
	if len(bead.Labels) != 1 || bead.Labels[0] != "label-a" {
		t.Errorf("Labels = %v, want [label-a]", bead.Labels)
	}
	if !bead.Ephemeral {
		t.Error("Ephemeral = false, want true")
	}
	if !bead.NoHistory {
		t.Error("NoHistory = false, want true")
	}
	if bead.DeferUntil == nil || !bead.DeferUntil.Equal(deferUntil) {
		t.Errorf("DeferUntil = %v, want %v", bead.DeferUntil, deferUntil)
	}
	if bead.AwaitType != issue.AwaitType {
		t.Errorf("AwaitType = %q, want %q", bead.AwaitType, issue.AwaitType)
	}
	if bead.CreatedBy != issue.CreatedBy {
		t.Errorf("CreatedBy = %q, want %q", bead.CreatedBy, issue.CreatedBy)
	}
	if bead.Owner != issue.Owner {
		t.Errorf("Owner = %q, want %q", bead.Owner, issue.Owner)
	}
	if bead.Notes != issue.Notes {
		t.Errorf("Notes = %q, want %q", bead.Notes, issue.Notes)
	}
}

// TestBeadFromNativeIssueLeavesUpdatedAtZeroWhenSourceIsZero pins the other
// half: a genuinely unset UpdatedAt must stay zero rather than being
// back-filled from CreatedAt. Bead.UpdatedAt documents zero as meaningful
// ("zero for legacy beads; UpdatedBefore falls back to CreatedAt"), so
// synthesizing a value here would destroy the distinction the fallback relies
// on.
func TestBeadFromNativeIssueLeavesUpdatedAtZeroWhenSourceIsZero(t *testing.T) {
	bead, err := beadFromNativeIssue(&beadslib.Issue{
		ID:        "gcw-proj2",
		Status:    beadslib.StatusOpen,
		CreatedAt: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("beadFromNativeIssue: %v", err)
	}
	if !bead.UpdatedAt.IsZero() {
		t.Errorf("UpdatedAt = %v, want zero for an issue with no UpdatedAt", bead.UpdatedAt)
	}
}
