# bdshim — correctness-contract research

**Date:** 2026-07-19 · **Author:** gas-city-wbern/architect (research pass) ·
**Subject:** Is the #4441 claim *"passthrough is always byte-identical; routing
is pure latency, never a correctness requirement"* true? Adversarial audit
against the shim source, the controller federation path, and live crm evidence.

> **Scope note.** This is a correctness-model audit, not a perf audit. It builds
> on `bd-shim-prior-art-research.md` (comparators/freshness best practice) and
> `bd-shim-upstream-issue-draft.md` (the posted #4441 text). It does **not**
> edit code or the public issue; it recommends the corrected contract.

---

## TL;DR (decision-oriented)

1. **The #4441 "pure latency, never a correctness requirement" claim is FALSE
   as written.** It is true on exactly *one* axis of divergence (graph-store
   vs work-store, which is pinned OFF) and silently ignores *two* axes that are
   LIVE: **cross-rig federation** (scope) and **write/freshness lag**. Routed
   reads return a **semantically different, more-complete set** than raw
   `bd`. **CONFIRMED** in the shim's own source and the controller code.

2. **The shim's own code already knows this.** `cmd/bdshim/main.go` loud-fails a
   routed read when the controller is down *precisely because* falling through
   to raw `bd` would return a narrower, wrong/empty answer — an explicit
   admission that passthrough ≠ routed for federated verbs. The prose in #4441
   contradicts the rationale in the code it describes.

3. **The right contract is per-verb and scope-aware, not "byte-identical."**
   Byte-identity is achievable only for a **single-store point lookup**
   (`show <id>` within one rig). `ready`/`list`/`query` are **federated
   aggregations** and are divergent *by design* — that is the feature, not a
   bug. State it honestly.

4. **The live codex `no_work` bug is a scope/incompleteness bug, not a latency
   bug.** codex agents exec `bash -lc` (a login shell) which re-prepends
   `~/go/bin` ahead of the process-PATH shim dir; there is no bash equivalent of
   the zsh `.zshenv` re-front guard (deferred as **gcw-b8yk**), so they resolve
   **raw `bd`** → the **rig-local, non-federated** view → miss freshly-slung
   city/federated work → `no_work` → pool spawn-loop. **Fix = close gcw-b8yk**
   (make the shim the universal read path), *not* narrow the routing.

5. **The dangerous quiet-wrong failure mode is already live** and it is *this
   bug*: raw `bd` returns an **honest but incomplete** set with exit 0, and the
   agent reads "no ready work" as truth. That is a silent false-negative by
   omission — worse than a loud failure.

---

## The mechanism (CONFIRMED against source)

### Routed verbs and their dispositions

`internal/bdshim/classify.go` — `RoutedVerbs = {close, show, ready, update,
reopen, delete, create, list}` plus `query` (ephemeral shape) and
`mol current|progress`. `ClassifyVerb` returns `Route` / `Passthrough` /
`Refuse`. Routing is gated per-verb by routability predicates (`ReadyRoutable`,
`ListRoutable` requires `--json`, `UpdateClaimShape`, `CreateRoutable`, …); an
unroutable flag set passes through to real `bd`.

`splitPhase` is **pinned `false`** in both `cmd/bdshim/main.go` (line 88) and
`cmd/gc`'s `runBdShim`, matching gc's hardcoded `graphStoreSQLiteEnabled=false`.

### Three independent axes of divergence between routed and raw `bd`

| Axis | What differs | Live today? | Does #4441's "byte-identical" defense cover it? |
| --- | --- | --- | --- |
| **A. Graph store** | routing sees SQLite graph/wisp beads a work-only `bd` misses | **OFF** (`splitPhase=false`; identity phase) | ✅ Yes — this is the *only* axis the claim addresses |
| **B. Federation (scope)** | controller reads **city store + every rig store**; raw `bd` is **cwd-scoped to one store** | **ON** | ❌ No — ignored |
| **C. Write/freshness** | controller-mediated writes appear in the warm view before a fresh raw-`bd` process sees them on-disk | **ON** | ❌ No — ignored |

**Axis A (CONFIRMED, but OFF).** `classify.go:43` — routed verbs exist "so graph
beads in the embedded SQLite store are seen and mutated, not just Dolt work
beads." `GraphTouchingUnroutedVerbs` (line 291-296): passthrough "would SILENTLY
miss graph beads once graph_store=sqlite is on — so in the split phase the shim
refuses them loudly." So the code *itself* frames routing as a completeness
mechanism, not latency. It is inert only because `splitPhase` is pinned false.
The #4441 "byte-identical" claim rides entirely on this pin.

**Axis B — federation (CONFIRMED, LIVE).** `internal/api/huma_handlers_beads.go`
builds the ready federation source list explicitly:

```go
sources = append(sources, readySource{label: "city", store: s.state.CityBeadStore()})
for _, rigName := range rigNames {                 // every rig
    sources = append(sources, readySource{label: "rig " + rigName, store: stores[rigName]})
}
// each read via beads.HandlesFor(src.store).Live.Ready()  (authoritative backing read)
```

`client.go:1965` — `ReadyBeads` fetches "the federated ready set across the
controller's bead stores." Raw `bd ready`, by contrast, reads only the single
store its cwd/config scopes to. **This is the 50-vs-78 divergence** in the crm
rig: routed `ready` = city store + gcw + gc2 + gci + crm; raw `bd ready` = crm
only. The result sets are different by construction.

`main.go:122-126` states the consequence directly: a controller-down routed read
"fails loudly (rc=1) rather than silently passing through to the work-only bd,
**whose cwd scope cannot answer a city-wide read** — matching the fat shim's
pure-HTTP contract." The loud-fail exists *because passthrough is not
equivalent*. #4441's "passthrough is always byte-identical" is refuted by the
design decision it sits next to.

**Axis C — freshness (CONFIRMED direction; the "~3 crm beads" attribution is
HYPOTHESIS).** Two distinct sub-effects are being conflated in the field report:

- **Scope, mislabeled as freshness.** `ready` federates the **city store**,
  which holds control beads and graph.v2 molecule roots
  (`huma_handlers_beads.go:492-494`: "City-scope ready work … lives in the city
  store, so it must be federated explicitly or HTTP `bd ready` would never
  surface it (#3817)"). A rig-scoped raw `bd` **never** sees those regardless of
  freshness. Some of the "~3 fresh crm beads" are likely **city-store-scope**
  beads, i.e. an axis-B scope difference, **not** a temporal lag.
- **True write lag.** Beads created through the controller write-path
  (routed `create`, sling) are visible to the warm federation before a *fresh*
  raw-`bd` process observes the committed on-disk row. Note `ready` reads
  `.Live.Ready()` (authoritative backing, not the response cache), so the lag
  here is **store-commit/propagation**, not cache staleness. (Prior-art #2987
  documents the *opposite* skew — the response cache can lag *minutes* under
  write load for `all=true` reads — so freshness is **bidirectional**: routed
  can be fresher for controller-writes and staler for cache-served history.)

> **Adversarial flag:** the field claim "the on-disk store hasn't got [the 3
> beads] yet (freshness/propagation lag)" is **partially a scope difference, not
> a lag**. Confirm by diffing the crm rig store's `ready` against the city
> store's contribution before asserting propagation lag as the cause.

---

## Q1 — What SHOULD a read-routing shim's correctness contract be?

**The line is: "same answer, faster" vs "different/fresher answer."** A shim may
only claim byte-identity where it returns *the set the authoritative computation
would return, just cheaper*. The comparators draw it sharply (primary sources
via `bd-shim-prior-art-research.md`):

- **git fsmonitor / watchman** — the daemon accelerates but returns the **same**
  file set a full scan would; when it *cannot* guarantee that, it signals
  divergence out-of-band (`is_fresh_instance`; an opaque token encoding the
  daemon PID) so the client does a **full resync**. It never silently returns a
  different set. **The bdshim `ready` path is not this shape** — it returns a
  deliberately *different* (federated) set, so the fsmonitor "identical, faster"
  contract does not apply to it.
- **bazel / gradle / adb** — on any version/flag skew they **restart** the
  daemon rather than interoperate, preserving a strict same-answer contract.
- **git plumbing vs porcelain** — the *machine* format (`--json`) is the stable
  contract; human output is explicitly not. (#4441's follow-up already switched
  the contract to "identical `--json`" — keep that.)
- **sccache** — a warm-vs-cold *hit* must be provably identical; on server I/O
  error it **fails the build** by default rather than serve a wrong object.

**Per-verb achievability for bdshim:**

| Verb class | Example | Byte-identical to raw `bd`? |
| --- | --- | --- |
| Single-store point lookup | `show <id>` (id in the caller's rig) | ✅ Achievable (and largely holds) |
| Cross-store point lookup | `show <id>` (id in another rig) | ❌ Routed **finds** it; raw `bd` reports absent (empty array, exit 0) — a silent false-negative in the *raw* direction |
| Federated aggregation | `ready`, `list --json`, `query ephemeral` | ❌ **Inherently divergent** — routed is a city-wide **superset**; this is the feature |
| Mutation | `create`, `update`, `close`, `--claim` | N/A (write-through to controller store; freshness lag vs on-disk) |

**Conclusion:** the honest contract is **not** "byte-identical everywhere." It
is: *routing is the authoritative, city-complete read path; raw-`bd` passthrough
is a strictly narrower (single-store) fallback that is correct only where a
single-store answer is the intended scope.* Byte-identity is a property of the
**single-store point lookup**, nothing more.

## Q2 — Is federation a correctness feature or a contract violation?

**It is a deliberate SCOPE CHANGE — a feature for the callers that want it, a
violation only if sold as "pure latency."** Agents polling for ready work under
GUPP *want* city-wide completeness (that is the whole reason `/beads/ready`
federates — #3817). Framing it as "identical to `bd`, just faster" is the
dishonest part, not the federation itself.

**Honest #4441 framing:** *"Routing changes the result **scope** (city-federated
vs cwd-local) and **freshness** (controller-mediated writes before on-disk
propagation). This is an intentional completeness gain, opt-in and default-off,
not a transparent latency shim. Callers that require rig-local semantics use raw
`bd` or an explicit `--rig` scope."* Do **not** claim byte-identity for
federated verbs; claim it only for the single-store point lookup, and only for
`--json` output.

## Q3 — Freshness/scope convergence options, ranked for OUR architecture

Constraints: Dolt-backed beads, warm gc controller, per-agent `bd` subprocess
model, ZERO hardcoded roles, keep-upstream-mergeable.

**Rank 1 — (b) Make ALL agents use the shim (close gcw-b8yk).** Fixes the live
codex bug directly and converges *by making routed the only read path*, so
raw-vs-routed equality stops mattering (no agent reads raw). The bash gap is a
known, bounded fork-local install detail: add a `BASH_ENV`/`$ENV` re-front for
non-interactive bash and a login-shell (`~/.bash_profile`/`~/.profile`) re-front
mirroring the existing zsh `.zshenv`/`.zshrc` guard. Upstream-neutral (it is
install/runtime plumbing, not SDK domain logic). **Lowest risk, highest
leverage.** This is the recommended immediate fix.

**Rank 2 — (a) Write-through so raw `bd` and the controller read one store.**
The deeper correctness fix for **axis C** (freshness): if routed writes commit
to the same on-disk store a fresh raw `bd` reads, the temporal lag closes. BUT
it does **nothing for axis B** — raw `bd` is still cwd-scoped to one store and
still cannot see city/other-rig ready work. So (a) is *necessary for freshness
honesty* but *insufficient for scope convergence*. Pair with (b); do not ship
alone expecting the codex bug to close.

**Rank 3 — (d) Freshness tokens / read-your-writes.** Addresses only axis C, is
the heaviest to build (opaque monotonic token per store, client resync
protocol), and is the most upstream-invasive (touches the bead store contract).
Over-engineered for our case: the codex bug is scope (B), not a read-your-writes
race. Defer.

**Rank 4 (reject) — (c) Narrow routing to only truly-identical verbs.** This
**throws away the federation feature** agents actually depend on (city-wide
`ready`). Wrong direction. The problem is not "routing does too much"; it is
"one class of agent isn't routing at all." Narrowing would make the codex bug
*worse* by shrinking what the shim provides while leaving the raw-`bd`-resolution
gap open.

**Recommended combination:** **(b) now** (close gcw-b8yk → universal routing,
kills the spawn-loop) **+ (a) next** (write-through → make the freshness claim
honest and let a controller-down fallback be less lossy) **+ corrected #4441
language** (state scope+freshness as intentional). (d) only if a real
read-your-writes race is later observed.

## Q4 — Concrete recommendation

**Correctness model.** Adopt a **scope-aware, per-verb** contract:

- *Single-store point lookup* (`show <id>`) → byte-identical `--json` within a
  store; across the city, routed is a superset (finds cross-rig ids). Document
  the cross-rig case explicitly.
- *Federated verbs* (`ready`, `list --json`, `query`) → **authoritative
  city-federated set**; not byte-identical to raw `bd` and not meant to be.
- *Mutations* → write-through to the controller store; the only correctness
  obligation is that a subsequent routed read reflects the write (read-your-
  writes **on the routed path**, which holds because both hit the same warm
  store). Claim keeps its liveness-probe + atomic-`bd`-fallback special case.
- *Fallback rule* — routed reads **fail loud (rc=1)** when the controller is
  down (already implemented); never silently fall through to the narrower
  single-store answer. `--claim` may fall back to raw `bd`'s atomic claim
  because that is a *correct, narrower* substitute (a mutation on a known id),
  not a scope-changing read.

**Fix for the codex `no_work` bug (gcw-b8yk).** The process-PATH front
(`prependGCBinDirToPATH`) is shell-agnostic but is defeated by a login shell's
profile re-prepending `~/go/bin`. Add the bash counterpart of the existing zsh
guard:
- non-interactive bash: point `BASH_ENV` (and POSIX `$ENV`) at a gc-managed rc
  that re-fronts the shim bin dir (mirror `.zshenv`);
- login bash: write `~/.bash_profile`/`~/.profile` shims under the gc-managed
  home that source the user's real profile then re-front the shim dir last
  (mirror the `.zshrc`/`.zprofile` pattern in `ensureCityBdShimZdotdir`).
This makes codex resolve the shim → federated view → claims freshly-slung work.

**Corrected #4441 language** (replace the "What we built" bullet that reads
*"Passthrough is always byte-identical to raw bd; routing is a pure latency
optimization, never a correctness requirement"*):

> **Routing changes read *scope* and *freshness*, not just latency — by design.**
> - `show <id>` is byte-identical (`--json`) to raw `bd` **within a store**;
>   across a federated city it additionally resolves ids resident in other rig
>   stores that a cwd-scoped raw `bd` reports absent.
> - `ready` / `list` / `query` return the **city-federated** set — a superset of
>   any single-store raw `bd` — served from the warm controller (which reflects
>   controller-mediated writes ahead of on-disk propagation). This is an
>   intentional completeness/freshness gain, **not** a transparent latency shim.
> - Because routed reads are federated and raw `bd` is cwd-local, a
>   controller-down routed read **fails loud (rc=1)** rather than fall through to
>   a narrower local answer that would silently under-report.
>
> Net: routing is the authoritative, city-complete read path; raw-`bd`
> passthrough is a strictly narrower fallback, correct only where single-store
> scope is the intent. (Byte-identity holds on the graph axis only while
> `graph_store=sqlite` is off; the classifier already refuses graph-touching
> verbs loudly when it is on.)

## Q5 — Where can we return a QUIET WRONG answer to an agent that trusts `bd`?

Ranked by live danger:

1. **Raw `bd` for `ready`/hook in a codex agent (LIVE, the reported bug).**
   Returns an honest but **rig-local, non-federated** set with exit 0. The agent
   reads "no ready work" as truth → `no_work` → spawn-loop. A **silent
   false-negative by omission.** This is the dangerous mode and it is *the very
   bug under investigation.* Fix via gcw-b8yk (Q4).
2. **Cross-rig `show <id>` via raw `bd`.** Returns empty array / exit 0
   ("absent") for an id that exists in another store. A consumer doing
   `bd show … --json | jq '.[0]'` reads absence. Silent false-negative. Closed
   for routed callers; open for any raw-`bd` caller (same root cause as #1).
3. **`bd_shim=on` but no `bdshim` beside gc.** The `bd` symlink silently targets
   real `bd` (`bd_shimbin.go:197-201` warns at install; the doctor `bd-shim`
   check warns) — but at *runtime* each call silently gets the narrower local
   view. Degraded-but-quiet. Mitigation exists (warn), but it is install-time,
   not per-call.
4. **`--claim` fallback under a concurrent writer.** Controller-down →
   passthrough to raw `bd`'s atomic claim (cwd-scoped). If the target bead lives
   in another rig store, raw `bd` cannot claim it → **error (loud)**, not a wrong
   answer. Acceptable, but confirm the atomic-claim fallback is write-through
   correct under a concurrent controller-side claim (prior-art Q5 flagged
   `claim`-as-a-routed-mutation).
5. **Freshness bidirectionality.** For `all=true`/history `list`, the controller
   response cache can lag *minutes* (#2987) — routed can be **staler** than raw
   `bd` there, the inverse of the create-lag case. A caller assuming "routed =
   freshest" is wrong for cache-served history. Document which reads are
   `.Live` (ready) vs cache-served (`all=true` list).

**Loud-fail paths that are correct (keep):** controller-down routed read → rc=1;
graph-touching verb under `splitPhase` → `Refuse`. These fail loud rather than
serve a plausible wrong answer — the right posture.

---

## Verification ledger

- **CONFIRMED (source-read this pass):** routed-verb list and per-verb
  routability (`internal/bdshim/classify.go`); route/passthrough/refuse +
  loud-fail-on-down + claim fallback (`cmd/bdshim/main.go`,
  `cmd/bdshim/claim.go`); federation = city store + every rig store via
  `.Live.Ready()` (`internal/api/huma_handlers_beads.go:498-521`,
  `internal/api/client.go:1965`); process-PATH front is load-bearing and the
  bash re-front gap is deferred as **gcw-b8yk** (`cmd/gc/bd_shimbin.go:206-214`,
  `cmd/gc/agent_env_path.go`, `internal/doctor/checks_bd_shim.go`); `splitPhase`
  pinned false (`cmd/bdshim/main.go:88`).
- **CONFIRMED (field, per task):** crm rig raw `bd ready` = 50, `bdshim ready` =
  78; codex uses `bash -lc`, Claude uses a guarded zsh tool shell.
- **HYPOTHESIS (needs a live store-diff to settle):** the "~3 fresh crm beads"
  are propagation lag. At least some are more likely **city-store-scope**
  (control/molecule) beads a rig-scoped raw `bd` never sees — an axis-B scope
  difference, not an axis-C temporal lag. Diff the crm rig store's `ready`
  against the city store's contribution to attribute them before asserting lag.
- **Unchanged from prior art:** transport should move to a UDS; contract is
  `--json` not human output; a shim↔controller version handshake is still
  advisable. (All already in the #4441 follow-up comment.)
