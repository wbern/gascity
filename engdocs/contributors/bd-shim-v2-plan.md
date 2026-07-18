# bd-shim v2 — implementation plan (route the hot passthrough verbs)

**Bead:** gcw-j8oq · **Author:** gas-city-wbern/architect · **Date:** 2026-07-18
· **Depends on assessment:** `engdocs/contributors/dolt-bd-shim-value-assessment.md`

## Goal & why

The single local managed `dolt sql-server` burns CPU on **connection churn**,
not query work: ~445 new connections/min, driven by subprocess-per-operation —
every agent `bd` call spawns a fresh process that opens a short-lived Dolt SQL
connection. The bd-shim already routes `show/ready/update/create/close/--claim/
mol/heartbeat` through the warm controller (one pooled connection + in-memory
`CachingStore`). v2 routes the **still-passthrough verbs that dominate the
churn**, so those connections collapse into the warm pool.

Priorities are **telemetry-driven** (from `~/gc2/.gc/bd-bypass.log`, 21,183
passthrough calls over 15h) and **feasibility-ranked** (what the warm cache can
actually serve without a per-call Dolt hit).

## Passthrough traffic (15h window) & routing verdict

| Verb | calls/15h | dominant shape | cache-servable? | routing verdict |
| --- | --- | --- | --- | --- |
| `list` | 6506 | hook `--status in_progress --assignee` (2115); refinery `--metadata-field pr_number --exclude-type=epic` (3315) | **open work: YES** (warm map, 0 Dolt); `--all`/closed: no | **Slice 1** (biggest, lowest effort) |
| `query` | 2203 | `ephemeral=true AND status=…` (2192) | wisp tier — **pools only** (relocates read, cuts the dial) | **Slice 2** |
| `dep list` | 563 | `dep list <id> --json` (560) | `down`: YES; `up`: no | **Slice 3** |
| `stats`/`count` | 0 | — | Counter unimpl on NativeDolt | **DROP from scope** |
| *(`bd context`)* | **10245** | `bd context --json` (gc-invoked) | — | **NOT a v2 verb → separate bead** (biggest single source) |

Honest framing: routing a verb helps CPU whenever it removes a **direct worker
dial** (that's the churn), even if the controller still executes the read. For
*open-work* `list`/`dep-down` it's a double win (no Dolt hit at all). For
`query`/ephemeral it's connection-pooling only. Anything needing strict
freshness (lifecycle gates) must keep `Live=true` and should NOT be routed.

---

## Slice 0 (PREREQUISITE) — order/gate-exec must inject `GC_BD_REAL`

**Why first:** DevOps hit 182 order failures because order-exec'd `bd` calls
reach the shim with no `GC_BD_REAL` → `dispatchBdShimArgv0` refuses
(`cmd/gc/cmd_bd_shim.go:1189-1192`). They band-aided it in the supervisor plist;
the durable gc-core fix must land before we widen routing (more routed verbs =
more shim exposure on the order path).

Session-exec does this correctly: `sessionGCBinForCity` sets
`agentEnv[GC_BD_REAL]` via `resolveRealBdExcludingDir` (`cmd/gc/bd_shimbin.go:
148-170,178`), wired at `cmd/gc/template_resolve.go:322`.

Order-exec has **two env-construction sites that omit it**:
1. `internal/convergence/condition.go:79` `ConditionEnv.Environ()` — builds a
   whitelisted env; passes `GC_INTEGRATION_REAL_BD` (`:129-130`) + BEADS/DOLT
   keys but **not `GC_BD_REAL`**. Fronts PATH via `conditionPATH()` (`:31-53`)
   which can resolve `bd`→shimbin.
2. `internal/orders/triggers.go:301` `checkCondition` — `cmd.Env =
   mergeConditionEnv(os.Environ(), opts.ConditionEnv)`; `ConditionEnv` is never
   populated with `GC_BD_REAL` (reachable caller passes `TriggerOptions{}`).

**Fix:** resolve `GC_BD_REAL` for the order's city via
`resolveRealBdExcludingDir(cityBdShimbinDir(cityPath))` and inject at both sites
(add to the `condition.go:79` whitelist next to the `GC_INTEGRATION_REAL_BD`
block; thread through `TriggerOptions.ConditionEnv` for `triggers.go:301`).
**TDD:** table test asserting a constructed order/gate condition env contains an
absolute, stat-able `GC_BD_REAL`; regression test that a condition script
running `bd ...` under a fronted PATH resolves the real bd, not a refusing shim.

---

## Slice 1 — route `bd list` (open-work shapes)

Endpoint (`GET /beads`) and client (`client.ListBeads`, `internal/api/client.go:
1000-1043`) **already exist** — this slice is mostly shim wiring.

Steps (all `cmd/gc/cmd_bd_shim.go` unless noted):
1. Add `"list": true` to `bdShimRoutedVerbs` (`:76`).
2. Add `bdListRoutableFlags` map + `bdListRoutable(args)` predicate, modeled on
   `bdReadyRoutable` (`:207-240`). **Scope the allowlist to cache-servable
   flags:** `--status/-s`, `--assignee/-a`, `--type/-t`, `--label/-l`,
   `--json`, `--limit/-n`, `--all`. Any other flag → predicate false →
   passthrough (the closed-allowlist safety property: unknown shapes degrade to
   real bd, never silently mis-answer).
3. Classifier: add `case "list": if !bdListRoutable(args) { return bdPassthrough }`
   in the `bdShimRoutedVerbs` switch (`:309-328`).
4. Dispatch: add `case "list":` in `dispatchBdShimVerbViaAPI` (`:415-535`) →
   `parseBdListOpts(args)` (model on `parseBdReadyParams` `:862-906`) →
   `client.ListBeads(opts)` → `writeReadyJSON` (`cmd/gc/bd_shim_helpers.go:29`).

**Covers the 2115-call GUPP hook shape** (`--status in_progress --assignee`) —
clean warm-cache win. The **3315-call refinery shape** uses `--metadata-field
pr_number` + `--exclude-type`, which `ListBeadsOpts`/`ListQuery` do **not**
express today, so it stays passthrough under the closed allowlist (safe, just
not yet a win).

**Slice 1b (optional, decide later):** add metadata-field + exclude-type
filtering to `ListBeadsOpts`/`ListQuery` + the `/beads` handler to capture the
3315 refinery calls — **or** fix the refinery to poll less (it's a PR-status
poll; a coarser interval or event-driven check may be the better lever than
routing a tight poll). Recommend measuring after Slice 1 before committing to
1b.

**TDD:** client-level tests mirroring the v1 bd-shim write tests (`3f1c9c75a`):
routable-flag classification table; `parseBdListOpts` arg→opts table; a
supervisor round-trip asserting a routed `list --status in_progress --assignee X
--json` returns the same beads as real bd and is served from cache
(`X-GC-Cache-Age-S` present).

---

## Slice 2 — route `bd query` (ephemeral discovery)

The classifier, arg parser, and predicate mapping are **already built and
gated** (`bdQueryRoutingEnabled=false`, `cmd/gc/cmd_bd_shim.go:279`;
`parseBdQueryEphemeral` `:552-598`). It handles exactly the observed shape
(`ephemeral=true [AND key=value]…`, keys status/label/type/assignee/parent),
which is 2192/2203 live calls. The block is a **missing endpoint + client
method**:

1. Add Huma `GET /v0/city/{cityName}/beads/ephemeral` (recipe below), returning
   `ListOutput[beads.Bead]`. Reads the wisp/ephemeral tier
   (`ListQuery` `WithEphemeral`/`TierWisps`).
2. Implement `client.EphemeralBeads(opts) → CachedRead[[]beads.Bead]` — the
   dangling stub at `internal/api/client.go:1862-1865` (opts type
   `EphemeralBeadsOpts` already exists `:1852-1860`).
3. Regenerate spec + genclient (see recipe).
4. Add `case "query":` in `dispatchBdShimVerbViaAPI` → `client.EphemeralBeads` →
   `writeReadyJSON`.
5. Flip `bdQueryRoutingEnabled = true` (`:279`).

**Honesty note in the plan:** ephemeral reads bypass the active `CachingStore`
(different tier), so this is **connection-pooling, not cache-avoidance** — it
still cuts ~2.2k direct dials (the churn), which is the CPU win, but don't
advertise it as a read-elimination.

**TDD:** parser tables already exist (keep/extend); add a round-trip test
(routed `query 'ephemeral=true AND status=open' --json` == real bd) and a
fallback test (endpoint absent / controller down → refuse, per pure-HTTP
contract).

---

## Slice 3 — route `bd dep list <id> --down`

Genclient has `GetV0CityByCityNameBeadByIdDeps` but **no high-level
`client.BeadDeps` wrapper**, and the existing `/bead/{id}/deps` endpoint returns
**children (parent→child), not dependency edges** (`huma_handlers_beads.go:
509-541`). True edges come from `store.DepList(id,"down"/"up")`; only the graph
endpoint emits them today (`humaHandleBeadGraph:430` via `collectWorkflowDeps`).

Steps: add a `GET /v0/bead/{id}/depedges` endpoint returning typed `[]beads.Dep`
(or reuse the graph endpoint's edge projection), add `client.BeadDeps(id,dir)`
wrapper, add `case "dep":` gated to the `dep list <id> --json --direction down`
shape (`down` is cache-servable via `CachingStore.DepList` when `depsComplete`,
`caching_store_reads.go:653-670`; `up` always hits Dolt → keep passthrough).
560 calls, maintenance-driven (not per-tick) — lowest priority.

**TDD:** dep-edge round-trip vs real bd; `up` correctly falls through to
passthrough.

---

## Cross-cutting — routed-vs-passthrough telemetry (ship with Slice 1)

Today `telemetry.RecordBDCall` (`internal/telemetry/recorder.go:494-523`) fires
**only** on the passthrough exec path (`internal/beads/bdstore.go:146-169`,
guarded `name=="bd"`), so routed verbs are invisible. Add a `disposition`
attribute (`route`/`passthrough`/`refuse` — `bdShimDisposition.String()` already
yields these, `cmd_bd_shim.go:62-71`): emit `route`/`refuse` from
`dispatchBdShimVerbViaAPI` + the `runBdShim` switch, and label the existing
passthrough call `passthrough`. This gives a live routed-vs-passthrough ratio so
(a) each slice's connection cut is measurable and (b) a silent shim-install
regression (the Slice-0 class of bug) is never invisible again.

---

## Add-endpoint recipe (for Slices 2 & 3; strict typed-wire CI)

Template = the `GET /beads` list endpoint. Read `engdocs/architecture/
api-control-plane.md` + `engdocs/contributors/huma-usage.md` first.
1. Input struct in `internal/api/huma_types_beads.go` (embed `CityScope`,
   `PaginationParam`; **query params must be value types** — Huma panics on
   pointer query params; use `OptionalParam[T]` only to distinguish
   absent-vs-empty). Reuse `ListBody[beads.Bead]`/`ListOutput` to avoid new
   generated TS types.
2. Handler in `internal/api/huma_handlers_beads.go` (doc-comment mandatory;
   return only `apierr.*` typed errors; gate cache with `cacheLiveOr503`).
3. Register in `internal/api/supervisor_city_routes.go` via `cityGet(...)` with
   `errorStatuses(...)` — **never** raw `huma.Register` (loses auto operationId).
4. Regenerate: `go run ./cmd/genspec` + `go generate ./internal/api/genclient`
   (the `spec-ci` target; pre-commit runs it). CI gates: `TestOpenAPISpecInSync`,
   `TestGeneratedClientInSync`.
5. Client method in `internal/api/client.go` — pattern of `ListBeads`/`GetBead`;
   typed decode via `beadsFromGenList` (never hand-parse JSON); return
   `CachedRead[T]` carrying `AgeSeconds`.

## Read-lag correctness (applies to every routed read)
Active cache has **no TTL**; it's invalidate-on-write (write-through) and gated
by `cacheLiveOr503` (503 → client falls back while priming). Post-write refresh
carries the `e67844e01` force-write defense (ClaimAs/Close force the committed
status/assignee onto the refreshed row). Routed **discovery** reads inherit this
and are safe; routed reads feeding **lifecycle gates** must set `Live=true`
(bypasses cache, hits Dolt) — so scope routing to discovery only.

## Sequencing & effort
1. **Slice 0** (GC_BD_REAL) — prerequisite, gc-core, small, must land first.
2. **Slice 1** (`list` hook shape) + telemetry — biggest win, lowest effort.
3. Measure. Decide **Slice 1b** (refinery metadata-field vs poll-less refinery).
4. **Slice 2** (`query`) — endpoint + client port, then flip the gate.
5. **Slice 3** (`dep list --down`) — lowest priority.

Each slice is independently shippable, TDD-first (client-level tests like
`3f1c9c75a`), fork-isolated behind `cmd/gc` + `internal/api/client.go`.

## Out of scope but higher-ROI than any single verb: `bd context`
`bd context --json` is the **#1 churn source (10,245/15h)**, gc-invoked for
preflight identity. The per-process memoization (`87bb30d87`) can't help because
each short-lived `gc`/`bd` invocation is a fresh process. Tracked separately
(see companion bead) — options: route context through the warm controller, an
on-disk short-TTL identity cache, or reducing the caller frequency.

---

## Refinement outcomes (2026-07-18, post-build)

Each slice was refined to 100% confidence *before* building. Two of the
planned slices were corrected or rejected when live `bd-bypass.log` telemetry
contradicted the plan's assumptions — the refinement gate did its job.

- **Slice 0/1/2 — shipped** (`dbb6ea8c9`, `8d32a78a9`, `fdcf9a17d`, on
  `develop`).
- **Cross-cutting telemetry — shipped** (`e4bec069a`, bead `gcw-j8oq.1`).
  *Plan correction:* the plan pointed telemetry at `internal/beads/bdstore.go`
  `RecordBDCall`, but that instruments gc-core's **own** internal bd exec, not
  the shim passthrough (the shim execs real bd via `execRealBd`). Disposition
  telemetry therefore lives at the **shim boundary** (`runBdShim`): a new
  `telemetry.RecordBDShimDisposition` records `route`/`passthrough`/`refuse`
  reflecting the actual path taken (claim fallbacks → passthrough).
- **`bd context` cache — shipped** (`75c78d403`, bead `gcw-40jw`, verified
  on the live preflight path).
- **Slice 1b — blocked, not built** (bead `gcw-j8oq.2`). Source pinned: the
  3315 calls are `find_bead()` in `pr-{merge,review}-patrol.sh`, a **per-PR**
  `list --metadata-field pr_number=$N --exclude-type=epic` lookup by the
  crm-pr-* orders. Option A (route it, gc-core) vs Option B (batch the per-PR
  fan-out into one `--has-metadata-key pr_number` list/tick, in the CRM pack
  lane) — B is likely the better lever but cross-lane. Gated on the post-deploy
  telemetry window. **Do not build blind.**
- **Slice 3 — rejected as specced** (bead `gcw-j8oq.3` closed). Live telemetry:
  **751/878** dep-list calls are `--direction=up --type=blocks --json`; **zero**
  are `--down`. The plan's `--down` routing target has no live traffic, and the
  dominant `--up` shape is exactly what the plan excludes (always hits Dolt).
  The plan's `[]beads.Dep` **edge** output contract was also wrong — real
  `bd dep list --json` emits full dep-**issue** objects, so an edge-returning
  endpoint would silently mis-answer consumers. The real opportunity
  (gc-invoked `--up` blocker-check churn, an **invocation-layer** fix like
  `bd context`, not shim routing) is re-filed as `gcw-j8oq.4`, measurement-gated.
