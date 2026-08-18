package beads

import (
	"encoding/json"
	"fmt"
	"sync"

	"github.com/gastownhall/gascity/internal/deps"
)

const bdReadyProjectionMinVersion = "1.0.5"

// ReadyProjectionCapabilityCache memoizes the `bd version` capability probe
// that gates ready-projection enrichment, keyed by store directory.
//
// The memo cannot usefully live on a BdStore. The control dispatcher opens a
// fresh store on every tick (makeSourceWorkflowStoresLister), so an
// instance-scoped memo is cold every time and the probe degenerates into one bd
// subprocess per reconcile — measured on the live gc2 city at ~20/min, 211
// seconds of wall clock per 50 minutes, re-reading a constant (gcw-clnxz).
// Sharing one cache across the stores a process opens collapses that to a
// single probe per directory.
//
// Keying by directory rather than using one process-wide answer is deliberate:
// workspacePinnedBdBinary resolves bd from the owning city's workspace PATH, so
// stores rooted in different places may legitimately drive different bd
// binaries and must not share a verdict.
//
// The zero value is not usable; construct with NewReadyProjectionCapabilityCache.
type ReadyProjectionCapabilityCache struct {
	mu      sync.Mutex
	enabled map[string]bool
}

// NewReadyProjectionCapabilityCache returns an empty capability cache ready to
// be shared by every BdStore a process opens.
func NewReadyProjectionCapabilityCache() *ReadyProjectionCapabilityCache {
	return &ReadyProjectionCapabilityCache{enabled: make(map[string]bool)}
}

// lookup reports the memoized verdict for dir, if one was recorded.
func (c *ReadyProjectionCapabilityCache) lookup(dir string) (bool, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	enabled, ok := c.enabled[dir]
	return enabled, ok
}

// record memoizes a successful probe verdict for dir. Probe FAILURES are
// deliberately never recorded, so a transient bd error is retried on the next
// call rather than latching ready enrichment off for the life of the process.
//
// The cache lock is not held across the probe subprocess, so concurrent first
// callers for the same directory may each probe once before either records.
// That is deliberate: the probe is idempotent and the duplicate is bounded by
// the number of racing callers, which is a far better trade than blocking every
// reconcile behind a ~142ms subprocess held under a lock.
func (c *ReadyProjectionCapabilityCache) record(dir string, enabled bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.enabled[dir] = enabled
}

type bdReadyProjectionRow struct {
	ID        string       `json:"id"`
	IsBlocked optionalBool `json:"is_blocked"`
}

func (s *BdStore) enrichReadyProjectionForCache(items []Bead) ([]Bead, error) {
	if len(items) == 0 {
		return items, nil
	}
	ids := make([]string, 0, len(items))
	seen := make(map[string]struct{}, len(items))
	for _, item := range items {
		// Message and nudge beads are notifications, not dependency-blocked ready
		// work, and bd's denormalized is_blocked column can flap NULL<->false for
		// them. Enriching those rows makes the CachingStore reconciler re-emit
		// bead.updated on every cycle (an event flood that starves gc-hook work
		// queries). Leave their IsBlocked at bd's nil fallback so the reconcile
		// diff converges.
		if skipBDReadyProjectionEnrichment(item) {
			continue
		}
		if _, ok := seen[item.ID]; ok {
			continue
		}
		seen[item.ID] = struct{}{}
		ids = append(ids, item.ID)
	}
	if len(ids) == 0 {
		return items, nil
	}
	enabled, err := s.bdReadyProjectionEnabled()
	if err != nil {
		return items, err
	}
	if !enabled {
		return items, nil
	}

	projection, err := s.fetchReadyProjection(ids)
	if err != nil {
		return items, err
	}
	enriched := make([]Bead, len(items))
	copy(enriched, items)
	for i := range enriched {
		if skipBDReadyProjectionEnrichment(enriched[i]) {
			continue
		}
		blocked, ok := projection[enriched[i].ID]
		if !ok {
			continue
		}
		enriched[i].IsBlocked = cloneBoolPtr(&blocked)
	}
	return enriched, nil
}

func skipBDReadyProjectionEnrichment(item Bead) bool {
	return item.ID == "" ||
		item.Status == "closed" ||
		item.IsBlocked != nil ||
		item.Type == "message" ||
		beadHasLabel(item, "gc:nudge")
}

func (s *BdStore) bdReadyProjectionEnabled() (bool, error) {
	// Probe the bd version once per capability-cache scope. Operators must
	// restart gc after changing bd versions to re-evaluate ready-projection
	// support. Stores opened through the city/rig factories share one cache for
	// the life of the process; a store built directly gets its own.
	if enabled, ok := s.readyProjectionCapability.lookup(s.dir); ok {
		return enabled, nil
	}
	out, err := s.runner(s.dir, "bd", "version")
	if err != nil {
		return false, fmt.Errorf("bd ready projection version gate: %w", err)
	}
	version, err := parseBDVersion(string(out))
	if err != nil {
		return false, fmt.Errorf("bd ready projection version gate: %w", err)
	}
	enabled := deps.CompareVersions(version, bdReadyProjectionMinVersion) >= 0
	s.readyProjectionCapability.record(s.dir, enabled)
	return enabled, nil
}

func (s *BdStore) fetchReadyProjection(ids []string) (map[string]bool, error) {
	result := make(map[string]bool, len(ids))
	wanted := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		if id != "" {
			wanted[id] = struct{}{}
		}
	}
	if len(wanted) == 0 {
		return result, nil
	}

	// bd exposes this as an active-row projection: the SQL filters out closed
	// rows so cache prime/reconcile cost stays O(active work) instead of
	// scanning unbounded closed issue/wisp history every cycle. The ids
	// argument is a cache-side allow-list so callers can keep their requested
	// surface bounded. A row that races closed between the list snapshot and
	// this fetch drops out of the projection; the reconciler preserves its last
	// cached is_blocked (preserveCachedReadyProjectionLocked) so the absence
	// does not flap a spurious bead.updated.
	out, err := s.runner(s.dir, "bd", "sql", readyProjectionSQL(), "--json")
	if err != nil {
		return nil, fmt.Errorf("bd sql ready projection: %w", err)
	}
	var rows []bdReadyProjectionRow
	if err := json.Unmarshal(extractJSON(out), &rows); err != nil {
		return nil, fmt.Errorf("bd sql ready projection: parsing JSON: %w", err)
	}
	for _, row := range rows {
		if row.ID == "" || !row.IsBlocked.set {
			continue
		}
		if _, ok := wanted[row.ID]; !ok {
			continue
		}
		result[row.ID] = row.IsBlocked.value
	}
	return result, nil
}

func readyProjectionSQL() string {
	return "select id,is_blocked from issues where status <> 'closed' union all select id,is_blocked from wisps where status <> 'closed'"
}
