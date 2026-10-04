---
status: accepted
date: 2026-10-04
complexity: standard
applies_to:
  - "cmd/bdshim/**/*.go"
  - "internal/bdshim/**/*.go"
  - "internal/bddispatch/**/*.go"
  - "cmd/gc/cmd_bd.go"
  - "cmd/gc/bd_*.go"
pre_filter:
  - "bdshim"
  - "bddispatch"
  - "bd_shim"
  - "GC_BDSHIM"
  - "shimbin"
---

# 8. Retire bdshim: `bd` on PATH is real bd; fleet runs `bd_shim = "off"`

Supersedes [ADR-0002](0002-bdshim-stays-gc-bd-does-not-become-standalone.md).

**Decided by William on 2026-10-04** (gcw-sx0km): skip the "slim" step and
retire. ADR-0002 recorded the opposite direction on 2026-08-08. Timing was
agreed with the fleet operator: gc2 first, then GC3 after a soak.

## Context

ADR-0002 kept bdshim because retiring it bought little, and the one capability
that looked shim-only (federated `bd ready`) turned out to be a scope choice
served by `gc`'s own API. Since then:

- **gc runs native beads v1.3.1, and bd is 1.3.1 everywhere** (gcw-o4qtt S5:
  gc2 and GC3 at schema 66/26). gc's own store access no longer depends on the
  shim.
- **The shim is the slow path.** gc2 `bdshim.log`, last 50k records: routed
  `ready` median **9.8s** (n=554) against passthrough **0.9s** (n=35,485).
  Dispositions: 44,926 passthrough, 4,186 route, 45 refuse. Nearly all traffic
  is passthrough already, so the shim adds a hop and fork cost for no routing
  benefit (study attached to gcw-sx0km).
- **The shim is fork-only.** There are 0 shim files on `upstream/main`. It is
  ~5.4k lines in 4 core paths (~10k including related code), and every upstream
  sync must carry it.
- **What the shim alone provides today:** (1) the managed-session HQ-store guard
  for bare `bd` (the city.toml allowlist); (2) an output-size cap on bare `bd`
  reads; (3) the `bdshim.log` traffic log. William decided (1) is not needed.
  (2) and (3) are conveniences, not invariants.
- **Its known defect stays with it.** `gcw-cdgj`: `bd ready` advertises
  federated IDs that `bd show` then cannot find.

## Decision

1. The fleet runs with `[session] bd_shim = "off"`. `bd` on PATH in managed
   sessions is real bd. Cross-rig reads use `gc bd` (prefix-routed), as
   `AGENTS.md` already instructs.
2. `gc bd` keeps reaching `bd` by exec. ADR-0002's ruling against an in-process
   `bd_fastpath` stands; this ADR does not reopen it.
3. The bdshim code stays in the tree, dormant behind the existing switch, for
   one release cycle as the rollback path (`bd_shim = "auto"`). Deleting
   `cmd/bdshim`, `internal/bdshim` and `internal/bddispatch` is a follow-up,
   done only after a full cycle with no rollback.

## Consequences

- **Lost on purpose:** the bare-`bd` HQ-store guard, the bare-read output cap,
  and the per-call traffic log. Anyone who needs per-call bd telemetry
  reinstates it outside the shim (gcw-yr0o.4 lineage) rather than reviving
  routing.
- **`bd ready` in a rig returns that rig's ready work only.** Federation is
  `gc bd --city <path> ready` or the API. Prompts or packs that relied on
  federated bare `bd ready` must move to `gc bd`. Audit before rollout.
- **Removing `.gc/shimbin` is destructive to sessions kept across a bounce.**
  While the shim is on, managed sessions get `GC_BIN=<city>/.gc/shimbin/gc` and
  a ZDOTDIR that fronts `.gc/shimbin` on PATH (`cmd/gc/bd_shimbin.go:92-99,
  :151+`). `bd_shim = "off"` runs `removeCityBdShim`, which does `RemoveAll` on
  the whole directory. A preserved session (as gc2's control-dispatchers were
  on 2026-10-04) keeps the stale `GC_BIN`, so any later `$GC_BIN ...` fails
  with "no such file". The rollout must therefore recycle every managed session
  in the same window. Any ops file kept in `.gc/shimbin` (the zstd guard) must
  move out first.
- **Upstream syncs get lighter** once the dormant code is deleted.
- **Rollback** is `bd_shim = "auto"` plus a bounce plus a session recycle. The
  launchd PATH entry and the GC3 `GC_BD_REAL` pin are restored from the
  pre-change copies.
