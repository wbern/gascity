---
status: accepted
date: 2026-08-08
applies_to:
  - "internal/dispatch/control.go"
  - "internal/graphroute/graphroute.go"
  - "internal/agent/session_name.go"
  - "cmd/gc/build_desired_state.go"
pre_filter:
  - "Assignee"
  - "SessionName"
  - "SanitizeQualifiedNameForSession"
  - "UnsanitizeQualifiedNameFromSession"
---

# 4. A bead's `assignee` holds a session name, not a qualified identity

## Context

One agent identity appears in the bead store under two spellings,
concurrently. Measured 2026-08-08 across non-closed beads on `gc2`:

```
gas-city-wbern/architect    27      gas-city-wbern--architect   12   (same identity, 39)
gas-city-infra/devops       12      gas-city-infra--devops       7   (same identity, 19)

gc bd list --assignee gas-city-wbern/architect   -> 27   (69% of that agent's work)
gc bd list --assignee gas-city-wbern--architect  -> 12   (31%)
```

Neither query errors, neither warns, and both look complete. This was filed as
a P1 data-integrity bug (`gcw-2ozk`) on the reasoning that a transport encoding
had leaked into a persisted field. **That reasoning was wrong**, and the code
says so plainly:

- `internal/agent/session_name.go` documents `rig--agent` as the deterministic
  **tmux-safe encoding** of the qualified identity `rig/agent` (`/`→`--`,
  `.`→`__`) and ships a decoder, `UnsanitizeQualifiedNameFromSession`. One
  identity, two encodings, by design.
- `internal/dispatch/control.go:1121` and
  `internal/graphroute/graphroute.go:206` both assign
  `step.Assignee = binding.SessionName`. The `--` rows are written
  deliberately: `assignee` names the **session** that holds the work.
- `gc hook` already normalizes — measured, it returns the same result for
  both spellings. Normalization also exists at
  `cmd/gc/build_desired_state.go:4363` and on read in `cmd/gc/pool.go` and two
  API handlers.

The bug was the expectation, not the data. `gc bd list --assignee` is a
beads-level **exact-match string filter** (verified: `--assignee architect`
returns 0, so no substring matching). Nothing in it resolves an identity.

The same misexpectation occurred twice in one day: `gc bd list` without
`--status` is a documented "not closed" filter that two readers each used as
"open" or "all". Both are documented exact-match filters that were expected to
perform resolution, and because a filter returns a well-formed subset rather
than an error, a wrong expectation reads as a correct answer.

## Decision

`assignee` holds a **session name** — the tmux-safe encoding — when written by
the dispatch and graph-routing paths. This is intended, is not a leak, and is
not to be "fixed" by normalizing writes to the qualified form.

Identity resolution across the two encodings is the job of identity-aware
surfaces (`gc hook`, the pool and API handlers), not of raw beads filters.

**A filter answers the question you typed. A resolver answers the question you
meant.** Callers of `gc bd list --assignee` must treat the result as scoped to
one encoding and state that scope alongside any number derived from it.

## Consequences

Nobody should file the two-spellings observation as a defect again. It has been
filed once, at P1, by an architect holding the relevant framework at the time;
this ADR is the artifact whose absence made that possible.

A genuine open question survives, at low priority (`gcw-2ozk`, now P3 and
retyped from bug to task): there is no identity-aware surface answering
"everything assigned to me" across both encodings. `gc hook` answers "what is
*ready* on my hook" — it returned 1 while 39 beads carried that identity — so
it is not that surface. Whether one should exist is a DX question, not a
correctness one.

Anyone adding a third identity spelling, or writing a qualified identity into
`assignee` from a new path, is changing this decision and should supersede this
ADR. Note that `created_by` already carries session-derived forms matching no
agent name (`architect-gc2-wisp-7nars3`, `s-gc2-c6za`), so identity is
fragmented in at least three shapes; that is out of scope here and unowned.
