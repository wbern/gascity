# Upstream issue draft — t3bridge startup envelope drops the managed-Dolt store-connection env

> **STATUS: HELD — do NOT file until post-bounce validation confirms.** The fix
> is landed on the fork (`dbf126b3d`) and code-proven, but the last link (the
> T3 runtime applying `gcEnv` onto the codex process env) is validated only at
> bounce. File the issue/PR upstream once a fresh codex session shows
> `GC_DOLT_PORT` in-session and claims its routed work. Target repo:
> `gastownhall/gascity`. Tracking (local): `gci-x8zo`, `gc2-nvf76`.

---

**Title:** t3bridge codex/pool sessions can't reach the managed Dolt store — startup envelope's `gcEnv` drops the store-connection env → `gc hook --claim` returns `no_work` → pool spawn-loop

## Summary

On a city whose beads backend is a **managed Dolt SQL server** (`bd init
--server` / the gc-managed `dolt sql-server`), agent sessions launched through
the **t3bridge** runtime provider (e.g. a `codex` pool worker) cannot reach that
server. Their `bd` falls back to a different/empty store, so a routed-work query
returns nothing, `gc hook --claim` reports `no_work`, and the pool respawns the
worker in a tight loop that never claims its routed work.

The same work is claimed immediately by a **tmux**-launched session (e.g. a
`claude` pool worker), because tmux sessions receive the full session env while
t3bridge sessions receive only a small hand-picked subset.

## Root cause (code-proven)

`cmd/gc/template_resolve.go` (Step 8) builds the full session `agentEnv`,
including the managed-Dolt store-connection env: the projected Dolt keys
(`GC_DOLT_HOST/PORT/USER/PASSWORD`, `BEADS_DOLT_SERVER_HOST/PORT/USER`,
`BEADS_DOLT_PASSWORD`, `BEADS_CREDENTIALS_FILE`) plus `GC_BEADS_SCOPE_ROOT`,
`BEADS_DIR`, `BEADS_ACTOR`, `GC_SESSION_ID`. `GC_DOLT_PORT` is rebuilt per
managed-server restart, so it is the live coordinate a session's `bd` needs to
connect.

- **tmux path:** the session process receives this entire `agentEnv`. `bd`
  connects to the managed server; routed-work queries see the controller-written
  work. ✅
- **t3bridge path:** `cmd/gc/template_resolve_t3bridge.go`
  `buildT3BridgeStartupEnvelope` projects a **hand-picked allowlist** into the
  startup envelope's `context.gcEnv`:

  ```go
  "gcEnv": map[string]any{
      "GC_AGENT":        tp.Env["GC_AGENT"],
      "GC_PROVIDER":     provider,
      "GC_TEMPLATE":     tp.Env["GC_TEMPLATE"],
      "GC_CITY_PATH":    tp.Env["GC_CITY_PATH"],
      "GC_RIG":          tp.Env["GC_RIG"],
      "GC_SESSION_NAME": tp.Env["GC_SESSION_NAME"],
  },
  ```

  It forwards **none** of the store-connection env. So the t3bridge session's
  `bd` has no managed-server coordinates and resolves a different store view that
  lacks the routed work. ❌

## Symptom → mechanism

1. Pool routes `mol-*-work` to a codex pool; dispatcher spawns a t3bridge worker.
2. Worker runs the claim-first work query
   `bd ready --metadata-field gc.routed_to=<pool> --unassigned --json`.
3. Its `bd` can't reach the managed store → query returns `[]`.
4. `gc hook --claim` → `{"action":"drain","reason":"no_work"}` → worker drains.
5. Routed work still looks ready → dispatcher respawns → **spawn-loop**; the bead
   never gets claimed (stays `open` / unassigned).

A tmux worker on the same routed bead claims it on the first turn.

## Proposed fix

Project the store-connection env into `gcEnv` alongside the identity vars —
ideally by reusing the canonical key list rather than a hand-maintained
allowlist that silently loses vars as new backend keys are added:

- the projected Dolt keys (the same slice the runtime env mirror uses), plus
- `GC_BEADS_SCOPE_ROOT`, `BEADS_DIR`, `BEADS_ACTOR`, `GC_SESSION_ID`,
  `GC_BEADS_BACKEND` / `BEADS_BACKEND`, `GC_CITY`.

Read them at envelope-build time so `GC_DOLT_PORT` reflects the current managed
server. (Fork implementation: build `gcEnv` from the identity vars + a loop over
`projectedDoltEnvKeys` + the scope/actor/session keys.)

**Note / open question for maintainers:** this forwards the vars in the startup
*envelope*; the t3bridge runtime must then apply `context.gcEnv` onto the child
process env for the fix to take effect. If the runtime already does that, the
envelope change is sufficient; if not, the process-env application step is the
real gap and belongs there.

## Questions

1. Is projecting the full store-backend env into the t3bridge envelope the
   intended shape, or should t3bridge sessions inherit the session `agentEnv`
   wholesale (as tmux does) so this class of "t3bridge is missing var X" bug
   can't recur?
2. Should the projected set be derived from a single canonical source shared
   with the tmux/runtime env path, to prevent drift?

## Evidence

- Diagnosis reproduced on a managed-Dolt city: codex pool worker loops on a
  properly-slung routed bead; tmux/claude worker claims the same bead.
- The env asymmetry is code-visible: `agentEnv` (Step 8) vs the `gcEnv`
  allowlist above; the allowlist contains zero store-connection keys.
- Regression test:
  `TestBuildT3BridgeStartupEnvelope_ForwardsBdShimEnvSoCodexRoutesThroughController`
  asserts the store-connection vars are present in the envelope's `gcEnv`.
