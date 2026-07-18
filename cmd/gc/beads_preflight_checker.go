package main

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/gastownhall/gascity/internal/beads/contract"
	"github.com/gastownhall/gascity/internal/fsys"
)

func newBeadsPreflightChecker(cityPath, provider string) contract.PreflightChecker {
	return contract.PreflightChecker{
		FS:                        fsys.OSFS{},
		Provider:                  provider,
		BDContext:                 preflightBDContextReader(cityPath),
		DatabaseProjectID:         preflightDatabaseProjectIDReader(cityPath),
		DeferIdentityToNativeOpen: preflightIdentityDeferredReader(cityPath),
	}
}

func preflightBDContextReader(cityPath string) func(scope string) (contract.PreflightBDContext, error) {
	return func(scope string) (contract.PreflightBDContext, error) {
		// Cached per (cityPath, scope, backend target): the bd context identity is
		// process-stable, so the `bd context --json` spawn runs at most once per
		// scope. Resolution order is L1 in-process memo -> L2 on-disk TTL cache ->
		// cold `bd context` spawn; a cold spawn populates both tiers. Errors are
		// not cached (retry next open).
		v, err := preflightBDContextCached(cityPath, preflightScopeKey(cityPath, scope), func() (preflightBDContextValue, error) {
			out, err := bdCommandRunnerForCity(cityPath)(scope, "bd", "context", "--json")
			if err != nil {
				return preflightBDContextValue{}, err
			}
			var raw struct {
				Backend       string `json:"backend"`
				DoltMode      string `json:"dolt_mode"`
				BDVersion     string `json:"bd_version"`
				SchemaVersion int    `json:"schema_version"`
			}
			if err := json.Unmarshal(out, &raw); err != nil {
				return preflightBDContextValue{}, fmt.Errorf("parse bd context --json: %w", err)
			}
			return preflightBDContextValue{
				Backend:       raw.Backend,
				DoltMode:      raw.DoltMode,
				BDVersion:     raw.BDVersion,
				SchemaVersion: raw.SchemaVersion,
			}, nil
		})
		if err != nil {
			return contract.PreflightBDContext{}, err
		}
		return contract.PreflightBDContext{
			Backend:       v.Backend,
			DoltMode:      v.DoltMode,
			BDVersion:     v.BDVersion,
			SchemaVersion: v.SchemaVersion,
		}, nil
	}
}

// preflightIdentityDeferredReader reports whether a scope resolves to an
// external Dolt endpoint (e.g. a hosted beads-gateway). The direct root/plaintext
// project_id probe cannot authenticate such endpoints, so when it comes back
// unconfirmed the identity check defers to beadslib's native-open verification
// (which authenticates via the credential command and refuses to connect on a
// _project_id mismatch) instead of degrading the scope off the native store.
func preflightIdentityDeferredReader(cityPath string) func(scope string) bool {
	return func(scope string) bool {
		target, ok, err := canonicalScopeDoltTarget(cityPath, scope)
		if err != nil || !ok {
			return false
		}
		return target.External
	}
}

func preflightDatabaseProjectIDReader(cityPath string) func(scope string) (string, bool, error) {
	return func(scope string) (string, bool, error) {
		// Memoized per (cityPath, scope, backend target): the project id (and the
		// unconfirmed/confirmed outcome) is stable for a fixed backend, so the Dolt
		// open+ping runs once per scope instead of on every store-open. A probe
		// error is not cached, so a transient Dolt blip retries on the next open
		// rather than sticking the scope in a degraded state until a bounce.
		v, err := preflightProjectIDMemo.getOrCompute(preflightScopeKey(cityPath, scope), func() (preflightProjectIDValue, error) {
			target, ok, err := canonicalScopeDoltTarget(cityPath, scope)
			if err != nil || !ok {
				// err may be nil here (scope not authoritative): that is a stable
				// "unconfirmed" outcome and is cached as ("", false).
				return preflightProjectIDValue{}, err
			}
			db, err := managedDoltOpenDatabase(target.Host, target.Port, target.User, target.Database)
			if err != nil {
				return preflightProjectIDValue{}, err
			}
			defer db.Close() //nolint:errcheck // read-only best-effort close

			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := db.PingContext(ctx); err != nil {
				return preflightProjectIDValue{}, err
			}
			id, confirmed, err := readDatabaseProjectID(ctx, db)
			if err != nil {
				return preflightProjectIDValue{}, err
			}
			return preflightProjectIDValue{id: id, ok: confirmed}, nil
		})
		if err != nil {
			return "", false, err
		}
		return v.id, v.ok, nil
	}
}
