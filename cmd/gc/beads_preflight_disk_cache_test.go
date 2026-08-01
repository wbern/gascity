package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// The on-disk L2 preflight identity cache lets short-lived gc/bd processes reuse
// a recent `bd context --json` identity instead of re-spawning it on every
// store-open. These tests pin its load-bearing properties: a cold miss spawns
// once and persists the identity, a warm disk entry serves a fresh process
// without spawning, a stale entry re-spawns, a corrupt/missing file is a silent
// miss, and distinct scope keys never read each other's entry.

// withFreshL1Memo swaps the process-wide L1 memo for an empty one so a test can
// simulate a brand-new process (which has no in-memory cache) while still
// sharing the on-disk L2 cache directory.
func withFreshL1Memo(t *testing.T) {
	t.Helper()
	prevIdentity := preflightBDContextMemo
	preflightBDContextMemo = newPreflightScopeMemo[preflightBDContextValue]()
	t.Cleanup(func() {
		preflightBDContextMemo = prevIdentity
	})
}

// withFixedPreflightNow pins the injectable clock so TTL behavior is
// deterministic, returning a setter to advance it.
func withFixedPreflightNow(t *testing.T, start time.Time) func(time.Time) {
	t.Helper()
	prev := preflightNow
	cur := start
	preflightNow = func() time.Time { return cur }
	t.Cleanup(func() { preflightNow = prev })
	return func(at time.Time) { cur = at }
}

// withGCBuildVersion pins the gc build stamped into and compared against cache
// entries, so a test can simulate the same build or a rebuilt binary.
func withGCBuildVersion(t *testing.T, build string) {
	t.Helper()
	prev := version
	version = build
	t.Cleanup(func() { version = prev })
}

func fakeIdentity() preflightBDContextValue {
	return preflightBDContextValue{Backend: "dolt", DoltMode: "managed", BDVersion: "1.2.3", SchemaVersion: 7}
}

func TestPreflightBDContextCached_ColdMissSpawnsOnceAndWritesDisk(t *testing.T) {
	city := t.TempDir()
	withFreshL1Memo(t)
	withFixedPreflightNow(t, time.Unix(1000, 0))
	key := preflightScopeKeyFor(city, "rig-a", "host:3306/db|ext=false")

	calls := 0
	run := func() (preflightBDContextValue, error) { calls++; return fakeIdentity(), nil }

	got, err := preflightBDContextCached(city, key, run)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != fakeIdentity() {
		t.Fatalf("got %+v, want %+v", got, fakeIdentity())
	}
	if calls != 1 {
		t.Fatalf("run ran %d times, want 1", calls)
	}
	if _, err := os.Stat(preflightIdentityCachePath(city)); err != nil {
		t.Fatalf("disk cache not written: %v", err)
	}
	if v, ok := readPreflightIdentityDisk(city, key); !ok || v != fakeIdentity() {
		t.Fatalf("disk read back = %+v ok=%v, want identity", v, ok)
	}
}

func TestPreflightBDContextCached_WarmDiskHitInFreshProcess(t *testing.T) {
	city := t.TempDir()
	advance := withFixedPreflightNow(t, time.Unix(1000, 0))
	key := preflightScopeKeyFor(city, "rig-a", "host:3306/db|ext=false")

	// Process 1: cold miss populates disk.
	withFreshL1Memo(t)
	if _, err := preflightBDContextCached(city, key, func() (preflightBDContextValue, error) { return fakeIdentity(), nil }); err != nil {
		t.Fatalf("cold populate: %v", err)
	}

	// Process 2: fresh L1 memo, still within TTL -> disk hit, no spawn.
	withFreshL1Memo(t)
	advance(time.Unix(1030, 0)) // well inside preflightIdentityDiskTTL
	calls := 0
	got, err := preflightBDContextCached(city, key, func() (preflightBDContextValue, error) {
		calls++
		return preflightBDContextValue{}, nil
	})
	if err != nil {
		t.Fatalf("warm hit error: %v", err)
	}
	if calls != 0 {
		t.Fatalf("run ran %d times, want 0 (served from disk)", calls)
	}
	if got != fakeIdentity() {
		t.Fatalf("got %+v, want %+v from disk", got, fakeIdentity())
	}
}

func TestPreflightBDContextCached_StaleEntryReSpawns(t *testing.T) {
	city := t.TempDir()
	advance := withFixedPreflightNow(t, time.Unix(1000, 0))
	key := preflightScopeKeyFor(city, "rig-a", "host:3306/db|ext=false")

	withFreshL1Memo(t)
	if _, err := preflightBDContextCached(city, key, func() (preflightBDContextValue, error) { return fakeIdentity(), nil }); err != nil {
		t.Fatalf("cold populate: %v", err)
	}

	withFreshL1Memo(t)
	advance(time.Unix(1000, 0).Add(preflightIdentityDiskTTL + time.Second)) // past TTL
	calls := 0
	if _, err := preflightBDContextCached(city, key, func() (preflightBDContextValue, error) {
		calls++
		return fakeIdentity(), nil
	}); err != nil {
		t.Fatalf("stale re-spawn error: %v", err)
	}
	if calls != 1 {
		t.Fatalf("run ran %d times, want 1 (stale entry must re-spawn)", calls)
	}
}

// The cache's whole purpose is to stop periodic callers from re-spawning
// `bd context` every tick. Gas City's order fleet runs on cooldowns measured in
// minutes — on the live gc2 city, 73 of 74 orders sit at 1m or longer and the
// mass of them is 2m-15m. A TTL at or below that band is therefore never able to
// serve a single order tick: the entry is always stale by the time the next tick
// arrives, so the cache only ever collapses bursts inside one window and every
// periodic caller pays a full bd spawn forever. That is exactly what a 60s TTL
// did (measured 2026-08-01: `bd context` was 30.8% of all bd traffic at a flat
// ~1/min floor around the clock, 54.6% of it failing, and a controlled repeat at
// 70s re-spawned while the same repeat at 10s did not).
//
// These cases pin the TTL against the cadences that must survive it. They are
// deliberately expressed as cache hits at real order cooldowns rather than as an
// assertion on the constant, so the property survives any future retuning.
func TestPreflightIdentityDiskServesTheOrderCooldownBand(t *testing.T) {
	for _, test := range []struct {
		name    string
		elapsed time.Duration
	}{
		{name: "1m order tick", elapsed: time.Minute},
		{name: "2m order tick", elapsed: 2 * time.Minute},
		{name: "5m order tick (modal cooldown)", elapsed: 5 * time.Minute},
		{name: "10m order tick", elapsed: 10 * time.Minute},
	} {
		t.Run(test.name, func(t *testing.T) {
			city := t.TempDir()
			withGCBuildVersion(t, "test-build")
			start := time.Unix(1000, 0)
			advance := withFixedPreflightNow(t, start)
			key := preflightScopeKeyFor(city, "rig-a", "host:3306/db|ext=false")

			withFreshL1Memo(t)
			if _, err := preflightBDContextCached(city, key, func() (preflightBDContextValue, error) {
				return fakeIdentity(), nil
			}); err != nil {
				t.Fatalf("cold populate: %v", err)
			}

			// The next order tick runs in a brand-new short-lived gc process, so it
			// has no L1 memo and must be served by the on-disk L2 entry.
			withFreshL1Memo(t)
			advance(start.Add(test.elapsed))
			calls := 0
			got, err := preflightBDContextCached(city, key, func() (preflightBDContextValue, error) {
				calls++
				return fakeIdentity(), nil
			})
			if err != nil {
				t.Fatalf("order tick error: %v", err)
			}
			if calls != 0 {
				t.Fatalf("a %s order tick spawned bd context %d times, want 0 (served from disk)", test.elapsed, calls)
			}
			if got != fakeIdentity() {
				t.Fatalf("got %+v, want %+v from disk", got, fakeIdentity())
			}
		})
	}
}

// The deterministic non-repository result is the larger half of the wasted
// spawns (54.6% of live `bd context` calls), because a city root is not a Git
// worktree and never becomes one. It must ride the same cooldown-surviving TTL.
func TestPreflightBDContextUnavailableDiskServesTheOrderCooldownBand(t *testing.T) {
	city := t.TempDir()
	withGCBuildVersion(t, "test-build")
	start := time.Unix(1000, 0)
	advance := withFixedPreflightNow(t, start)
	key := preflightScopeKeyFor(city, city, "host:3306/db|ext=false")
	notRepository := errors.New("exit status 1: cannot resolve repo context: cannot determine repository root: not a git repository")

	withFreshL1Memo(t)
	calls := 0
	if _, err := preflightBDContextCached(city, key, func() (preflightBDContextValue, error) {
		calls++
		return preflightBDContextValue{}, notRepository
	}); !errors.Is(err, errPreflightBDContextNotGitRepository) {
		t.Fatalf("cold error = %v, want classified non-repository error", err)
	}

	withFreshL1Memo(t)
	advance(start.Add(5 * time.Minute))
	if _, err := preflightBDContextCached(city, key, func() (preflightBDContextValue, error) {
		calls++
		return fakeIdentity(), nil
	}); !errors.Is(err, errPreflightBDContextNotGitRepository) {
		t.Fatalf("5m order tick = %v, want the persisted non-repository error", err)
	}
	if calls != 1 {
		t.Fatalf("a 5m order tick spawned bd context %d times, want 1 total (the cold probe)", calls)
	}
}

// REGRESSION (measured in production 2026-08-01): an earlier revision of this
// cache rejected any entry whose gc_build differed from the running binary. That
// was strictly worse than no build check at all.
//
// A session-preserving rebuild-bounce leaves long-lived detached helpers (e.g.
// `gc nudge poll`) running the PREVIOUS build. Those keep writing entries with no
// build stamp; every new process rejected them, re-probed, and rewrote a stamped
// entry, which the old process then overwrote unstamped. The live hq entry was
// watched flipping stamped -> unstamped -> stamped inside 90 seconds while
// `bd context` spawns stayed at the pre-fix rate, because essentially every read
// missed. ONE stale writer poisons the cache for the whole fleet, indefinitely.
//
// So an entry from another build — or from before build stamping existed — MUST
// be served while it is inside the TTL. The TTL is the whole freshness test.
func TestReadPreflightIdentityDisk_ServesEntriesFromAnotherGCBuild(t *testing.T) {
	city := t.TempDir()
	withFixedPreflightNow(t, time.Unix(1000, 0))
	key := preflightScopeKeyFor(city, "rig-a", "host:3306/db|ext=false")

	withGCBuildVersion(t, "build-a")
	if err := writePreflightIdentityDisk(city, key, fakeIdentity()); err != nil {
		t.Fatalf("write under build-a: %v", err)
	}

	// A DIFFERENT build must still be served: rejecting here is what caused the
	// production ping-pong.
	withGCBuildVersion(t, "build-b")
	if v, ok := readPreflightIdentityDisk(city, key); !ok || v != fakeIdentity() {
		t.Fatalf("entry from another gc build must be served while fresh: %+v ok=%v", v, ok)
	}

	// And it must not trigger a re-probe.
	withFreshL1Memo(t)
	calls := 0
	if _, err := preflightBDContextCached(city, key, func() (preflightBDContextValue, error) {
		calls++
		return fakeIdentity(), nil
	}); err != nil {
		t.Fatalf("serve across builds: %v", err)
	}
	if calls != 0 {
		t.Fatalf("run ran %d times, want 0 — a foreign build stamp must not force a re-probe", calls)
	}
}

func TestReadPreflightBDContextUnavailableDisk_ServesEntriesFromAnotherGCBuild(t *testing.T) {
	city := t.TempDir()
	withFixedPreflightNow(t, time.Unix(1000, 0))
	key := preflightScopeKeyFor(city, city, "host:3306/db|ext=false")

	withGCBuildVersion(t, "build-a")
	if err := writePreflightBDContextUnavailableDisk(city, key); err != nil {
		t.Fatalf("write under build-a: %v", err)
	}
	withGCBuildVersion(t, "build-b")
	if !readPreflightBDContextUnavailableDisk(city, key) {
		t.Fatalf("non-repository entry from another gc build must be served while fresh")
	}
}

// An entry persisted by a gc that predates build stamping carries no build. It is
// exactly what a surviving old-binary helper writes, so it must be served too.
func TestReadPreflightIdentityDisk_ServesLegacyEntryWithoutBuild(t *testing.T) {
	city := t.TempDir()
	withFixedPreflightNow(t, time.Unix(1000, 0))
	withGCBuildVersion(t, "build-a")
	key := preflightScopeKeyFor(city, "rig-a", "host:3306/db|ext=false")

	legacy := map[string]preflightIdentityDiskEntry{
		key: {Backend: "dolt", DoltMode: "managed", BDVersion: "1.2.3", SchemaVersion: 7, CachedAtUnix: 1000},
	}
	data, err := json.Marshal(legacy)
	if err != nil {
		t.Fatalf("marshal legacy: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(preflightIdentityCachePath(city)), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(preflightIdentityCachePath(city), data, 0o644); err != nil {
		t.Fatalf("write legacy: %v", err)
	}

	if v, ok := readPreflightIdentityDisk(city, key); !ok || v != fakeIdentity() {
		t.Fatalf("unstamped legacy entry must be served while fresh: %+v ok=%v", v, ok)
	}
}

// The build stamp is still RECORDED — it is what made the ping-pong diagnosable.
func TestWritePreflightIdentityDisk_RecordsTheGCBuild(t *testing.T) {
	city := t.TempDir()
	withFixedPreflightNow(t, time.Unix(1000, 0))
	withGCBuildVersion(t, "build-xyz")
	key := preflightScopeKeyFor(city, "rig-a", "host:3306/db|ext=false")
	if err := writePreflightIdentityDisk(city, key, fakeIdentity()); err != nil {
		t.Fatalf("write: %v", err)
	}
	m := readPreflightIdentityDiskMap(city)
	if got := m[key].GCBuild; got != "build-xyz" {
		t.Fatalf("gc_build recorded = %q, want %q", got, "build-xyz")
	}
}

func TestPreflightBDContextCached_CachesOnlyNotGitRepositoryFailure(t *testing.T) {
	city := t.TempDir()
	advance := withFixedPreflightNow(t, time.Unix(1000, 0))
	key := preflightScopeKeyFor(city, city, "host:3306/db|ext=false")
	notRepository := errors.New("exit status 1: cannot resolve repo context: cannot determine repository root: not a git repository")

	withFreshL1Memo(t)
	calls := 0
	if _, err := preflightBDContextCached(city, key, func() (preflightBDContextValue, error) {
		calls++
		return preflightBDContextValue{}, notRepository
	}); !errors.Is(err, errPreflightBDContextNotGitRepository) {
		t.Fatalf("first error = %v, want classified non-repository error", err)
	}
	if calls != 1 {
		t.Fatalf("first run calls = %d, want 1", calls)
	}

	// A new gc process has no L1 entry, so this verifies the persisted L2
	// result prevents another bd context subprocess during the TTL.
	withFreshL1Memo(t)
	advance(time.Unix(1030, 0))
	if _, err := preflightBDContextCached(city, key, func() (preflightBDContextValue, error) {
		calls++
		return fakeIdentity(), nil
	}); !errors.Is(err, errPreflightBDContextNotGitRepository) {
		t.Fatalf("warm failure = %v, want classified non-repository error", err)
	}
	if calls != 1 {
		t.Fatalf("warm disk hit ran subprocess %d times, want 1", calls)
	}

	// Repository membership can change, so the deterministic result must expire
	// on the same TTL as a successful identity and then re-probe.
	withFreshL1Memo(t)
	advance(time.Unix(1000, 0).Add(preflightIdentityDiskTTL + time.Second))
	if _, err := preflightBDContextCached(city, key, func() (preflightBDContextValue, error) {
		calls++
		return fakeIdentity(), nil
	}); err != nil {
		t.Fatalf("expired failure re-probe error = %v", err)
	}
	if calls != 2 {
		t.Fatalf("expired failure calls = %d, want 2", calls)
	}
}

func TestPreflightBDContextCached_DoesNotCacheOtherFailures(t *testing.T) {
	city := t.TempDir()
	withFixedPreflightNow(t, time.Unix(1000, 0))
	key := preflightScopeKeyFor(city, "rig-a", "host:3306/db|ext=false")
	transient := errors.New("dial tcp 127.0.0.1:3306: connect: connection refused")

	withFreshL1Memo(t)
	calls := 0
	if _, err := preflightBDContextCached(city, key, func() (preflightBDContextValue, error) {
		calls++
		return preflightBDContextValue{}, transient
	}); !errors.Is(err, transient) {
		t.Fatalf("first error = %v, want transient error", err)
	}

	withFreshL1Memo(t)
	if _, err := preflightBDContextCached(city, key, func() (preflightBDContextValue, error) {
		calls++
		return preflightBDContextValue{}, transient
	}); !errors.Is(err, transient) {
		t.Fatalf("second error = %v, want transient error", err)
	}
	if calls != 2 {
		t.Fatalf("transient failure calls = %d, want 2", calls)
	}
}

func TestPreflightBDContextUnavailableReason_RequiresFullNonRepositorySignature(t *testing.T) {
	for _, test := range []struct {
		name string
		err  error
		want string
	}{
		{
			name: "exact bd error",
			err:  errors.New("cannot resolve repo context: cannot determine repository root: not a git repository"),
			want: preflightBDContextUnavailableNotGitRepository,
		},
		{
			name: "generic repository wording",
			err:  errors.New("not a git repository"),
		},
		{
			name: "transient root lookup failure",
			err:  errors.New("cannot determine repository root: permission denied"),
		},
		{
			name: "nil",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := preflightBDContextUnavailableReason(test.err); got != test.want {
				t.Fatalf("reason = %q, want %q", got, test.want)
			}
		})
	}
}

func TestReadPreflightIdentityDisk_CorruptOrMissingIsSilentMiss(t *testing.T) {
	city := t.TempDir()
	withFixedPreflightNow(t, time.Unix(1000, 0))
	key := preflightScopeKeyFor(city, "rig-a", "host:3306/db|ext=false")

	// Missing file -> miss, no panic/error surface.
	if _, ok := readPreflightIdentityDisk(city, key); ok {
		t.Fatalf("missing file must be a miss")
	}

	// Corrupt file -> miss, and a subsequent reader must re-spawn.
	if err := os.MkdirAll(filepath.Dir(preflightIdentityCachePath(city)), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(preflightIdentityCachePath(city), []byte("{not json"), 0o644); err != nil {
		t.Fatalf("write corrupt: %v", err)
	}
	if _, ok := readPreflightIdentityDisk(city, key); ok {
		t.Fatalf("corrupt file must be a miss")
	}

	withFreshL1Memo(t)
	calls := 0
	if _, err := preflightBDContextCached(city, key, func() (preflightBDContextValue, error) {
		calls++
		return fakeIdentity(), nil
	}); err != nil {
		t.Fatalf("reader over corrupt cache errored: %v", err)
	}
	if calls != 1 {
		t.Fatalf("run ran %d times, want 1 (corrupt cache is a miss)", calls)
	}
}

func TestReadPreflightIdentityDisk_DistinctScopeKeysDoNotCross(t *testing.T) {
	city := t.TempDir()
	withFixedPreflightNow(t, time.Unix(1000, 0))
	keyA := preflightScopeKeyFor(city, "rig-a", "host:3306/db|ext=false")
	keyB := preflightScopeKeyFor(city, "rig-b", "host:3306/db|ext=false")

	withFreshL1Memo(t)
	if _, err := preflightBDContextCached(city, keyA, func() (preflightBDContextValue, error) { return fakeIdentity(), nil }); err != nil {
		t.Fatalf("populate A: %v", err)
	}

	// keyB must NOT resolve A's entry.
	if _, ok := readPreflightIdentityDisk(city, keyB); ok {
		t.Fatalf("scope B must not read scope A's cached entry")
	}
	// keyA still resolves.
	if v, ok := readPreflightIdentityDisk(city, keyA); !ok || v != fakeIdentity() {
		t.Fatalf("scope A entry = %+v ok=%v, want identity", v, ok)
	}
}

func TestWritePreflightIdentityDisk_PreservesOtherScopeEntries(t *testing.T) {
	city := t.TempDir()
	withFixedPreflightNow(t, time.Unix(1000, 0))
	keyA := preflightScopeKeyFor(city, "rig-a", "host:3306/db|ext=false")
	keyB := preflightScopeKeyFor(city, "rig-b", "host:3306/db|ext=false")

	valA := preflightBDContextValue{Backend: "dolt", BDVersion: "a", SchemaVersion: 1}
	valB := preflightBDContextValue{Backend: "dolt", BDVersion: "b", SchemaVersion: 2}
	if err := writePreflightIdentityDisk(city, keyA, valA); err != nil {
		t.Fatalf("write A: %v", err)
	}
	if err := writePreflightIdentityDisk(city, keyB, valB); err != nil {
		t.Fatalf("write B: %v", err)
	}
	if v, ok := readPreflightIdentityDisk(city, keyA); !ok || v != valA {
		t.Fatalf("A clobbered: %+v ok=%v", v, ok)
	}
	if v, ok := readPreflightIdentityDisk(city, keyB); !ok || v != valB {
		t.Fatalf("B not present: %+v ok=%v", v, ok)
	}
}
