package main

import (
	"strings"
	"testing"
	"time"
)

// bd does not emit blocked_by or is_blocked on these rows — both are null — so
// the hook's readiness filter saw no blocker and dispatched a genuinely blocked
// step. Measured shape, taken from gci-mhpd on 2026-08-13 (gcw-kiwk7).
const depBlockedRow = `[{"id":"gci-mhpd","status":"open","blocked_by":null,"is_blocked":null,` +
	`"dependency_count":2,"dependencies":[` +
	`{"id":"gci-irzq","status":"open","dependency_type":"blocks"},` +
	`{"id":"gci-z2lz","status":"in_progress","dependency_type":"tracks"}]}]`

func TestFilterUnreadyStripsCandidateBlockedOnlyByDependencies(t *testing.T) {
	got := filterUnreadyHookCandidates(depBlockedRow, time.Now())
	if strings.Contains(got, "gci-mhpd") {
		t.Fatalf("blocked step survived the readiness filter: %s", got)
	}
}

// The discriminator is the dependency TYPE, not merely a non-closed status.
// Every step bead tracks its parent workflow, which stays in_progress for the
// whole run: treating any non-closed dependency as blocking stalls the entire
// graph rather than only its blocked steps.
func TestFilterUnreadyKeepsCandidateWhoseOnlyOpenDependencyIsNonBlocking(t *testing.T) {
	row := `[{"id":"gci-knge","status":"open","blocked_by":null,"is_blocked":null,` +
		`"dependency_count":2,"dependencies":[` +
		`{"id":"gci-irzq","status":"closed","dependency_type":"blocks"},` +
		`{"id":"gci-z2lz","status":"in_progress","dependency_type":"tracks"}]}]`
	got := filterUnreadyHookCandidates(row, time.Now())
	if !strings.Contains(got, "gci-knge") {
		t.Fatalf("ready step was stranded by the readiness filter: %s", got)
	}
}

func TestFilterUnreadyStripsWaitsForAndConditionalBlockers(t *testing.T) {
	for _, depType := range []string{"waits-for", "conditional-blocks"} {
		row := `[{"id":"gci-x","status":"open","dependencies":[` +
			`{"id":"gci-blocker","status":"open","dependency_type":"` + depType + `"}]}]`
		if got := filterUnreadyHookCandidates(row, time.Now()); strings.Contains(got, "gci-x") {
			t.Errorf("%s blocker did not strip the candidate: %s", depType, got)
		}
	}
}

// The legacy blocked_by path must keep working; this filter is the only gate
// between a pre-assigned blocked step and a worker that will loop on it.
func TestFilterUnreadyStillHonoursBlockedBy(t *testing.T) {
	row := `[{"id":"gci-y","status":"open","blocked_by":["gci-blocker"]}]`
	if got := filterUnreadyHookCandidates(row, time.Now()); strings.Contains(got, "gci-y") {
		t.Fatalf("blocked_by candidate survived: %s", got)
	}
}
