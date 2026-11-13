package main

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/nudgequeue"
)

// A claim decides on a lock-free dry run and then runs again under the lock,
// whose answer is authoritative (nudgeClaimAfterDryRun). The queue may move in
// between. The continuation epoch is not a claim gate: a matching session
// fence is claimable including a stale epoch (queuedNudgeClaimableForTarget,
// Leg A), and the delivery-time check judges the epoch. What the claim must
// never do is act on the dry run's copy of a nudge, so an epoch that changed
// after the dry run reaches the claimed item and its in-flight copy as the
// current value, and no nudge is lost or duplicated. A fence that changed is
// covered by TestNudgeClaimCensusJudgesOnlyFencesItsSnapshotHeld.
// PROVISIONAL (T7): the fork's earlier test deferred a re-epoched nudge to the
// next census; upstream's locked run claims it with the current epoch.
func TestNudgeClaimCarriesTheCurrentEpochOfAnItemChangedAfterTheDryRun(t *testing.T) {
	for _, changedEpoch := range []bool{false, true} {
		name := "unchanged epoch"
		if changedEpoch {
			name = "epoch changed after the dry run"
		}
		t.Run(name, func(t *testing.T) {
			cityPath := t.TempDir()
			now := time.Now()
			first := newQueuedNudge("worker", "first", now.Add(-time.Minute))
			first.SessionID = "session-1"
			first.ContinuationEpoch = "old-continuation"
			second := newQueuedNudge("worker", "second", now.Add(-time.Minute))
			second.SessionID = "session-1"
			second.ContinuationEpoch = "old-continuation"
			if err := nudgequeue.WithState(cityPath, func(state *nudgequeue.State) error { state.Pending = append(state.Pending, first); return nil }); err != nil {
				t.Fatal(err)
			}
			reached := false
			nudgeClaimAfterDryRun = func() {
				nudgeClaimAfterDryRun = nil
				reached = true
				if err := nudgequeue.WithState(cityPath, func(state *nudgequeue.State) error {
					state.Pending = append(state.Pending, second)
					if changedEpoch {
						state.Pending[0].ContinuationEpoch = "new-continuation"
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			}
			t.Cleanup(func() { nudgeClaimAfterDryRun = nil })
			target := nudgeTarget{cityPath: cityPath, sessionID: "session-1", continuationEpoch: "old-continuation", agent: config.Agent{Name: "worker"}}
			claimed, err := claimDueQueuedNudgesForTarget(cityPath, target, now)
			if err != nil {
				t.Fatal(err)
			}
			if !reached {
				t.Fatal("claim did not reach the dry-run boundary")
			}
			state, err := nudgequeue.LoadState(cityPath)
			if err != nil {
				t.Fatal(err)
			}
			wantEpoch := "old-continuation"
			if changedEpoch {
				wantEpoch = "new-continuation"
			}
			find := func(id string, items []queuedNudge) (queuedNudge, bool) {
				for _, item := range items {
					if item.ID == id {
						return item, true
					}
				}
				return queuedNudge{}, false
			}
			// No nudge is lost or duplicated, whatever the locked run decides.
			for _, item := range []queuedNudge{first, second} {
				_, pending := find(item.ID, state.Pending)
				_, inFlight := find(item.ID, state.InFlight)
				if btoi(pending)+btoi(inFlight) != 1 {
					t.Fatalf("nudge %s held pending=%v inFlight=%v, want exactly once", item.ID, pending, inFlight)
				}
			}
			got, ok := find(first.ID, claimed)
			if !ok {
				t.Fatalf("the first nudge was not claimed: claimed=%+v", claimed)
			}
			if got.ContinuationEpoch != wantEpoch {
				t.Fatalf("claimed copy epoch = %q, want the current %q (not the dry run's copy)", got.ContinuationEpoch, wantEpoch)
			}
			if held, ok := find(first.ID, state.InFlight); !ok || held.ContinuationEpoch != wantEpoch {
				t.Fatalf("in-flight copy = %+v (found %v), want epoch %q", held, ok, wantEpoch)
			}
		})
	}
}

func btoi(b bool) int {
	if b {
		return 1
	}
	return 0
}

// A fenced nudge's census is read outside the queue lock, and only a census
// that proves the old owner gone and the target live adopts it.
func TestNudgeClaimSnapshotRequiresACompleteFenceCensus(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		name string
		live map[string]struct{}
		want bool
	}{
		{"unavailable", nil, false},
		{"target unproven", map[string]struct{}{}, false},
		{"old owner still live", map[string]struct{}{"new-session": {}, "old-session": {}}, false},
		{"retired owner and live target", map[string]struct{}{"new-session": {}}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cityPath := t.TempDir()
			item := newQueuedNudge("worker", "fenced reminder", now.Add(-time.Minute))
			item.SessionID = "old-session"
			if err := nudgequeue.WithState(cityPath, func(state *nudgequeue.State) error { state.Pending = []queuedNudge{item}; return nil }); err != nil {
				t.Fatal(err)
			}
			censusRead := false
			target := nudgeTarget{cityPath: cityPath, sessionID: "new-session", agent: config.Agent{Name: "worker"}}
			target.fenceCensus = func() (map[string]struct{}, bool) {
				censusRead = true
				if err := probeNudgeQueueLockFree(cityPath); err != nil {
					t.Fatalf("census ran under queue lock: %v", err)
				}
				return tc.live, tc.live != nil
			}
			claimed, err := claimDueQueuedNudgesForTarget(cityPath, target, now)
			if err != nil {
				t.Fatal(err)
			}
			if !censusRead {
				t.Fatal("fenced candidate did not read its census")
			}
			if got := len(claimed) == 1; got != tc.want {
				t.Fatalf("candidate claimed=%v want %v (claimed=%+v)", got, tc.want, claimed)
			}
		})
	}
}

// probeNudgeQueueLockFree fails when the queue's exclusive lock is held: a
// non-blocking attempt to take it must succeed immediately.
func probeNudgeQueueLockFree(cityPath string) error {
	path := nudgequeue.LockPath(cityPath)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return err
	}
	defer f.Close() //nolint:errcheck
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return err
	}
	return syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
}
