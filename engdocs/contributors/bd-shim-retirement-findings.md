# Retiring bdshim: findings

**Date:** 2026-08-01 · **Author:** gas-city-wbern/architect · **Driver:** William

**Status: MIXED — read this first.**

- **§7 is shipped and validated in production.** The context/preflight fix is
  live: 83.2% fewer `bd context` spawns, against a prediction pre-registered
  before deploying, and independently confirmed by a second method.
- **§5 is verified** to the standard described there (the `resolve_scope` claim),
  **but §5.5's revised `--rig` scope is not reachable** — see §8, which is the
  newest section and the one that changes what to build next.
- **§8 blocks `gcw-yr0o.1` as written.** No verb both routes to the controller
  and honours a rig filter, and three endpoints silently discard `rig=` today.
  §8 also **retracts** a claim of mine from the same day (§8.1).
- **§1–4 are measured but not independently reproduced**, and §2's traffic table
  is now **superseded by §7** — the mix moved after the deploy. Treat those
  sections as evidence to confirm, not as settled fact.
- **Three claims were withdrawn and one recommendation retracted** (§3, §5b, §7).
  They are kept, not deleted, because the failure mode repeated and the method
  lesson is the durable part.

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

>  **SUPERSEDED by §7.** These shares are pre-deploy. `context` has since dropped
>  ~83%, and `gate` doubled on 07-29/30 from a newly-added caller. The *callers*
>  below are still correct; the *shares* are not. Use §7 for prioritisation.

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

> The *per-call* finding above stands. The **priority** conclusion drawn from it
> — "therefore do `show` first" — is **retracted in §7**: `show` is bursty
> (0–56 across eight identical windows) and that snapshot caught it active.

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
    main.loadCityConfig                        150 ms  <- see the caveat below
    beads.OpenStoreAtForCity                   140 ms
      StoreOpenOptions.openNativeStore         130 ms
        main.nativeDoltOpenEnvForScope         120 ms  <- opens a Dolt connection
      contract.PreflightChecker.Check
        PreflightChecker.readBDContext                 <- spawns `bd context`
```

The entry point is `bdBeadExists` (`cmd_bd.go:128`, a package-level func var, so
pprof labels it `main.init.func69`). `resolveBdScopeTarget` matches the target rig
from the bead-ID prefix in argv, then does not trust it: it opens that rig's store
and does a `Get` to confirm the bead exists, throws the store away, and spawns
`bd` to do the actual read. The probe is a deliberate guard against hyphenated
flag values retargeting the command.

So on a single **read**, gc:

1. opens **its own native Dolt connection** (~130 ms) purely to confirm a bead
   ID it had already matched by prefix,
2. runs the store preflight, which **spawns `bd context`**,
3. then spawns `bd`, which opens **another** connection to the same Dolt server,
4. while the controller sits there holding an already-warm store that answers
   the same query in 11 ms.

> **CAVEAT — do not read a second config load into that `loadCityConfig` frame.**
> An earlier revision of this doc claimed gc loads the city config twice, ~300 ms.
> That claim is **withdrawn** (§5b): `openStoreResultAtForCityWithConfig` reloads
> only when `cfg == nil`, and `bdBeadExists` passes the config the caller already
> loaded — the function carries a comment saying exactly that, and it was fixed
> before this investigation started. The frame appears because pprof attributes
> inlined store-open frames to the nil-config variant. The load-bearing finding
> here is the **Dolt connection and the preflight**, not a duplicated config load.

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
- **`--city` must keep the full validating path** — it pins a *different* city,
  and the cheap resolution assumes the ambient one.
- **`--rig` should NOT.** An earlier revision of this doc lumped it in with
  `--city`; that was wrong and gave up the largest win on the fleet for no
  reason. `GET /v0/city/{city}/beads` already accepts a `rig` query parameter,
  verified live and correctly filtered: `?rig=gas-city-infra` → 17.8 ms,
  `?rig=crm` → 18.1 ms, `?rig=gas-city-infra&type=gate` → 12.1 ms, against
  `gc bd gate list --rig gas-city-infra` at 294 ms (~24×). And the fleet's
  single largest bd consumer uses `--rig` *deliberately* (§7). Routing it must
  preserve gc's rig-validation errors — unknown rig, declared-but-unbound rig —
  so they do not degrade into a bare empty result.
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

## 7. Deployed 2026-08-01: the context fix, and what production taught

The preflight-TTL work (`gcw-5oio`, `gcw-75s5`) is **live and validated**. It is
the only change from this investigation that has shipped.

| metric | before | after | |
|---|---|---|---|
| `bd context` calls | 2.120/min | **0.357/min** | **83.2% fewer** |
| failures | 1.067/min | 0.159/min | 85.1% fewer |

Measured over 25.2 min (1.7 TTL cycles) against a baseline captured on the same
metric in the 60 min before the bounce. The prediction — 82%, derived from the
order-cooldown model — was **pre-registered on the bead before deploying**.

Independently confirmed by a second method: the same 36-min clock window on
eight consecutive days, which controls for time-of-day. `context` sat at 77–97
in that window every prior day and was 23 on deploy day.

### It took two deploys, and the failure is the lesson

Deploy 1 (`61eb7e5f8`) moved the live rate 2.120 → 1.984/min. **6.4%** — nothing,
against an isolated single-binary A/B that had shown the mechanism working
perfectly.

The cause was the `gc_build` stamp added in that same commit. **A
session-preserving rebuild-bounce does not replace long-lived detached helpers.**
Two `gc nudge poll` daemons were still running binaries from the previous day.
They kept writing entries with no build stamp; every new process rejected them,
re-probed, and rewrote a stamped entry, which the old process overwrote
unstamped. The live `hq` entry was watched flipping
stamped → unstamped → stamped **inside 90 seconds**. One stale writer poisoned
the cache for the whole fleet, indefinitely.

Deploy 2 (`c45bd6783`) removed the *rejection* — the stamp is still recorded,
because it is what made this diagnosable — and produced the 83.2%.

> **The generalisable rule (`gcw-tr46`): a shared on-disk format must stay
> readable by, and tolerant of, the previous binary, because the previous binary
> is still running after a successful bounce.** Verify with
> `ps -Ao pid=,lstart=,command= | grep 'gc nudge poll'`; anything with an
> `lstart` before the bounce is running old code.

The 83.2% is a **floor** — those stale pollers are still on the old 60 s TTL.

### The traffic mix moved, and one earlier recommendation is retracted

Same eight-day controlled windows:

- **`list` is 51–80 on every single day** — the only high-volume verb that is
  stable, and therefore the best target despite its per-call payoff being
  unproven (the `+62 ms` claim was withdrawn in §5b).
- **`gate` doubled on 07-29/30.** Split by shape: `gate check --escalate` has been
  flat at ~1,080–1,190/day throughout, while `gc bd gate list` was *absent* until
  07-29 and then jumped to ~1,512/day. A new core-pack caller appeared —
  `gate-sweep` (2 m cooldown) → `renudge-stale-human-gates.sh`, which loops
  `gc bd gate list` over HQ and every rig. At 294 ms/call that is ~444 s/day, now
  the largest single identifiable bd cost on the fleet. Its own comment says it
  uses `--rig` *because* that routes through `gc bd` — so it sits deliberately on
  the slow path, and §5.5's revised `--rig` scope is what would fix it.
- **`show` is retracted as the top candidate.** It ranges 0–56 across those eight
  identical windows and was 1 on deploy day. It is bursty and agent-driven, so its
  share depends entirely on when you sample; the earlier snapshot happened to
  catch it active. The per-call analysis on `gcw-9kwl` still stands — what does
  not stand is "therefore do `show` first".

> **Method rule, earned twice:** prioritise on a time-of-day-controlled series,
> never on a single window. A single snapshot produced both the withdrawn `list`
> payoff and the retracted `show` priority.

## 8. The `--rig` routing scope §5.5 promised does not exist yet

Investigated 2026-08-01, later the same day, against the live controller on
`fork-c45bd6783`. §5.5 revised `--rig` *into* scope and §7 named
`gc bd gate list --rig` the largest single identifiable bd cost on the fleet
(~444 s/day). This section checks whether that scope is reachable. **It is not,
for a reason neither section anticipated.**

Confidence labels used below: **VERIFIED** = live, reproduced, n≥3.
**MEASURED** = live, small n. **UNVERIFIED** = argued from source, never
exercised. **RETRACTED** = claimed here earlier and wrong.

### 8.1 RETRACTED: "`resolve_scope` costs nothing"

I profiled `gc bd list --json` (n=5) and found `resolve_scope` at
**0.03–0.09 ms**, and concluded `gcw-yr0o.1`'s premise was falsified. **That
conclusion was wrong.** `resolve_scope` is verb-class-dependent:

| command | `resolve_scope` | why |
|---|---|---|
| `gc bd show <id> --json` (n=3) | **153.9 / 159.1 / 173.7 ms** (~30%) | ID in argv ⇒ `bdBeadExists` opens a store to confirm it |
| `gc bd list --json` (n=5) | **0.03–0.09 ms** | no bead ID ⇒ `resolveBdScopeTarget` never fires |

§5 is right. It profiled `show`; I profiled `list`. The cost is real on
**ID-bearing** reads and absent on list-like ones.

Note the drift worth watching: §5.1 recorded `resolve_scope` at 483.8 ms for the
same command that now measures ~160 ms. That is **consistent with** §7's
preflight-TTL fix removing the `bd context` spawn §5.2 found *inside*
`resolve_scope` — but §5.5 already admits that figure swung 484→617 ms across
runs, so treat this as a corroborating signal, **not** as a measured effect size.

> **Method rule, now earned a third time:** a phase cost is a property of the
> *verb class*, not of `gc bd`. Profile the verb you intend to change.

### 8.2 VERIFIED: routable ∩ rig-aware is the empty set

This is the finding that blocks §5.5's revised scope. A verb can be routed to
the controller, or it can honour a rig filter. **No verb does both.**

| verb | `ClassifyVerb` | in fastpath shape allowlist | honours `rig=` |
|---|---|---|---|
| `list` | **passthrough** (`classify.go:355`) | no | **yes** |
| `show` | **passthrough** (`classify.go:355`) | no | n/a (ID-scoped) |
| `ready` | route | **no** | **no** |
| `query` (ephemeral) | route | yes | **no** |
| `mol current\|progress` | route | yes | n/a (ID-scoped) |

Dispositions are test-verified against `bdshim.ClassifyVerb` directly, not read
off the source. The shape allowlist is `earlyBdExperimentShape`
(`cmd/gc/bd_fastpath.go:298`), which approves **only** `ShapeQueryEphemeral`,
`ShapeMolCurrent` and `ShapeMolProgress`.

The two halves fail for unrelated reasons, so neither is a quick fix:

- **`list` cannot route** because of the output contract at `classify.go:350`.
  Verified by diffing field sets live: `bd list --json` emits `priority`,
  `parent`, `dependencies`, `dependency_count`, `dependent_count`,
  `comment_count`, `notes`, `owner`, `created_by`, `updated_at` — the
  controller's `/beads` emits **none of those ten**. Under §1's *semantic, not
  syntactic* bar the gap is smaller than full `IssueWithCounts` parity, but
  `priority`, `parent` and the dependency edges are load-bearing data, not
  formatting. The refusal comment is accurate and current.
- **`ready` / `query` cannot be rig-scoped** because the rig dimension does not
  exist on them: `BeadReadyInput` and `BeadEphemeralInput` have no `Rig` field
  (`internal/api/huma_types_beads.go:35,47`), while `BeadListInput` does
  (line 27).

There is also a structural reason the sets do not overlap by accident: for
ID-bearing verbs the bead ID *already* determines the rig, and §5.3 verified the
controller resolves cross-rig from a city-scoped request. **`rig=` is only
meaningful for list-like queries — which are exactly the ones that cannot
route.**

### 8.3 FIXED in `bb53ffc8d`: the silent-ignore defect family — three endpoints

> **Status: shipped to `develop`, not yet deployed.** The fix is committed and
> green (full `internal/api` suite, `go vet ./...`, `make dashboard-ci`), and was
> validated over a real TCP round-trip through the production mux — every case
> below now inverts. It reaches the running fleet only on the next
> rebuild-bounce, like `b1fdaef30` and `0227f3a42` before it.
>
> ```
> /beads?rig=no-such-rig            404 rig-not-found
>                                   "rig no-such-rig has no bead store (unknown or unbound rig)"
> /beads/ready?rig=no-such-rig      404 rig-not-found
> /beads/ephemeral?rig=no-such-rig  404 rig-not-found
> /beads/ready?rig=rig2             200, only rig2 — city store no longer federated under ?rig=
> ```
>
> One correction to what this section originally claimed: **the dashboard does
> send `rig=`** (`beadReads.ts:60`). "No production caller" was true of Go
> (`ListBeadsOpts.Rig` is assigned only in `client_test.go`) and false of the
> TypeScript client. It is still safe — `Beads.tsx:146-150` resets the filter to
> ALL whenever it is absent from the live rig list, and a rejecting per-rig
> `listBeads` already degrades the snapshot rather than collapsing it. But the
> claim as written was wrong.

The defect as originally measured, kept because it is the before-side evidence:

Routing `--rig` today would re-ship `0227f3a42`. Proven live, not inferred:

```
GET /beads/ready?rig=gas-city-wbern      -> 200, 402 beads across gc2/crm/gci/gcw
GET /beads/ephemeral?rig=gas-city-wbern  -> 200, identical to no-rig and to rig=nonsense
GET /beads?rig=nope-does-not-exist       -> 200 {"items":[],"total":0}
gc bd list --rig gcw                     -> exit 1, `gc bd: rig "gcw" not found`
```

`ready` is the verb from the original `0227f3a42` report. Three distinct
defects:

1. **`/beads`** honours `rig=` but does not validate it. `huma_handlers_beads.go:130`
   has no `else`: an unknown rig leaves `rigNames` nil and federates nothing.
   gc's loud error degrades into a silent empty 200.
2. **`/beads/ready`** federates city + every rig unconditionally
   (`huma_handlers_beads.go:488`). `rig=` is not a parameter, so it is discarded
   as an unknown query param.
3. **`/beads/ephemeral`** same, at `huma_handlers_beads.go:437`.

Unknown query params are silently dropped generally — `?totallyBogusParam=xyz`
returns a full 200 — so on `ready` and `ephemeral` a `rig=` typo and a real rig
are indistinguishable to the caller. That is strictly worse than an unknown-rig
404: the request *looks* scoped and is not.

Two counter-arguments were tested and **failed**, which is why the fix is safe:

- *"A 404 would misfire on suspended rigs."* No. Suspension gates only
  background refresh (`rigStoreBackgroundRefresh`); `stores[rig.Name]` is
  populated either way.
- *"A 404 would misfire on declared-but-unbound rigs."* That is the intended
  behaviour, already written down: `buildStores` skips unbound rigs *"so the API
  reports no store for the rig and operators notice the unbound state"*
  (`cmd/gc/api_state.go:334-342`). The silent 200 is what defeats that stated
  intent today.

Blast radius is near zero: **no production caller sets `rig=`** on any of these
endpoints — `ListBeadsOpts.Rig` is assigned only in `client_test.go:1838`, and
`ParseListOpts` never populates it. `apierr.RigNotFound` (`rig-not-found`, 404)
already exists in the catalog, so no new problem type is needed.

### 8.4 VERIFIED: the direct path works, and §5.4's projection was right

The 30 ms end-state is not a projection any more:

```
gc bd query --json 'ephemeral=true'   0.10 / 0.03 / 0.03 s   -> 100 real rows
gc bd list  --json                    0.90 / 0.43 / 0.44 s
```

30 ms against a ~505 ms median for `list` — **~17×**, matching §5.4's ~30 ms
almost exactly. The architecture is validated. What is *not* validated is its
reach: it currently serves only `query`-ephemeral and `mol`.

The cost `list` pays instead decomposes cleanly (n=5, medians): `bd_subprocess`
~42%, `load_city_config` ~48% — together ~90%. **Routing removes the subprocess;
the fastpath removes the config load.** The 30 ms figure is what you get only
when both go, which is why neither half alone reaches it.

### 8.5 UNVERIFIED: the gate-list target is the highest-value one, and may be blocked

§7 makes `gc bd gate list --rig` the top fleet cost (~444 s/day at 294 ms/call),
and §5.5 measured `GET /beads?rig=gas-city-infra&type=gate` at **12.1 ms** —
a ~24× headroom on the single largest identifiable line item.

The output contract is unusually small. `renudge-stale-human-gates.sh:152-160`
consumes exactly four fields:

```sh
GATES_JSON="$(gc bd gate list ${RIG_ARG1:+...} --limit 0 --json)"
... | jq -r 'select(.await_type == "human" and .status == "open")
             | "\(.id)\t\(.created_at // "")"'
```

`id`, `status` and `created_at` are already on the controller's bead. **The open
question is `await_type`,** and it is a real risk rather than a detail: the
string does not appear anywhere in Gas City's Go source, so it is a `bd`-side
gate concept. It reaches a controller response only if `bd` persists it into
bead metadata that the controller's `metadata` map passes through.

**This could not be tested.** There are zero gate beads in the city right now
(`gc bd gate list` returns `null`; `?type=gate` returns `total: 0`), so there
was nothing to inspect. Do not treat gate routing as shovel-ready until one gate
bead has been checked for `await_type` through `/beads`.

Note also that `gate` is not in the fastpath allowlist *or* in `RoutedVerbs`, so
this needs a new routable shape as well as the rig plumbing — two pieces, not
one.

### 8.6 What this changes

- **`gcw-yr0o.1` should not be built as written.** Its ID-bearing-read premise
  (§8.1) is sound, but the `--rig` half it was revised to include has no valid
  target verb (§8.2). Split the bead: keep the cheap-controller-reach half,
  drop or re-scope the `--rig` half.
- **The rig-validation work is worth doing on its own** (§8.3) — small, uses an
  existing error type, near-zero blast radius, and it is the guard that makes
  any later rig routing safe. It is a *prerequisite*, not a competitor.
- **`--rig` routing is gated on a routable rig-aware verb existing.** Today that
  means either giving `ready`/`ephemeral` a rig dimension, or closing enough of
  `list`'s ten-field gap to route it under §1's semantic bar.
- **§5.5's `--rig` bullet is correct about the endpoint and wrong about the
  reach.** `GET /beads?rig=` genuinely is ~24× faster; no verb that can reach it
  honours a rig.

## 9. Re-baseline, 2026-08-01 — five of the plan's numbers were wrong

Everything below replaces the corresponding figure in §1–§8. The earlier mix came
from a 6,000-call / 14.1h window; this is **88,331 calls since 2026-07-26**, read
from `~/gc2/.gc/bdshim.log`. Where the two disagree, prefer this one — but note
the log spans binaries, so per-verb *disposition* is only trustworthy for the
current binary (`show` appears to route in older rows; today it never does).

### 9.1 Verb mix

| verb | plan | measured | note |
|---|---|---|---|
| `context` | 20.6% | **24.7%** | now redirected off the shim entirely (`5e56f2b6e`) |
| `list` | 31.0% | **23.3%** | still the largest routable gap |
| `ready` | 10.2% | **16.2%** | routed |
| `gate` | 24.6% | **13.9%** | only 38% of it is routable — see 9.3 |
| `show` | 6.8% | **8.3%** | |
| `dep` | 1.7% | **5.8%** | measured *slower* routed; stays on real bd |
| `query` | 2.6% | **1.8%** | routed |
| `update` | — | **1.2%** | routed |

### 9.2 The ≥95% coverage gate is wrong in both directions

**It is unnecessary.** At the inherited CPU-time constants (fast ≈35ms,
`doBd` passthrough ≈450ms, shim blended ≈188ms), "within 90% of shim" means a
207ms ceiling, which needs only **≈59% coverage** — 59% × 35 + 41% × 450 ≈ 205ms.

**It is also unreachable.** ~12.7% of shim-facing traffic must never route, and
each of those is now a settled correctness decision rather than pending work:
`create` (no id ⇒ no resolvable store, `5188bd0eb`), `dep` (measured slower),
`note`/`comment`/`unknown` (no path). Ceiling ≈87%.

Use **59%** as the gate. Coverage after the commits of 2026-08-01 is ≈42.7%.

### 9.3 `gate` is 7.1 points, not 24.6

Only `gate list --json [--limit]` is routable: 4,687 of 12,366 gate calls. The
other 62% is `bd gate check`, which evaluates gates and **closes** the resolved
ones — a mutation with no controller equivalent. Routed in `b0d5d9cfc` as a
shape allowlist, not a verb allowlist.

### 9.4 The list/show projection gap was ten fields, then six, now three

`priority`, `parent`, `dependencies` and `updated_at` had already landed when the
plan still listed them. Of the remaining six, `await_type` (`c86f146c9`) and
`created_by`/`owner`/`notes` (`1f2d14628`) are plain columns and are done.

Left: **`comment_count`, `dependency_count`, `dependent_count`** — all
backend-computed over relations and comments, which is the expensive part and the
whole remaining blocker for `list` (21.2 pts) and `show` (11.2 pts).

Consumer survey found **zero** fleet consumers of any of the six; every
`bd list/show --json` consumer reads `.metadata`, `.status`, `.id`, `.title`. So
the risk being bought is an unknown future consumer, not a known one.

### 9.5 Method notes earned this round

- **The route log's `dur_ms` is unusable for latency.** It is wall clock on a
  loaded box: the median for `bd version` reads as 49 seconds. Use it for mix and
  disposition only; latency must come from CPU-time measurement.
- **A hand-written mapping can silently undo a wire change.** `beadFromGen`
  (`internal/beadclient/wire_shared.go`) converts genclient → `beads.Bead` by
  hand, so four fields added to the type and the spec were still dropped on the
  way back until it was updated. A spec-in-sync check cannot catch this; only a
  dispatch-level test did.
- **Adding a column to the main list SELECT is a fleet risk.** Resolve it through
  `tableHasColumnCtx` as the ephemeral/no_history flags do, so a snapshot at an
  older migration yields empty rather than erroring every list read.
