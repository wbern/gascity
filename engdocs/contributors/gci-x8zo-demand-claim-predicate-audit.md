# Demand ↔ claim readiness audit (gci-x8zo) — corrected root-cause map + re-aim plan

Status: investigation complete 2026-07-20 (architect). The codex-furiosa
`no_work` spawn-loop **incident is recovered** (store healed), but the fix
shipped for it (`8d4d75bb1`) is **inert on this fleet's production path**. This
doc records the corrected map so the effort is not lost and the real hardening
is aimed correctly.

## TL;DR

- The loop was **store-degradation-driven** (`gci-8qm3`: crm Dolt compaction
  blocked by a stale `compact-pending-push` marker → bloat → stale/inconsistent
  `bd ready` readiness projection). It stopped reproducing once the store
  healed.
- **Three separate readiness predicates** decide "is this bead workable," and
  they can drift under a degraded store:
  1. **`store.Ready()` / `IsReadyCandidate`** (in-process Go) — the **actual
     default-pool demand** path.
  2. **`poolDemandCountShell`** (shell `bd ready … | jq length`) — used only in
     **legacy `store==nil`** mode and for **custom `scale_check`** pools.
  3. **`filterUnreadyHookCandidates`** (worker Go, `cmd/gc/cmd_hook.go`) —
     applied on the **claim** path (`cmd_hook_claim.go:138`).
- The shipped fix (`8d4d75bb1`) aligned #2 with #3. **But this fleet's pools
  never use #2** (see reachability below), so the fix is dead code here. The
  real default-pool demand is #1, which was left untouched.
- The live "validation" (furiosa claimed `crm-trl91t` → PR #514 → merged) proves
  the **store healed**, not that the fix works — a healed store yields a clean
  claim regardless.

## Which demand path does a pool actually use?

`buildDesiredState` (`cmd/gc/build_desired_state.go`) routes each pool:

| Pool kind                                   | Branch | Demand path | Readiness predicate |
| ------------------------------------------- | ------ | ----------- | ------------------- |
| store-backed, **no** `scale_check` (default)| `continue` at ~:637 → `defaultScaleTargets` | `defaultScaleCheckCountsAndDemand` → `controllerDemandReady` → `store.Ready()` | **#1 `IsReadyCandidate`** |
| store-backed, **custom** `scale_check`      | `pendingPools` (`newDemand=true`) | `evaluatePoolNewDemandFiltered` → but `NewDemandRowsCheck==""` for custom → falls back to `evaluatePoolNewDemand` → the user's `scale_check` | opaque user int |
| legacy `store==nil`                         | `pendingPools` (`newDemand=false`) | `evaluatePool` → `poolDemandCountShell` | **#2 shell** |

`evaluatePoolNewDemandFiltered` (the fix) only does its filtered counting when
`NewDemandRowsCheck != ""`, which is only populated for **default** pools — but
default store-backed pools **never reach `pendingPools`** (they `continue`
first). So the filtered path has **no reachable production caller** with a
populated rows query. Confirmed: there is **no `scale_check` anywhere** in the
gc2 city config, and the store is Dolt-backed ⇒ every pool is default ⇒
`store.Ready()` is the only live demand path.

## Reachability finding (the headline)

`8d4d75bb1` is **inert** in this fleet. It is harmless (correct, tested, off the
hot path) but provides **no protection** against the loop it was written for,
because it fixes predicate #2 while production runs predicate #1. The unit test
`TestEvaluatePoolNewDemandFilteredMatchesWorkerReadiness` pins a function
production never calls, and it is additionally near-tautological (feeds identical
rows to both sides — see meta-lessons).

## The real asymmetry (where hardening belongs)

`store.Ready()` (#1) is **mostly aligned** with the worker's
`filterUnreadyHookCandidates` (#3) — which is *why* furiosa works on a healed
store. Residual divergence under a degraded store:

- **stale `is_blocked`:** native DoltLite keeps `is_blocked` `nil` and recomputes
  readiness from deps, so `store.Ready()` can count a bead ready while the
  worker's `bd ready` CLI row carries a **stale `is_blocked==true`** that
  `isSelfBlockedHookCandidate` strips → demand≥1 but `no_work` → loop.
- pure **read non-determinism** (`bd ready` returns different rows at different
  instants) is unaddressed by any predicate alignment; it needs store
  determinism (`gci-8qm3` class) + the diagnostic net.

## Reachable adjacent lanes (adjacency sweep, → `gcw-ehvg`)

Same bug class, on paths that **are** reached in production:

- **P1 — assigned-work resume/wake lane.** `computePoolDesiredStates`
  (`cmd/gc/pool_desired_state.go`) gates resume/wake on status only
  (`in_progress`/`open`), with no `defer_until`/`blocked_by`/`is_blocked` check;
  its input `poolWorkBeads` admits open-routed beads **without a readiness
  verdict** (`markReadyAssigned` skipped in `appendOpenRoutedWorkUnique`). The
  per-bead `readyAssigned` verdict *is* computed and *is* consulted by the awake
  lane, but **not** passed here — the function's own docstring is violated.
  Direct analog of the fixed bug, on the demand lane that was **not** hardened.
- **P1 — convoy/scope completion gates.** `autocloseConvoyIfComplete` /
  `hasOpenScopeMembers` count any non-terminal (`open`) member as outstanding; a
  member that is open **and** unclaimable (stale `is_blocked`, dep-blocked,
  future defer) is counted forever → convoy/workflow **never completes**
  (quiet starvation, not busy loop).
- **P2 — `store.Ready()` predicate copy** may drift from the worker filter over
  time (two implementations of readiness).
- **P3 (documented footgun)** — custom `scale_check` stays on the unfiltered int
  path; a user `scale_check` that raw-counts `bd ready` reintroduces the loop.

`session_affinity` was **cleared**: it is advisory-only, read by no
routing/demand/claim path, so it cannot create a count↔claim asymmetry.

## Re-aim plan (TDD)

1. **Reproduce on the real path, not a hand-rolled array.** Build an
   integration-style test that drives `store.Ready()` demand and the worker
   claim over the *same* store rows including a stale-`is_blocked` bead; assert
   demand-count == worker-claimable. (Avoids the tautology of the shipped test.)
2. **Align #1 with #3 at the source.** Either have the demand path apply the
   worker's readiness semantics, or make both derive from one shared readiness
   function. Preserve DoltLite `is_blocked==nil` recompute semantics.
3. **Harden the P1 lanes:** pass `readyAssigned` into `computePoolDesiredStates`
   and gate open beads on it (match the awake lane); apply a "no claimable
   descendants" check to convoy/scope completion.
4. **Keep** the `ce1476243` diagnostic (on the real worker path) as the
   recurrence classifier.

## What to keep vs retire from the shipped work

- **Keep:** `28de22ead` (pool-vs-instance refutation guard tests — the
  refutation stands; `gc2-nvf76` can close), `ce1476243` (worker-path diagnostic
  log).
- **Inert/re-aim:** `8d4d75bb1` demand-count fix — harmless but off the
  production path; supersede via the re-aim above rather than banking it as the
  cure.

## Meta-lessons

- **On this store, one `bd show` is not ground truth.** Stale reads under
  `gci-8qm3` served `open/None` while a claim had actually persisted — nearly
  produced a false "loop reproduced" call. Peek the live session + resample
  before concluding.
- **Tautological tests hide reachability gaps.** Feeding identical rows to both
  sides of a symmetry test proves the shared function, not the production
  divergence. Test through the real path.
- **Verify production reachability before claiming a fix.** A correct, tested,
  green change can still be dead code. The wiring is part of the fix.

## Worth assessment & disposition (2026-07-20)

Each identified item triaged by worth × reachability × safety. Implemented the
clearly-worthwhile+safe ones (TDD + review); every deferral records its reason.

### Implemented

- **P1a defer gate — SHIPPED (`7bc4ebc18`).** `ComputePoolDesiredStates` woke a
  session for an OPEN assigned bead hidden by a future `defer_until`
  (characterization test confirmed it fired). Gated open beads on
  `beads.IsDeferred`. Reachable at the function level, safe (in-progress work
  never gated; self-heals on defer-clear), narrow. Reuses the existing helper.

### Deferred (with reason)

- **P1a `is_blocked` / dependency-blocked gating — DEFER.** The dep-aware
  readiness signal is the store-scoped `readyAssigned` verdict keyed by
  `(storeRef, beadID)`; `computePoolDesiredStates` receives `[]beads.Bead`
  without store refs, so threading it cleanly means plumbing store-scoped keys
  through a multi-caller public API. Reachability of the *loop* here is also
  unproven: the open+assigned beads that reach this function are largely
  orphan-release fodder assigned to *dead instance ids*, which the resume tier
  already skips (`!isKnownPoolTemplate` at `pool_desired_state.go:209`).
  `beads.IsReadyCandidate` deliberately excludes `is_blocked` ("dependency
  checks are store-specific"), so it would not cover this case anyway. The
  reliability of `is_blocked` is really the F0 / `gci-8qm3` concern below.

- **F0 — align `store.Ready()`/`IsReadyCandidate` (#1) with worker
  `filterUnreadyHookCandidates` (#3) — DEFER.** These diverge on `is_blocked`:
  the store **recomputes** readiness from actual deps (ignores denormalized
  `is_blocked`); the worker **trusts** the denormalized `is_blocked` in the
  `bd ready` row. Under a lying/degraded store both directions have failure
  modes — trusting strands ready work; not-trusting acts on truly-blocked work
  (the exact bug `filterUnreadyHookCandidates` was added to prevent). So this is
  a **store-determinism** problem, not cleanly resolvable by predicate
  alignment. Primary fix = `gci-8qm3` (Dolt compaction / read determinism),
  owned by devops. Predicate alignment would only trade one failure mode for
  another; revisit only if `gci-8qm3` proves insufficient.

- **P1b — convoy/scope completion status-only gates — DEFER.** Failure mode is
  **starvation** (a workflow never completes because an open-but-unclaimable
  member is counted outstanding), not a busy loop. It is loud (stuck workflow),
  self-heals when the store heals, and lives in central convoy/dispatch
  completion logic (high blast radius). Needs its own characterization test +
  careful design before touching completion semantics. Tracked in `gcw-ehvg`;
  not rushed.

- **F1–F5 (tier-union, limit-window, control-dispatcher, perf, tautological
  test) — DEFER.** All pertain to predicate #2 (`poolDemandCountShell` /
  `evaluatePoolNewDemandFiltered`), which is **dormant in this fleet** (no
  `scale_check` + store-backed ⇒ default pools use `store.Ready`, never this
  path). Latent only; relevant if a custom `scale_check` or legacy `store==nil`
  mode is ever adopted. Revisit then.

- **P2 — `store.Ready` predicate copy drift — DEFER.** Hygiene; currently
  mostly-aligned with the worker. Fold into F0 if store-determinism work touches
  it.

- **P3 — `computeWorkSet` (dormant; production `WorkSet` is empty, test-only) and
  in-progress-tier mark-ready-without-check (in-progress work rarely dep-blocked
  and resumes regardless by design) — DEFER.** Low worth.

### Shipped-work disposition

- **`28de22ead`** (pool-vs-instance refutation tests) — KEEP; `gc2-nvf76` closed.
- **`8d4d75bb1`** (demand-count fix) — KEEP as latent defense for the
  custom-`scale_check`/legacy path (F1–F5 live there), but it is **inert for
  this fleet's default pools and is NOT the cure** for the incident. Do not cite
  it as validated.
- **`ce1476243`** (worker-path strip diagnostic) — KEEP (real path).
- **`7bc4ebc18`** (P1a defer gate) — SHIPPED improvement.
