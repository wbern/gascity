package main

import (
	"fmt"
	"sync"
)

// The beads preflight identity check (bd context spawn + Dolt project-id ping)
// runs on EVERY store-open (internal/beads/factory.go PreflightChecker.Check),
// yet the identity it reads — bd version, backend, dolt_mode, project_id — is
// process-stable for a fixed backend target. Re-running it per open is pure
// churn: a fresh `bd context` process (observed ~5/min idle) and a redundant
// Dolt connection each time. These memos cache the stable result per scope so it
// is computed once and reused.
//
// Two invariants keep the cache sound:
//   - Keyed by (cityPath, scope, resolved backend target), so a config/backend
//     change (even an in-process reload) yields a different key and re-checks.
//   - Successful identities are cached; transient probe failures are never
//     cached, so they retry on the next open rather than permanently degrading
//     the scope off the native store. The disk layer has one deliberately
//     narrower exception for bd's deterministic non-repository context error.

// preflightScopeMemo caches a per-key value computed by a slow probe.
type preflightScopeMemo[T any] struct {
	mu sync.Mutex
	m  map[string]T
}

func newPreflightScopeMemo[T any]() *preflightScopeMemo[T] {
	return &preflightScopeMemo[T]{m: make(map[string]T)}
}

// getOrCompute returns the cached value for key, or runs compute and caches a
// successful result. compute runs OUTSIDE the lock so an unrelated scope's slow
// probe cannot serialize others; a concurrent cold miss may compute twice, which
// is harmless because the value is stable. Errors are never cached.
func (c *preflightScopeMemo[T]) getOrCompute(key string, compute func() (T, error)) (T, error) {
	c.mu.Lock()
	if v, ok := c.m[key]; ok {
		c.mu.Unlock()
		return v, nil
	}
	c.mu.Unlock()

	v, err := compute()
	if err != nil {
		var zero T
		return zero, err
	}

	c.mu.Lock()
	c.m[key] = v
	c.mu.Unlock()
	return v, nil
}

// preflightScopeKeyFor builds the cache key from its parts. Separated from the
// target resolution so the key composition is unit-testable.
func preflightScopeKeyFor(cityPath, scope, targetFingerprint string) string {
	return cityPath + "\x00" + scope + "\x00" + targetFingerprint
}

// preflightScopeKey builds the memo key for a scope, folding in the resolved
// backend target so a backend change invalidates the entry. When the target
// cannot be resolved (degraded/unconfigured scope) it falls back to a constant
// marker; (cityPath, scope) alone still separates distinct scopes.
func preflightScopeKey(cityPath, scope string) string {
	fingerprint := "unresolved-target"
	if target, ok, err := canonicalScopeDoltTarget(cityPath, scope); err == nil && ok {
		fingerprint = fmt.Sprintf("%v:%v/%v|ext=%t", target.Host, target.Port, target.Database, target.External)
	}
	return preflightScopeKeyFor(cityPath, scope, fingerprint)
}

// preflightBDContextMemo caches the `bd context --json` identity per scope.
var preflightBDContextMemo = newPreflightScopeMemo[preflightBDContextValue]()

// preflightBDContextValue mirrors contract.PreflightBDContext, held as a plain
// value so the memo type does not depend on the contract package.
type preflightBDContextValue struct {
	Backend       string
	DoltMode      string
	BDVersion     string
	SchemaVersion int
}

// preflightProjectIDMemo caches the Dolt project-id probe result per scope. The
// ok flag (unconfirmed vs confirmed) is a stable, cacheable outcome, so it is
// carried alongside the id.
var preflightProjectIDMemo = newPreflightScopeMemo[preflightProjectIDValue]()

type preflightProjectIDValue struct {
	id string
	ok bool
}
