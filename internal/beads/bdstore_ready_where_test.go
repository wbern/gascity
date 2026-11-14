package beads_test

import (
	"fmt"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// readyWhereRunner answers `bd ready` with two candidates that each carry a
// closed blocking dependency, and counts the blocker lookups (bd list / bd
// query) the work-outcome veto issues.
type readyWhereRunner struct {
	ready, lookups int
	blockedOutcome bool
}

func (r *readyWhereRunner) run(_, _ string, args ...string) ([]byte, error) {
	switch args[0] {
	case "ready":
		r.ready++
		return []byte(`[
{"id":"bd-mine","status":"open","issue_type":"task","assignee":"me","dependency_count":1,"dependencies":[{"issue_id":"bd-mine","depends_on_id":"bd-blocker","type":"blocks"}]},
{"id":"bd-other","status":"open","issue_type":"task","assignee":"someone-else","dependency_count":1,"dependencies":[{"issue_id":"bd-other","depends_on_id":"bd-blocker","type":"blocks"}]}
]`), nil
	case "list":
		r.lookups++
		metadata := ""
		if r.blockedOutcome {
			metadata = `,"metadata":{"gc.work_outcome":"blocked"}`
		}
		return []byte(`[{"id":"bd-blocker","status":"closed","issue_type":"task"` + metadata + `}]`), nil
	case "query":
		r.lookups++
		return []byte(`[]`), nil
	default:
		return nil, fmt.Errorf("unexpected bd command: %v", args)
	}
}

// A caller that only wants a slice of the frontier must not pay the closed
// blocker lookups for every unrelated ready bead: the work-outcome veto is
// per-candidate, so it is only owed for the rows the caller keeps.
func TestBdStoreReadyWhereSkipsBlockerLookupsForRowsNotKept(t *testing.T) {
	runner := &readyWhereRunner{}
	store := beads.NewBdStore(t.TempDir(), runner.run)
	rows, err := store.ReadyWhere(beads.ReadyQuery{TierMode: beads.TierBoth}, func(b beads.Bead) bool {
		return b.Assignee == "nobody"
	})
	if err != nil {
		t.Fatalf("ReadyWhere: %v", err)
	}
	if len(rows) != 0 || runner.ready != 1 || runner.lookups != 0 {
		t.Fatalf("rows=%v ready calls=%d blocker lookups=%d; want 0 rows, 1 ready call, 0 lookups", rows, runner.ready, runner.lookups)
	}
}

// The veto still applies, unchanged, to the rows the caller keeps.
func TestBdStoreReadyWhereKeepsWorkOutcomeVetoForKeptRows(t *testing.T) {
	for _, blocked := range []bool{false, true} {
		t.Run(fmt.Sprintf("blocked=%t", blocked), func(t *testing.T) {
			runner := &readyWhereRunner{blockedOutcome: blocked}
			store := beads.NewBdStore(t.TempDir(), runner.run)
			rows, err := store.ReadyWhere(beads.ReadyQuery{TierMode: beads.TierBoth}, func(b beads.Bead) bool {
				return b.Assignee == "me"
			})
			if err != nil {
				t.Fatalf("ReadyWhere: %v", err)
			}
			want := 1
			if blocked {
				want = 0
			}
			if len(rows) != want || runner.lookups == 0 {
				t.Fatalf("rows=%v blocker lookups=%d; want %d rows and a blocker lookup", rows, runner.lookups, want)
			}
			if want == 1 && rows[0].ID != "bd-mine" {
				t.Fatalf("rows=%v; want only bd-mine", rows)
			}
		})
	}
}

// Ready is ReadyWhere with no predicate: every candidate is checked.
func TestBdStoreReadyStillVetoesWholeFrontier(t *testing.T) {
	runner := &readyWhereRunner{blockedOutcome: true}
	store := beads.NewBdStore(t.TempDir(), runner.run)
	rows, err := store.Ready(beads.ReadyQuery{TierMode: beads.TierBoth})
	if err != nil {
		t.Fatalf("Ready: %v", err)
	}
	if len(rows) != 0 || runner.lookups == 0 {
		t.Fatalf("rows=%v blocker lookups=%d; want both candidates vetoed", rows, runner.lookups)
	}
}
