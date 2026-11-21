package main

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"log"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/gastownhall/gascity/internal/beads/contract"
)

// The control dispatcher's --follow loop re-scans its ready queue on every
// wake: at most every workflowServeMaxIdleSleep when nothing happens, because a
// raw `bd close` publishes no event the loop could wake on. Each scan of a bd
// scope forks `bd ready --json --include-ephemeral --limit 0`, which opens a
// fresh Dolt connection, probes the schema and reads the whole ready frontier
// -- on an idle city, to learn again that nothing is ready.
//
// The idle gate removes that fork when it provably cannot change the answer.
// Before a scan it reads the scope database's content hash (DOLT_HASHOF_DB(),
// which covers the working set, committed transactions and dolt_ignore'd
// tables alike -- pinned against a real server by the integration tests in
// dispatch_control_ready_idle_gate_dolt_integration_test.go) over the
// process's pooled connection. A live scan that came back EMPTY is remembered
// with the hash taken before it; the next scan whose hash still matches is
// answered empty without running. Any write to the database -- a raw bd
// close, a gc write, another dispatcher -- changes the hash, so the very next
// tick scans exactly as it does today: graph-hop latency is unchanged. The
// gate never answers for a scope whose last scan returned work, errored, was
// served from a snapshot instead of a live read, or whose hash it cannot read,
// and a skipped scope is still fully re-scanned every
// controlReadyIdleRescanInterval so readiness inputs outside the database (a
// defer_until passing, a dependency in another database) keep a hard bound.

// controlReadyIdleRescanInterval bounds how long the gate answers a scope
// from an unchanged hash before forcing a full scan anyway.
const controlReadyIdleRescanInterval = 30 * time.Second

// controlReadyVersionTargetTTL bounds how long a resolved (or refused) Dolt
// target is reused. Nothing cheap announces a scope's config or a managed
// port change, so the resolution is re-derived from config at most this often;
// it shares the rescan bound so a stale target can hide nothing for longer
// than the forced rescan already allows.
const controlReadyVersionTargetTTL = controlReadyIdleRescanInterval

// controlReadyIdleProbeTimeout bounds the hash read; a slow probe falls back
// to the scan rather than delaying it.
const controlReadyIdleProbeTimeout = 2 * time.Second

// controlReadyStoreVersionFn returns an opaque content version for the
// database the scope's control-ready scan reads. ok=false means the scope
// cannot be versioned (not a directly reachable managed Dolt server, a
// relocated graph class, a probe failure) and the scan must run.
var controlReadyStoreVersionFn = controlReadyDoltStoreVersion

type controlReadyIdleEntry struct {
	key       string
	version   string
	scannedAt time.Time
}

var controlReadyIdleGate = struct {
	mu    sync.Mutex
	byDir map[string]controlReadyIdleEntry
}{byDir: make(map[string]controlReadyIdleEntry)}

// tryControlReadyScanIdleGated is tryControlReadyFromCacheOrFallback behind the idle gate.
// Queries that are not control-ready queries pass straight through, so the
// caller's shell fallback runs exactly as before.
func tryControlReadyScanIdleGated(workQuery, dir string, env map[string]string) ([]hookBead, bool, error) {
	parsed, ok := parseControlReadyQuery(workQuery)
	if !ok {
		return tryControlReadyFromCacheOrFallback(workQuery, dir, env)
	}
	key := controlReadyScopedQueryKey(parsed, mergeRuntimeEnv(os.Environ(), env))
	version, versioned := controlReadyStoreVersionFn(dir)
	if versioned && controlReadyIdleUnchanged(dir, key, version) {
		workflowTracef("control-ready idle-skip dir=%s version=%s", dir, version)
		return nil, true, nil
	}
	queue, handled, live, err := controlReadyScanWithSource(workQuery, dir, env)
	if versioned && handled && live && err == nil && len(queue) == 0 {
		controlReadyIdleRecord(dir, key, version)
	} else {
		controlReadyIdleForget(dir)
	}
	return queue, handled, err
}

func controlReadyIdleUnchanged(dir, key, version string) bool {
	controlReadyIdleGate.mu.Lock()
	defer controlReadyIdleGate.mu.Unlock()
	entry, ok := controlReadyIdleGate.byDir[dir]
	return ok && entry.key == key && entry.version == version &&
		controlReadyNow().Sub(entry.scannedAt) < controlReadyIdleRescanInterval
}

func controlReadyIdleRecord(dir, key, version string) {
	controlReadyIdleGate.mu.Lock()
	defer controlReadyIdleGate.mu.Unlock()
	controlReadyIdleGate.byDir[dir] = controlReadyIdleEntry{key: key, version: version, scannedAt: controlReadyNow()}
}

func controlReadyIdleForget(dir string) {
	controlReadyIdleGate.mu.Lock()
	defer controlReadyIdleGate.mu.Unlock()
	delete(controlReadyIdleGate.byDir, dir)
}

// controlReadyVersionTargetEntry is one scope's cached resolution. ok=false
// entries are cached too: a scope the gate refuses (externally bound,
// proxied, a non-bd provider, a non-authoritative config) must not re-read
// config and re-dial every tick to be refused again.
type controlReadyVersionTargetEntry struct {
	target     contract.DoltConnectionTarget
	ok         bool
	resolvedAt time.Time
}

// controlReadyVersionTargets caches each scope's resolved Dolt target for
// controlReadyVersionTargetTTL, and remembers which scopes' probes are failing
// so the failure is logged on transition rather than on every tick. A failed
// probe drops the cached target so the next tick re-resolves (a managed
// restart can move the port).
var controlReadyVersionTargets = struct {
	mu      sync.Mutex
	byDir   map[string]controlReadyVersionTargetEntry
	failing map[string]bool
}{byDir: make(map[string]controlReadyVersionTargetEntry), failing: make(map[string]bool)}

// controlReadyDoltStoreVersion reads DOLT_HASHOF_DB() for the managed Dolt
// database behind dir, when that is provably the database the scan's bd
// reads.
func controlReadyDoltStoreVersion(dir string) (string, bool) {
	cityPath := cityForStoreDir(dir)
	if controlGraphRelocated(cityPath, dir) {
		// The scan reads the graph binding, which this hash does not cover.
		return "", false
	}
	target, ok := controlReadyVersionTarget(cityPath, dir)
	if !ok {
		return "", false
	}
	version, err := controlReadyReadDoltVersion(target)
	if err == nil && version == "" {
		err = fmt.Errorf("SELECT DOLT_HASHOF_DB() on %q returned no hash", target.Database)
	}
	controlReadyNoteProbeResult(dir, err)
	if err != nil {
		return "", false
	}
	return version, true
}

// controlReadyNoteProbeResult drops a failed probe's target and logs probe
// health on transition only: an unreachable server must not add a log line to
// every idle tick.
func controlReadyNoteProbeResult(dir string, err error) {
	controlReadyVersionTargets.mu.Lock()
	wasFailing := controlReadyVersionTargets.failing[dir]
	if err != nil {
		delete(controlReadyVersionTargets.byDir, dir)
		controlReadyVersionTargets.failing[dir] = true
	} else {
		delete(controlReadyVersionTargets.failing, dir)
	}
	controlReadyVersionTargets.mu.Unlock()
	switch {
	case err != nil:
		workflowTracef("control-ready idle-probe-error dir=%s err=%v", dir, err)
		if !wasFailing {
			log.Printf("control-ready idle gate: reading the store version for %s: %v; scanning every tick until it recovers", dir, err)
		}
	case wasFailing:
		log.Printf("control-ready idle gate: store version for %s readable again", dir)
	}
}

func controlReadyVersionTarget(cityPath, dir string) (contract.DoltConnectionTarget, bool) {
	now := controlReadyNow()
	controlReadyVersionTargets.mu.Lock()
	entry, cached := controlReadyVersionTargets.byDir[dir]
	controlReadyVersionTargets.mu.Unlock()
	if cached && now.Sub(entry.resolvedAt) < controlReadyVersionTargetTTL {
		return entry.target, entry.ok
	}
	target, ok := controlReadyResolveVersionTargetFn(cityPath, dir)
	controlReadyVersionTargets.mu.Lock()
	controlReadyVersionTargets.byDir[dir] = controlReadyVersionTargetEntry{target: target, ok: ok, resolvedAt: now}
	controlReadyVersionTargets.mu.Unlock()
	return target, ok
}

// controlReadyResolveVersionTargetFn is the seam for one uncached resolution.
var controlReadyResolveVersionTargetFn = controlReadyResolveVersionTarget

// controlReadyCanonicalTargetFn is the seam for canonicalScopeDoltTarget.
var controlReadyCanonicalTargetFn = canonicalScopeDoltTarget

// controlReadyProjectedScopeEnvFn returns the Dolt environment the control
// bd runner projects for scopeRoot (see controlBdCommandRunnerForCity and
// controlBdCommandRunnerForRig), without managed-runtime recovery.
var controlReadyProjectedScopeEnvFn = controlReadyProjectedScopeEnv

// controlReadyResolveVersionTarget admits a scope only when the database the
// gate would hash is provably the one the control scan's bd reads: the scope
// is opened through the control bd store (openControlStoreAtForCity), gc owns
// its store, the canonical target is a directly reachable managed server, and
// the env the bd runner projects for that scope points at the same server and
// database.
func controlReadyResolveVersionTarget(cityPath, dir string) (contract.DoltConnectionTarget, bool) {
	scopeRoot := resolveStoreScopeRoot(cityPath, dir)
	if !controlReadyProviderVersionable(rawBeadsProviderForScope(scopeRoot, cityPath)) {
		return contract.DoltConnectionTarget{}, false
	}
	if scopeBackendIsDoltlite(cityPath, scopeRoot) {
		return contract.DoltConnectionTarget{}, false
	}
	// Same guard every other canonicalScopeDoltTarget caller applies: a store
	// served by a storage binding is not gc's, and its canonical target does
	// not describe what bd opens. An unreadable binding is refused too.
	if bound, err := scopeStoreIsExternallyBound(cityPath, scopeRoot); err != nil || bound {
		return contract.DoltConnectionTarget{}, false
	}
	target, ok, err := controlReadyCanonicalTargetFn(cityPath, scopeRoot)
	if err != nil || !ok || !controlReadyTargetVersionable(target) {
		return contract.DoltConnectionTarget{}, false
	}
	projected, err := controlReadyProjectedScopeEnvFn(cityPath, scopeRoot)
	if err != nil || !controlReadyTargetMatchesEnv(target, mergeRuntimeEnv(os.Environ(), projected)) {
		return contract.DoltConnectionTarget{}, false
	}
	return target, true
}

// controlReadyProviderVersionable admits only the providers
// openControlStoreAtForCity serves through the control bd store: `file` and
// every `exec:` provider open some other store whose content this hash does
// not describe.
func controlReadyProviderVersionable(provider string) bool {
	provider = strings.TrimSpace(provider)
	return provider == "" || provider == "bd"
}

func controlReadyProjectedScopeEnv(cityPath, scopeRoot string) (map[string]string, error) {
	if samePath(scopeRoot, cityPath) {
		return bdRuntimeEnvWithErrorNoRecovery(cityPath)
	}
	cfg, err := loadCityConfig(cityPath, io.Discard)
	if err != nil {
		return nil, err
	}
	return bdRuntimeEnvForRigWithErrorNoRecovery(cityPath, cfg, scopeRoot)
}

// controlReadyTargetVersionable admits only a managed Dolt server gc can reach
// directly with its managed credentials: a proxied scope has no direct
// endpoint, and an external one may need credentials the pool does not hold.
func controlReadyTargetVersionable(target contract.DoltConnectionTarget) bool {
	if target.External || target.DoltMode == "proxied-server" {
		return false
	}
	if strings.TrimSpace(target.Database) == "" {
		return false
	}
	return strings.TrimSpace(target.Socket) != "" || strings.TrimSpace(target.Port) != ""
}

// controlReadyTargetMatchesEnv reports whether envList -- the effective env
// the scope's bd runner runs with -- explicitly points bd at target: same
// database, and the same socket or port. An env that names no endpoint is
// refused rather than trusted, because bd would then fall back to its own
// discovery, which the hash cannot be shown to describe.
func controlReadyTargetMatchesEnv(target contract.DoltConnectionTarget, envList []string) bool {
	if db := strings.TrimSpace(envListValue(envList, "BEADS_DOLT_SERVER_DATABASE")); db != "" && db != strings.TrimSpace(target.Database) {
		return false
	}
	if socket := strings.TrimSpace(target.Socket); socket != "" {
		return strings.TrimSpace(envListValue(envList, "BEADS_DOLT_SERVER_SOCKET")) == socket
	}
	if strings.TrimSpace(envListValue(envList, "BEADS_DOLT_SERVER_SOCKET")) != "" {
		return false
	}
	pinned := false
	for _, key := range []string{"GC_DOLT_PORT", "BEADS_DOLT_SERVER_PORT"} {
		port := strings.TrimSpace(envListValue(envList, key))
		if port == "" {
			continue
		}
		if port != strings.TrimSpace(target.Port) {
			return false
		}
		pinned = true
	}
	return pinned
}

// controlReadyOpenVersionDB is the seam for the pooled handle the hash is read
// over. The handle belongs to internal/doltpool; callers must not Close it.
var controlReadyOpenVersionDB = func(target contract.DoltConnectionTarget) (*sql.DB, error) {
	if socket := strings.TrimSpace(target.Socket); socket != "" {
		return managedDoltOpenDatabaseSocket(socket, target.User, target.Database)
	}
	return managedDoltOpenDatabase(target.Host, target.Port, target.User, target.Database)
}

func controlReadyReadDoltVersion(target contract.DoltConnectionTarget) (string, error) {
	db, err := controlReadyOpenVersionDB(target)
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(context.Background(), controlReadyIdleProbeTimeout)
	defer cancel()
	var version sql.NullString
	if err := db.QueryRowContext(ctx, "SELECT DOLT_HASHOF_DB()").Scan(&version); err != nil {
		return "", fmt.Errorf("SELECT DOLT_HASHOF_DB() on %q: %w", target.Database, err)
	}
	return strings.TrimSpace(version.String), nil
}
