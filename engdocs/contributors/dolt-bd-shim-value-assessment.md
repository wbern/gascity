# Dolt / BD-Shim Value Assessment — post remote→local cutover

**Generated:** 2026-07-18 · **Author:** gas-city-wbern/architect · **Context:** William
moved the beads DB from a remote WAN Dolt endpoint to a single local gc-managed
`dolt sql-server` (cutover ~12:00, 2026-07-18; remote host `100.123.67.94`
retired, data verified byte-identical). His Mac is CPU-constrained. This assesses
which of the fork's Dolt / bd-shim machinery still earns its keep under the new
topology, and what to keep / reshape / retire.

Baseline: live city runs `origin/develop`; comparisons against `upstream/main`
(gastownhall/gascity). Five parallel research passes (lifecycle, resilience,
bd-shim, query-cost/CPU-profile, pinning/cutover) fed this synthesis.

---

## TL;DR

1. **Most of the Dolt machinery is upstream-owned, not a fork liability.** The
   lifecycle/cleanup/watchdog/port subsystem and nearly all of the
   connection-resilience layer are byte-identical to `upstream/main`. There is
   almost nothing to "delete" fork-side — deleting it would mean diverging from
   upstream to remove working, maintained code.

2. **The machinery was built for a churny *remote* Dolt; it's now cold
   insurance.** Reconnect/rebind/port-rediscovery/transient-retry all defend
   against a server that restarts, moves ports, or drops WAN connections. Under
   one stable local server they fire ~never. They cost ~nothing on the success
   path, so the correct action is *keep* (as cheap insurance), not *retire* —
   with one exception (the dormant fork-only write-retry slice).

3. **The CPU pain is new, local, and structural.** The live `dolt sql-server`
   runs 40–146% CPU and the supervisor 75–210%, because beads access is
   **subprocess-per-operation**: every read/write shells out to a fresh `bd`
   that opens a short-lived Dolt SQL connection and often exits without a clean
   `COM_QUIT`. `dolt.log` shows ~330 new connections/min and 131k `i/o timeout`
   reap errors. Before the cutover this cost sat on the remote host; now it sits
   on William's Mac. **This is the actual problem to solve.**

4. **The bd-shim is the one fork-owned asset that directly attacks it** — it
   collapses N per-agent connections into one warm pooled connection through the
   controller. It's v1/partial. **Keep and invest** (route the remaining hot
   read verbs). Everything else is either upstream or a config dial.

---

## What the fork actually owns (vs upstream)

| Cluster | Fork-owned? | CPU relevance | Verdict |
| --- | --- | --- | --- |
| Managed-Dolt lifecycle / cleanup / reaper / watchdog / port / preflight | **No** — byte-identical to upstream | ~0 (watchdog is one `os.Stat`/30s at 0.0% CPU; reaper/preflight are start-path/manual) | KEEP (upstream), no fork action |
| Connection resilience: reconnect / rebind / transient classifier / busy-retry | **Mostly upstream** (#3940/#4155/#4188/#4197 merged); fork-only: `IsTransientConnError`, nudge-retry, dormant write-retry | ~0 steady-state (all error-path-only) | KEEP as insurance; RETIRE-or-UPSTREAM the dormant write-retry |
| **bd-shim** (thin-client bd → warm controller) | **Yes, fork-only** | **Directly reduces** connection count/CPU | **KEEP + INVEST** |
| Query-cost opts (ready-snapshot memo, batch cascade, bound reads, memoized identity, ambient-env, order-check bypass) | Fork-only, all already live on develop | Reduce per-pass connection *count*; CPU high *despite* them | KEEP; two are clean upstream candidates |
| bd/DoltLite pinning + doctor + doltversion/doltauth/beads-exec | pin-guard fork-only; rest upstream | ~0 | KEEP guard; pin is already moot live |

---

## The CPU story (where the 146% goes)

Root cause spanning everything: **subprocess-per-operation → connection churn.**
Dolt CPU scales with *connection count* (TCP accept + session setup + reaping
dead Sleep sockets), not query complexity. Ranked hottest generators on live
`origin/develop`:

1. **Control-dispatcher control-ready query — dominant.**
   `workflowServeControlReadyQueryForBeads` wakes on *every*
   `BeadCreated/Closed/Updated` event, idle floor **5s on develop**, and spawns
   **~4–6 `bd --sandbox ready` subprocesses per poll** — multiplied by one
   dispatcher for the city + one per rig (~6 with 5 rigs). Fork commit
   `0d87004e9` exists purely because this path is so hot it kept losing its Dolt
   env. **Levers:** raise idle floor 5s→30s, debounce bead-event wakes,
   consolidate the N per-candidate `bd ready` spawns into one `assignee IN (…)`
   query.

2. **30s patrol reconcile tick.** One reconciler, 6 stores, ~6 reads/store/tick
   (~70 reads/min) after the ready-snapshot memo (`4fac64a60`), plus one
   `scale_check` subprocess per custom-scale_check agent per tick. **Levers:**
   widen `patrol_interval` 30s→45–60s (~30% cut to this path); fold implicit
   scale_checks into the memoized snapshot.

3. **Order gate evaluation** — bounded (≤4 dispatches/tick), tracking-index
   cached. `9a51fd979` optimizes only the *CLI* `gc order check`/doctor variant,
   not the in-daemon path — small effect on the 67/146%.

4. **Per-agent `gc hook` / `gc bd` calls** — diffuse, scales with active-agent
   count. Individual `bd` subprocesses sampled at 49–64% CPU each (Go startup +
   fresh connect). This is exactly what the bd-shim removes.

5. **Store-open preflight** — now memoized (`87bb30d87`), small. **Wisp GC** —
   OFF live. **Event flush** — OFF (`disable-event-flush: true`).

---

## Keep / Reshape / Retire — the fork's Dolt commits

### KEEP + INVEST (the fork-owned CPU lever)
- **bd-shim v1** (`fe3f570df`, `2440d781a`, `5ddc20413`, `c0c03d984`,
  `df28b4ef1`, `e67844e01`, `3f1c9c75a`). Routes `show/ready/update/create/close/
  --claim/mol/heartbeat` through the warm controller's `CachingStore` over one
  pooled `NativeDoltStore` connection. Measured `show` 2.5s→0.42s (WAN-era
  numbers; local benefit is connection-*concentration*, not latency).
  **v2 priority:** route the passthrough tail that still churns —
  `bd query` (parser exists, gated off) → `list` → `stats`/`count` → `dep`.
  Add a routed-vs-passthrough telemetry ratio (`RecordBDCall` already exists) so
  silent install-chain breakage is visible. Watch the read-lag correctness tax
  (every routed write needs a cache force-write like `ClaimAs`/`Close`).

### KEEP (cheap insurance / already-live opts)
- All upstream lifecycle + resilience (no fork action).
- Fork-only `IsTransientConnError` classifier (`4de6c1c02`) + nudge-retry
  (`e1361e645`) — trivial, error-gated; **good upstream-contribution candidates**
  to erase divergence.
- Query-cost opts: ready-snapshot memo (`4fac64a60`) and memoized identity
  (`87bb30d87`) are the cleanest **upstream candidates** (self-contained,
  measured, per-store/scope). `762b64b0f` (batch cascade delete) is latent —
  KEEP (wisp GC is off; enabling GC without it would be catastrophic).
- `9a51fd979`, `0d87004e9`, `e6373c634` — keep, but note they're bandaids over
  subprocess fragility that pooled reads would obviate.
- beads version pin-guard (`7e6b37971`, `scripts/check-beads-bd-version.sh`) —
  version-agnostic (compares go.mod ↔ `bd version` at build), catches gc↔bd
  skew that silently disables the native store. **KEEP.**

### RESHAPE
- **`e6373c634` request-cancellation is *creating* the `i/o timeout`/`broken
  pipe` log flood** — killing `bd` children mid-read drops connections uncleanly.
  Pooled reads make it moot; until then, accept the noise or bound differently.
- **Config dials** (zero code — see below).

### LEAVE DORMANT (William's call, 2026-07-18)
- **Fork-only dormant write-retry slice** (`withOpRetry` / `WriteRetryEnabled` /
  `createWithRetry` from `a1346c837` + `1b1425084`). Config-gated OFF, never
  enabled live; motivated by mid-rebind write ambiguity — a churny-remote
  artifact. **Decision: keep it config-gated-off as-is, revisit later.** Zero
  runtime cost; accepts a small fork-maintenance surface. Its *direction* (native
  pooled writes + reconnect) is correct and aligns with the bd-shim/native-pool
  investment, so it stays as a foundation rather than being torn out.

### Already moot
- **beads v1.0.5 pin** — live/develop already runs v1.1.0 = bd 1.1.0 = upstream
  1.1.0; only `origin/main` is stale and self-heals on next mergedown. No task
  needed.

---

## Immediate CPU levers (ranked by effort→payoff)

**Config-only (minutes, zero code):**
1. `dolt_auto_gc_enabled=OFF` (live config has it ON; origin/main template
   defaults OFF). Sheds background GC/compaction CPU; trade = noms-journal
   growth → pair with a scheduled `gc dolt compact`.
2. Widen `patrol_interval` 30s→45–60s.
3. Raise control-dispatcher idle floor 5s→30s + debounce bead-event wakes.
4. Maintenance orders: DevOps already re-tuned post-cutover (dolt-health 5m,
   pileup 2m/--reap, store-watchdog 30m; remote-dolt-watchdog disabled;
   `dolt-remotes-patrol` correctly targets GitHub *mirrors* = backup, keep). No
   further pruning needed — earlier "retire remotes-patrol" read was wrong.

**Structural (the real fix):**
5. Finish the bd-shim / native pooled reads — collapse ~330 conn/min into a warm
   pool. Highest ROI; fork-owned.
6. Consolidate control-ready query: N `bd ready` subprocesses → 1 predicated
   query.
7. Clean `bd` client shutdown (`COM_QUIT` on exit) — upstream `bd` fix; removes
   the reap-error flood at the source.

---

## Live validation (2026-07-18, DevOps gcw-gy67)

Both config levers applied live + mirrored to city-template. Deltas are
**directionally positive but confounded** (applying lever 1 required a fresh
dolt process, which alone lowers CPU; the city was also quiet):

- **Lever 1 — `auto_gc_enabled=false`:** dolt `%CPU` ~39% avg → ~19% avg.
- **Lever 2 — `patrol_interval` 30s→45s:** supervisor `%CPU` ~10% median
  (+266% transient) → ~idle (0–6%).

**Key confirmation — the churn is untouched.** connID rate stayed ~**445/min**
on the fresh server, `i/o-timeout` ~4/min → ~2/min (within noise). The levers
reduce *background-GC + reconcile* CPU; they do **not** touch
subprocess-per-op connection churn. **That churn is exactly what bd-shim v2
(gcw-j8oq) removes** — so the assessment's ranking holds: config levers = cheap
partial relief; bd-shim v2 = the durable fix.

**Operational lessons (DevOps):**
- `[dolt]` config changes only take effect via `gc dolt restart` (config
  regenerates on dolt start), **not** a supervisor bounce (bounce adopts the
  running dolt). A first attempt bounced-then-restarted, wedging the old server
  into a lock-holding zombie → ~6-min store blip (13:06–13:12), rode out via bd
  retry + circuit breaker, recovered by SIGKILL + keeper respawn. The
  resilience layer this doc rates as "cold insurance" earned its keep here.
- **Install-surface fragility CONFIRMED (the cluster-3 prediction):**
  `GC_BD_REAL` was missing from the supervisor plist env, so order-exec'd
  commands (gate-sweep, dolt-remotes-patrol, pileup-watchdog) hit the bd shim
  with no real-bd fallback → **182 order failures**. Patched live
  (`GC_BD_REAL=/Users/willi/go/bin/bd` in the plist). **Durable fix belongs in
  gc-core: order-exec must inject `GC_BD_REAL` the way session-exec does** —
  folded into the bd-shim v2 scope (gcw-j8oq) as a robustness prerequisite, and
  it validates the "add routed-vs-passthrough telemetry" recommendation.

## Latent bugs surfaced (not CPU-critical, worth a trace)
- `dolt.log`: `syntax error at position 9 near '$'` — an unexpanded `$` var
  leaking into a query (likely shell interpolation in a `bd --sandbox` call).
- `dolt.log`: `table "d" does not have column "depends_on_issue_id"` — schema
  drift on a dep query.

## Related follow-ups (outside the code inventory)
- **DevOps `gci-4x7s` cluster re-scope** (gci-bu92/eew9/wx8s/idmk/vn76/imbq):
  remote-Dolt monitoring is superseded by removing the remote. Re-point the
  cluster at (a) local connection-churn / CPU guardrails and (b) the
  idle-reap-vs-bd-reuse recycle-storm local guard DevOps flagged; drop the
  WAN-RTT / remote-monitor framing entirely.
- **Security-debt bead**: passwordless `root@'%'` on the retired remote Dolt is
  obsolete on retirement — close it.
