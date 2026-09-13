package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
)

func writeAnalyzeContinuityCity(t *testing.T, dir string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "city.toml"), []byte("[workspace]\nname = \"test\"\n"), 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}
}

// withContinuityStore points continuityStoreOpener at a fixed in-memory
// store for the duration of the test, restoring the real opener after.
func withContinuityStore(t *testing.T, store beads.Store) {
	t.Helper()
	prev := continuityStoreOpener
	continuityStoreOpener = func(string, execStoreTarget) (beads.Store, error) {
		return store, nil
	}
	t.Cleanup(func() { continuityStoreOpener = prev })
}

// seededStore builds a MemStore directly from fully-specified beads,
// bypassing Create's normalization (which forces Status="open" and
// CreatedAt/UpdatedAt=now) so tests can pin exact status/timestamp fixtures.
func continuitySeededStore(beadList ...beads.Bead) *beads.MemStore {
	return beads.NewMemStoreFrom(0, beadList, nil)
}

func TestRunAnalyzeContinuity_RecycledSessionRetainedMetadata(t *testing.T) {
	dir := t.TempDir()
	writeAnalyzeContinuityCity(t, dir)

	now := time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC)
	store := continuitySeededStore(beads.Bead{
		ID:        "crm-459akft",
		Title:     "Diagnose regression",
		Status:    "open",
		Type:      "task",
		CreatedAt: now.Add(-2 * time.Hour),
		UpdatedAt: now.Add(-90 * time.Minute),
		Metadata: beads.StringMap{
			beadmeta.WorkspaceOwnerMetadataKey: "crm/gastown.furiosa",
			beadmeta.SessionNameMetadataKey:    "crm-gastown-furiosa-gc3-x9t6ki",
			beadmeta.InputConvoyIDMetadataKey:  "gci-0pq6w",
		},
		// No current assignee — the session was recycled.
	})
	withContinuityStore(t, store)

	var stdout, stderr bytes.Buffer
	opts := continuityCmdOptions{cityPath: dir, agent: "crm/gastown.furiosa", since: "7d", jsonOut: true}
	err := runAnalyzeContinuity(opts, &stdout, &stderr)
	if err != nil {
		t.Fatalf("runAnalyzeContinuity: %v", err)
	}

	var report ContinuityReport
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatalf("unmarshal report: %v\noutput: %s", err, stdout.String())
	}
	if !report.Complete {
		t.Errorf("report.Complete = false, want true (no source failures): %+v", report.Unavailable)
	}
	if len(report.Results) != 1 {
		t.Fatalf("len(Results) = %d, want 1: %+v", len(report.Results), report.Results)
	}
	got := report.Results[0]
	if got.BeadID != "crm-459akft" {
		t.Errorf("BeadID = %q, want crm-459akft", got.BeadID)
	}
	if got.Campaign != "gci-0pq6w" {
		t.Errorf("Campaign = %q, want gci-0pq6w", got.Campaign)
	}
	if got.Assignee != "" {
		t.Errorf("Assignee = %q, want empty — recycled session has no current assignee", got.Assignee)
	}
	if got.LastParticipant != "crm/gastown.furiosa" {
		t.Errorf("LastParticipant = %q, want crm/gastown.furiosa", got.LastParticipant)
	}
}

func TestRunAnalyzeContinuity_HumanGateNeverStalled(t *testing.T) {
	dir := t.TempDir()
	writeAnalyzeContinuityCity(t, dir)

	now := time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC)
	store := continuitySeededStore(beads.Bead{
		ID:        "crm-459akft",
		Title:     "Diagnose regression",
		Status:    "open",
		Type:      "task",
		AwaitType: "human",
		CreatedAt: now.Add(-5 * time.Hour),
		UpdatedAt: now.Add(-4*time.Hour - 43*time.Minute),
		Metadata: beads.StringMap{
			beadmeta.WorkspaceOwnerMetadataKey: "crm/gastown.furiosa",
		},
	})
	withContinuityStore(t, store)

	var stdout, stderr bytes.Buffer
	opts := continuityCmdOptions{cityPath: dir, agent: "crm/gastown.furiosa", since: "7d", jsonOut: true}
	if err := runAnalyzeContinuity(opts, &stdout, &stderr); err != nil {
		t.Fatalf("runAnalyzeContinuity: %v", err)
	}

	var report ContinuityReport
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(report.Results) != 1 {
		t.Fatalf("len(Results) = %d, want 1", len(report.Results))
	}
	got := report.Results[0]
	if got.Hold == "" {
		t.Errorf("Hold is empty, want a human-gate hold")
	}
	if got.ExpectedNextTransition != "wait-for-gate" {
		t.Errorf("ExpectedNextTransition = %q, want wait-for-gate — a gate is an intentional wait, never overdue/stalled", got.ExpectedNextTransition)
	}
}

func TestRunAnalyzeContinuity_NotObservedIsScopedNotFleetWide(t *testing.T) {
	dir := t.TempDir()
	writeAnalyzeContinuityCity(t, dir)

	// Seed unrelated work so the store is non-empty but nothing matches "bob".
	store := continuitySeededStore(beads.Bead{
		ID:        "gcw-1",
		Title:     "Unrelated",
		Status:    "open",
		Type:      "task",
		Assignee:  "someone-else",
		CreatedAt: time.Now().UTC(),
		UpdatedAt: time.Now().UTC(),
	})
	withContinuityStore(t, store)

	var stdout, stderr bytes.Buffer
	opts := continuityCmdOptions{cityPath: dir, rig: "", agent: "bob", since: "30d", jsonOut: true}
	if err := runAnalyzeContinuity(opts, &stdout, &stderr); err != nil {
		t.Fatalf("runAnalyzeContinuity: %v", err)
	}

	var report ContinuityReport
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !report.Complete {
		t.Errorf("Complete = false, want true — a fully-queried empty scope is complete-empty, not partial")
	}
	if len(report.Results) != 0 {
		t.Errorf("len(Results) = %d, want 0", len(report.Results))
	}
	if report.ScopeKind == "" {
		t.Errorf("ScopeKind is empty — a not-observed result must still be scope-qualified")
	}
}

func TestRunAnalyzeContinuity_EndedCampaignDiscoverableInWindow(t *testing.T) {
	dir := t.TempDir()
	writeAnalyzeContinuityCity(t, dir)

	now := time.Now().UTC()
	store := continuitySeededStore(beads.Bead{
		ID:        "gcw-done",
		Title:     "Completed work",
		Status:    "closed",
		Type:      "task",
		Assignee:  "worker-1",
		CreatedAt: now.Add(-3 * 24 * time.Hour),
		UpdatedAt: now.Add(-2 * 24 * time.Hour),
	})
	withContinuityStore(t, store)

	var stdout, stderr bytes.Buffer
	opts := continuityCmdOptions{cityPath: dir, agent: "worker-1", since: "7d", jsonOut: true}
	if err := runAnalyzeContinuity(opts, &stdout, &stderr); err != nil {
		t.Fatalf("runAnalyzeContinuity: %v", err)
	}
	var report ContinuityReport
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(report.Results) != 1 {
		t.Fatalf("len(Results) = %d, want 1 — closed work inside --since must remain discoverable", len(report.Results))
	}
	if !report.Results[0].Ended {
		t.Errorf("Ended = false, want true for closed work")
	}
	if report.Results[0].LastTransition != "closed" {
		t.Errorf("LastTransition = %q, want closed", report.Results[0].LastTransition)
	}
}

func TestRunAnalyzeContinuity_OutsideWindowExcluded(t *testing.T) {
	dir := t.TempDir()
	writeAnalyzeContinuityCity(t, dir)

	now := time.Now().UTC()
	store := continuitySeededStore(beads.Bead{
		ID:        "gcw-old",
		Title:     "Ancient work",
		Status:    "closed",
		Type:      "task",
		Assignee:  "worker-1",
		CreatedAt: now.Add(-90 * 24 * time.Hour),
		UpdatedAt: now.Add(-89 * 24 * time.Hour),
	})
	withContinuityStore(t, store)

	var stdout, stderr bytes.Buffer
	opts := continuityCmdOptions{cityPath: dir, agent: "worker-1", since: "7d", jsonOut: true}
	if err := runAnalyzeContinuity(opts, &stdout, &stderr); err != nil {
		t.Fatalf("runAnalyzeContinuity: %v", err)
	}
	var report ContinuityReport
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(report.Results) != 0 {
		t.Errorf("len(Results) = %d, want 0 — evidence outside --since must not appear", len(report.Results))
	}
}

func TestRunAnalyzeContinuity_AliasHistoryResolvesRenamedSession(t *testing.T) {
	dir := t.TempDir()
	writeAnalyzeContinuityCity(t, dir)

	now := time.Now().UTC()
	// The session bead records that "old-alias" is a prior name for the
	// session now known as "new-alias".
	store := continuitySeededStore(
		beads.Bead{
			ID:        "session-1",
			Type:      "session",
			Status:    "open",
			CreatedAt: now.Add(-1 * time.Hour),
			Metadata: beads.StringMap{
				"alias":         "new-alias",
				"alias_history": "old-alias",
			},
		},
		beads.Bead{
			ID:        "gcw-work",
			Title:     "Work done under the old alias",
			Status:    "open",
			Type:      "task",
			Assignee:  "old-alias",
			CreatedAt: now.Add(-30 * time.Minute),
			UpdatedAt: now.Add(-20 * time.Minute),
		},
	)
	withContinuityStore(t, store)

	var stdout, stderr bytes.Buffer
	opts := continuityCmdOptions{cityPath: dir, agent: "new-alias", since: "7d", jsonOut: true}
	if err := runAnalyzeContinuity(opts, &stdout, &stderr); err != nil {
		t.Fatalf("runAnalyzeContinuity: %v", err)
	}
	var report ContinuityReport
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(report.Results) != 1 {
		t.Fatalf("len(Results) = %d, want 1 — work under a prior alias must resolve via alias history", len(report.Results))
	}
	if report.Results[0].BeadID != "gcw-work" {
		t.Errorf("BeadID = %q, want gcw-work", report.Results[0].BeadID)
	}
}

func TestRunAnalyzeContinuity_StoreUnavailableIsPartialNotEmpty(t *testing.T) {
	dir := t.TempDir()
	writeAnalyzeContinuityCity(t, dir)

	prev := continuityStoreOpener
	continuityStoreOpener = func(string, execStoreTarget) (beads.Store, error) {
		return nil, errors.New("store unreachable")
	}
	t.Cleanup(func() { continuityStoreOpener = prev })

	var stdout, stderr bytes.Buffer
	opts := continuityCmdOptions{cityPath: dir, agent: "anyone", since: "7d", jsonOut: true}
	err := runAnalyzeContinuity(opts, &stdout, &stderr)
	if !errors.Is(err, errContinuityPartial) {
		t.Fatalf("runAnalyzeContinuity error = %v, want errContinuityPartial", err)
	}

	var report ContinuityReport
	if jsonErr := json.Unmarshal(stdout.Bytes(), &report); jsonErr != nil {
		t.Fatalf("unmarshal: %v\noutput: %s", jsonErr, stdout.String())
	}
	if report.Complete {
		t.Errorf("Complete = true, want false — an unreadable store must never look like complete absence")
	}
	if len(report.Unavailable) == 0 {
		t.Errorf("Unavailable is empty, want the failed source named")
	}
}

func TestRunAnalyzeContinuity_RequiresAgentFlag(t *testing.T) {
	dir := t.TempDir()
	writeAnalyzeContinuityCity(t, dir)
	withContinuityStore(t, beads.NewMemStore())

	var stdout, stderr bytes.Buffer
	opts := continuityCmdOptions{cityPath: dir, agent: "", since: "7d"}
	if err := runAnalyzeContinuity(opts, &stdout, &stderr); err == nil {
		t.Fatalf("runAnalyzeContinuity with empty --agent: want error, got nil")
	}
}

func TestRunAnalyzeContinuity_TableOutputNotJSON(t *testing.T) {
	dir := t.TempDir()
	writeAnalyzeContinuityCity(t, dir)
	withContinuityStore(t, beads.NewMemStore())

	var stdout, stderr bytes.Buffer
	opts := continuityCmdOptions{cityPath: dir, agent: "nobody", since: "7d", jsonOut: false}
	if err := runAnalyzeContinuity(opts, &stdout, &stderr); err != nil {
		t.Fatalf("runAnalyzeContinuity: %v", err)
	}
	if got := stdout.String(); got == "" {
		t.Fatalf("table output is empty")
	}
	var probe map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &probe); err == nil {
		t.Fatalf("table output parsed as JSON — jsonOut:false must not emit JSON")
	}
}

func TestResolveContinuityIdentities_NeverEmptyEvenWithoutSessionMatch(t *testing.T) {
	ids := resolveContinuityIdentities("solo-agent", nil)
	if len(ids) != 1 || ids[0] != "solo-agent" {
		t.Fatalf("resolveContinuityIdentities = %v, want [solo-agent]", ids)
	}
}
