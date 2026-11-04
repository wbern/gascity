package main

import (
	"fmt"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
)

// deadBacklogID returns a zero-padded ID so that lexicographic order (the
// nudgequeue.SortState ID tiebreak, used when every item shares the same
// DeadAt/CreatedAt as seeded here) matches seed/index order 0..n-1.
func deadBacklogID(i int) string {
	return fmt.Sprintf("nudge-dead-%04d", i)
}

func seedDeadBacklog(t *testing.T, cityPath string, now time.Time, n int) map[string]string {
	t.Helper()
	buckets := make(map[string]string, n)
	if err := withNudgeQueueState(cityPath, func(state *nudgeQueueState) error {
		for i := 0; i < n; i++ {
			id := deadBacklogID(i)
			buckets[id] = "dead"
			state.Dead = append(state.Dead, queuedNudge{
				ID: id, BeadID: "bead-" + id,
				Agent: "gascity/deployer", Source: "sling", Message: "backlog",
				CreatedAt: now.Add(-2 * time.Hour), DeadAt: now.Add(-2 * time.Hour),
				LastError: "expired",
			})
		}
		return nil
	}); err != nil {
		t.Fatalf("seeding backlog: %v", err)
	}
	return buckets
}

type enqueueTiming struct {
	virtualElapsed time.Duration
	operations     int64
}

func timeEnqueue(t *testing.T, backlog int, latency time.Duration) enqueueTiming {
	t.Helper()
	cityPath := t.TempDir()
	fakeClock := &clock.Fake{Time: time.Now().UTC()}
	seededBuckets := seedDeadBacklog(t, cityPath, fakeClock.Now(), backlog)
	payNudgeDebtsWithTestClock(t, fakeClock)
	maxOps := int64(nudgeEnqueueMaintenanceBudget/latency) + advancingNudgeStoreSetupOps + advancingNudgeStoreDeadItemOps
	store := &advancingNudgeStore{fakeClock: fakeClock, latency: latency, failAfter: maxOps}
	item := queuedNudge{ID: "nudge-new", Agent: "gascity/deployer", Source: "sling", Message: "Work slung. Check your hook."}
	start := fakeClock.Now()
	if err := enqueueQueuedNudgeWithStoreAndClock(cityPath, beads.NudgesStore{Store: store}, item, fakeClock); err != nil {
		t.Fatalf("enqueue (backlog=%d): %v", backlog, err)
	}
	timing := enqueueTiming{
		virtualElapsed: fakeClock.Now().Sub(start),
		operations:     store.operations(),
	}
	if timing.operations <= advancingNudgeStoreSetupOps {
		t.Fatal("enqueue did not exercise any backlog payments")
	}
	if maxElapsed := nudgeEnqueueMaintenanceBudget + (advancingNudgeStoreSetupOps+advancingNudgeStoreDeadItemOps)*latency; timing.virtualElapsed > maxElapsed {
		t.Fatalf("virtual enqueue elapsed (backlog=%d) = %v, want at most %v", backlog, timing.virtualElapsed, maxElapsed)
	}
	if timing.operations > maxOps {
		t.Fatalf("advancing store ops (backlog=%d) = %d, want at most %d", backlog, timing.operations, maxOps)
	}

	buckets := nudgeQueueBucketsByID(t, cityPath)
	processed := 0
	for i := 0; i < backlog; i++ {
		id := deadBacklogID(i)
		bucket, present := buckets[id]
		closed := store.closed["bead-"+id]
		if closed {
			processed++
			if present {
				t.Fatalf("paid aged item %s remains in %s", id, bucket)
			}
		} else if bucket != seededBuckets[id] {
			t.Fatalf("unpaid item %s lost or moved: %q", id, bucket)
		}
	}
	if processed == 0 || processed >= backlog {
		t.Fatalf("processed %d of %d, want a nonempty bounded subset", processed, backlog)
	}
	if got, want := len(buckets), backlog-processed+1; got != want {
		t.Fatalf("queue has %d items, want %d; unpaid data was lost or duplicated", got, want)
	}
	if bucket := buckets[item.ID]; bucket != "pending" {
		t.Fatalf("new queued nudge bucket = %q, want pending; buckets=%v", bucket, buckets)
	}
	return timing
}

// The foreground `--nudge` enqueue must be bounded regardless of nudge-queue
// backlog. The advancing store makes the aggregate deadline observable without
// spending the maintenance budget in real time.
func TestSlingNudgeEnqueueBoundedByBacklog(t *testing.T) {
	for _, latency := range []time.Duration{20 * time.Millisecond, 100 * time.Millisecond} {
		t.Run(latency.String(), func(t *testing.T) {
			small := timeEnqueue(t, 40, latency)
			big := timeEnqueue(t, 160, latency)
			if small != big {
				t.Fatalf("shadow I/O grew with backlog at latency %s: 40=%+v 160=%+v", latency, small, big)
			}
			if fullBacklogOps := int64(advancingNudgeStoreSetupOps + advancingNudgeStoreDeadItemOps*40); small.operations >= fullBacklogOps {
				t.Fatalf("processed the whole backlog: %d ops", small.operations)
			}
		})
	}
}
