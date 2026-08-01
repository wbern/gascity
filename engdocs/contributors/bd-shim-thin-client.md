# bdshim — the tiny bd thin client

**Date:** 2026-07-19 · **Author:** gas-city-wbern/architect · **Driver:** William
(overnight goal: "focus on the shim, properly land it as a smart little trick,
load the city, measure measure")

`cmd/bdshim` is a ~18 MB pure-Go bd-CLI-compatible front end that replaces the
117 MB gc-as-bd shim on an agent's PATH. It routes the cache-servable verbs
through the already-warm controller HTTP API and execs the real bd
(`GC_BD_REAL`) for everything else — **without paying gc's ~200 ms cold-start
per bd call.**

## Why (historical live gc2 measurement)

The felt slowness had a concrete cause: every agent `bd` call cold-started the
117 MB gc binary (the fork symlinks `bd` → gc). Before the compatibility
boundary below, boot-floor and end-to-end warm-cache runs measured:

| | boot floor | show | list | ready |
| --- | --- | --- | --- | --- |
| fat gc-shim (117 MB) | 228 ms | 230 ms | 265 ms | 650 ms |
| **bdshim (18 MB)** | **9.6 ms** | delegates to bd.real | delegates to bd.real | **367 ms** |
| raw bd.real (187 MB) | 99 ms | 99 ms | 119 ms | 110 ms |

Key findings that shaped the design:

- **Size is not boot cost.** bd.real is *bigger* (187 MB) than gc (117 MB) yet
  boots faster (99 vs 228 ms). The driver is package-level `init()` breadth — gc
  wires the whole orchestration graph at startup; bd only wires the ledger. Only
  a genuinely small program with a tiny init surface boots fast; CGO=0 (which
  halved gc's *size*) did not touch the boot floor.
- **The fat gc-shim was 2–6× slower than plain bd.real** on real reads (ready
  620 vs 107 ms, list 232 vs 109 ms) — it bought warm-cache routing at the price
  of a gc cold-start per call. bdshim retains the small-process benefit for
  safely routed verbs; compatibility reads intentionally pay real-bd latency.
- **`ready` is still slow (367 ms)** because the controller's ready endpoint
  itself is ~355 ms — it shells out `bd ready` per rig (~5–6×) via `.Live`
  instead of serving from its warm in-memory cache. That is a separate lever,
  tracked in **gcw-s5i0** (deferred: it gates dispatch, needs maintainer review).

## Design

A faithful behavioral clone of `cmd/gc`'s `runBdShim`, as a small standalone
binary that imports only the dependency-light `internal/bdshim` (classifier) and
`internal/bddispatch` (→ `internal/api`), never the SDK's config/session/worker
wiring.

- **splitPhase pinned false** to match gc's `graphStoreSQLiteEnabled` (hardcoded
  false → identity phase). In this phase passthrough delegates to raw bd
  unchanged. Routed operations still have a distinct, controller-federated
  scope; they are not generally substitutes for a raw cwd-scoped bd command.
- **No token** — the controller ignores `Authorization` on localhost.
- **Resolution (light):** cityName = `basename(GC_CITY_PATH)`; baseURL =
  `GC_API_URL` | `~/.gc/supervisor.toml` port | default `http://127.0.0.1:8372`.
- **Routed verbs** reuse `bddispatch.DispatchViaAPI` → the same controller
  endpoints the fat shim used. Their output contract is the controller's typed
  projection, not a blanket promise of raw-bd byte identity.
- **Compatibility reads:** `show` and every `list` shape delegate to
  `GC_BD_REAL`. `bd show --json` emits `IssueDetails` and `bd list --json`
  emits `IssueWithCounts`, including backend-computed fields the controller
  `Bead` does not retain. In a future split-store phase they must refuse loudly
  until a complete typed controller projection exists, rather than silently
  omitting graph-resident data.
- **Passthrough** execs `GC_BD_REAL` with the caller's inherited env + cwd
  (correct for an agent, whose env/cwd already scope the store).
- **Controller-down:** routed reads/writes **fail loud (rc=1)** rather than
  silently passing through to the work-only bd (whose cwd scope cannot answer a
  city-wide read — a silent passthrough returns wrong/empty). Only the
  infrequent **claim** path probes liveness and falls back to bd.real's atomic
  claim (a correct, just-not-warm substitute).
- **Structured JSONL log** at `$GC_CITY_PATH/.gc/bdshim.log` (or
  `GC_BDSHIM_LOG`) records verb / disposition / exit / dur_ms for insight.

## Install

`ensureCityBdShimbin` (cmd/gc/bd_shimbin.go) points `.gc/shimbin/bd` at bdshim
**when it is installed beside the gc binary** (`bdShimTarget` / `bdshimBesideGC`,
following the gc symlink), and falls back to the gc-as-bd shim when bdshim is
absent — so a partial or rolled-back install always keeps a working `bd`. The
Makefile `build`/`install` targets produce and place bdshim beside gc (CGO-free,
stripped). `GC_BD_REAL` + `ZDOTDIR` injection are unchanged.

## Validation

Compatibility verified live: candidate `show` and `list` output (including
stderr and exit status) is byte-identical to `bd.real` because both now execute
the same real command. Unit tests pin this boundary, the future split-store
refusal, scope-flag extraction, heartbeat rewrite, city/URL resolution,
passthrough exec, claim fallback, and loud-fail-on-down for still-routed reads.

## Status

> **SUPERSEDED as strategy, 2026-08-01 — bdshim is being RETIRED.** William ruled
> out both premises this document rests on: byte-identical `bd` output (the
> consumer here is an LLM, not a script) and the binary-size argument for keeping
> a second small binary. The direction is now that every bead operation goes
> through the gc controller, with no early escape to `bd`. See
> [bd-shim-retirement-findings.md](bd-shim-retirement-findings.md) and epic
> `gcw-yr0o`.
>
> The measurements below remain accurate and useful — in particular the boot-floor
> decomposition and the "size is not boot cost" finding. What has changed is the
> conclusion drawn from them. The controller serves the same reads from its warm
> index in 7–11 ms, i.e. **15–28× faster than bd**, so the shim's advantage was
> only ever over *cold bd*, never over the index.

Landed on `develop` (abc02d6f5). Live via a manual `.gc/shimbin/bd` → bdshim
swap (holds until the next supervisor restart; `ensureCityBdShimbin` is
start-only). Durable install lands via a blessed `gc-rebuild-bounce` of develop
(devops requested — the new gc's `bdShimTarget` + Makefile make it survive
future bounces). Rollback: `ln -sf .gc/shimbin/gc .gc/shimbin/bd`.

Remaining ready lever: **gcw-s5i0** (controller ready endpoint cache-serve) —
note its root-cause decomposition was re-measured on 2026-08-01 and **no longer
reproduces**; see the retirement findings §5c before acting on it.
