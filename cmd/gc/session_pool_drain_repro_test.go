package main

import (
	"context"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
)

// Fork reproduction for upstream #5995 (gcw-a9yvf): a desired pool seat parked
// in unfinalized drain (sleep_reason=drained, empty last_woke_at) with no
// assigned work stays open indefinitely and keeps holding its pool name.
func TestForkRepro5995_StuckDrainedPoolSeatHoldsSlotForever(t *testing.T) {
	const seatName = "worker-1-pool"
	env := newReconcilerTestEnv()
	env.cfg = &config.City{Agents: []config.Agent{{Name: "worker", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(2)}}}
	env.addDesired(seatName, "worker", false)
	seat := env.createSessionBead(seatName, "worker")
	env.setSessionMetadata(&seat, map[string]string{
		"state": "drained", "sleep_reason": "drained", "last_woke_at": "", "pool_slot": "1",
		"pool_managed": "true", "session_origin": "ephemeral",
		"drain_at": env.clk.Now().Add(-time.Minute).UTC().Format(time.RFC3339),
	})
	if err := env.sp.Start(context.Background(), seatName, runtime.Config{Command: "test-cmd"}); err != nil {
		t.Fatal(err)
	}
	// 15 one-minute ticks, then a jump well past a 30m bound.
	for i := 0; i < 16; i++ {
		cur, err := env.store.Get(seat.ID)
		if err != nil {
			t.Fatal(err)
		}
		env.reconcile([]beads.Bead{cur})
		if i == 14 {
			env.clk.Time = env.clk.Time.Add(31 * time.Minute)
		} else {
			env.clk.Time = env.clk.Time.Add(time.Minute)
		}
	}
	got, err := env.store.Get(seat.ID)
	if err != nil {
		t.Fatal(err)
	}
	snap, err := loadSessionBeadSnapshot(env.store)
	if err != nil {
		t.Fatal(err)
	}
	nameErr := ensurePoolSessionNameAvailable(env.store, env.cfg, snap, seatName, "worker-1")
	t.Logf("after ~46m of drain: status=%s state=%s running=%v providerCalls=%v nameAvailableErr=%v", got.Status, got.Metadata["state"], env.sp.IsRunning(seatName), len(env.sp.Calls), nameErr)
	if got.Status != "closed" || nameErr != nil {
		t.Fatalf("stuck drained pool seat never retired: status=%s state=%s name still held: %v", got.Status, got.Metadata["state"], nameErr)
	}
}
