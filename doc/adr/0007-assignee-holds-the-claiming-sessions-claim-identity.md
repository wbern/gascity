---
status: accepted
date: 2026-10-03
complexity: standard
applies_to:
  - "internal/dispatch/control.go"
  - "internal/graphroute/graphroute.go"
  - "internal/agent/session_name.go"
  - "cmd/gc/build_desired_state.go"
  - "cmd/gc/cmd_hook_claim*.go"
  - "cmd/gc/pool*.go"
pre_filter:
  - "Assignee"
  - "SessionName"
  - "BEADS_ACTOR"
  - "GC_AGENT"
  - "TransferIfCurrent"
---

# 7. A bead's `assignee` holds the claiming session's claim identity

Supersedes [ADR-0004](0004-assignee-holds-a-session-name-not-a-qualified-identity.md).

## Context

ADR-0004 recorded that `assignee` holds a **session name** (the tmux-safe
`rig--agent` encoding) when written by the dispatch and graph-routing paths,
and that adding a third identity spelling, or writing a different identity
into `assignee` from a new path, changes that decision.

The upstream sync of 2026-10-03 (gcw-o4qtt, merging gastownhall/gascity
`d7c81acd2`) adopts upstream's fix for a claim/close deadlock (upstream #5716,
landed as #6324 `c153c77b8` and #6640 `1337499b7`). An unaliased pool or
ephemeral session's runtime name changed to `<template>-<beadID>` (#6549),
and `bd close`/`bd update` from the worker were rejected with "assignee
mismatch" because the claim and the close used different spellings of the
same session. Upstream resolved it by making the session **bead id** the
single claim identity for unaliased sessions:

- `gc hook --claim` records the session bead id as `assignee` for an
  unaliased pool/ephemeral session, and the session's alias for a named
  session.
- Those sessions export `GC_AGENT` and `BEADS_ACTOR` as the same value, so
  the worker's own writes match the claim.
- When `gc hook --claim` adopts an in-progress bead held under a legacy
  spelling of the same session, it re-stamps the assignee by CAS
  (`bd update --if-assignee <old> --if-status in_progress --assignee <new>`,
  `BdStore.TransferIfCurrent`); a lost CAS is not adopted.

That is a third spelling written from a path ADR-0004 did not cover, so this
ADR supersedes it rather than leaving the reversal implicit.

## Decision

`assignee` holds the **claim identity of the session that holds the work**:

| Writer | Value written |
| --- | --- |
| dispatch / graph routing (`internal/dispatch/control.go`, `internal/graphroute`), step bound to a direct session | the session bead id (`binding.directSessionID` / `binding.DirectSessionID`) |
| dispatch / graph routing, any other binding | no assignee; the step is routed through `gc.routed_to` |
| `gc hook --claim` | the claiming session's alias when it has one, otherwise its session bead id |

Every row is intended. None is to be "fixed" by normalizing writes to a
single form. ADR-0004's dispatch/graph-routing writes of `binding.SessionName`
(`control.go:1121`, `graphroute.go:206` at the time) no longer exist: upstream
replaced them with the direct-session bead id above, so the `rig--agent`
session-name spelling now survives only on beads written before the sync.

One exception to the CAS re-stamp: when the work store's bd rejects
`--if-assignee` (bd < 1.2.1, which also predates bd's close/update actor
fence), `gc hook --claim` adopts the legacy spelling as-is rather than
refusing; it moves on the first claim after bd is upgraded.

ADR-0004's reading rule stands unchanged: identity resolution across
spellings is the job of identity-aware surfaces, not raw beads filters. The
assigned-work tier of the generated work query tries `GC_SESSION_ID`,
`GC_SESSION_NAME` and `GC_ALIAS` in turn, so beads assigned under a legacy
spelling keep matching after rollout, and adoption converges them onto the
claim identity. `gc bd list --assignee` remains an exact-match filter scoped
to the one spelling you typed.

## Consequences

- After the gcw-o4qtt rollout, a pool worker's in-progress bead may change
  assignee from a session name to a bead id when it is next adopted. That is
  the CAS convergence above, not data corruption.
- Counting "everything assigned to session X" from raw filters now needs up
  to three spellings; the open DX question in ADR-0004 (`gcw-2ozk`) is
  correspondingly larger, still not a correctness issue.
- A future change that writes yet another spelling into `assignee`, or that
  normalizes these writes, changes this decision and must supersede this ADR.
