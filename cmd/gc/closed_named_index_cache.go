package main

import (
	"reflect"
	"sync"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/session"
)

// closedNamedIndexTTL bounds how long a built closed named session index is
// reused. The index lists every session bead including closed ones, which the
// caching store cannot serve (IncludeClosed lists go to the backing store), so
// rebuilding it on every desired-state pass put a full scan of all session
// beads on Dolt several times a minute (gcw-7gn7c). The only consequence of a
// stale index is that a phantom closed within the TTL has its runtime-name
// assignee form enumerated up to one TTL later.
const closedNamedIndexTTL = 2 * time.Minute

// closedNamedIndexCacheMaxStores caps the number of stores tracked. A
// supervisor serves a handful of cities; exceeding the cap (only in tests that
// build many stores) drops every entry rather than growing without bound.
const closedNamedIndexCacheMaxStores = 32

// closedNamedIndexNow is the cache clock; tests replace it.
var closedNamedIndexNow = time.Now

// closedNamedIndexBuild builds the index; tests may replace it.
var closedNamedIndexBuild = session.BuildClosedNamedSessionBeadIndex

type closedNamedIndexEntry struct {
	index   session.ClosedNamedSessionBeadIndex
	builtAt time.Time
}

// closedNamedIndexStoreCache reuses a closed named session index per store
// for closedNamedIndexTTL. Only complete builds are cached: a hard failure or
// a partial read is returned as is and rebuilt on the next call.
type closedNamedIndexStoreCache struct {
	mu      sync.Mutex
	entries map[beads.Store]closedNamedIndexEntry
}

var closedNamedIndexCache = &closedNamedIndexStoreCache{}

// get returns the cached index for store when it is younger than
// closedNamedIndexTTL, otherwise builds, caches (when complete) and returns it.
func (c *closedNamedIndexStoreCache) get(store beads.Store) (session.ClosedNamedSessionBeadIndex, error) {
	if store == nil || !reflect.TypeOf(store).Comparable() {
		// A non-comparable store cannot key the map; build uncached rather
		// than panic.
		return closedNamedIndexBuild(store)
	}
	now := closedNamedIndexNow()
	c.mu.Lock()
	entry, ok := c.entries[store]
	c.mu.Unlock()
	if ok && now.Sub(entry.builtAt) < closedNamedIndexTTL {
		return entry.index, nil
	}

	index, err := closedNamedIndexBuild(store)
	if err != nil {
		return index, err
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil || len(c.entries) >= closedNamedIndexCacheMaxStores {
		c.entries = make(map[beads.Store]closedNamedIndexEntry)
	}
	c.entries[store] = closedNamedIndexEntry{index: index, builtAt: now}
	return index, nil
}

// reset drops every cached index.
func (c *closedNamedIndexStoreCache) reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries = nil
}
