# Architecture Decision Records

Decisions that constrain code in this repository, and the files each one
governs. See [ADR-0001](0001-record-architectural-decisions-with-adr-lint.md)
for why this exists and how it is enforced.

| ADR | Status | Decision | Governs |
| --- | --- | --- | --- |
| [0001](0001-record-architectural-decisions-with-adr-lint.md) | accepted | Record decisions as ADRs under `doc/adr/`, linted by `adr-lint`, advisory, local-only | `doc/adr/**`, `.githooks/**` |
| [0002](0002-bdshim-stays-gc-bd-does-not-become-standalone.md) | accepted | bdshim stays; `gc bd` reaches `bd` by exec, not in-process | `cmd/bdshim/**`, `internal/bdshim/**`, `internal/bddispatch/**`, `cmd/gc/cmd_bd.go` |
| [0003](0003-worktree-removal-must-check-ignored-files.md) | accepted | Automated worktree removal must check ignored files; `git worktree remove` is not a backstop | the three `cmd/gc` worktree-removal paths, `internal/git/**` |
| [0004](0004-assignee-holds-a-session-name-not-a-qualified-identity.md) | accepted | A bead's `assignee` holds a session name, not a qualified identity | `internal/dispatch/control.go`, `internal/graphroute/graphroute.go`, `internal/agent/session_name.go` |

## Writing one

Copy [`templates/template.md`](templates/template.md) — it documents every
frontmatter field. The two that matter most:

- **`applies_to`** — doublestar globs for the files this decision governs.
  This is the load-bearing field. An ADR without accurate `applies_to` is
  documentation: `adr-lint` will never surface it against the change that
  violates it. Negation works (`"!vendor/**"`).
- **`pre_filter`** — cheap substring gate. If none of these strings appear in
  the diff, the model call is skipped entirely. Use it for ADRs that forbid
  specific symbols or libraries.

Set `status: proposed` when the decision is not yours to make. ADR-0002 was
filed that way — it recorded a choice already taken in code so it could be
ratified or reversed explicitly rather than remaining implicit. It was ratified
the same day, which closed an epic that had been blocked for weeks on a
decision nobody had written down.

## Superseding

Do not delete or silently edit an accepted ADR. Write a new one and mark the
old `superseded` with `superseded_by`. The failure this whole mechanism exists
to prevent is a decision reversed in code with no retraction — measured twice
in this fleet on 2026-08-08, once costing five days and a nearly-deployed
repair that would have re-armed something a rig had deliberately disabled.

## Running the linter

```bash
go install github.com/wbern/adr-lint/go/cmd/adr-lint@latest
adr-lint            # validates staged changes against doc/adr/
```

It runs automatically from `.githooks/pre-commit`, **advisory** — it reports
and never blocks, because the analysis is model-backed and non-deterministic.
It is not wired into CI, by adr-lint's own ADR-0003.

Requires the Claude Code CLI on PATH and logged in. Without it the hook prints
that it skipped and the commit proceeds; the skip is never silent.
