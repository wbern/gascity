package main

import (
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/nudgequeue"
)

const (
	advancingNudgeStoreSetupOps = 1
	// advancingNudgeStoreDeadItemOps is the store-op cost of repairing one dead
	// backlog item during a maintenance sweep: FindIncludingTerminal (List) to
	// check terminal state, then Terminalize (SetMetadataBatch + Close) to repair
	// it. Terminalize's own nil return is sufficient confirmation (see
	// pruneDeadQueuedNudgesWithClock, gastownhall/gascity#5278), so there is no
	// second FindIncludingTerminal call to account for here.
	advancingNudgeStoreDeadItemOps = 3
)

type advancingNudgeStore struct {
	beads.Store
	fakeClock *clock.Fake
	latency   time.Duration
	failAfter int64
	ops       int64
	closed    map[string]bool
}

func (s *advancingNudgeStore) tick() error {
	op := atomic.AddInt64(&s.ops, 1)
	s.fakeClock.Advance(s.latency)
	if s.failAfter > 0 && op > s.failAfter {
		return fmt.Errorf("advancing nudge store operation %d exceeded test budget %d", op, s.failAfter)
	}
	return nil
}

func (s *advancingNudgeStore) operations() int64 {
	return atomic.LoadInt64(&s.ops)
}

func (s *advancingNudgeStore) List(beads.ListQuery) ([]beads.Bead, error) {
	if err := s.tick(); err != nil {
		return nil, err
	}
	return []beads.Bead{{
		ID:       "shadow-open",
		Type:     nudgeBeadType,
		Status:   "open",
		Labels:   []string{nudgeBeadLabel},
		Metadata: map[string]string{"state": "queued"},
	}}, nil
}

func (s *advancingNudgeStore) Create(b beads.Bead) (beads.Bead, error) {
	if err := s.tick(); err != nil {
		return beads.Bead{}, err
	}
	if b.ID == "" {
		b.ID = "created-shadow"
	}
	b.Status = "open"
	return b, nil
}

func (s *advancingNudgeStore) Get(id string) (beads.Bead, error) {
	if err := s.tick(); err != nil {
		return beads.Bead{}, err
	}
	return beads.Bead{ID: id, Type: nudgeBeadType, Status: "open", Metadata: map[string]string{"state": "queued"}}, nil
}

func (s *advancingNudgeStore) Close(id string) error {
	if err := s.tick(); err != nil {
		return err
	}
	if s.closed == nil {
		s.closed = make(map[string]bool)
	}
	s.closed[id] = true
	return nil
}

func (s *advancingNudgeStore) SetMetadata(string, string, string) error {
	return s.tick()
}

func (s *advancingNudgeStore) SetMetadataBatch(string, map[string]string) error {
	return s.tick()
}

func seedNudgeBudgetPreservationBacklog(t *testing.T, cityPath string, now time.Time, reference *nudgeReference, deadCount int) map[string]string {
	t.Helper()
	now = now.UTC()
	buckets := make(map[string]string, deadCount+4)
	if err := withNudgeQueueState(cityPath, func(state *nudgeQueueState) error {
		for i := 0; i < 2; i++ {
			id := fmt.Sprintf("nudge-pending-preserve-%d", i)
			buckets[id] = "pending"
			state.Pending = append(state.Pending, queuedNudge{
				ID:           id,
				BeadID:       "bead-" + id,
				Agent:        "gascity/deployer",
				Source:       "sling",
				Message:      "pending supersede tail",
				Reference:    reference,
				CreatedAt:    now.Add(-time.Hour),
				DeliverAfter: now.Add(time.Hour),
				ExpiresAt:    now.Add(time.Hour),
			})
		}
		for i := 0; i < 2; i++ {
			id := fmt.Sprintf("nudge-in-flight-preserve-%d", i)
			buckets[id] = "in-flight"
			state.InFlight = append(state.InFlight, queuedNudge{
				ID:           id,
				BeadID:       "bead-" + id,
				Agent:        "gascity/deployer",
				Source:       "sling",
				Message:      "in-flight supersede tail",
				Reference:    reference,
				CreatedAt:    now.Add(-time.Hour),
				DeliverAfter: now.Add(-time.Minute),
				ExpiresAt:    now.Add(time.Hour),
				ClaimedAt:    now.Add(-time.Minute),
				LeaseUntil:   now.Add(time.Hour),
			})
		}
		for i := 0; i < deadCount; i++ {
			id := fmt.Sprintf("nudge-dead-preserve-%03d", i)
			buckets[id] = "dead"
			state.Dead = append(state.Dead, queuedNudge{
				ID:        id,
				BeadID:    "bead-" + id,
				Agent:     "gascity/deployer",
				Source:    "sling",
				Message:   "dead backlog",
				CreatedAt: now.Add(-2 * time.Hour),
				DeadAt:    now.Add(-2 * time.Hour),
				LastError: "expired",
			})
		}
		return nil
	}); err != nil {
		t.Fatalf("seeding nudge backlog: %v", err)
	}
	return buckets
}

func nudgeQueueBucketsByID(t *testing.T, cityPath string) map[string]string {
	t.Helper()
	state, err := nudgequeue.LoadState(cityPath)
	if err != nil {
		t.Fatal(err)
	}
	buckets := make(map[string]string)
	add := func(id, bucket string) {
		if previous, exists := buckets[id]; exists {
			t.Fatalf("duplicate queued ID %s occurs in %s and %s", id, previous, bucket)
		}
		buckets[id] = bucket
	}
	for _, item := range state.Pending {
		add(item.ID, "pending")
	}
	for _, item := range state.InFlight {
		add(item.ID, "in-flight")
	}
	for _, item := range state.Dead {
		add(item.ID, "dead")
	}
	return buckets
}

// payNudgeDebtsWithTestClock uses the production payer and confirmation phase,
// with virtual store latency so the cooperative I/O budget is observable.
func payNudgeDebtsWithTestClock(t *testing.T, clk clock.Clock) {
	t.Helper()
	previous := settleNudgeShadowDebts
	settleNudgeShadowDebts = func(cityPath string, store beads.NudgesStore, debts []queuedNudge, budget time.Duration, now time.Time) {
		payments := payNudgeShadowDebtsWithClock(nudgeFrontDoor(store), debts, clk.Now().Add(budget), clk)
		if err := confirmNudgeShadowDebts(cityPath, payments, now); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { settleNudgeShadowDebts = previous })
}

type advancingReadClock struct {
	base *clock.Fake
	step time.Duration
}

func (c advancingReadClock) Now() time.Time { now := c.base.Now(); c.base.Advance(c.step); return now }

// Exhaust the pure callback budget partway through dead-letter maintenance.
// Pausing payment at the durable debt boundary exposes the processed prefix
// and untouched tail, while the later supersede loop must preserve its inputs.
func TestSlingNudgeEnqueueBudgetPreservesQueuedItems(t *testing.T) {
	const (
		deadBacklog = 160
		latency     = 40 * time.Millisecond
	)
	reference := &nudgeReference{Kind: "bead", ID: "ga-budget-preservation"}
	cityPath := t.TempDir()
	fakeClock := &clock.Fake{Time: time.Now().UTC()}
	seededBuckets := seedNudgeBudgetPreservationBacklog(t, cityPath, fakeClock.Now(), reference, deadBacklog)
	maxOps := int64(nudgeEnqueueMaintenanceBudget/latency) + advancingNudgeStoreSetupOps + advancingNudgeStoreDeadItemOps
	store := &advancingNudgeStore{fakeClock: fakeClock, latency: latency, failAfter: maxOps}
	item := newQueuedNudgeWithOptions("gascity/deployer", "Work slung. Check your hook.", "sling", fakeClock.Now(), queuedNudgeOptions{
		ID:        "nudge-new-preservation",
		Reference: reference,
	})

	previousPayer := settleNudgeShadowDebts
	taken := 0
	settleNudgeShadowDebts = func(_ string, _ beads.NudgesStore, debts []queuedNudge, _ time.Duration, _ time.Time) {
		taken += len(debts)
	}
	t.Cleanup(func() { settleNudgeShadowDebts = previousPayer })
	exhausted := advancingReadClock{base: fakeClock, step: 100 * time.Millisecond}
	if err := enqueueQueuedNudgeWithStoreAndClock(cityPath, beads.NudgesStore{Store: store}, item, exhausted); err != nil {
		t.Fatal(err)
	}
	if ops := store.operations(); ops != advancingNudgeStoreSetupOps {
		t.Fatalf("exhausted maintenance made %d store calls, want only setup", ops)
	}
	state, err := nudgequeue.LoadState(cityPath)
	if err != nil {
		t.Fatal(err)
	}
	byID := make(map[string]queuedNudge, len(state.Dead))
	for _, dead := range state.Dead {
		byID[dead.ID] = dead
	}
	processed := 0
	reachedTail := false
	for i := 0; i < deadBacklog; i++ {
		id := fmt.Sprintf("nudge-dead-preserve-%03d", i)
		dead, exists := byID[id]
		if !exists {
			t.Fatalf("unpaid dead item %s disappeared", id)
		}
		if dead.Shadow == nil {
			reachedTail = true
			continue
		}
		if reachedTail || !dead.Shadow.Verify || dead.Shadow.Paid {
			t.Fatalf("invalid processed prefix at %s: %+v", id, dead.Shadow)
		}
		processed++
	}
	if processed == 0 || processed >= deadBacklog || taken == 0 {
		t.Fatalf("processed=%d taken=%d, want a nonempty prefix and untouched tail", processed, taken)
	}

	buckets := nudgeQueueBucketsByID(t, cityPath)
	if len(buckets) != len(seededBuckets)+1 {
		t.Fatalf("queue lost or duplicated unprocessed items: %d, want %d", len(buckets), len(seededBuckets)+1)
	}
	for id, want := range seededBuckets {
		if got := buckets[id]; got != want {
			t.Fatalf("unprocessed %s moved from %s to %s", id, want, got)
		}
	}
	if buckets[item.ID] != "pending" {
		t.Fatalf("new item not queued: %v", buckets)
	}
}

// TestSlingNudgeEnqueueEmptyBacklogFast pins that an empty queue still
// enqueues near-instantly: none of the three maintenance loops iterate, so
// the deadline check never fires regardless of nudgeEnqueueMaintenanceBudget.
func TestSlingNudgeEnqueueEmptyBacklogFast(t *testing.T) {
	cityPath := t.TempDir()
	store := &advancingNudgeStore{fakeClock: &clock.Fake{Time: time.Now().UTC()}, failAfter: 4}
	item := newQueuedNudgeWithOptions("gascity/deployer", "Work slung. Check your hook.", "sling", time.Now(), queuedNudgeOptions{
		ID: "nudge-empty-backlog",
	})

	start := time.Now()
	if err := enqueueQueuedNudgeWithStore(cityPath, beads.NudgesStore{Store: store}, item); err != nil {
		t.Fatalf("enqueueQueuedNudgeWithStore: %v", err)
	}
	elapsed := time.Since(start)
	if elapsed > 500*time.Millisecond {
		t.Fatalf("empty-backlog enqueue elapsed = %v, want under 500ms", elapsed.Round(time.Millisecond))
	}
	if ops := store.operations(); ops > 2 {
		t.Fatalf("advancing store ops = %d, want at most backing-bead setup ops for an empty backlog", ops)
	}

	buckets := nudgeQueueBucketsByID(t, cityPath)
	if got := buckets[item.ID]; got != "pending" {
		t.Fatalf("new queued nudge bucket = %q, want pending; buckets=%v", got, buckets)
	}
	if got, want := len(buckets), 1; got != want {
		t.Fatalf("queued item count = %d, want %d; buckets=%v", got, want, buckets)
	}
}
