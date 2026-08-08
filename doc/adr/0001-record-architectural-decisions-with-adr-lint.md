---
status: accepted
date: 2026-08-08
applies_to:
  - "doc/adr/**/*.md"
  - ".githooks/**"
pre_filter:
  - "adr"
  - "ADR"
---

# 1. Record architectural decisions as ADRs, linted by adr-lint

## Context

This fork records decisions in at least five places: `AGENTS.md`, `engdocs/`
(`architecture/`, `design/`, `proposals/`, `contributors/`, `plans/`,
`research/`), bead notes, commit messages, and script header comments. None of
them has an edge to the code they govern, so a decision and the file it
constrains drift apart silently.

Two measured incidents on 2026-08-08 motivated this:

- `scripts/install-push-gate.sh:9-10` in the `gas-city-infra` rig recorded a
  decision — "ships only `pre-commit` — deliberately, because CRM disables
  pre-push on purpose … Correct for CRM", dated 2026-08-02. Commit `af3a4a5`
  reversed it the next day with no retraction. The reversal went unnoticed for
  five days and was found only because an adversarial review went looking. In
  the meantime a repair was very nearly deployed that would have armed a
  pre-push hook in a rig that had deliberately disabled it, in an area with an
  open incident bead (`gci-rtr1`) about hooks detaching HEAD in the real repo.
- Commit `b38447716` deleted `cmd/gc/bd_fastpath.go`, the in-process bd path.
  Nothing recorded whether that was a deliberate reversal of the "retire
  bdshim" direction. The epic `gcw-yr0o` is consequently blocked on an
  unrecorded decision rather than on any technical obstacle. See ADR-0002.

Both are the same defect: a durable claim about *intent*, stored where it was
written rather than where it is needed, with nothing that notices when the code
moves out from under it. Bead `gcw-ajoj` tracks the general class.

Two mechanisms already exist in this fleet for the same problem, and both work:
the CRM rig's `applies_to` frontmatter plus `scripts/adr-for-files.mjs` (65
ADRs, 65 of them carrying `applies_to`, 3 superseded, wired into `pr-ci.yml`),
and `gas-city-infra`'s `durable-sinks.toml` plus `check-durable-sinks.sh`.

## Decision

Record architectural decisions in this repository as ADRs under `doc/adr/`,
using the `adr-lint` frontmatter schema (`status`, `date`, `applies_to`,
`complexity`, `pre_filter`, `enforced_by`, `diff_context`, `superseded_by`).

`doc/adr/` — not `engdocs/` — because both `adr-lint` and the CRM rig already
use that path, and a shared convention is worth more than local tidiness.

Validate changes against them with `adr-lint`
(`github.com/wbern/adr-lint`, v0.1.3), invoked from the existing
`.githooks/pre-commit`. We do **not** introduce `lefthook`; this repo already
has a pre-commit hook and a second hook manager would be one more thing to keep
in sync.

`adr-lint` runs **locally only, never in CI**, following the tool's own
ADR-0003. That decision is adopted here rather than re-derived: it turns on CI
needing an auth token for someone's Claude Code subscription as a repo secret,
external contributors' PRs burning the maintainer's quota, and added PR
latency. Those constraints apply identically here.

The check is **advisory** — it reports, it does not block a commit. The
analysis is model-backed and therefore non-deterministic: the same diff can
pass once and fail once. A blocking gate that flakes trains people to bypass
it, and a bypassed gate is worse than none because it still reads as coverage.

## Consequences

Judgment moves out of Go and into the model, which is what `AGENTS.md` asks for
("Go handles transport, not reasoning"). A glob-matching linter would put a
crude judgment — does this path match? — into code; `adr-lint` keeps Go as
transport and gets more useful as models improve, satisfying the primitive test
in `engdocs/contributors/primitive-test.md`.

The corpus is the bottleneck, not the tooling. An ADR only catches drift if it
exists and its `applies_to` covers the changed file; `adr-lint` over an empty
corpus is a green check that cannot fail. That failure mode was demonstrated
twice in this fleet on 2026-08-08 — a drift check aimed at a pair of files that
has never diverged, and a `make check` assertion green for a gate deployed
nowhere — so ADRs are written first and the hook is wired second.

We start with a deliberately small set and add ADRs as decisions arise. We do
not backfill exhaustively: an ADR nobody needed is indistinguishable from
documentation, and it dilutes the signal for the ones that matter.

Contributors need the Claude Code CLI on PATH and logged in. Without it the
hook skips, prints that it skipped, and the commit proceeds — a silent skip
would be the same lying-status defect this fork spent 2026-08-08 removing from
`bd`.
