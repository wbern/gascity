package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/citylayout"
)

// The bd context identity (backend / dolt_mode / bd_version / schema_version) is
// process-stable and read from config files only (no Dolt connection), yet the
// in-process memo (preflightBDContextMemo) is per-process, so every short-lived
// gc/bd invocation re-spawns `bd context --json` on its first store-open. This
// on-disk L2 cache sits behind the L1 memo and lets independent short-lived
// processes reuse a recent identity, cutting bd process-spawn churn.
//
// This file is a TTL'd config-identity cache under .gc/cache, NOT a
// process-status file: it records what `bd context` reported (advisory
// identity), never "what is running". So it does not violate the project's
// no-status-files rule — a stale entry cannot mask a real outage because the
// preflight verdict is advisory (contract.PreflightChecker.Check upgrades a
// degraded verdict to eligible when bd context is unreachable, and the factory
// still opens and falls back). Only the bd context identity is cached here; the
// correctness-sensitive Dolt project-id probe is deliberately NOT cached to disk.

// preflightIdentityDiskTTL bounds how long a persisted bd context identity is
// served before a fresh `bd context` spawn is required. Short by design: long
// enough to collapse a burst of short-lived processes, short enough that a real
// config/backend change is picked up promptly.
const preflightIdentityDiskTTL = 60 * time.Second

// preflightIdentityCacheFile is the cache filename under the city cache root.
const preflightIdentityCacheFile = "preflight-identity.json"

const preflightBDContextUnavailableNotGitRepository = "not_git_repository"

var errPreflightBDContextNotGitRepository = errors.New("bd context unavailable: scope is not in a git repository")

// preflightNow is the injectable clock used for TTL evaluation. Tests override
// it to make TTL behavior deterministic.
var preflightNow = time.Now

// preflightIdentityDiskEntry is one persisted identity plus the wall-clock time
// it was cached, so reads can enforce the TTL.
type preflightIdentityDiskEntry struct {
	Backend                    string `json:"backend"`
	DoltMode                   string `json:"dolt_mode"`
	BDVersion                  string `json:"bd_version"`
	SchemaVersion              int    `json:"schema_version"`
	UnavailableBDContextReason string `json:"unavailable_bd_context_reason,omitempty"`
	CachedAtUnix               int64  `json:"cached_at_unix"`
}

// preflightIdentityCachePath returns the absolute path to the on-disk preflight
// identity cache for a city.
func preflightIdentityCachePath(cityPath string) string {
	return citylayout.CachePath(cityPath, preflightIdentityCacheFile)
}

// readPreflightIdentityDiskMap loads the whole scopeKey->entry map. A missing,
// unreadable, or corrupt file is treated as an empty map (never an error): the
// cache is advisory, so a MISS just triggers a recompute.
func readPreflightIdentityDiskMap(cityPath string) map[string]preflightIdentityDiskEntry {
	data, err := os.ReadFile(preflightIdentityCachePath(cityPath))
	if err != nil {
		return nil
	}
	var m map[string]preflightIdentityDiskEntry
	if err := json.Unmarshal(data, &m); err != nil {
		return nil
	}
	return m
}

// readPreflightIdentityDisk returns the cached identity for scopeKey when a
// fresh entry (now - cached_at < TTL) exists. A missing, stale, corrupt, or
// unreadable cache is reported as a miss (ok=false) and never surfaces an error.
func readPreflightIdentityDisk(cityPath, scopeKey string) (preflightBDContextValue, bool) {
	m := readPreflightIdentityDiskMap(cityPath)
	entry, ok := m[scopeKey]
	if !ok || entry.UnavailableBDContextReason != "" {
		return preflightBDContextValue{}, false
	}
	age := preflightNow().Sub(time.Unix(entry.CachedAtUnix, 0))
	if age < 0 || age >= preflightIdentityDiskTTL {
		return preflightBDContextValue{}, false
	}
	return preflightBDContextValue{
		Backend:       entry.Backend,
		DoltMode:      entry.DoltMode,
		BDVersion:     entry.BDVersion,
		SchemaVersion: entry.SchemaVersion,
	}, true
}

// readPreflightBDContextUnavailableDisk returns the one deterministic bd
// context failure that may be cached. Any unknown reason is a miss so a future
// error class cannot silently inherit this cache policy.
func readPreflightBDContextUnavailableDisk(cityPath, scopeKey string) bool {
	m := readPreflightIdentityDiskMap(cityPath)
	entry, ok := m[scopeKey]
	if !ok || entry.UnavailableBDContextReason != preflightBDContextUnavailableNotGitRepository {
		return false
	}
	age := preflightNow().Sub(time.Unix(entry.CachedAtUnix, 0))
	return age >= 0 && age < preflightIdentityDiskTTL
}

// writePreflightIdentityDisk persists a SUCCESSFUL identity for scopeKey,
// preserving other scopes' entries via a read-modify-write of the map and an
// atomic temp+rename replace. Errors (e.g. a losing race between two processes)
// are best-effort by design and are surfaced only so callers may log; the cache
// being advisory means a lost write merely costs a later recompute.
func writePreflightIdentityDisk(cityPath, scopeKey string, v preflightBDContextValue) error {
	dir := filepath.Dir(preflightIdentityCachePath(cityPath))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	m := readPreflightIdentityDiskMap(cityPath)
	if m == nil {
		m = make(map[string]preflightIdentityDiskEntry)
	}
	m[scopeKey] = preflightIdentityDiskEntry{
		Backend:       v.Backend,
		DoltMode:      v.DoltMode,
		BDVersion:     v.BDVersion,
		SchemaVersion: v.SchemaVersion,
		CachedAtUnix:  preflightNow().Unix(),
	}
	data, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return atomicWriteFile(preflightIdentityCachePath(cityPath), data)
}

// writePreflightBDContextUnavailableDisk records a deterministic non-repository
// result. It shares the identity cache's scope key and short TTL, so a scope
// that becomes a Git worktree is probed again promptly.
func writePreflightBDContextUnavailableDisk(cityPath, scopeKey string) error {
	dir := filepath.Dir(preflightIdentityCachePath(cityPath))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	m := readPreflightIdentityDiskMap(cityPath)
	if m == nil {
		m = make(map[string]preflightIdentityDiskEntry)
	}
	m[scopeKey] = preflightIdentityDiskEntry{
		UnavailableBDContextReason: preflightBDContextUnavailableNotGitRepository,
		CachedAtUnix:               preflightNow().Unix(),
	}
	data, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return atomicWriteFile(preflightIdentityCachePath(cityPath), data)
}

// preflightBDContextUnavailableReason identifies a stable bd context failure
// without treating broader command errors as stable. These exact fragments are
// emitted by bd when it walks upward from a city directory that is not in a
// Git worktree; connection failures, timeouts, and JSON errors do not match.
func preflightBDContextUnavailableReason(err error) string {
	if err == nil {
		return ""
	}
	message := strings.ToLower(err.Error())
	if strings.Contains(message, "cannot resolve repo context") &&
		strings.Contains(message, "cannot determine repository root") &&
		strings.Contains(message, "not a git repository") {
		return preflightBDContextUnavailableNotGitRepository
	}
	return ""
}

// preflightBDContextCached resolves the bd context identity for scopeKey through
// the L1 in-process memo, then the L2 on-disk cache, spawning the cold compute
// (run) only on a full miss. A cold compute populates BOTH tiers: the memo (via
// getOrCompute) and the disk cache. Errors are never cached, except bd's exact
// deterministic "not a git repository" context failure. That outcome retains
// the same short TTL as a successful identity; all transport, parse, and other
// command failures retry on the next open.
func preflightBDContextCached(cityPath, scopeKey string, run func() (preflightBDContextValue, error)) (preflightBDContextValue, error) {
	if readPreflightBDContextUnavailableDisk(cityPath, scopeKey) {
		return preflightBDContextValue{}, errPreflightBDContextNotGitRepository
	}
	return preflightBDContextMemo.getOrCompute(scopeKey, func() (preflightBDContextValue, error) {
		if cached, ok := readPreflightIdentityDisk(cityPath, scopeKey); ok {
			return cached, nil
		}
		v, err := run()
		if err != nil {
			if preflightBDContextUnavailableReason(err) == preflightBDContextUnavailableNotGitRepository {
				// Best-effort: a failed write only costs a later re-probe. The
				// next open will reuse the persisted result while it is fresh.
				_ = writePreflightBDContextUnavailableDisk(cityPath, scopeKey)
				return preflightBDContextValue{}, errPreflightBDContextNotGitRepository
			}
			return preflightBDContextValue{}, err
		}
		// Best-effort: a failed disk write only costs a future recompute.
		_ = writePreflightIdentityDisk(cityPath, scopeKey, v)
		return v, nil
	})
}
