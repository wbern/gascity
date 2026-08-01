# Retiring bdshim: findings

**Date:** 2026-08-01 · **Author:** gas-city-wbern/architect · **Driver:** William

**Status: PROVISIONAL.** Section 5 (the `resolve_scope` claim) is verified to
the standard described there. Everything else is measured but not independently
reproduced, and two claims in section 3 were withdrawn after re-checking — treat
the rest as evidence to confirm, not as settled fact.

Tracking: epic `gcw-yr0o`. Related: `gcw-5oio`, `gcw-9kwl`, `gcw-a7z0`,
`gcw-s5i0`, `gcw-2wi0`, `gcw-joxf`.

## 1. The decisions this rests on

Two things William ruled **out** of the equation, both deliberately:

1. **Byte-identical `bd` output.** The consumer on this path is an LLM, not a
   script. The compatibility bar is **semantic** (never silently omit beads,
   never return stale or wrong values), not **syntactic** (identical field set
   and ordering).
2. **gc's binary size**, and the ~9 ms/call plus page-cache argument for keeping
   a second small binary. Not worth it.

And one ruling **in**: no early escape to `bd`, no shim-inside-gc. Bead
operations go through the gc controller; get the speed from gc internals and
indexing.

## 2. Where the traffic actually comes from

Measured from `$GC_CITY_PATH/.gc/bdshim.log` (60k recent calls):

| verb | share | disposition | caller |
|---|---|---|---|
| `context` | 26.1% | passthrough | **gc itself** (proven) |
| `list` | 22.5% | mostly passthrough | mixed |
| `ready` | 20.1% | **routed** | agents + gc |
| `gate` | 15.5% | passthrough | **gc itself** (proven) |
| `show` | 6.6% | mixed | agents |
| `dep` + long tail | ~9% | passthrough | mixed |

**Roughly half of "bd traffic" is not an agent.** `context` and `gate` were both
caught live: their callers are core-pack order scripts under `gc supervisor run`
and gc's own store-open preflight. gc's own fleet scripts additionally call
`bd list` 64× and `bd show` 22× across the tree.

The LLM's actual hot path — `ready`, `update --claim`, `close` — **already
routes**.

## 3. Two claims that did not survive re-checking

Recorded because the method matters more than the numbers.

**`list` "+62 ms proven" — WITHDRAWN.** It came from the 2026-07-28 flip that
switched show/list from route to passthrough. A before/after switchover is not
an A/B. The arms never share an hour (route 00–13h, passthrough 14–23h; the one
overlapping hour has n=3 at p50=2579 ms, the flip moment), and within-arm hourly
variance (route 41–364 ms, passthrough 152–443 ms) overlaps almost entirely.

**`show` "+152 ms" — SURVIVES**, despite the same time separation, because the
bands do not overlap at all (route 1–10 ms vs passthrough 113–283 ms) and an
internal control rules out machine drift: in the *same hours*, with both verbs
routed, `show`-route was 1–10 ms while `list`-route was 41–364 ms (14–121×). The
morning was not uniformly fast.

> **Method rule:** a before/after switchover is not an A/B on a machine with this
> much hourly variance. `internal/bdexperiment` already does randomized
> **per-call** arm assignment, which is the design that defeats this. Any future
> payoff claim must come from that harness.

A third correction: a live "proof" that attributed subprocess spawns by
ps-polling on PPID reported a false zero — the poller missed a ~120 ms child. The
reliable signal for that class of question is the **cache write** (a re-probe
always writes; a cache hit never does), not process polling.

## 4. Controller vs direct-to-bd

Measured live, 2026-08-01:

| path | time |
|---|---|
| `GET /beads?limit=20` — served from the warm index | **7.0 ms** |
| `GET /bead/{id}` — served from the warm index | **11.0 ms** |
| `GET /beads/ready` — shells out to bd per rig | **195.6 ms** |
| `bdshim show` / `raw bd show` | 165 / 160 ms |
| `raw bd ready` | ~106 ms |

Where the controller uses its index it is **15–28× faster than bd**. The one
endpoint that looks bad is bad *precisely because it escapes to bd internally* —
`humaHandleBeadReady` calls `.Live.Ready()` per rig, sequentially, and each
`.Live` shells out to the bd binary. `CachingStore.Ready()` answers the same
query in-memory in 44.8 µs with the same result set (`gcw-s5i0`).

Direct-to-bd never wins on merit. It only ever looked competitive because the
controller is sometimes a bd *proxy* where it should be an *index*.

**Neither gc nor the controller is slow. Both do expensive work to avoid
trusting an index that already exists.**

## 5. VERIFIED: `resolve_scope` is ~484 ms and a routed read needs none of it

This is the one claim validated to the 100% standard.

### 5.1 Cost (phase profile, `GC_BD_PROFILE_DIR`, `gc bd show --json`, n=3)

Phases are nested; children sum inside parents.

```
total                             781.6 ms
  command_execute                 779.0 ms
    resolve_scope                 483.8 ms   <- 62% of total
      load_city_config            147.6 ms
        config_builtin_pack_includes 117.9 ms
    bd_subprocess                 122.3 ms
    prepare_subprocess             25.3 ms
  command_tree (cobra)              1.4 ms
  early_shim_probe                  0.0 ms   <- show is not fastpath-eligible
```

`resolve_scope` had 336 ms unattributed, unexplained since `gcw-2wi0`.

### 5.2 What that 336 ms is (CPU profile, `go tool pprof -peek`)

63% of samples are in `syscall.rawsyscalln` — this is I/O, not compute. The
chain, resolved by peeking each frame:

```
resolve_scope
  main.openStoreAtForCity                      290 ms  (51% of CPU samples)
    main.loadCityConfig                        150 ms  <- a SECOND config load
    beads.OpenStoreAtForCity                   140 ms
      StoreOpenOptions.openNativeStore         130 ms
        main.nativeDoltOpenEnvForScope         120 ms  <- opens a Dolt connection
      contract.PreflightChecker.Check
        PreflightChecker.readBDContext                 <- spawns `bd context`
```

So on a single **read**, gc:

1. loads city config **twice** (~300 ms total),
2. opens **its own native Dolt connection** (~130 ms),
3. runs the store preflight, which **spawns `bd context`**,
4. then spawns `bd`, which opens **another** connection to the same Dolt server,
5. while the controller sits there holding an already-warm store that answers
   the same query in 11 ms.

Three connections to the same data and two config loads, to answer a question
the controller answers in 11 ms.

This also independently confirms, by a completely different method, why
`context` is 26% of bd traffic: it is gc's own store-open preflight, reached
here directly on the `gc bd` read path.

### 5.3 That work is not needed to reach the controller

`gcw-yr0o.1` proposes resolving the controller target cheaply instead:
`cityName = basename(GC_CITY_PATH)`, `baseURL = GC_API_URL | ~/.gc/supervisor.toml
port | default 127.0.0.1:8372`. Env plus one small file read.

**Verified:** one city-scoped controller request resolves beads belonging to
*different rigs*. Real bead IDs, single `GET /v0/city/gc2/bead/{id}`:

| rig | bead | result |
|---|---|---|
| gas-city-wbern | `gcw-5oio` | HTTP 200 |
| hq | `gc2-d4r7t` | HTTP 200 |
| crm | `crm-uy153y` | HTTP 200 |
| gas-city-infra | `gci-ptxu` | HTTP 200 |

The controller does rig resolution **server-side**, which is architecturally
where it belongs — it is the component that actually knows the city. gc does not
need local scope resolution to route a read.

### 5.4 Projected cost

```
gc pre-cobra decision   ~21 ms   (arm=shim 70 ms vs bdshim standalone 49 ms)
controller cached read  ~7-11 ms
                        -------
                        ~30 ms   vs 781.6 ms today
```

A routed read pays **none** of `resolve_scope` (484 ms) and **none** of
`bd_subprocess` (122 ms).

### 5.5 Limits of this validation — read before extending it

- **Reads only.** Writes (`close`, `update`) go through gc's write-guard and
  close gate, which legitimately read the store (see `690675170`,
  "reuse the write-guard's store read in the bd close gate"). Do **not** blanket-
  apply this to writes without separate validation.
- **Explicit `--city` / `--rig` must keep the full validating path.** Those ask
  gc to validate a specific scope. `hasExplicitBdScopeFlag()` already encodes
  this carve-out; reuse it rather than inventing a new condition.
- **Cross-rig resolution proven for 4 of 6 rigs.** `statusline` and
  `gascity-packs` had no beads to test with.
- **Controller-down behaviour is undecided.** bdshim fails loud (rc=1) for
  routed reads rather than silently passing through to a cwd-scoped bd that
  cannot answer a city-wide question. gc must make the same choice explicitly.
- **n=3** for the phase profile, on a busy workstation. The 484 ms figure varied
  (one run showed 617 ms). The *shape* is stable and is what the argument rests
  on; the absolute is not precise.

## 5b. Inefficiencies: what survived adversarial review

Each candidate was attacked before being filed. Three survived (`gcw-yr0o.6`,
`.7`, `.8`); three did not. The refutations are recorded because the failure mode
repeated.

### Survived

- **`bdBeadExists` opens a Dolt store per candidate rig** to confirm a bead ID —
  260 ms, 43% of CPU samples on a read (`gcw-yr0o.6`). `resolveBdScopeTarget`
  matches the rig from the bead-ID prefix, then does not trust it: it opens that
  rig's store and does a `Get`, throws the store away, and spawns `bd` to do the
  real read. The probe is a real guard (it stops hyphenated flag values
  retargeting the command), so it cannot just be deleted. **Its value depends on
  `.1`:** a routed read never calls `resolveBdScopeTarget` at all, so this cost
  vanishes for reads and is only independently worth fixing for writes and
  explicit-scope invocations. Sequence it after `.1` and re-measure.
- **Ready-federation parallelism is shipped, tested, and off** (`gcw-yr0o.7`).
  Already implemented at `huma_handlers_beads.go:515-539` with a process-global
  semaphore, a max-8 clamp, and tests for both paths — gated on
  `GC_READY_FEDERATION_CONCURRENCY`, which defaults to 1. See 5c: the win is
  **not** quantified.
- **The telemetry log is unbounded** (`gcw-yr0o.8`), 65 MB and growing. It
  matters because `.4` requires the telemetry to *survive* retirement, so the
  successor must not inherit an unbounded design. Archive, do not delete — it is
  the before/after baseline.

### Refuted

- **"gc loads the city config twice on a read (~300 ms)."** Wrong.
  `openStoreResultAtForCityWithConfig` reloads only when `cfg == nil`;
  `bdBeadExists` passes the loaded config, and carries a comment saying exactly
  that. Already fixed. The claim came from summing **nested** phase data
  (`load_city_config` 147.6 ms and a pprof `loadCityConfig` frame at 150 ms are
  the *same* load) compounded by pprof attributing inlined frames to the
  nil-config variant.
- **"`--rig` is what makes `gc bd gate list` slow."** Wrong — measured with and
  without: 294.1 vs 292.6 ms. `gate` simply is not fastpath-eligible.
- **The ready-federation cost model** — see 5c.

> **Lesson, now three times over:** nested profile data, inlined pprof frames,
> and cost models all invite confident double-counting. Confirm a
> duplicated-work claim by reading the call site, and validate a model against a
> known measurement *before* using it to predict.

## 5c. The ready-federation experiment refuted its own model

Method: build a cost model, validate it against the measured endpoint, and only
then predict the parallel win. The validation step failed.

```
per-store `bd ready --json --limit 0`, n=5 each — remarkably uniform:
  city/hq 151.1 | gas-city-wbern 152.8 | gascity-packs 151.8
  crm 154.8 | gas-city-infra 150.5 | statusline 150.9
MODEL sequential (sum of 6)     = 911.9 ms
MEASURED GET /beads/ready n=10  = 200.3 ms   (min 191.5, max 215.7 — tight)
                                  366% error
```

The controller cannot be paying six sequential cold bd spawns: 200 ms total is
less than **two** cold shell invocations. Its real per-source cost is ~33 ms, not
~151 ms — roughly 4.5× cheaper, cause not yet determined (plausibly a warm
long-lived process with page cache and pre-resolved env, no shim indirection, or
sources that are not all cold bd-backed).

**This makes `gcw-s5i0`'s root cause stale.** Its "~5–6 sequential bd subprocess
spawns per ready call = ~355 ms" no longer reproduces, and its 2800× cache-serve
figure was derived from the same decomposition — so it must be re-derived before
being used to justify touching freshness semantics, which is the genuinely risky
option since ready gates dispatch.

**No speedup number should be quoted for parallelism.** What is solid: the code
is shipped, tested on both paths, semantically identical (same `.Live` reads,
scheduling only), clamped, and reverts by unsetting one env var. What is not
solid: how much it buys. `GC_READY_FEDERATION_CONCURRENCY` is read once at
startup, so validating it needs a controller restart — the same bounce gated on
`gcw-75s5`.

## 6. What retirement still needs

See `gcw-yr0o`. In short: reach the controller cheaply (`.1`), make the
controller serve `ready` from its index rather than `.Live` bd spawns
(`gcw-s5i0` — the biggest single win, but it gates dispatch, so the freshness
semantics need maintainer sign-off), decouple the fastpath from the bdshim
binary (`.2` — today even the in-process arm is gated on the shim existing),
widen controller coverage (`.3`), and eliminate gc's own self-calls (`.5`).

**Do not remove bdshim before `.4`.** Its JSONL log is the only traffic
telemetry this fleet has; every number in this document came from it. gc's
`cmd/gc/route_log.go` is not equivalent — it emits `route=...` debug lines to
stderr, not a durable JSONL. Removing the shim first makes the fleet
unmeasurable and makes the retirement unprovable.

### The only exec that remains, and why it is not a shim

About 1% of traffic is not bead CRUD at all — `bd prime`, `config`, `version`,
`types`, `remember`/`recall`/`memories`. That is bd's own tooling, not queries
against the ledger, and the controller has no business projecting it. Those
either keep invoking bd directly or leave gc's surface. Either way they are off
the hot path, and the rule holds without exception: **every bead operation goes
through the controller.**
