# Code Review Gate

This document is the semantic review checklist for `gas-city-wbern` (the Gas
City SDK and its fork integration layer). Mechanical checks (`go vet`, the
test suites, `make dashboard-ci`, CI) say whether the branch builds and its
tests pass. This gate says whether the branch should merge.

It is deliberately modelled on the CRM rig's `docs/code-review.md` and on
`gas-city-infra`'s `doc/code-review.md` so the same diff-driven reviewer can
grade all three, but the lenses below are this repo's own. This repo ships
the orchestration SDK other cities configure themselves on top of. The
difference that matters for review: **a defect here does not produce a bad
screen, it produces a role name baked into Go, a layering violation, or an
untyped wire field — damage that is invisible in this rig and only surfaces
in a consumer city's behavior.**

The reviewer reads the bead, the branch diff against the target branch, the
applicable ADRs from `doc/adr/README.md`, `AGENTS.md`, and any docs the diff
touches. Findings are blocking only when the reviewer has high confidence the
branch regresses behaviour, safety, or a standing rule.

## Required Inputs

- Work bead ID, title, description, acceptance criteria, notes, and metadata.
- Source branch, target branch (default `main`), diff from `origin/<target>...HEAD`.
- ADR index at `doc/adr/README.md`.
- `AGENTS.md` (design decisions, layering invariants, active migrations).
- Mechanical check results (`go vet`, relevant package tests, `make
  dashboard-ci` when the diff touches `internal/api/`).

## The sourcing rule

A finding may block **only** if it cites one of: an ADR section, a lens in
this document, a named check script or test (e.g.
`TestEveryKnownEventTypeHasRegisteredPayload`), or a `file:line` precedent in
this repo. "Best practice", "this seems fragile", and "I would have done it
differently" are not sources and are not blocking findings. Say them as `nit`
or do not say them.

## Review Lenses

### 1. Behavioural Correctness

Does the change do what its bead and its own comments claim? Trace the actual
control flow rather than the described one, including error paths and the
boundary conditions of any new config/CLI flag.

### 2. ZERO Hardcoded Roles

**The core invariant of this SDK.** No line of Go may reference a specific
role name (`mayor`, `polecat`, `deacon`, or any pack-specific role). `if role
== "mayor"` is a design error, not a style nit — roles are pure
user-supplied configuration. A finding here blocks regardless of how small
the diff is, because the whole point of Gas City is that the SDK has no
built-in roles. Also check for role names smuggled in as string constants,
log messages used for control flow, or test fixtures that leak into
non-test code paths.

### 3. Keep Judgment Out Of Go

Go handles transport, not reasoning. If a line of Go contains a judgment call
— `if stuck then restart`, a heuristic threshold, a decision tree over agent
behavior — that is framework intelligence that belongs in a prompt template,
not in the SDK. Ask: does any line of Go decide something a human or model
should decide? A primitive must also become **more** useful as models
improve; a hardcoded heuristic becomes **less** useful. Flag any PR that adds
decision logic in place of config or prompt-driven behavior.

### 4. Layering Invariants

- **No upward dependencies.** Layer N never imports Layer N+1 (Session/Task
  Store/Event Bus/Config/Prompts, then Messaging/Formulas/Dispatch/Health
  Patrol above them — see `AGENTS.md` "Code-layering view").
- **Beads is the universal persistence substrate** for domain state — no
  parallel ad hoc state files for data that beads already models.
- **Events are the universal outbound-notification mechanism.**
- **Config is the universal activation mechanism.**
- **Side effects (I/O, process spawning) are confined to Layer 0** —
  `internal/runtime/`, not scattered through formula/dispatch logic.
- **No status files.** Never write PID files, lock files, or state files to
  track running processes; query live state (process table, `ps`, `lsof`)
  instead. A status file goes stale on crash and creates false positives.

### 5. Typed Wire And Typed Events

- **No hand-written JSON on any HTTP or SSE wire path.** No `map[string]any`
  or `json.RawMessage` on wire types outside the documented exceptions in
  `engdocs/architecture/api-control-plane.md`. All endpoints are
  Huma-registered; the OpenAPI spec is generated, never hand-written
  (`TestOpenAPISpecInSync`).
- **Every event constant needs a registered payload.** A new entry in
  `events.KnownEventTypes` without a matching `events.RegisterPayload` call
  (or explicit `events.NoPayload`) fails
  `TestEveryKnownEventTypeHasRegisteredPayload` — treat a PR that adds one
  without the other as incomplete, not just untested.
- **Vendor-neutral hosted-service wire stays vendor-neutral.** Account/
  commercial policy (trial, billing, credit, plan, quota, org/tenant
  identity) must never become a wire field in `internal/cliauth` /
  `internal/serviceproto`; it only ever travels in the opaque server-authored
  `message`/`links` fields. Enforced by `scripts/check-core-boundary.sh`
  check (f).

### 6. Active Migration Compliance

Check `AGENTS.md`'s "Active migrations" section against the diff:

- New non-test `cmd/gc/*.go` session-creation code must route through
  `worker.Handle`, not `session.NewManagerWithOptions(`,
  `worker.SessionHandle`, `sessionlog`, or other bypasses
  (`TestGCNonTestFilesStayOnWorkerBoundary`).
- No reconstruction of the removed Agent Protocol `Agent`/`Handle`
  interfaces; `internal/agent/` stays a small helper package, not a
  primitive.

### 7. Config Field Sync

A new field on `config.Agent` must also land in `AgentPatch` and
`AgentOverride`, be wired into `applyAgentMutation`
(`internal/config/patch.go`), be copied in `AgentOverride.toAgentPatch`, and
— if it is a slice/map/pointer — be deep-copied in `Agent.Clone`
(`internal/config/config.go`). A PR that adds the field but skips one of
these should fail `TestAgentFieldSync`,
`TestApplyAgentPatchCoversAllFields`/`TestApplyAgentOverrideCoversAllFields`,
or `TestAgentCloneIsDeep` — if it doesn't, that is itself a finding (the test
coverage has a gap). A new field on `config.Rig` needs the corresponding
`RigPatch` field and `applyRigPatch` wiring; there is no field-sync test for
`Rig`, so check it by hand.

### 8. ADR Compliance

Cross-check the diff against `doc/adr/README.md`'s `applies_to` globs. A
change that reverses a recorded decision without superseding the ADR in the
same change is a finding (see the ADR-0001 rationale in `AGENTS.md`: a
decision reversed silently once went unnoticed for five days and nearly
caused a repair that re-armed something deliberately disabled).

### 9. Build, Cache, And Test Hygiene

- No `go clean -cache` in any script, hook, or agent-facing code path —
  it corrupts the shared `GOCACHE` for every concurrent executor
  (`go clean -testcache` is fine).
- No `GOCACHE`/`TMPDIR` pointed at `/tmp` (size-capped tmpfs shared
  fleet-wide); an isolated cold build must use `mktemp -d -p /var/tmp` with a
  `trap` cleanup.
- `Makefile`'s `TEST_ENV` and the hermetic wrappers in
  `scripts/test-local-parallel`, `scripts/test-go-test-shard`, and
  `scripts/test-integration-shard` must stay in sync on
  `GIT_CONFIG_NOSYSTEM=1` / `GIT_CONFIG_GLOBAL=/dev/null` — editing one
  without the others reintroduces non-hermetic git config.
- After adding/changing imports, `make bazel-sync` output (gazelle +
  regenerated BUILD files) must be committed, or the "BUILD files are in
  sync" CI gate fails.
- A bug fix carries a test that fails without it, next to the code it fixes
  (`config.go` → `config_test.go`). Integration tests needing real
  infrastructure belong in `test/` with `//go:build integration`.

### 10. Git And Tmux Safety

- No bare pathspec `git checkout <ref> -- .` in a shared worktree — it
  overwrites the index and working tree with no reflog entry and stages
  nothing, so overwritten uncommitted work is unrecoverable.
- No bare `tmux kill-server`, and no cleanup that is not scoped to a named
  city/test socket (`tmux -L <socket> ...`) or routed through `gc stop`.

### 11. Documentation And Rationale

Comment blocks and ADRs are load-bearing — they are the record of *why* a
guard or a design choice exists, not restated history of what changed. A
non-obvious decision (a workaround, an invariant, a measured incident) needs
its rationale inline or in an ADR; a change that deletes such a block without
preserving the knowledge is a finding. Every exported function needs a doc
comment.

## Non-Blocking Cases

- Pre-existing failures on the target branch are not branch regressions. File
  or find a bead for them and continue.
- Documentation-only changes do not require tests unless they change a
  contract code relies on.
- Style preferences with no behavioural, safety, layering, or ADR impact are
  not findings.
- Adding `pkg/` exports, new SDK-exported interfaces, or new primitives is
  **out of scope for a single PR** unless the bead explicitly asks for it —
  "No premature abstraction" is a settled design decision; a reviewer should
  not request a general mechanism in response to a one-off change.

## Reviewer Output

Findings first, ordered by severity, each with file and line references:

- `severity`: `blocker`, `major`, `minor`, or `nit`.
- `confidence`: `high`/`medium`/`low`.
- `source`: the ADR section, lens number, or named check being cited.
- `evidence`: changed file and the exact reason.
- `expected fix`: a concrete action.

If there are no findings, say so explicitly and summarise residual risk,
including any pre-existing target-branch failures.
