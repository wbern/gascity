package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

// delayReadyStore decorates a Store so the ready read the handler performs
// (beads.HandlesFor(store).Live.Ready()) sleeps, letting a test observe whether
// the /beads/ready federation reads its stores sequentially (elapsed ≈ sum of
// delays) or concurrently (elapsed ≈ max delay). It implements Handles()
// explicitly so HandlesFor routes .Live.Ready() straight to the sleeping reader.
type delayReadyStore struct {
	beads.Store
	delay time.Duration
	ready []beads.Bead
}

func (d *delayReadyStore) Handles() beads.StoreHandles {
	r := delayLiveReader{delay: d.delay, ready: d.ready, base: d.Store}
	return beads.StoreHandles{Live: r, Cached: r, Writer: d.Store}
}

// delayLiveReader is delayReadyStore's LiveReader/CachedReader whose Ready sleeps.
type delayLiveReader struct {
	delay time.Duration
	ready []beads.Bead
	base  beads.Store
}

func (r delayLiveReader) Ready(_ ...beads.ReadyQuery) ([]beads.Bead, error) {
	time.Sleep(r.delay)
	return r.ready, nil
}
func (r delayLiveReader) Get(id string) (beads.Bead, error)            { return r.base.Get(id) }
func (r delayLiveReader) List(q beads.ListQuery) ([]beads.Bead, error) { return r.base.List(q) }
func (r delayLiveReader) DepList(id, dir string) ([]beads.Dep, error)  { return r.base.DepList(id, dir) }

// withReadyFederationParallelism sets the opt-in federation concurrency (and its
// semaphore) for one test, restoring the previous values on cleanup.
func withReadyFederationParallelism(t *testing.T, n int) {
	t.Helper()
	prevN, prevSem := readyFederationParallelism, readyFederationSem
	readyFederationParallelism = n
	readyFederationSem = make(chan struct{}, n)
	t.Cleanup(func() {
		readyFederationParallelism = prevN
		readyFederationSem = prevSem
	})
}

// TestBeadReadyFederatesSequentiallyByDefault confirms the opt-in default: with
// the flag off (parallelism 1), the federation runs its per-store reads one at a
// time (elapsed ≈ sum of delays), i.e. unchanged from the pre-parallel behavior.
func TestBeadReadyFederatesSequentiallyByDefault(t *testing.T) {
	withReadyFederationParallelism(t, 1) // explicit: default off
	const delay = 40 * time.Millisecond

	state := newFakeState(t)
	delete(state.stores, "myrig") // drop the harness's pre-seeded empty store
	state.cityBeadStore = &delayReadyStore{Store: beads.NewMemStore(), delay: delay, ready: []beads.Bead{{ID: "city-1"}}}
	state.stores["rig0"] = &delayReadyStore{Store: beads.NewMemStore(), delay: delay, ready: []beads.Bead{{ID: "rig0-1"}}}
	state.stores["rig1"] = &delayReadyStore{Store: beads.NewMemStore(), delay: delay, ready: []beads.Bead{{ID: "rig1-1"}}}

	h := newTestCityHandler(t, state)
	req := httptest.NewRequest("GET", cityURL(state, "/beads/ready"), nil)
	rec := httptest.NewRecorder()
	start := time.Now()
	h.ServeHTTP(rec, req)
	elapsed := time.Since(start)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	// 3 stores × delay run sequentially → elapsed ≥ ~3×delay. Assert ≥ 2×delay
	// (well above the concurrent ~1×delay) to prove the default is sequential.
	if elapsed < 2*delay {
		t.Fatalf("sequential default took %v; expected ≥ %v (3 stores × %v serialized)", elapsed, 2*delay, delay)
	}
}

// TestBeadReadyFederatesConcurrentlyWhenEnabled proves the opt-in flag makes the
// federation read its stores in parallel: with N stores each sleeping `delay`,
// the concurrent path finishes in ~delay, well under the sequential ~N×delay, and
// every store's ready bead still appears.
func TestBeadReadyFederatesConcurrentlyWhenEnabled(t *testing.T) {
	const nStores = 4
	withReadyFederationParallelism(t, nStores) // opt in, cap fits all stores
	const delay = 100 * time.Millisecond

	state := newFakeState(t)
	delete(state.stores, "myrig")
	state.cityBeadStore = &delayReadyStore{Store: beads.NewMemStore(), delay: delay, ready: []beads.Bead{{ID: "city-1"}}}
	for i := 0; i < nStores-1; i++ {
		state.stores[fmt.Sprintf("rig%d", i)] = &delayReadyStore{
			Store: beads.NewMemStore(),
			delay: delay,
			ready: []beads.Bead{{ID: fmt.Sprintf("rig%d-1", i)}},
		}
	}

	h := newTestCityHandler(t, state)
	req := httptest.NewRequest("GET", cityURL(state, "/beads/ready"), nil)
	rec := httptest.NewRecorder()
	start := time.Now()
	h.ServeHTTP(rec, req)
	elapsed := time.Since(start)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	// Sequential would be ≥ nStores×delay; concurrent ≈ delay. Assert well under
	// the sequential floor (with margin for scheduling on a busy CI box).
	if elapsed >= (nStores-1)*delay {
		t.Fatalf("federation took %v; concurrent should finish in ~%v (sequential floor %v)", elapsed, delay, nStores*delay)
	}

	var resp struct {
		Items []beads.Bead `json:"items"`
		Total int          `json:"total"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Total != nStores {
		t.Fatalf("total = %d, want %d (every store's ready bead should survive parallel federation)", resp.Total, nStores)
	}
}
