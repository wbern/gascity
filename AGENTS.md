# Gas City

Gas City is an orchestration-builder SDK — a Go toolkit for composing
multi-agent coding workflows. It extracts the battle-tested subsystems from
Steve Yegge's Gas Town (github.com/steveyegge/gastown) into a configurable
SDK where **all role behavior is user-supplied configuration** and the SDK
provides only infrastructure. The core principle: **ZERO hardcoded roles.**
The SDK has no built-in Mayor, Deacon, Polecat, or any other role. If a
line of Go references a specific role name, it's a bug.

You can build Gas Town in Gas City, or Ralph, or Claude Code Agent Teams,
or any other orchestration pack — via specific configurations.

**Why Gas City exists:** Gas Town proved multi-agent orchestration works,
but all its roles are hardwired in Go code. Steve realized the way work is
expressed — as beads composed into formulas — was powerful enough to abstract
roles into configuration. Gas City extracts that insight into an SDK where Gas
Town becomes one configuration among many.

## Current integration mission

This fork is integrating **Gas City + T3 Code + a DoltLite-backed beads
store**. The target is not a permanent divergence from upstream Gas City.
The target is a maintainable integration branch whose useful changes can
track upstream easily and whose fork-specific behavior is isolated behind
small, obvious ownership boundaries.

When working here, assume three codebases matter:

- **This repo** (`/data/projects/gascity`): the Gas City SDK and the fork
  integration layer.
- **T3 Code** (`/data/projects/t3code` when present): the UI/runtime that
  hosts visible agent threads through the `t3bridge` runtime provider.
- **Beads / bd with DoltLite**: the work ledger backend. Gas City should use
  the normal beads abstractions and keep DoltLite-specific read/write behavior
  contained in beads/provider boundaries.

### Upstream alignment rules

- Keep `upstream/main` easy to merge. Prefer new files, small adapters,
  and fork-owned packages over broad edits to upstream-owned code.
- If upstream code must change, make the patch minimal and idiomatic so it
  can be rebased, dropped, or proposed upstream cleanly.
- Do not bury T3 Code or DoltLite assumptions in generic SDK paths. Put
  provider-specific behavior behind the existing runtime, config, or beads
  backend boundaries.
- Before rebuilding a missing feature from scratch, search history. This
  fork has repeatedly lost working code during branch churn; older branches
  and commits often already contain the fix.
- Treat archived plans and audits as evidence, not gospel. Confirm against
  current code and current upstream before porting.

### Feature archaeology workflow

Use git history deliberately when a feature appears missing or regressed:

```bash
git remote -v
git fetch upstream
git log --all --oneline --decorate --grep '<keyword>'
git log --all --oneline --decorate -- <path>
git show <commit>:<path>
git diff upstream/main...HEAD -- <path>
git range-diff upstream/main...HEAD
```

Useful search targets:

- T3 bridge/runtime: `t3bridge`, `T3Bridge`, `internal/runtime/t3bridge`,
  `cmd/gc/template_resolve_t3bridge.go`
- DoltLite/beads backend: `doltlite`, `DoltLite`, `internal/beads`,
  `providers.go`, `beads_provider_lifecycle`
- Prior parity work: `engdocs/archive/analysis/gastown-upstream-audit.md`,
  `engdocs/archive/analysis/feature-parity.md`,
  `engdocs/contributors/dolt-regression-audit.md`

If history contains working code, prefer porting the smallest proven slice
instead of inventing a parallel mechanism.

This file holds only what applies everywhere. Rules for a specific area live
next to that code; read the matching file before changing it.

## Read first

| When you touch | Read first |
|---|---|
| `internal/api/`, `internal/events/`, `internal/extmsg/`, CLI code that constructs events or calls the API client, the OpenAPI spec, or the dashboard | `internal/api/AGENTS.md` |
| `internal/config/` (agent or rig config fields) | `internal/config/AGENTS.md` |
| `internal/session/` | `internal/session/AGENTS.md` |
| `internal/runtime/acp/` | `internal/runtime/acp/AGENTS.md` |
| Session creation or lifecycle from `cmd/gc` or `internal/api` | `internal/worker/AGENTS.md` |
| `internal/cliauth/`, `gc login`, `gc whoami` | `internal/cliauth/AGENTS.md` |
| `scripts/`, test runners, or git hooks | `scripts/AGENTS.md` |
| Any test | `TESTING.md` |
| Anything under `docs/` | `.claude/skills/gascity-docs/SKILL.md` |
| New SDK surface or a new primitive | `engdocs/contributors/primitive-test.md` |
| A controller or session reconciler incident | `engdocs/contributors/reconciler-debugging.md` (use `gc trace`) |
| Worktree isolation or worker cleanup | `engdocs/archive/backlogs/worktree-roadmap.md` (lifecycle analysis and cleanup-bug lessons) |
| A `release-gates/*.md` deploy gate | `engdocs/contributors/release-gate-criteria-conventions.md` |
| Bazel builds | `engdocs/bazel-quickstart.md` |
| Where a package lives | `engdocs/contributors/codebase-map.md`, then the package's `go doc` |
| What is planned | `ROADMAP.md` |

## How work flows here

- **GitHub Issues is the public tracker; use an issue when it adds
  context.** Open or link one for a user-visible bug, a behavior or design
  change that needs discussion, or work that spans several pull requests:
  the issue carries the reproduction, impact, and evidence for a bug, or the
  motivation, risk, and verification plan for a change. A small,
  self-explanatory fix (a typo, a flake, a refactor, a CI or docs tweak) can
  go straight to a pull request whose description covers the why. Every pull
  request shows end-to-end evidence that the change works; when an issue
  exists, the pull request says `Closes #<issue>`. See `CONTRIBUTING.md`.
- **Agents:** when you do file an issue, use the bug or feature form fields,
  fill each from evidence, and answer `NOT_ENOUGH_INFO` where the evidence
  runs out.
- Branch from `main` (`fix/*`, `feat/*`, `refactor/*`, `docs/*`), use
  Conventional Commits, and push to your branch — never to `main`.
- A fix that changes an invariant or boundary updates the owning `AGENTS.md`
  in the same pull request.
- Maintainers also keep an internal bd ledger; it is optional and never
  required for contributions. See
  `engdocs/contributors/maintainer-environment.md`.

## Where new capability belongs

Prefer the lowest rung that works: a prompt or template change → pack
configuration (agents, formulas, orders) → composing existing formula and
order machinery → an adapter behind an existing provider interface → new SDK
surface. New SDK surface must pass the Primitive Test
(`engdocs/contributors/primitive-test.md`).

## Development approach

**TDD.** Write the test first, watch it fail, make it pass. Every package
has `*_test.go` files next to the code. Integration tests that need real
infrastructure (tmux, filesystem) go in `test/` with build tags.

**The architecture docs are a reference, not a blueprint.** When the DX
conflicts with the docs, DX wins. We update the docs to match.

**Search history before rebuilding.** If a feature looks missing or
regressed, check `git log -S <symbol>` / `git log --all --grep <keyword>`
first; working code is often already in history, and a removal may have been
deliberate. Treat archived plans and audits as evidence, not gospel — confirm
against current code.

## Architecture

**Orchestration is the value.** A formula is a method for how a job gets
done, and the controller (`engdocs/architecture/controller.md`) runs it as a
graph — decomposing the job into beads, fanning the ready ones out to many
agents at once, gating each step on its dependencies, retrying failures, and
draining convoys in parallel, driving the work to completion outside the
user's session. The control dispatcher (`internal/dispatch`) executes control
beads — check, retry, fan-out, tally, drain, scope-check, workflow-finalize
(`docs/reference/specs/formula-spec-v2.md` sec 0) — and that dispatcher is
the engine. Orders trigger formulas on a schedule or event; health patrol
keeps the fleet alive.

**This orchestration is composed from primitives, with ZERO hardcoded
roles.** Beads are the universal persistence substrate — work survives
sessions — and every orchestration mechanism is provably composable from the
six primitives (**Agent** = WHO, **Bead** = WHAT, **Formula** = HOW,
**Rig** = WHERE, **Pack** = CONFIGURES, **Event** = OBSERVE), defined in
`docs/getting-started/how-gas-city-works.md`. That composability is what lets
the same SDK be configured as Gas Town, Ralph, or any other pack; no role
name appears in Go. (A single agent running a formula's steps in sequence in
the user's own session — formula v1 — is still supported as a peer shape,
but graph orchestration is why Gas City exists.)

The code-layering view — how sessions, the bead store, the event bus,
config, prompt templates, and the dispatch/health machinery implement the
six primitives, plus the progressive capability levels — is
`engdocs/architecture/nine-concepts.md`.

### Layering invariants

1. **No upward dependencies.** Layer N never imports Layer N+1.
2. **Beads is the universal persistence substrate** for domain state.
3. **Events are the universal outbound-notification mechanism** — fired by
   activity so humans and agents can watch; the bus is delivery machinery.
4. **Config is the universal activation mechanism.**
5. **Side effects (I/O, process spawning) are confined to Layer 0.**
6. **The controller drives all SDK infrastructure operations.**
   No SDK mechanism may require a specific user-configured agent role.

### Invariants enforced by CI

Violating any fails the build. Details live in the linked files.

- **Object model at the center.** The CLI (`cmd/gc/`) and the HTTP+SSE API
  (`internal/api/`) are projections over the domain packages; neither
  re-implements domain logic (`internal/api/AGENTS.md`).
- **Typed wire and typed events.** No hand-written JSON or untyped wire
  types; the OpenAPI spec is generated (`TestOpenAPISpecInSync`); every
  event type has a registered payload
  (`TestEveryKnownEventTypeHasRegisteredPayload`).
- **Vendor-neutral hosted-service wire.** Commercial policy is never a wire
  field (`internal/cliauth/AGENTS.md`, `scripts/check-core-boundary.sh`).
- **Worker boundary (active migration).** Production `cmd/gc` code reaches
  sessions through `worker.Handle`
  (`TestGCNonTestFilesStayOnWorkerBoundary`; `internal/worker/AGENTS.md`).
- **Agent config field sync.** A new `config.Agent` field is wired in every
  copy and merge site (`TestAgentFieldSync` and siblings;
  `internal/config/AGENTS.md`).

**Session-first (completed `dd90ac0a` on Mar 8 2026).** The former Agent
Protocol primitive was removed; responsibilities moved to
`internal/session/` (lifecycle) and `internal/runtime/` (providers).
`internal/agent/` is now a helper package with session-name utilities and
startup hints — not a primitive. Do not reconstruct the `Agent` / `Handle`
interfaces.

## Design decisions (settled)

These decisions are final. Do not revisit them.

- **City-as-directory model.** A city is a directory on disk containing
  `city.toml`, `.gc/` runtime state, and `rigs/` infrastructure.
- **Fresh binary, not a Gas Town fork.** We build `gc` from scratch.
- **TOML for config.** `pack.toml` (definition) and `city.toml` (deployment) are the config files.
- **Tutorials win over architecture docs.** When the docs disagree, we update the docs.
- **No premature abstraction.** Don't build interfaces until two
  implementations exist.
- **Mayor is overseer, not worker.** The mayor plans; coding agents work.
- **`internal/` packages for now.** SDK exports (`pkg/`) are future work.
  Everything is private to the `gc` binary until the API stabilizes.
- **ZERO hardcoded roles.** Roles are pure configuration. No role name
  appears in Go source code.

## Key design principles

- **Keep judgment out of Go.** Go handles transport, not reasoning. The
  framework moves work; it doesn't reason about it. If a line of Go contains
  a judgment call, it's a violation. **The test:** does any line of Go contain
  a judgment call? An `if stuck then restart` is framework intelligence. Move
  the decision to the prompt.
- **A primitive must become more useful as models improve.** Every primitive
  should grow MORE useful as models improve, not less. Don't build heuristics
  or decision trees.
- **If you find work on your hook, you run it.** No confirmation, no waiting.
  The hook having work IS the assignment. This is rendered into agent prompts
  via templates, not enforced by Go code.
- **The system converges because work persists.** The system converges to
  correct outcomes because work (beads), hooks, and molecules are all
  persistent. Sessions come and go; the work survives. Multiple independent
  observers check the same state idempotently and converge on it. Redundancy
  is the reliability mechanism.
- **No status files — query live state.** Never write PID files, lock files,
  or state files to track running processes. Always discover state by querying
  the system directly (process table, port scans, `ps`, `lsof`). Status files
  go stale on crash and create false positives. The process table is the
  single source of truth for "what is running."
- **SDK self-sufficiency.** Every SDK infrastructure operation (gate
  evaluation, health patrol, bead lifecycle, order dispatch) must
  function with only the controller running. No SDK operation may
  depend on a specific user-configured agent role existing. The
  controller drives infrastructure; user agents execute work. Test:
  if removing a `[[agent]]` entry breaks an SDK feature, it's a
  violation.

## What Gas City does NOT contain

These are permanent exclusions, not "not yet." Each fails the test of
becoming more useful as models improve — it becomes LESS useful instead.

- **No skills system** — the model IS the skill system
- **No capability flags** — a sentence in the prompt is sufficient
- **No MCP/tool registration** — if a tool has a CLI, the agent uses it
- **No decision logic in Go** — the agent decides from prompt and reality
- **No hardcoded role names** — roles are pure configuration

## Code conventions

- Unit tests next to code: `config.go` → `config_test.go`
- `t.TempDir()` for filesystem tests
- Integration tests use `//go:build integration`
- `cobra` for CLI, `github.com/BurntSushi/toml` for config
- Atomic file writes: temp file → `os.Rename`
- No panics in library code — return errors
- Error messages include context: `fmt.Errorf("adding rig %q: %w", name, err)`
- Don't swallow errors: no silently filled-in defaults, masked decode
  failures, or ignored timeouts.
- Comments describe current behavior; don't narrate removed functionality.
- Role names never appear in Go code. If you're writing `if role == "mayor"`,
  it's a design error.
- **Tmux safety:** Never run bare `tmux kill-server` as cleanup. Never kill the
  default tmux server. If tmux cleanup is required, target only the known
  city/test socket explicitly with `tmux -L <socket> ...`, or prefer `gc stop`
  for city shutdown. Treat personal tmux servers as out of bounds.
- **Git safety:** Never run `git checkout <ref> -- .` (or any pathspec
  checkout) in a worktree you do not own — above all the shared rig root
  (`$GC_RIG_ROOT`). Unlike `git checkout <ref>`, the pathspec form overwrites
  the index and worktree for every tracked path, moves no HEAD (so no reflog
  entry) and stages nothing (so no dangling blob): overwritten uncommitted
  work is unrecoverable. To read a file at a ref use `git show <ref>:<path>`.
  To check something out, use your own worktree or a disposable
  `git worktree add`.
- **Non-interactive shell:** `cp`, `mv`, and `rm` may be aliased to `-i` and
  hang an agent; use `cp -f`, `mv -f`, `rm -f`, `rm -rf`, `cp -rf`. Use
  `-o BatchMode=yes` for `ssh`/`scp`, `-y` for `apt-get`, and
  `HOMEBREW_NO_AUTO_UPDATE=1` for `brew`.

## Build and test

**Bazel is the build and test system; `bazel test` is what CI gates on**
(`.github/workflows/bazel.yml`). Plain `go test` is a quick inner-loop
convenience only: it skips nogo lint/vet, formatting, generated-artifact
and policy targets, and a green `go test` is not evidence a change passes CI.

| Tier | Command (`make` alias) |
|---|---|
| Unit + nogo + format + generated + policy | `bazel test //...` (`make test`) |
| One package while iterating | `bazel test //internal/config:config_test` |
| Acceptance (Tier A) | `bazel test --config=acceptance //test/acceptance:acceptance_test` (`make test-acceptance`) |
| Integration-tagged packages (gating) | `bazel test --config=integration //test:integration_packages` |
| `test/integration` (evidence-only in CI) | `bazel test --config=integration //test/integration:integration_test` |
| Docs sync | `bazel test //test/docsync:docsync_test` (`make check-docs`) |

- **Where it runs.** Agent hosts' `~/.bazelrc` names rbe-west's executor, so
  plain `bazel test` executes remotely; never run whole-repo `go test`
  fan-out on shared hosts. Contributors use `--config=fork-cache` (anonymous
  read-only cache, local execution of misses); maintainers with an rbe-west
  client certificate use `--config=remote-exec` (ask your human before
  adding either to `.bazelrc.local`). Details: TESTING.md
  "Bazel cache tiers" and `engdocs/bazel-quickstart.md`.
- **BUILD files.** After changing imports or adding packages or files, run
  `make bazel-sync` and commit the result; the `BUILD files in sync` CI gate
  fails otherwise. Never commit machine-specific endpoints; they belong in
  `.bazelrc.local`.
- **Go-native twins** (`make test-go`, `make check-go`, `make
  test-acceptance-go`, `make test-integration-go`, `make test-fast-parallel`)
  exist for offline work and hosts Bazel does not serve (macOS jobs).
- **Never run `go clean -cache`** — it corrupts shared build caches.
  `go clean -testcache` is fine. Maintainers on the shared build hosts: read
  `engdocs/contributors/maintainer-environment.md` before touching `GOCACHE`
  or `TMPDIR`.
- **Git hooks:** `make setup` installs `.githooks` as `core.hooksPath`;
  `make check-hooks` verifies it. Pre-commit runs nogo on staged packages;
  pre-push runs `bazel test //...` and says loudly when it falls back to
  `go test`. Beads' installer can silently take the path over and skip every
  gate — see "Git hook ownership" in `CONTRIBUTING.md`.

## Code quality gates

Before considering any task complete:

- `make check` passes (`bazel test //...` plus the shell guards; nogo is
  lint and vet)
- Acceptance or integration behavior changed: the matching `--config` tier
  above passes
- `.githooks/pre-commit` is active locally (verify with `make check-hooks`)
  and has run for the staged change
- `make dashboard-ci` passes and the dashboard serves locally for any change
  touching the API, the OpenAPI spec, or the dashboard
  (`internal/api/AGENTS.md`)
- Every exported function has a doc comment
- No premature abstractions
- Tests cover happy path AND edge cases

<!-- BEGIN BEADS INTEGRATION -->
## Beads Task Integration (MANDATORY)

This project uses `bd` (beads) for task tracking and development workflow.
Every development session MUST follow these rules:

1. **Start with context** - Run `bd ready` to find available work
2. **Claim your work** - Run `bd update <id> --claim` before starting
3. **Keep beads updated** - Update status as you make progress
4. **Coordinate schema and Dolt migrations deliberately** - Beads in gascity
   stores tasks in shared Dolt SQL databases (`beads_*.db`), tracked by git in
   `.beads/`. The beads client auto-commits metadata on writes.

   Never run `bd dolt pull`, `bd dolt push`, or `bd dolt remote add` here -- they fail and re-introduce
   a doomed `origin` remote (ga-9wsri). Use `git push` only.

   That same no-remote shape is why bd >= 1.3.0 refuses to auto-apply pending
   schema migrations to gascity's shared Dolt sql-server: migrating would lock
   out every co-resident bd still on the old schema. If a bd WRITE fails with a
   refusal naming pending migrations, the sanctioned fix is `bd migrate schema`
   run once by a designated migrator after every bd client is upgraded -- NOT
   `bd dolt pull`, and not an ad-hoc `BD_ALLOW_REMOTE_MIGRATE=1`.
5. **Clean up** - Clear stashes, prune remote branches
6. **Verify** - All changes committed AND pushed
7. **Hand off** - Provide context for next session

**CRITICAL RULES:**

- Work is NOT complete until `git push` succeeds
- NEVER stop before pushing - that leaves work stranded locally
- NEVER say "ready to push when you are" - YOU must push
- If push fails, resolve and retry until it succeeds
<!-- END BEADS INTEGRATION -->

## Architecture Best Practices

These apply to all code in this project — frontend and server:

- **TDD (Test-Driven Development)** - write the tests first; the implementation
  code isn't done until the tests pass.
- **Consider First Principles** to assess your current architecture against the
  one you'd use if you started over from scratch.
- **Leverage Types** using statically typed languages (TypeScript, Rust, etc) so
  that we can leverage the power of the compiler as guardrails and immediate
  feedback on our code at build-time instead of waiting until run-time.
- **DRY (Don't Repeat Yourself)** – eliminate duplicated logic by extracting
  shared utilities and modules.
- **Separation of Concerns** – each module should handle one distinct
  responsibility.
- **Single Responsibility Principle (SRP)** – every class/module/function/file
  should have exactly one reason to change.
- **Clear Abstractions & Contracts** – expose intent through small, stable
  interfaces and hide implementation details.
- **Low Coupling, High Cohesion** – keep modules self-contained, minimize
  cross-dependencies.
- **Scalability & Statelessness** – design components to scale horizontally and
  prefer stateless services when possible.
- **Observability & Testability** – build in logging, metrics, tracing, and
  ensure components can be unit/integration tested.
- **KISS (Keep It Simple, Sir)** - keep solutions as simple as possible.
- **YAGNI (You're Not Gonna Need It)** – avoid speculative complexity or
  over-engineering.
- **Don't Swallow Errors** by catching exceptions, silently filling in required
  but missing values, masking deserialization with nulls or empty lists, or
  ignoring timeouts when something hangs. All of those are errors (client-side
  and server-side) and must be tracked in a centralized log so it can be used to
  improve the app over time. Also, inform the user as appropriate so that they
  can take necessary action.
- **No Placeholder Code** - we're building production code here, not toys.
- **No Comments for Removed Functionality** - the source is not the place to
  keep history of what's changed; it's the place to implement the current
  requirements only.
- **Layered Architecture** - organize code into clear tiers where each layer
  depends only on the one(s) below it, keeping logic cleanly separated.
- **Use Non-Nullable Variables** when possible; use nullability only when there
  is NO other possibility.
- **Use Async Notifications** when possible over inefficient polling.
- **Eliminate Race Conditions** that might cause dropped or corrupted data
- **Write for Maintainability** so that the code is clear and readable and easy
  to maintain by future developers.
- **Arrange Project Idiomatically** for the language and framework being used,
  including recommended lints, static analysis tools, folder structure and
  gitignore entries.
- **Keep Serialization/Deserialization At The Edges** to make full use of
  type-safe objects in the app itself and to centralize error handling for
  type-system translation. Do NOT allow untyped data with known shapes to flow
  through the system and subvert the type system.
- **Prefer Well-Known, High Quality OSS Libraries** instead of hand-rolling your
  own behavior to get more robust, better maintained and better tested results.
- **Treat Static Warnings And Info As Errors To Be Fixed**. The whole point of
  static checking (linting, compilers, etc) is that they surface issues at
  build-time so that they can be fixed now instead of lead to errors at runtime.
  Take advantage of that feedback to fix those errors!
- **Use Centralized Semantic Constant Values** using enums and constants instead
  of spreading magic numbers throughout the code.
