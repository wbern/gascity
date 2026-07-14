# Fork-Patch Inventory (wbern/gascity `develop` vs `upstream/main`)

**Generated:** 2026-07-14 · **Author:** gas-city-wbern/architect · **Bead:** gcw-blz8.1

Exhaustive classification of every fork-unique commit on `origin/develop` that is
**not** patch-equivalent to `upstream/main`. Baseline: `develop` @ `f3ad19579`,
`upstream/main` @ `9b6d91e17` (fetched 2026-07-14). `git cherry upstream/main
origin/develop` reported **59 fork-unique (`+`)** commits and 35 already-equivalent
(`-`); this inventory covers all 59.

## Purpose

Serve the integration mission — *keep `upstream/main` easy to merge, shrink
fork divergence to only what is genuinely fork-specific.* Every commit is tagged
with an upstream status (verified via `gh pr view` / `git grep upstream/main`)
and a recommendation.

## Recommendation taxonomy

| Tag | Meaning | Action |
| --- | --- | --- |
| `RETIRE-AFTER-MERGEDOWN` | Already MERGED upstream; de-duplicates automatically on the next `develop ← upstream/main` merge | None — let it drop |
| `KEEP-PENDING-MERGE` | Fork is ahead; an upstream PR is OPEN | Keep; chase the PR |
| `KEEP-FORK-PERMANENT` | Fork-specific (T3 bridge / DoltLite / deps / gitignore / build); never upstream | Keep |
| `CONTRIBUTE-UPSTREAM` | General-purpose fork fix, no upstream equivalent, no PR | Propose upstream |
| `RETIRE-NOW-OBSOLETE` | Upstream converged via a *different* commit; fork patch is redundant and may **conflict** on mergedown | Drop from develop **before** next mergedown |
| `INVESTIGATE` | Ambiguous | Decide |

## Summary counts

| Bucket | Count |
| --- | --- |
| RETIRE-AFTER-MERGEDOWN | 18 |
| KEEP-PENDING-MERGE | 4 |
| KEEP-FORK-PERMANENT | 8 |
| CONTRIBUTE-UPSTREAM | 21 |
| RETIRE-NOW-OBSOLETE | 7 |
| INVESTIGATE | 1 |
| **Total** | **59** |

---

## Headline actions (the parts that need a human decision)

1. **Pre-mergedown cleanup — 7 `RETIRE-NOW-OBSOLETE` + 1 duplicate.** These are
   redundant *today* and several will **actively conflict** when develop next merges
   `upstream/main`. Dropping them proactively keeps the mergedown clean:
   - `a14cca88b` (CGO test-shard snapshot) — upstream references the CGO vars
     directly; the fork's `TEST_LOCAL_CGO_*` snapshot **+ its contract test** will
     fail against upstream's script. **Will conflict.**
   - `b24a8685d` (gcw-tjeg) + `a6ace4cf0` (gcw-kcm4) — control-dispatcher rig-store
     fanout. Upstream **#4175** (MERGED 2026-07-13) solved the same bug with an
     *incompatible* design (per-rig scoped dispatcher sessions). **Will collide.**
   - `19f8f52a5` (gci-a8y re-nudge) — develop's `idle_nudge.go` is already
     byte-identical to upstream (#4083); pure redundancy.
   - `4f66d7242` (canonicalize stale-alias) — upstream converged via
     `buildSessionAssigneeIndex`/`alias_history`.
   - `c715e832a` (dolt quarantine doc) — doubly dead: not upstream **and** already
     removed from fork HEAD.
   - `f1285efb3` (`--wait-timeout`) — upstream shipped the same flag with
     **different semantics** (`0` → 120s default vs fork's `0` = wait forever).
     ⚠ If the wait-forever behavior is still wanted, re-propose it on top of
     upstream; do not silently dedupe.
   - **Duplicate:** `aacd07fb0` (fork-authored) vs `22c75f175` (#3940, MERGED) —
     same "activate native store when bd context unreachable" fix, both on develop.
     Retire the fork-authored `aacd07fb0`; keep the merged form.

2. **Chase 4 pending upstream PRs (`KEEP-PENDING-MERGE`).** These are fork work
   actively being contributed up — landing them removes the divergence permanently:
   - `f3ad19579` → upstream **#4137** (claim-holder recovery) — OPEN, green, mergeable.
   - `e0ff4684b` + `a3a02917c` + `aee42c3f3` → upstream **#3954** (claude
     `session_key` capture, wbern-authored) — OPEN.

3. **21 `CONTRIBUTE-UPSTREAM` candidates.** Genuine fork fixes with no upstream
   equivalent. Highest-value / cleanest first (general, low-coupling):
   - `08d6f1b32` `gc worktree scan` (new CLI, no fork coupling)
   - `0b37f91ed` tmux `recordPokeAt` (poke-discount on nudge path) — *the fix the
     claim-holder feature depends on; upstream lacks it*
   - `8dd704296` skill-content-edit no longer recycles every session
   - `77688848f` + `95aa213b6` (gcw-z7b1) session-pointer writes scoped to city store
   - `833a0de70` (gcw-8co2) clear `reset_committed_at` on wake (false `reset_stalled` storm)
   - `4de6c1c02` + `e1361e645` (gcw-2gc) shared transient-conn classifier + nudge retry
   - `5255042ba` + `1051b1989` pool-route fail-fast for min=0 pools
   - `088f9dba8` + `2f7598a43` codex-hooks double-write fix
   - `518da1a64` (gcw-7te.2) bound start-path metadata writes
   - `79fcadae9` (gcw-7te.1) `SessionStartStalled` event
   - `433394d65` derive `session_name` from alias; `f6ecd19df` renamed-session mail resolve
   - `7fa4be147` `default_sling_strategy`; `c2d8f359b` session-kill help; `736681f68` mail-inject refactor
   - `6df53edd0` context-inject handoff tiers (references fork self-restart skill — light fork coupling)
   - `9546405a9` pool-worker nudge redelivery (**needs t3bridge decoupling** before a PR)

4. **1 `INVESTIGATE`:** `e105746e8` (canonical `worker_dir` dual-stamp) — upstream
   converged the read-precedence half but not the creation-time dual-stamp, and the
   area was refactored (#4158/#4017) so the fork patch won't apply cleanly. Decide:
   re-derive on upstream, or drop if the read-side precedence suffices.

---

## Full inventory

### RETIRE-AFTER-MERGEDOWN (18) — merged upstream, self-dedupes

| SHA | What | Upstream |
| --- | --- | --- |
| 7f5517949 | wake assigned root-only molecule wisps | in main (evolved signature) |
| 201de3475 | test: missing-session-bead hook claim | #3882 MERGED |
| 598c62ef3 | tolerate missing session bead during drain-ack | #3882 MERGED |
| 450e8175a | tolerate non-string bead metadata (StringMap) | #3857 MERGED |
| b5199648c | gc-hook skip is_blocked routed beads | #3881 MERGED (wbern) |
| aacd07fb0 | activate native store on unreachable bd ctx | #3940 MERGED — **dup of 22c75f175** |
| 238145365 | tini PID-1 orphan reap | #4004 MERGED |
| 20947c7fb | exclude order-tracking events from triggers | #3720 MERGED |
| f977c6820 | ignore bare-template demand for expanded identity | #3865 MERGED |
| a9ac94917 | wake warm tmux pool workers on routed work | #4083 MERGED |
| 4fe89d878 | confirm orphans dead before restart | #4089 MERGED |
| 4fac64a60 | share one ready snapshot per store | #4177 MERGED |
| f31df4522 | retry sqlite busy/locked in write classifier | #4155 MERGED |
| 22c75f175 | activate native store on unreachable bd ctx | #3940 MERGED (wbern) |
| b520cd759 | conditional orphan release (CAS) | #4151 MERGED |
| 762b64b0f | batch molecule-closure deletes via cascade | #4202 MERGED |
| b30b7a83c | bound native reconnect lifecycle | #4197 MERGED |
| 5efe8dadc | project GH_TOKEN into exec-order env | #4111 MERGED |

### KEEP-PENDING-MERGE (4) — open upstream PR

| SHA | What | Upstream PR |
| --- | --- | --- |
| f3ad19579 | recycle claim-holding wedged sessions | **#4137** OPEN (green/mergeable) |
| e0ff4684b | capture claude resume session_key | **#3954** OPEN (wbern) |
| a3a02917c | harden claude session_key test coverage | #3954 OPEN |
| aee42c3f3 | make claude session_key capture observable | #3954 OPEN |

### KEEP-FORK-PERMANENT (8) — fork-specific

| SHA | What | Why fork-only |
| --- | --- | --- |
| c6bc99684 | t3bridge honor wake_mode=fresh | T3 bridge (fork-only runtime) |
| 7e6b37971 | pin beads v1.0.5 + build guard | fork dep pin (upstream on v1.1.0) |
| a03b51a73 | show fork revision in gc version | fork build/version |
| baa33c8f9 | pass waitTimeout in fork-only test | fork-only test file |
| 8f8077dc9 | workspace-cleanup engdoc | fork engdoc |
| 819925b32 | gitignore runtime skill symlinks | fork worktree housekeeping |
| 01c8db7bf | gitignore genspec + .metadata_never_index | fork build/OS artifacts |
| f5439a4db | untrack generated provider configs | fork provider-config housekeeping |

### CONTRIBUTE-UPSTREAM (21) — fork fixes, no upstream equivalent

| SHA | What | Note |
| --- | --- | --- |
| 08d6f1b32 | gc worktree scan CLI | clean, no coupling |
| 0b37f91ed | tmux recordPokeAt (poke-discount nudge) | claim-holder dep |
| 8dd704296 | skill-content edit stops session recycle | clean |
| 77688848f | session pointers on city store | pairs w/ 95aa213b6 |
| 95aa213b6 | scope session-pointer writes to city store env (gcw-z7b1) | pairs w/ 77688848f |
| 833a0de70 | clear reset_committed_at on wake (gcw-8co2) | clean |
| 4de6c1c02 | shared transient-conn classifier (gcw-2gc) | pairs w/ e1361e645 |
| e1361e645 | retry nudge notify on transient Dolt err (gcw-2gc) | depends on 4de6c1c02 |
| 5255042ba | pool-route fail-fast for min=0 pools | pairs w/ 1051b1989 |
| 1051b1989 | test floor worker for above | travels w/ 5255042ba |
| 088f9dba8 | codex-hooks double-write fix (gcw-mnck) | pairs w/ 2f7598a43 |
| 2f7598a43 | test codex convergence keeps hooks | travels w/ 088f9dba8 |
| 518da1a64 | bound start-path metadata writes (gcw-7te.2) | clean |
| 79fcadae9 | emit SessionStartStalled event (gcw-7te.1) | clean |
| 433394d65 | derive session_name from alias | clean |
| f6ecd19df | resolve renamed named-session mail targets | clean |
| 7fa4be147 | default_sling_strategy (random/round_robin) | clean feature |
| c2d8f359b | clarify gc session kill help text | trivial docs |
| 736681f68 | extract mail-inject reminder builder | pure refactor (marginal) |
| 6df53edd0 | context-inject tiers → gc handoff | light fork coupling (self-restart skill) |
| 9546405a9 | redeliver pool-worker nudges (gcw-7xh4) | **needs t3bridge decouple** first |

### RETIRE-NOW-OBSOLETE (7) — upstream converged differently

| SHA | What | Superseded by | Conflict risk |
| --- | --- | --- | --- |
| a14cca88b | CGO test-shard snapshot vars | upstream refs CGO vars directly | **YES** (contract test fails) |
| b24a8685d | control-dispatcher rig-store fanout (gcw-tjeg) | #4175 (incompatible design) | **YES** |
| a6ace4cf0 | per-rig work-query env fanout (gcw-kcm4) | #4175 | **YES** |
| 19f8f52a5 | re-nudge tmux pool slots (gci-a8y) | #4083 (idle_nudge.go already identical) | low |
| 4f66d7242 | canonicalize stale-alias assignee | buildSessionAssigneeIndex/alias_history | low |
| c715e832a | dolt quarantine recovery doc | already removed from fork HEAD | none (dead) |
| f1285efb3 | --wait-timeout flag | upstream flag, **different `0` semantics** | ⚠ behavior loss |

### INVESTIGATE (1)

| SHA | What | Issue |
| --- | --- | --- |
| e105746e8 | canonical worker_dir dual-stamp (gcw-xgfk.1) | upstream converged read-side only; won't apply cleanly (area refactored #4158/#4017) |

---

## Method / reproduction

```bash
git fetch upstream
git cherry -v upstream/main origin/develop | grep '^+'     # the 59
# per commit:
git show <sha> --stat
gh pr view <NUM> --repo gastownhall/gascity --json state,mergedAt,title   # PR-numbered
git grep '<distinctive symbol>' upstream/main -- <file>                   # symbol search
```

Classification was fanned out across 6 parallel classifier agents (≈10 commits
each), each verifying upstream status by PR-merge-state and symbol search, then
synthesized here.
