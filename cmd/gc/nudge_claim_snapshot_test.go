package main

import (
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/nudgequeue"
)

func TestNudgeClaimOnlyTakesItemsValidatedByItsSnapshot(t *testing.T) {
	for _, changedEpoch := range []bool{false, true} {
		name := "new item waits for next census"
		if changedEpoch {
			name = "changed epoch waits for next census"
		}
		t.Run(name, func(t *testing.T) {
			cityPath := t.TempDir()
			now := time.Now()
			first := newQueuedNudge("worker", "first", now.Add(-time.Minute))
			second := newQueuedNudge("worker", "second", now.Add(-time.Minute))
			if err := nudgequeue.WithState(cityPath, func(state *nudgequeue.State) error { state.Pending = append(state.Pending, first); return nil }); err != nil {
				t.Fatal(err)
			}
			old := nudgeClaimAfterSnapshot
			t.Cleanup(func() { nudgeClaimAfterSnapshot = old })
			snapshotReached := false
			nudgeClaimAfterSnapshot = func() {
				snapshotReached = true
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
			claimed, err := claimDueQueuedNudgesForTarget(cityPath, nudgeTarget{cityPath: cityPath, agent: config.Agent{Name: "worker"}}, now)
			if err != nil {
				t.Fatal(err)
			}
			if !snapshotReached {
				t.Fatal("claim did not reach the snapshot boundary")
			}
			want := 1
			if changedEpoch {
				want = 0
			}
			if len(claimed) != want || (want == 1 && claimed[0].ID != first.ID) {
				t.Fatalf("claimed %+v, want only %d validated items", claimed, want)
			}
			state, err := nudgequeue.LoadState(cityPath)
			if err != nil {
				t.Fatal(err)
			}
			if len(state.Pending) != 2-want || len(state.InFlight) != want {
				t.Fatalf("pending/inflight=%d/%d, want %d/%d", len(state.Pending), len(state.InFlight), 2-want, want)
			}
			foundSecond := false
			for _, item := range state.Pending {
				if item.ID == second.ID {
					foundSecond = true
				}
			}
			if !foundSecond {
				t.Fatal("new item disappeared from pending before its census")
			}
		})
	}
}

func TestNudgeClaimSnapshotRequiresACompleteFenceCensus(t *testing.T) {
	cityPath := t.TempDir()
	now := time.Now()
	item := newQueuedNudge("worker", "fenced reminder", now.Add(-time.Minute))
	item.SessionID = "old-session"
	if err := nudgequeue.WithState(cityPath, func(state *nudgequeue.State) error { state.Pending = []queuedNudge{item}; return nil }); err != nil {
		t.Fatal(err)
	}
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
			censusRead := false
			eligible, err := claimableNudgeSnapshotWithCensus(cityPath, nudgeTarget{cityPath: cityPath, sessionID: "new-session", agent: config.Agent{Name: "worker"}}, now, func() map[string]struct{} {
				censusRead = true
				if err := nudgequeue.WithStateWithin(cityPath, "census-probe", 0, func(*nudgequeue.State) error { return nil }); err != nil {
					t.Fatalf("census ran under queue lock: %v", err)
				}
				return tc.live
			})
			if err != nil {
				t.Fatal(err)
			}
			if !censusRead {
				t.Fatal("fenced candidate did not read its census")
			}
			if got := len(eligible) == 1; got != tc.want {
				t.Fatalf("candidate eligible=%v want %v", got, tc.want)
			}
		})
	}
}
