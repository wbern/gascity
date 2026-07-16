# Route `bd update --claim` through the warm controller (gcw-7ejp / gci-p9mo)

## Problem

The CRM reviewer pool wedges: reviewers spawn but `gc hook --claim
--drain-ack` returns RC=124 (timeout), so routed review beads are never
claimed and PRs stall on the `code-review` commit-status gate.

Root cause (verified by trace, not assumed): the hook claim path is a `bd`
CLI **subprocess** (`BdStore.Claim` → `bd update <id> --claim`), not the
in-process `NativeDoltStore`. Under remote Dolt (Tailscale, ~1.5s cold dial
per `bd` invocation) the subprocess's bounded timeouts (`run_bounded`
`timeout`, exit 124; the 10s claim-mutation ctx; an outer `timeout N gc
hook` pool wrapper) fire below any adequate retry budget. The blessed bd-shim
(`cmd/gc/cmd_bd_shim.go`, shipped) deliberately **passes `bd update --claim`
through** to the real bd — `bdUpdateRoutableFlags` omits `--claim` — because
claim had no in-process translation yet. Claim is therefore the residual
remote-Dolt surface the shim never relieved.

`write_retry_enabled` / `withReadRetry` are **irrelevant** here: they live on
`NativeDoltStore` and the claim path never constructs one.

## Why the fix must run on the controller's *native* store

The warm-controller win only materialises if the claim executes on the
controller's **persistent** store handle. The live city runs native Dolt
(`[dolt] write_retry_enabled = true`, `conn_max_idle_time` — both
`NativeDoltStore`-only knobs), so the controller holds a warm `*sql.DB` pool
that reuses one connection instead of paying the ~1.5s cold dial per op. A
`bd`-subprocess claim *on the controller* would be just as cold — so the
claim must be a native-store operation, not a shelled-out `bd update`.

`NativeDoltStore` had `ReleaseIfCurrent` (conditional assignment *release*)
but no claim. Claim is its inverse.

## Actor conveyance (the crux)

`bd update --claim` claims for the `BEADS_ACTOR` env actor. The controller is
a different process with a different identity (`controller`/`gascity`), and
there is **no per-request actor plumbing** on any existing bead-write path.
So the claim must carry the *calling agent's* actor explicitly:

    shim reads $BEADS_ACTOR  →  POST /bead/{id}/claim {actor}  →  ClaimAs(id, actor)

`ClaimAs` takes the actor as a parameter (the net-new capability), rather than
baking it into a store's env at construction.

## Design

New optional store capability (discovered by type-assertion, like
`ConditionalAssignmentReleaser`):

    type ActorClaimer interface {
        // ClaimAs atomically claims id for actor: if the bead is claimable
        // (open/unassigned, or already held by actor) set assignee=actor,
        // status=in_progress and return (bead, true, nil); if another actor
        // holds it (or it is closed) return (currentBead, false, nil); if it
        // does not exist return (Bead{}, false, ErrNotFound).
        ClaimAs(id, actor string) (Bead, bool, error)
    }

Idempotent self-claim (already held by actor) returns ok=true with no state
change, so a `withOpRetry` reconnect-retry can safely re-run the whole op.

- `NativeDoltStore.ClaimAs` — `withOpRetry(RunInTransaction(check-and-set))`.
  Commit actor = the claiming actor, for provenance fidelity.
- `MemStore.ClaimAs` — same logic in-memory (unit-test backend).
- `CachingStore.ClaimAs` — forwards to the backing `ActorClaimer`, then on a
  successful claim refreshes the cache and fires `bead.updated` (mirrors
  `ReleaseIfCurrent`). Returns `ErrActorClaimUnsupported` if the backing is
  not an `ActorClaimer`.

HTTP:

- `POST /v0/city/{cityName}/bead/{id}/claim`, body `{ "actor": "<id>" }`,
  result `{ "claimed": bool, "bead": <Bead> }`. A lost race is `claimed:false`
  (200, not an error) carrying the current holder, mirroring the store's
  `(bead, false, nil)`. `404` for a missing bead. Unsupported backend surfaces
  as a typed error the shim can fall back on.
- `Client.ClaimBead(id, actor) (beads.Bead, bool, error)`.

Shim:

- `bd update <id> --claim` routes to `ClaimBead(id, $BEADS_ACTOR)` **only when
  `BEADS_ACTOR` is set**; otherwise passthrough (can't safely route).
- Output fidelity for the `gc hook` BdStore.Claim caller that parses it:
  success prints the claimed bead JSON (exit 0); a lost race prints an
  `already claimed by <holder>` message (exit 1) that `isBdClaimConflictMessage`
  matches; not-found prints a `not found` message (exit 1).
- Fallback: if the controller is unreachable or the backend can't claim, the
  shim execs the real bd (correctness preserved; no worse than today).

## Non-goals / follow-ups

- Secondary orphaned-wake-nudge purge (gcw-7ejp §2) is a separate slice.
- `BdStore.ClaimAs` / `FileStore.ClaimAs` are not implemented; those backends
  take the shim passthrough fallback. Live is native, which is covered.
