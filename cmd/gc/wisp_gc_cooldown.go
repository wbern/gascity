package main

import (
	"log"
	"sync"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/coordclass"
)

// orphanCooldownRegistry belongs to one CityRuntime, not to a replaceable GC
// tracker. A completion from an old tracker updates only its captured identity.
// Single-flight sweep admission already prevents overlapping arms; this lock
// protects the deadlines only and is never held across a store operation.
type orphanCooldownRegistry struct {
	mu        sync.Mutex
	deadlines map[beads.StoreIdentity]time.Time
}

func (r *orphanCooldownRegistry) remaining(key beads.StoreIdentity, now time.Time) time.Duration {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.deadlines[key].Sub(now)
}

func (r *orphanCooldownRegistry) complete(key beads.StoreIdentity, end time.Time, interval time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.deadlines == nil {
		r.deadlines = make(map[beads.StoreIdentity]time.Time)
	}
	r.deadlines[key] = end.Add(interval)
}

func (m *memoryWispGC) runOrphanArm(store beads.Store, cutoff time.Time) (int, error) {
	if reapOrphansEnforced() {
		return reapOrphanedClosedWisps(store, cutoff, wispGCReapOrphanBatchCap)
	}
	clock := m.orphanNow
	if clock == nil {
		clock = time.Now
	}
	registry, key, interval := m.orphanCooldown, m.orphanIdentity, m.interval
	known := registry != nil && key.CityPath != "" && key.ScopeRoot != "" && key.Provider != ""
	if known {
		if gap := registry.remaining(key, clock()); gap > 0 {
			log.Printf("wisp gc: orphan dry-run skipped: completion cooldown remaining=%s", gap)
			return 0, nil
		}
	} else {
		log.Printf("wisp gc: orphan cooldown unavailable: unknown graph store identity; running original dry-run arm")
	}
	start := clock()
	defer func() {
		end := clock()
		if known {
			registry.complete(key, end, interval)
		}
		log.Printf("wisp gc: orphan invocation completed start=%s end=%s", start.UTC().Format(time.RFC3339Nano), end.UTC().Format(time.RFC3339Nano))
	}()
	return reapOrphanedClosedWisps(store, cutoff, wispGCReapOrphanBatchCap)
}

func (cr *CityRuntime) standaloneStoreResult() beads.StoreOpenResult {
	cr.serviceStateMu.RLock()
	defer cr.serviceStateMu.RUnlock()
	return beads.StoreOpenResult{Store: cr.standaloneCityStore, Identity: cr.standaloneStoreIdentity}
}

// orphanGraphStore captures the actual graph handle and successful-open token
// together. Relocated routes do not currently expose an opened workspace token;
// preserve their original arm rather than inventing identity from a binding name.
func (cr *CityRuntime) orphanGraphStore() (beads.GraphStore, beads.StoreIdentity) {
	if store, relocated := cr.storageRoutes.storeFor(coordclass.ClassGraph); relocated { // residency:allow — snapshots the opened graph route to gate cooldown; resolves no bead
		return beads.GraphStore{Store: store}, beads.StoreIdentity{}
	}
	if cr.cs != nil {
		cr.cs.mu.RLock()
		defer cr.cs.mu.RUnlock()
		return beads.GraphStore{Store: cr.cs.cityBeadStore}, cr.cs.cityStoreIdentity
	}
	opened := cr.standaloneStoreResult()
	return beads.GraphStore{Store: opened.Store}, opened.Identity
}
