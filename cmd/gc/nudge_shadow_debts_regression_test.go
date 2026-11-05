package main

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"syscall"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/nudgequeue"
)

func TestAckShadowlessSubmitLeavesNoRetiredDebt(t *testing.T) {
	cityPath := t.TempDir()
	item := newQueuedNudgeWithOptions("worker", "submit prompt", "session", time.Now(), queuedNudgeOptions{})
	if err := nudgequeue.WithState(cityPath, func(state *nudgequeue.State) error { state.InFlight = []queuedNudge{item}; return nil }); err != nil {
		t.Fatal(err)
	}
	if err := ackQueuedNudges(cityPath, []string{item.ID}); err != nil {
		t.Fatal(err)
	}
	state, err := nudgequeue.LoadState(cityPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Pending) != 0 || len(state.InFlight) != 0 || len(state.Retired) != 0 {
		t.Fatalf("shadowless submit left queue/debt entries: %+v", state)
	}
}

func TestNudgeShadowReusePreservesAnActivePayersDebt(t *testing.T) {
	now := time.Now()
	state := nudgeQueueState{Retired: []queuedNudge{
		{ID: "repeat", BeadID: "shadow", Shadow: &nudgequeue.ShadowDebt{State: "injected"}},
		{ID: "repeat", BeadID: "shadow", Shadow: &nudgequeue.ShadowDebt{State: "injected", Leased: true, NotBefore: now.Add(time.Minute)}},
	}}
	before := append([]queuedNudge(nil), state.Retired...)
	err := dropRetiredDebtsOnShadow(&state, queuedNudge{ID: "repeat", BeadID: "shadow"}, now)
	if !errors.Is(err, nudgequeue.ErrQueueBusy) || !errors.Is(err, errNudgeShadowRetiring) {
		t.Fatalf("reuse error=%v, want retiring queue-busy", err)
	}
	if !reflect.DeepEqual(state.Retired, before) {
		t.Fatalf("busy reuse changed debts: %+v", state.Retired)
	}
}

func TestNudgeShadowReuseDropsOnlyTheUnleasedMatchingShadow(t *testing.T) {
	now := time.Now()
	state := nudgeQueueState{Retired: []queuedNudge{
		{ID: "repeat", BeadID: "old-shadow", Shadow: &nudgequeue.ShadowDebt{State: "injected"}},
		{ID: "repeat", BeadID: "reused-shadow", Shadow: &nudgequeue.ShadowDebt{State: "injected"}},
		{ID: "other", BeadID: "reused-shadow", Shadow: &nudgequeue.ShadowDebt{State: "injected"}},
	}}
	if err := dropRetiredDebtsOnShadow(&state, queuedNudge{ID: "repeat", BeadID: "reused-shadow"}, now); err != nil {
		t.Fatal(err)
	}
	if len(state.Retired) != 2 || state.Retired[0].BeadID != "old-shadow" || state.Retired[1].ID != "other" {
		t.Fatalf("wrong debts dropped: %+v", state.Retired)
	}
}

func TestEnqueueCannotReuseAShadowWhileItsRetiredPayerIsActive(t *testing.T) {
	for _, leased := range []bool{false, true} {
		name := "unleased debt keeps its own shadow"
		if leased {
			name = "active payer keeps its private shadow"
		}
		t.Run(name, func(t *testing.T) {
			cityPath := t.TempDir()
			now := time.Now()
			store := beads.NewMemStore()
			item := newQueuedNudgeWithOptions("worker", "replacement", "session", now, queuedNudgeOptions{ID: "stable-id"})
			shadow, _, err := ensureQueuedNudgeBead(beads.NudgesStore{Store: store}, item)
			if err != nil {
				t.Fatal(err)
			}
			old := item
			old.BeadID = shadow
			old.CreatedAt = now.Add(-time.Hour)
			old.Shadow = &nudgequeue.ShadowDebt{State: "injected", At: now, Leased: leased, NotBefore: now.Add(time.Minute)}
			if err := nudgequeue.WithState(cityPath, func(state *nudgequeue.State) error { state.Retired = []queuedNudge{old}; return nil }); err != nil {
				t.Fatal(err)
			}
			err = enqueueQueuedNudgeWithStore(cityPath, beads.NudgesStore{Store: store}, item)
			state, readErr := nudgequeue.LoadState(cityPath)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if err != nil {
				t.Fatal(err)
			}
			if len(state.Pending) != 1 || state.Pending[0].BeadID == shadow {
				t.Fatalf("new generation reused old shadow: %+v", state)
			}
			if leased && len(state.Retired) != 1 {
				t.Fatalf("new generation discarded active payer debt: %+v", state)
			}
			if !leased && len(state.Retired) != 1 {
				t.Fatalf("new generation discarded unpaid old debt: %+v", state)
			}
			bead, err := store.Get(shadow)
			if err != nil {
				t.Fatal(err)
			}
			if bead.Status != "open" {
				t.Fatalf("enqueue closed a serving or leased shadow: %+v", bead)
			}
			if err := payNudgeShadowDebt(nudgeFrontDoor(beads.NudgesStore{Store: store}), old); err != nil {
				t.Fatal(err)
			}
			serving, err := store.Get(state.Pending[0].BeadID)
			if err != nil || serving.Status != "open" {
				t.Fatalf("old debt closed new generation: %+v, %v", serving, err)
			}
		})
	}
}

func TestNudgeShadowReuseDoesNotAssumeAnExpiredPayerStopped(t *testing.T) {
	now := time.Now()
	state := nudgeQueueState{Retired: []queuedNudge{{ID: "repeat", BeadID: "shadow", Shadow: &nudgequeue.ShadowDebt{State: "injected", Leased: true, NotBefore: now.Add(-time.Minute)}}}}
	if err := dropRetiredDebtsOnShadow(&state, queuedNudge{ID: "repeat", BeadID: "shadow"}, now); !errors.Is(err, nudgequeue.ErrQueueBusy) {
		t.Fatalf("expired lease allowed reuse while a store call may still be running: %v", err)
	}
	if len(state.Retired) != 1 {
		t.Fatalf("expired payer's debt was discarded: %+v", state)
	}
}

func TestNudgeDebtConfirmationDoesNotRetryACommittedTransition(t *testing.T) {
	t.Setenv(hookDeadlineEnv, "")
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Dir(nudgequeue.LockPath(cityPath)), 0o700); err != nil {
		t.Fatal(err)
	}
	lock, err := os.OpenFile(nudgequeue.LockPath(cityPath), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lock.Close() }()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN) }()
	restoreWait := nudgequeue.SetLockWaitForTest(time.Nanosecond)
	defer restoreWait()
	oldBackoff := nudgeBookkeepingBackoff
	nudgeBookkeepingBackoff = func(int) time.Duration { return 0 }
	t.Cleanup(func() { nudgeBookkeepingBackoff = oldBackoff })
	attempts := 0
	nudgequeue.SetLockObserver(func(sample nudgequeue.LockSample) {
		if sample.Verb == "confirm_debts" {
			attempts++
		}
	})
	t.Cleanup(func() { nudgequeue.SetLockObserver(observeNudgeQueueLock) })
	err = confirmNudgeShadowDebts(cityPath, []nudgeDebtPayment{{item: queuedNudge{ID: "committed"}}}, time.Now())
	if !errors.Is(err, nudgequeue.ErrQueueBusy) {
		t.Fatalf("confirmation error=%v, want queue busy", err)
	}
	if attempts != 1 {
		t.Fatalf("already committed shadow confirmation made %d lock attempts, want one", attempts)
	}
}

// afterLookupNudgeStore finishes a payer at the old Save lookup boundary or
// at the fresh Create boundary, before the enqueue's final admission.
type afterLookupNudgeStore struct {
	beads.Store
	afterLookup func()
	afterCreate func(beads.Bead)
}

func (s *afterLookupNudgeStore) List(q beads.ListQuery) ([]beads.Bead, error) {
	items, err := s.Store.List(q)
	if s.afterLookup != nil {
		done := s.afterLookup
		s.afterLookup = nil
		done()
	}
	return items, err
}

func TestEnqueueRejectsShadowClosedAfterPrelockLookup(t *testing.T) {
	cityPath := t.TempDir()
	now := time.Now()
	mem := beads.NewMemStore()
	store := beads.NudgesStore{Store: mem}
	old := newQueuedNudgeWithOptions("worker", "old", "session", now.Add(-time.Hour), queuedNudgeOptions{ID: "repeat"})
	var err error
	old.BeadID, _, err = ensureQueuedNudgeBead(store, old)
	if err != nil {
		t.Fatal(err)
	}
	ledger := newNudgeShadowLedger(now)
	ledger.owe(&old, "injected", "old delivered", "provider-nudge-return", now)
	if err := nudgequeue.WithState(cityPath, func(s *nudgeQueueState) error { s.Retired = []queuedNudge{old}; return nil }); err != nil {
		t.Fatal(err)
	}
	wrapped := &afterLookupNudgeStore{Store: mem}
	finished := false
	finish := func() {
		if finished {
			return
		}
		finished = true
		if err := payNudgeShadowDebt(nudgeFrontDoor(store), old); err != nil {
			t.Fatal(err)
		}
		if err := confirmNudgeShadowDebts(cityPath, []nudgeDebtPayment{{item: old, reached: true}}, now); err != nil {
			t.Fatal(err)
		}
		state, err := nudgequeue.LoadState(cityPath)
		if err != nil || len(state.Retired) != 0 {
			t.Fatalf("payer did not finish: %+v, %v", state, err)
		}
	}
	wrapped.afterLookup = finish
	wrapped.afterCreate = func(beads.Bead) { finish() }

	next := newQueuedNudgeWithOptions("worker", "new", "session", now, queuedNudgeOptions{ID: old.ID})
	err = enqueueQueuedNudgeWithStore(cityPath, beads.NudgesStore{Store: wrapped}, next)
	if err != nil {
		t.Fatal(err)
	}
	state, err := nudgequeue.LoadState(cityPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Pending) != 1 {
		t.Fatalf("fresh generation was not admitted: %+v", state)
	}
	for _, pending := range state.Pending {
		if pending.BeadID == old.BeadID {
			t.Fatalf("pending new generation references closed old shadow %s", old.BeadID)
		}
	}
	shadow, err := mem.Get(old.BeadID)
	if err != nil {
		t.Fatal(err)
	}
	if shadow.Status != "closed" || shadow.Metadata["state"] != "injected" || shadow.Metadata["terminal_reason"] != "old delivered" {
		t.Fatalf("old terminal record overwritten: %+v", shadow)
	}
}

func (s *afterLookupNudgeStore) Create(b beads.Bead) (beads.Bead, error) {
	result, err := s.Store.Create(b)
	if err == nil && s.afterCreate != nil {
		done := s.afterCreate
		s.afterCreate = nil
		done(result)
	}
	return result, err
}

// Another enqueue completes its full lifecycle while this attempt creates its
// shadow. Distinct generation identities preserve both the completed terminal
// record and the newly admitted pending item across the empty-queue ABA.
func TestEnqueueFreshShadowSurvivesOtherGenerationABA(t *testing.T) {
	cityPath := t.TempDir()
	now := time.Now()
	mem := beads.NewMemStore()
	store := beads.NudgesStore{Store: mem}
	item := newQueuedNudgeWithOptions("worker", "new", "session", now, queuedNudgeOptions{ID: "repeat"})
	var adoptedID string
	wrapped := &afterLookupNudgeStore{Store: mem}
	wrapped.afterCreate = func(_ beads.Bead) {
		if err := enqueueQueuedNudgeWithStore(cityPath, store, item); err != nil {
			t.Fatal(err)
		}
		var old queuedNudge
		ledger := newNudgeShadowLedger(now)
		if err := nudgequeue.WithState(cityPath, func(state *nudgeQueueState) error {
			if len(state.Pending) != 1 {
				t.Fatalf("adopter did not enqueue: %+v", state)
			}
			old = state.Pending[0]
			adoptedID = old.BeadID
			ledger.owe(&old, "injected", "adopter delivered", "provider-nudge-return", now)
			state.Pending = nil
			state.Retired = []queuedNudge{old}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if err := payNudgeShadowDebt(nudgeFrontDoor(store), old); err != nil {
			t.Fatal(err)
		}
		if err := confirmNudgeShadowDebts(cityPath, []nudgeDebtPayment{{item: old, reached: true}}, now); err != nil {
			t.Fatal(err)
		}
	}
	err := enqueueQueuedNudgeWithStore(cityPath, beads.NudgesStore{Store: wrapped}, item)
	if err != nil {
		t.Fatalf("fresh generation rejected after ABA: %v", err)
	}
	state, err := nudgequeue.LoadState(cityPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Pending) != 1 || len(state.Retired) != 0 || state.Pending[0].BeadID == adoptedID {
		t.Fatalf("fresh generation lost or reused completed shadow: %+v", state)
	}
	pending, err := mem.Get(state.Pending[0].BeadID)
	if err != nil || pending.Status != "open" {
		t.Fatalf("pending shadow is closed/missing: %+v, %v", pending, err)
	}
	shadow, err := mem.Get(adoptedID)
	if err != nil {
		t.Fatal(err)
	}
	if shadow.Status != "closed" || shadow.Metadata["terminal_reason"] != "adopter delivered" || shadow.Metadata["close_reason"] != nudgequeue.CanonicalCloseReason("injected") {
		t.Fatalf("stale creator rollback corrupted adopted terminal record: %+v", shadow)
	}
}

func TestVerifyDebtReadsRecordedGeneration(t *testing.T) {
	mem := beads.NewMemStore()
	front := nudgeFrontDoor(beads.NudgesStore{Store: mem})
	item := queuedNudge{ID: "repeat"}
	oldID, _, err := front.Save(item)
	if err != nil {
		t.Fatal(err)
	}
	newer, err := mem.Create(beads.Bead{Title: "newer", Labels: []string{"gc:nudge", "nudge:repeat"}, Metadata: map[string]string{"nudge_id": "repeat", "state": "injected"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := mem.Close(newer.ID); err != nil {
		t.Fatal(err)
	}
	item.BeadID = oldID
	item.Shadow = &nudgequeue.ShadowDebt{State: "failed", Reason: "old failed", At: time.Now(), Verify: true}
	if err := payNudgeShadowDebt(front, item); err != nil {
		t.Fatal(err)
	}
	old, err := mem.Get(oldID)
	if err != nil {
		t.Fatal(err)
	}
	if old.Status != "closed" || old.Metadata["state"] != "failed" {
		t.Fatalf("newer terminal record hid old unpaid debt: %+v", old)
	}
}

func TestEnqueueUnrelatedCommitDoesNotRejectANewShadow(t *testing.T) {
	cityPath := t.TempDir()
	mem := beads.NewMemStore()
	now := time.Now()
	item := newQueuedNudgeWithOptions("worker", "first", "session", now, queuedNudgeOptions{ID: "first"})
	wrapped := &afterLookupNudgeStore{Store: mem}
	wrapped.afterCreate = func(beads.Bead) {
		other := newQueuedNudgeWithOptions("worker", "other", "session", now, queuedNudgeOptions{ID: "other"})
		if err := enqueueQueuedNudgeWithStore(cityPath, beads.NudgesStore{Store: mem}, other); err != nil {
			t.Fatal(err)
		}
	}
	if err := enqueueQueuedNudgeWithStore(cityPath, beads.NudgesStore{Store: wrapped}, item); err != nil {
		t.Fatalf("unrelated enqueue rejected independent producer: %v", err)
	}
	state, err := nudgequeue.LoadState(cityPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Pending) != 2 {
		t.Fatalf("lost independent enqueue: %+v", state)
	}
}

func TestConcurrentDuplicateEnqueueRollsBackOnlyItsPrivateShadow(t *testing.T) {
	cityPath := t.TempDir()
	mem := beads.NewMemStore()
	item := newQueuedNudgeWithOptions("worker", "same", "session", time.Now(), queuedNudgeOptions{ID: "duplicate"})
	wrapped := &afterLookupNudgeStore{Store: mem}
	var loserID string
	wrapped.afterCreate = func(b beads.Bead) {
		loserID = b.ID
		if err := enqueueQueuedNudgeWithStore(cityPath, beads.NudgesStore{Store: mem}, item); err != nil {
			t.Fatal(err)
		}
	}
	if err := enqueueQueuedNudgeWithStore(cityPath, beads.NudgesStore{Store: wrapped}, item); err != nil {
		t.Fatal(err)
	}
	state, err := nudgequeue.LoadState(cityPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Pending) != 1 || state.Pending[0].BeadID == loserID {
		t.Fatalf("duplicate did not preserve winner's private shadow: %+v", state)
	}
	winner, err := mem.Get(state.Pending[0].BeadID)
	if err != nil || winner.Status != "open" {
		t.Fatalf("dedupe closed winner: %+v, %v", winner, err)
	}
	loser, err := mem.Get(loserID)
	if err != nil || loser.Status != "closed" || loser.Metadata["close_reason"] != nudgequeue.EnqueueRollbackCloseReason {
		t.Fatalf("dedupe leaked losing shadow: %+v, %v", loser, err)
	}
	// Once the winner is visible, an ordinary duplicate must allocate no
	// further artifacts, including closed rollback shadows.
	if err := enqueueQueuedNudgeWithStore(cityPath, beads.NudgesStore{Store: mem}, item); err != nil {
		t.Fatal(err)
	}
	shadows, err := mem.List(beads.ListQuery{Label: nudgeBeadLabel, IncludeClosed: true})
	if err != nil || len(shadows) != 2 {
		t.Fatalf("ordinary duplicate allocated another shadow: %+v, %v", shadows, err)
	}
}

func TestEnqueueSlowCreateStillSupersedesPendingAndInFlight(t *testing.T) {
	cityPath := t.TempDir()
	now := time.Now().UTC()
	clk := &clock.Fake{Time: now}
	ref := &nudgequeue.Reference{Kind: "bead", ID: "work"}
	pending := newQueuedNudgeWithOptions("worker", "old pending", "sling", now, queuedNudgeOptions{ID: "old-pending", Reference: ref})
	inflight := newQueuedNudgeWithOptions("worker", "old flight", "sling", now, queuedNudgeOptions{ID: "old-flight", Reference: ref})
	inflight.ClaimedAt = now
	inflight.LeaseUntil = now.Add(time.Hour)
	if err := nudgequeue.WithState(cityPath, func(state *nudgeQueueState) error {
		state.Pending = []queuedNudge{pending}
		state.InFlight = []queuedNudge{inflight}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	store := &afterLookupNudgeStore{Store: beads.NewMemStore(), afterCreate: func(beads.Bead) { clk.Advance(3 * time.Second) }}
	replacement := newQueuedNudgeWithOptions("worker", "replacement", "sling", now, queuedNudgeOptions{ID: "new", Reference: ref})
	if err := enqueueQueuedNudgeWithStoreAndClock(cityPath, beads.NudgesStore{Store: store}, replacement, clk); err != nil {
		t.Fatal(err)
	}
	state, err := nudgequeue.LoadState(cityPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Pending) != 1 || state.Pending[0].ID != replacement.ID || len(state.InFlight) != 0 {
		t.Fatalf("slow create left superseded reminders deliverable: %+v", state)
	}
	if len(state.Dead) != 2 {
		t.Fatalf("superseded records lost: %+v", state)
	}
	for _, old := range state.Dead {
		if old.LastError != "superseded" || !old.DeadAt.Equal(clk.Now()) {
			t.Fatalf("wrong supersession record/time: %+v", old)
		}
	}
}
