package config

import (
	"strings"
	"testing"
)

// TestAssignedInProgressTierFederatesOnSplitTopology pins the tier-0 swap
// directly: on a federated topology the crash-recovery read goes through the
// city-wide reader and propagates a dead leg, so a session can see its own claim
// wherever that claim lives.
//
// The byte-identity control below is the other half. Together they say: the swap
// happens on exactly the topologies that need it and nowhere else.
func TestAssignedInProgressTierFederatesOnSplitTopology(t *testing.T) {
	for _, shellVar := range []string{"id", "cand"} {
		got := assignedInProgressTierCommand(shellVar, QueryTopology{FederatedReady: true})
		want := `r=$(gc ready --status in_progress --assignee="$` + shellVar + `" --json --brief --exclude-label='hold:mayor' --exclude-label='hold:external' --exclude-label='held' --exclude-label='hold:human' --limit=1) || exit $?; `
		if got != want {
			t.Errorf("federated tier-0 for $%s =\n  %q\nwant\n  %q", shellVar, got, want)
		}
	}

	a := &Agent{Name: "worker"}
	federated := a.EffectiveAssignedInProgressQueryFor(QueryTopology{FederatedReady: true})
	if strings.Contains(federated, bdListInProgressCommand) {
		t.Errorf("federated crash-recovery tier still shells %q, so a relocated claim stays invisible: %q", bdListInProgressCommand, federated)
	}
	if !strings.Contains(federated, `|| exit $?`) {
		t.Errorf("federated crash-recovery tier does not propagate a dead leg; an empty resume answer on a dead leg is the silent blindness this fixes: %q", federated)
	}
	// The federated reader already carries blocked_by, so the shell enrichment
	// must consult the row before falling back to a `bd show` that resolves
	// nothing for a relocated id.
	if !strings.Contains(federated, `.[0].blocked_by // empty`) {
		t.Errorf("federated crash-recovery tier does not presence-key its enrichment: %q", federated)
	}
}

// Single-store discovery keeps its local reader, with compact projection,
// hold filtering before the limit, and visible reader failures.
func TestAssignedInProgressTierUsesCompactSingleStoreRead(t *testing.T) {
	for _, shellVar := range []string{"id", "cand"} {
		got := assignedInProgressTierCommand(shellVar, QueryTopology{})
		want := `r=$(bd list --status in_progress --assignee="$` + shellVar + `" --json --brief --exclude-label='hold:mayor' --exclude-label='hold:external' --exclude-label='held' --exclude-label='hold:human' --limit=1) || exit $?; `
		if got != want {
			t.Errorf("single-store tier-0 for $%s =\n  %q\nwant compact work discovery\n  %q", shellVar, got, want)
		}
	}
	// The single-store enrichment must not grow the presence key either: its
	// reader is `bd list`, whose rows never carry blocked_by, so the branch could
	// only ever be dead shell on the path every deployment runs.
	single := inProgressBlockedByEnrichmentScript(false, true)
	if strings.Contains(single, "blocked_by // empty") {
		t.Errorf("single-store enrichment grew the federated presence key: %q", single)
	}
}
