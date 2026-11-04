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
		name := "unleased debt is replaced"
		if leased {
			name = "active payer blocks reuse"
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
			if leased {
				if !errors.Is(err, nudgequeue.ErrQueueBusy) {
					t.Fatalf("enqueue error=%v, want queue busy", err)
				}
				if len(state.Pending) != 0 || len(state.Retired) != 1 {
					t.Fatalf("busy enqueue changed state: %+v", state)
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				if len(state.Pending) != 1 || len(state.Retired) != 0 || !state.Pending[0].CreatedAt.Equal(item.CreatedAt) {
					t.Fatalf("replacement kept old debt or lost new item: %+v", state)
				}
			}
			bead, err := store.Get(shadow)
			if err != nil {
				t.Fatal(err)
			}
			if bead.Status != "open" {
				t.Fatalf("enqueue closed a serving or leased shadow: %+v", bead)
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
