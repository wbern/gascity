---
status: accepted
date: 2026-10-04
complexity: standard
applies_to:
  - "AGENTS.md"
  - "CLAUDE.md"
  - "TESTING.md"
  - "engdocs/contributors/**"
  - "internal/bootstrap/packs/**/*.md"
  - "internal/bootstrap/packs/**/*.toml"
  - "examples/**/*.md"
---

# 9. Agents work from evidence

This is the fork's counterpart of gas-city-infra ADR-0020. The stance is the
same; the examples are from this repository.

## Context

Most of the work on this fork is done by agents: they diagnose controller and
store problems, write fixes, review PRs, and plan rollouts that operators run
against live cities. The costliest mistakes have come from unverified belief,
not missing knowledge:

- `gcw-2ozk` was filed as a P1 bug against behaviour that
  `internal/agent/session_name.go` documents as intended (now ADR-0004/0007).
- During the 2026-10-04 GC3 maintenance window an agent explained a
  re-applied beads migration as a one-off race. The operator observed it
  repeat with the cursor already advanced. The real cause was a sentinel
  replay floor in beads (`leases.granted_node`, floor 11).
- A validation script reported a failing bead store on GC3. The cause was the
  script running `gc doctor` from outside the city directory; the store was
  healthy.
- The same window also shows what works. A query plan read with
  `EXPLAIN PLAN`, plus a read-only comparison on live data (115 vs 106 rows),
  turned a hunch about a slow migration into a measured correctness bug, and
  its replacement was tested on a cloned store before it touched production.

William's direction (2026-10-03): agents work evidence-based as their way of
working. They do not have to prove it literally in every PR, but how they work
must rest on evidence, not on plausibility.

## Decision

Agents working in this repository treat evidence as the basis for their claims
and actions.

1. **Observe before concluding.** Before asserting a cause, a state ("stuck",
   "done", "healthy", "a bug", "nobody is working on it") or that a fix worked,
   look at the system or the code itself. That means logs, commit statuses,
   bead state, process and session state, a reproduction, a test, or an ADR
   that says the behaviour is deliberate. A plausible story is a hypothesis
   until something observed supports it.
2. **Reproduce, then fix, then re-observe.** For a defect, first reproduce it
   with a failing test (TDD, per `TESTING.md`) or a live observation. Then
   change it, and observe the change working where it matters. Prefer the
   narrowest observation that would actually show the claim to be false.
3. **Say what the evidence is, and what it is not.** Reports, bead comments
   and PR descriptions name the observation behind each claim (command,
   timestamp, id, measurement) at the level of detail the reader needs.
   Unverified parts are labelled as such: "inferred", "not checked", "could
   not reproduce". Absence of a signal from a filtered or failing probe is not
   evidence of absence. When later evidence overturns a claim, say so plainly.
4. **Proportion, not ceremony.** The evidence matches the risk. A typo needs
   none; a migration bypass on a production store needs a tested equivalent
   and a live check. This ADR does not require proof artefacts in every PR, a
   fixed template, or a confidence score. It sets how agents work, not a
   paperwork gate.
5. **Destructive or outward-facing actions need evidence about the target.**
   Before killing, closing, re-dispatching, deleting, merging or messaging on
   the basis of a state, confirm that state on the thing itself. For example:
   the PR is actually merged, the process is actually the stale one, the
   cursor actually reads what you think.

## Consequences

- Contributor docs and bundled pack prompts, formulas and skills in scope
  should steer agents toward observing, reproducing and re-verifying. They
  must not instruct an agent to report success without an observation, or to
  treat silence as health.
- `adr-lint` and the review gate may flag an in-scope change that contradicts
  this, for example a formula step that closes work on assertion alone.
- This is about how agents work, not about SDK behaviour. It adds no role or
  judgment to Go code (see "Keep judgment out of Go" in `AGENTS.md`).
- Some work gets slower at the moment of claiming "done", in exchange for
  fewer confident-but-wrong outcomes that cost far more to unwind.

## References

- gas-city-infra ADR-0020 "Agents work from evidence" (wbern/gas-city-infra
  #1013); William, 2026-10-03.
- ADR-0004/0007 (the `gcw-2ozk` misfiling); the 2026-10-04 GC3 S5 window
  (gcw-o4qtt, gcw-6w15p).
- `TESTING.md` (test-first); `AGENTS.md` "Feature archaeology workflow".
