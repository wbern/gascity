package session

import (
	"context"
	"errors"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/rollout/gate"
)

func executionSessionFixture(t *testing.T, mem *beads.MemStore) beads.Bead {
	t.Helper()
	if err := beads.StampOpenedStore(mem, "memory", gate.Require, nil, nil); err != nil {
		t.Fatal(err)
	}
	b, err := mem.Create(beads.Bead{Title: "execution session", Type: BeadType, Metadata: map[string]string{
		"instance_token": "incarnation-one", "generation": "7", "state": "active",
	}})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestRecordStartedClaimPersistsAndIsIdempotent(t *testing.T) {
	mem := beads.NewMemStore()
	b := executionSessionFixture(t, mem)
	s := NewStore(beads.SessionStore{Store: mem})
	fence := ExecutionFence{InstanceToken: "incarnation-one", Generation: "7"}
	claim := WorkStartedEntry{Store: "project", Bead: "task-1"}
	if err := s.RecordStartedClaim(b.ID, fence, claim); err != nil {
		t.Fatal(err)
	}
	got, err := mem.Get(b.ID)
	if err != nil {
		t.Fatal(err)
	}
	cell, err := DecodeWorkStarted(got.Metadata[WorkStartedKey])
	if err != nil || !cell.Names("project", "task-1") {
		t.Fatalf("claim not persisted: cell=%+v err=%v", cell, err)
	}
	if len(cell.Claims) != 1 || cell.Claims[0].Gen != 7 {
		t.Fatalf("wrong execution generation: %+v", cell.Claims)
	}
	if err := s.RecordStartedClaim(b.ID, fence, claim); err != nil {
		t.Fatal(err)
	}
	again, _ := mem.Get(b.ID)
	if again.Metadata[WorkStartedKey] != got.Metadata[WorkStartedKey] {
		t.Fatal("idempotent record changed execution evidence")
	}
}

func TestRecordStartedClaimRefusesStaleOrAmbiguousIncarnation(t *testing.T) {
	for _, fence := range []ExecutionFence{
		{},
		{InstanceToken: "old-incarnation", Generation: "7"},
		{InstanceToken: "incarnation-one", Generation: "6"},
	} {
		mem := beads.NewMemStore()
		b := executionSessionFixture(t, mem)
		s := NewStore(beads.SessionStore{Store: mem})
		if err := s.RecordStartedClaim(b.ID, fence, WorkStartedEntry{Store: "project", Bead: "task-1"}); !errors.Is(err, ErrExecutionIncarnationChanged) {
			t.Fatalf("expected incarnation refusal, got %v", err)
		}
		got, _ := mem.Get(b.ID)
		if got.Metadata[WorkStartedKey] != "" {
			t.Fatal("refused incarnation wrote execution evidence")
		}
	}
}

type executionOpaqueStore struct{ beads.Store }

func TestRecordStartedClaimRequiresConditionalWriter(t *testing.T) {
	mem := beads.NewMemStore()
	b := executionSessionFixture(t, mem)
	s := NewStore(beads.SessionStore{Store: executionOpaqueStore{Store: mem}})
	err := s.RecordStartedClaim(b.ID, ExecutionFence{InstanceToken: "incarnation-one"}, WorkStartedEntry{Bead: "task-1"})
	if !errors.Is(err, beads.ErrConditionalWriteUnsupported) {
		t.Fatalf("expected unsupported fence refusal, got %v", err)
	}
	got, _ := mem.Get(b.ID)
	if got.Metadata[WorkStartedKey] != "" {
		t.Fatal("unsupported store received an unfenced write")
	}
}

type executionRestartRaceStore struct {
	*beads.MemStore
	raced bool
}

func (s *executionRestartRaceStore) UpdateIfMatch(id string, revision int64, opts beads.UpdateOpts) error {
	if !s.raced {
		s.raced = true
		if err := s.Update(id, beads.UpdateOpts{Metadata: map[string]string{"instance_token": "incarnation-two", "generation": "8"}}); err != nil {
			return err
		}
	}
	return s.MemStore.UpdateIfMatch(id, revision, opts)
}

var _ beads.ConditionalWriter = (*executionRestartRaceStore)(nil)

func TestRecordStartedClaimRefusesRestartDuringCAS(t *testing.T) {
	mem := beads.NewMemStore()
	b := executionSessionFixture(t, mem)
	racing := &executionRestartRaceStore{MemStore: mem}
	s := NewStore(beads.SessionStore{Store: racing})
	err := s.RecordStartedClaim(b.ID, ExecutionFence{InstanceToken: "incarnation-one", Generation: "7"}, WorkStartedEntry{Bead: "task-1"})
	if !errors.Is(err, ErrExecutionIncarnationChanged) || !racing.raced {
		t.Fatalf("restart race not refused: raced=%v err=%v", racing.raced, err)
	}
	got, _ := mem.Get(b.ID)
	if got.Metadata["instance_token"] != "incarnation-two" || got.Metadata[WorkStartedKey] != "" {
		t.Fatal("stale execution record overwrote the replacement session")
	}
}

func TestRecordStartedClaimPreservesUnreadableEvidence(t *testing.T) {
	mem := beads.NewMemStore()
	b := executionSessionFixture(t, mem)
	if err := mem.SetMetadata(b.ID, WorkStartedKey, "{"); err != nil {
		t.Fatal(err)
	}
	s := NewStore(beads.SessionStore{Store: mem})
	if err := s.RecordStartedClaim(b.ID, ExecutionFence{InstanceToken: "incarnation-one"}, WorkStartedEntry{Bead: "task-1"}); err == nil {
		t.Fatal("unreadable evidence was silently replaced")
	}
	got, _ := mem.Get(b.ID)
	if got.Metadata[WorkStartedKey] != "{" {
		t.Fatal("unreadable evidence changed")
	}
}

func TestExecutionAndReleaseReservationsExcludeEachOther(t *testing.T) {
	for _, startFirst := range []bool{false, true} {
		mem := beads.NewMemStore()
		b := executionSessionFixture(t, mem)
		s := NewStore(beads.SessionStore{Store: mem})
		fence := ExecutionFence{InstanceToken: "incarnation-one", Generation: "7"}
		claim := WorkStartedEntry{Store: "project", Bead: "task-1"}
		if startFirst {
			if err := s.RecordStartedClaim(b.ID, fence, claim); err != nil {
				t.Fatal(err)
			}
			if _, err := s.ReserveClaimRelease(b.ID, fence, ClaimRelease{Store: "project", Bead: "task-1", Revision: 9}); !errors.Is(err, ErrClaimAlreadyStarted) {
				t.Fatalf("started work admitted release: %v", err)
			}
		} else {
			reservation, err := s.ReserveClaimRelease(b.ID, fence, ClaimRelease{Store: "project", Bead: "task-1", Revision: 9})
			if err != nil || reservation.Revision != 9 {
				t.Fatalf("reserve: %+v %v", reservation, err)
			}
			if err := s.RecordStartedClaim(b.ID, fence, claim); !errors.Is(err, ErrClaimReleasePending) {
				t.Fatalf("pending release admitted execution: %v", err)
			}
			if err := s.CompleteClaimRelease(b.ID, fence, reservation); err != nil {
				t.Fatal(err)
			}
			if err := s.RecordStartedClaim(b.ID, fence, claim); err != nil {
				t.Fatalf("terminal release reservation was not cleared: %v", err)
			}
		}
	}
}

func TestReleaseRetryKeepsCapturedRevisionAndOldCompletionCannotClearReplacement(t *testing.T) {
	mem := beads.NewMemStore()
	b := executionSessionFixture(t, mem)
	s := NewStore(beads.SessionStore{Store: mem})
	fence := ExecutionFence{InstanceToken: "incarnation-one", Generation: "7"}
	old := ClaimRelease{Store: "project", Bead: "task-1", Revision: 9}
	if _, err := s.ReserveClaimRelease(b.ID, fence, old); err != nil {
		t.Fatal(err)
	}
	next := ClaimRelease{Store: "project", Bead: "task-1", Revision: 10}
	retained, err := s.ReserveClaimRelease(b.ID, fence, next)
	if err != nil || retained.Revision != old.Revision {
		t.Fatalf("retry adopted a replacement claim revision: %+v %v", retained, err)
	}
	if err := s.CompleteClaimRelease(b.ID, fence, old); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReserveClaimRelease(b.ID, fence, next); err != nil {
		t.Fatal(err)
	}
	if err := s.CompleteClaimRelease(b.ID, fence, old); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordStartedClaim(b.ID, fence, WorkStartedEntry{Store: "project", Bead: "task-1"}); !errors.Is(err, ErrClaimReleasePending) {
		t.Fatalf("old completion erased replacement reservation: %v", err)
	}
}

func TestExecutionEvidenceUsesOpaqueNonzeroSessionRevisions(t *testing.T) {
	for _, revision := range []int64{-71, 0, 71} {
		mem := beads.NewMemStoreFrom(1, []beads.Bead{{
			ID: "gc-1", Type: BeadType, Status: "open", Revision: revision,
			Metadata: map[string]string{"instance_token": "incarnation-one", "generation": "7"},
		}}, nil)
		if err := beads.StampOpenedStore(mem, "memory", gate.Require, nil, nil); err != nil {
			t.Fatal(err)
		}
		s := NewStore(beads.SessionStore{Store: mem})
		err := s.RecordStartedClaim("gc-1", ExecutionFence{InstanceToken: "incarnation-one", Generation: "7"}, WorkStartedEntry{Store: "project", Bead: "task-1"})
		if revision == 0 {
			if !errors.Is(err, ErrExecutionRevisionMissing) {
				t.Fatalf("zero revision admitted execution: %v", err)
			}
		} else if err != nil {
			t.Fatalf("valid opaque revision %d refused: %v", revision, err)
		}
	}
}

func TestReleaseReservationUsesOpaqueNonzeroWorkRevisions(t *testing.T) {
	for _, revision := range []int64{-91, 0, 91} {
		mem := beads.NewMemStore()
		b := executionSessionFixture(t, mem)
		s := NewStore(beads.SessionStore{Store: mem})
		captured, err := s.ReserveClaimRelease(b.ID, ExecutionFence{InstanceToken: "incarnation-one"}, ClaimRelease{Store: "project", Bead: "task-1", Revision: revision})
		if revision == 0 {
			if err == nil {
				t.Fatal("zero work revision reserved a release")
			}
		} else if err != nil || captured.Revision != revision {
			t.Fatalf("opaque work revision changed or refused: captured=%d err=%v", captured.Revision, err)
		}
	}
}

func TestExecutionEvidenceRejectsMalformedReleaseReservations(t *testing.T) {
	for _, raw := range []string{
		`{"v":1,"releases":[{"store":"city","bead":"task","revision":0}]}`,
		`{"v":1,"releases":[{"store":"","bead":"task","revision":-1}]}`,
		`{"v":1,"releases":[{"store":"city","bead":"","revision":1}]}`,
	} {
		if _, err := DecodeWorkStarted(raw); err == nil {
			t.Fatalf("accepted malformed release reservation: %s", raw)
		}
	}
}

// A completed drain must close admission before its caller acknowledges the
// runtime; an abandoned census must be reopenable by its exact owner only.
func TestDrainAdmissionExcludesStartedClaimsAndFencesCleanup(t *testing.T) {
	mem := beads.NewMemStore()
	row := executionSessionFixture(t, mem)
	store := NewStore(beads.SessionStore{Store: mem})
	fence := ExecutionFence{InstanceToken: "incarnation-one", Generation: "7"}
	claim := WorkStartedEntry{Store: "work", Bead: "task-1"}
	first, err := store.BeginDrainAdmission(row.ID, fence)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.RecordStartedClaim(row.ID, fence, claim); !errors.Is(err, ErrExecutionDraining) {
		t.Fatalf("admitted during census: %v", err)
	}
	second, err := store.BeginDrainAdmission(row.ID, fence)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.FinishDrainAdmission(row.ID, fence, first, false); err == nil {
		t.Fatal("superseded census reopened admission")
	}
	if err := store.FinishDrainAdmission(row.ID, fence, first, true); err == nil {
		t.Fatal("superseded census committed admission")
	}
	if err := store.FinishDrainAdmission(row.ID, fence, second, false); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordStartedClaim(row.ID, fence, claim); err != nil {
		t.Fatal(err)
	}
	last, err := store.BeginDrainAdmission(row.ID, fence)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.FinishDrainAdmission(row.ID, fence, last, true); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordStartedClaim(row.ID, fence, claim); !errors.Is(err, ErrExecutionDraining) {
		t.Fatalf("idempotent claim admitted after commit: %v", err)
	}
	if err := store.FinishDrainAdmission(row.ID, fence, last, false); err == nil {
		t.Fatal("committed drain reopened admission")
	}
	if err := mem.SetMetadataBatch(row.ID, map[string]string{"instance_token": "incarnation-two", "generation": "8"}); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordStartedClaim(row.ID, ExecutionFence{InstanceToken: "incarnation-two", Generation: "8"}, claim); err != nil {
		t.Fatalf("replacement incarnation refused: %v", err)
	}
}

func TestDrainAdmissionRefusesMissingGenerationWithoutWritingEvidence(t *testing.T) {
	mem := beads.NewMemStore()
	row := executionSessionFixture(t, mem)
	if err := mem.SetMetadata(row.ID, "generation", ""); err != nil {
		t.Fatal(err)
	}
	store := NewStore(beads.SessionStore{Store: mem})
	if _, err := store.BeginDrainAdmission(row.ID, ExecutionFence{InstanceToken: "incarnation-one"}); err == nil {
		t.Fatal("drain admitted without a persisted generation")
	}
	fresh, err := mem.Get(row.ID)
	if err != nil {
		t.Fatal(err)
	}
	if fresh.Metadata[WorkStartedKey] != "" {
		t.Fatal("refused drain left unreadable admission evidence")
	}
}

func TestDrainAdmissionRetryDoesNotTrustStaleCachedCommit(t *testing.T) {
	mem := beads.NewMemStore()
	row := executionSessionFixture(t, mem)
	cache := beads.NewCachingStoreForTest(mem, nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatal(err)
	}
	store := NewStore(beads.SessionStore{Store: cache})
	fence := ExecutionFence{InstanceToken: "incarnation-one", Generation: "7"}
	attempt, err := store.BeginDrainAdmission(row.ID, fence)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.FinishDrainAdmission(row.ID, fence, attempt, true); err != nil {
		t.Fatal(err)
	}
	// Model another process canceling through the authoritative backing while
	// this controller's session cache still carries its committed census.
	if err := mem.SetMetadata(row.ID, WorkStartedKey, `{"v":1,"claims":[]}`); err != nil {
		t.Fatal(err)
	}
	if err := store.FinishDrainAdmission(row.ID, fence, attempt, true); err == nil {
		t.Fatal("stale cached commit authorized an obsolete ACK retry")
	}
}

func TestDrainCancellationAndStopAcceptanceExcludeEachOther(t *testing.T) {
	mem := beads.NewMemStore()
	row := executionSessionFixture(t, mem)
	store := NewStore(beads.SessionStore{Store: mem})
	fence := ExecutionFence{InstanceToken: "incarnation-one", Generation: "7"}
	attempt, err := store.BeginDrainAdmission(row.ID, fence)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.FinishDrainAdmission(row.ID, fence, attempt, true); err != nil {
		t.Fatal(err)
	}
	if err := store.CancelDrainAdmission(row.ID, fence, attempt); err != nil {
		t.Fatal(err)
	}
	if err := store.AcceptDrainStop(row.ID, fence, attempt); !errors.Is(err, ErrDrainAdmissionChanged) {
		t.Fatalf("canceled ACK accepted: %v", err)
	}
	if err := store.RecordStartedClaim(row.ID, fence, WorkStartedEntry{Bead: "task-1"}); err != nil {
		t.Fatalf("canceled census did not reopen: %v", err)
	}
	second, err := store.BeginDrainAdmission(row.ID, fence)
	if err != nil {
		t.Fatal(err)
	}
	if second == attempt {
		t.Fatal("new census reused canceled attempt")
	}
	if err := store.FinishDrainAdmission(row.ID, fence, second, true); err != nil {
		t.Fatal(err)
	}
	if err := store.AcceptDrainStop(row.ID, fence, second); err != nil {
		t.Fatal(err)
	}
	if err := store.CancelDrainAdmission(row.ID, fence, second); !errors.Is(err, ErrDrainStopAccepted) {
		t.Fatalf("accepted stop reopened: %v", err)
	}
	if err := store.RecordStartedClaim(row.ID, fence, WorkStartedEntry{Bead: "task-2"}); !errors.Is(err, ErrExecutionDraining) {
		t.Fatalf("accepted stop admitted work: %v", err)
	}
	if err := mem.SetMetadataBatch(row.ID, map[string]string{"instance_token": "incarnation-two", "generation": "8"}); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordStartedClaim(row.ID, ExecutionFence{InstanceToken: "incarnation-two", Generation: "8"}, WorkStartedEntry{Bead: "task-2"}); err != nil {
		t.Fatalf("replacement incarnation refused: %v", err)
	}
}

func TestDrainReleaseRequiresItsActiveCensusAndRetainsPendingOutcome(t *testing.T) {
	mem := beads.NewMemStore()
	row := executionSessionFixture(t, mem)
	store := NewStore(beads.SessionStore{Store: mem})
	fence := ExecutionFence{InstanceToken: "incarnation-one", Generation: "7"}
	first, err := store.BeginDrainAdmission(row.ID, fence)
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.BeginDrainAdmission(row.ID, fence)
	if err != nil {
		t.Fatal(err)
	}
	proposal := ClaimRelease{Store: "work", Bead: "task-1", Revision: -71}
	if _, err := store.ReserveDrainClaimRelease(row.ID, fence, first, proposal); !errors.Is(err, ErrDrainAdmissionChanged) {
		t.Fatalf("superseded census reserved fresh work: %v", err)
	}
	captured, err := store.ReserveDrainClaimRelease(row.ID, fence, second, proposal)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CancelDrainAdmission(row.ID, fence, second); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ReserveDrainClaimRelease(row.ID, fence, second, ClaimRelease{Store: "work", Bead: "task-2", Revision: 81}); !errors.Is(err, ErrDrainAdmissionChanged) {
		t.Fatalf("canceled census captured new assignment: %v", err)
	}
	if err := store.RecordStartedClaim(row.ID, fence, WorkStartedEntry{Store: "work", Bead: "task-1"}); !errors.Is(err, ErrClaimReleasePending) {
		t.Fatalf("uncertain release forgotten by cancellation: %v", err)
	}
	if err := store.CompleteClaimRelease(row.ID, fence, captured); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordStartedClaim(row.ID, fence, WorkStartedEntry{Store: "work", Bead: "task-1"}); err != nil {
		t.Fatalf("definite completion did not permit fresh admission: %v", err)
	}
}

type executionDormantRaceStore struct {
	*beads.MemStore
	raced bool
}

func (s *executionDormantRaceStore) UpdateIfMatch(id string, revision int64, opts beads.UpdateOpts) error {
	if !s.raced {
		s.raced = true
		if err := s.SetMetadata(id, "state", string(StateAsleep)); err != nil {
			return err
		}
	}
	return s.MemStore.UpdateIfMatch(id, revision, opts)
}

func TestRecordStartedClaimRefusesDormantTransitionDuringCAS(t *testing.T) {
	mem := beads.NewMemStore()
	row := executionSessionFixture(t, mem)
	raced := &executionDormantRaceStore{MemStore: mem}
	store := NewStore(beads.SessionStore{Store: raced})
	err := store.RecordStartedClaim(row.ID, ExecutionFence{InstanceToken: "incarnation-one", Generation: "7"}, WorkStartedEntry{Bead: "task-1"})
	if err == nil {
		t.Fatal("dormant same-incarnation session admitted work after CAS retry")
	}
	saved, getErr := mem.Get(row.ID)
	if getErr != nil {
		t.Fatal(getErr)
	}
	if saved.Metadata[WorkStartedKey] != "" {
		t.Fatal("dormant session received a work-start marker")
	}
}

func TestControllerDrainIntentIsNotAReleaseCensus(t *testing.T) {
	mem := beads.NewMemStore()
	row := executionSessionFixture(t, mem)
	front := NewStore(beads.SessionStore{Store: mem})
	fence := ExecutionFence{InstanceToken: "incarnation-one", Generation: "7"}
	if err := front.RecordStartedClaim(row.ID, fence, WorkStartedEntry{Bead: "active-work"}); err != nil {
		t.Fatal(err)
	}
	first, err := front.BeginControllerDrainAdmission(row.ID, fence)
	if err != nil {
		t.Fatal(err)
	}
	gate, err := front.InspectDrainAdmission(row.ID, fence, first)
	if err != nil || gate.Committed {
		t.Fatalf("controller intent fabricated census: %+v %v", gate, err)
	}
	if err := front.AcceptDrainStop(row.ID, fence, first); !errors.Is(err, ErrDrainAdmissionChanged) {
		t.Fatalf("agent stop accepted incomplete census: %v", err)
	}
	if err := front.CancelDrainAdmission(row.ID, fence, first); err != nil {
		t.Fatal(err)
	}
	if err := front.AcceptControllerDrainStop(row.ID, fence, first); !errors.Is(err, ErrDrainAdmissionChanged) {
		t.Fatalf("canceled controller stop accepted: %v", err)
	}
	second, err := front.BeginControllerDrainAdmission(row.ID, fence)
	if err != nil || second == first {
		t.Fatalf("fresh controller issue failed: %v", err)
	}
	if err := front.AcceptControllerDrainStop(row.ID, fence, second); err != nil {
		t.Fatal(err)
	}
	if _, err := front.BeginDrainAdmission(row.ID, fence); !errors.Is(err, ErrDrainStopAccepted) {
		t.Fatalf("agent superseded accepted controller stop: %v", err)
	}
	if _, err := front.BeginControllerDrainAdmission(row.ID, fence); !errors.Is(err, ErrDrainStopAccepted) {
		t.Fatalf("controller superseded accepted stop: %v", err)
	}
	if err := front.FinishDrainAdmission(row.ID, fence, second, true); !errors.Is(err, ErrDrainStopAccepted) {
		t.Fatalf("accepted controller intent forged census commit: %v", err)
	}
	if _, err := front.ReserveDrainClaimRelease(row.ID, fence, second, ClaimRelease{Store: "work", Bead: "unstarted", Revision: -71}); !errors.Is(err, ErrDrainAdmissionChanged) {
		t.Fatalf("accepted controller intent reserved release: %v", err)
	}
	if err := front.CancelDrainAdmission(row.ID, fence, second); !errors.Is(err, ErrDrainStopAccepted) {
		t.Fatalf("accepted controller stop reopened: %v", err)
	}
	_, cell, _, err := front.ExecutionEvidence(row.ID)
	if err != nil || cell.Drain.Committed || !cell.Drain.Accepted || !cell.Names("", "active-work") {
		t.Fatalf("controller stop changed census or lost active work: %+v %v", cell, err)
	}
}

type executionHashPatchRaceStore struct {
	*beads.MemStore
	onPatch func()
}

func (s *executionHashPatchRaceStore) UpdateIfMatch(id string, revision int64, opts beads.UpdateOpts) error {
	if opts.Metadata["started_config_hash"] != "" && s.onPatch != nil {
		callback := s.onPatch
		s.onPatch = nil
		callback()
	}
	return s.MemStore.UpdateIfMatch(id, revision, opts)
}

func TestExecutionFencedHashPatchPreservesConcurrentEvidenceAndRefusesReplacement(t *testing.T) {
	for _, scenario := range []string{"claim", "new-drain", "new-incarnation"} {
		t.Run(scenario, func(t *testing.T) {
			mem := beads.NewMemStore()
			row := executionSessionFixture(t, mem)
			wrapped := &executionHashPatchRaceStore{MemStore: mem}
			front := NewStore(beads.SessionStore{Store: wrapped})
			base := NewStore(beads.SessionStore{Store: mem})
			fence := ExecutionFence{InstanceToken: "incarnation-one", Generation: "7"}
			wrapped.onPatch = func() {
				switch scenario {
				case "claim":
					if err := base.RecordStartedClaim(row.ID, fence, WorkStartedEntry{Store: "project", Bead: "concurrent-work"}); err != nil {
						t.Fatal(err)
					}
				case "new-drain":
					if _, err := base.BeginControllerDrainAdmission(row.ID, fence); err != nil {
						t.Fatal(err)
					}
				case "new-incarnation":
					if err := mem.SetMetadataBatch(row.ID, map[string]string{"instance_token": "incarnation-two", "generation": "8"}); err != nil {
						t.Fatal(err)
					}
				}
			}
			err := front.ApplyPatchWithExecutionFence(row.ID, fence, "", MetadataPatch{"started_config_hash": "accepted"})
			saved, readErr := mem.Get(row.ID)
			if readErr != nil {
				t.Fatal(readErr)
			}
			cell, readErr := DecodeWorkStarted(saved.Metadata[WorkStartedKey])
			if readErr != nil {
				t.Fatal(readErr)
			}
			switch scenario {
			case "claim":
				if err != nil || saved.Metadata["started_config_hash"] != "accepted" || !cell.Names("project", "concurrent-work") {
					t.Fatalf("patch lost concurrent execution evidence: cell=%+v err=%v", cell, err)
				}
			case "new-drain":
				if !errors.Is(err, ErrDrainAdmissionChanged) || saved.Metadata["started_config_hash"] != "" || cell.Drain == nil {
					t.Fatalf("patch adopted new drain: gate=%+v err=%v", cell.Drain, err)
				}
			case "new-incarnation":
				if !errors.Is(err, ErrExecutionIncarnationChanged) || saved.Metadata["started_config_hash"] != "" || saved.Metadata["instance_token"] != "incarnation-two" {
					t.Fatalf("patch adopted new incarnation: err=%v", err)
				}
			}
		})
	}
}
