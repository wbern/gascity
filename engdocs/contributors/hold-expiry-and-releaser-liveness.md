---
title: Holds that outlive their releaser
description: Why a correct pause is indistinguishable from a deadlock today, and why most of the fix already exists — bd --defer and dependency edges. Written from the GC3 incident of 2026-07-30, then cut down by an adversarial review for over-engineering.
---

## The incident, and the one fact that shapes everything below

At 23:11 on 2026-07-30 an agent mailed four CRM polecats an overnight hold:
*"do not claim additional work… stay idle unless DevOps gives case-specific
recovery confirmation."* It had no expiry and named a sole releaser that was
suspended at the time. It never lifted. Four workers sat idle for ~7 hours.

By morning **two of the four had no underlying problem at all** — one
workspace check had been passing since 22:39, another since 03:21. One was a
guard false-positive. Exactly one had a real blocker.

The workers behaved **correctly throughout**. They obeyed a valid instruction
from a legitimate sender. Nothing was broken. That is what makes this a design
problem rather than a bug.

**The fact that shapes the design:** those beads carried no `hold:` label.
Verified on GC3 after the fact — `crm-gk2mr` and `crm-rvay6` are `status=open`
with ordinary area labels and no hold marker of any kind. The hold existed
**only as prose in a mail**, in the workers' context windows.

So the obvious fix — add a TTL to `hold:<value>` labels — **would not have
prevented this incident.** The hold was never in that system. Any design that
starts there is solving the wrong problem.

## Why this is city-wide and not a pack or role fix

Any agent can park any other agent by mail, indefinitely, with no TTL, no
release predicate, and no check that the named releaser exists or is awake.
The channel is prose; the enforcement is the recipient's own compliance. There
is no component that could have known the instruction had outlived its
author's availability, because no component could see it.

Two further properties observed in the same incident:

- **Holds compose invisibly.** One worker carried two independent holds — the
  mailed one, plus a separate *"remain paused until DevOps supplies the
  current-main continuation"*. Clearing one did not clear the other, and
  nothing surfaced that a second existed. Any design must handle N concurrent
  holds, not one.
- **A hold point with no named receiver is indistinguishable from a hang.**
  From outside, "correctly waiting" and "wedged" look identical — which is
  precisely why the idle watchdog killed workers that were behaving correctly
  (see `gas-city-infra#86`).

## What exists today

`engdocs/contributors/hold-label-conventions.md` defines three orthogonal
"not ready" mechanisms: dependency edges, `status=blocked`, and the
`hold:<value>` label set via `bd set-state <id> hold=<value> --reason`. Only
`hold:mayor` and `hold:external` are canonical.

Measured against what a safe hold needs, all three fall short in the same way:

| Requirement | Dependency edge | `status=blocked` | `hold:<value>` | Mailed prose |
|---|---|---|---|---|
| Expires on its own | no | no | no | no |
| Releaser resolves to a live session | n/a | no | **names a role, not a session** | no |
| Worker can re-evaluate without the releaser | **yes** (edge closes) | no | no | no |
| Visible to any other component | yes | yes | yes | **no** |

The dependency edge is the only mechanism that already has the property that
matters: it is *computed from real state*, so it releases itself when the
blocker closes. Nobody has to remember.

Note that `hold:mayor` names a **role**, not a session. There is no session to
probe for liveness, so a liveness check is not merely unimplemented — it is
not expressible in the current shape.

## The design — and most of it already exists

An earlier draft of this page proposed three new invariants: an expiry, a
releaser-liveness check, and a self-evaluable predicate. An adversarial review
for over-engineering killed two and a half of them. What follows is what
survived.

**Expiry already exists.** `bd update <id> --defer <date>` hides a bead from
`bd ready` until the date, then it returns on its own. Verified live: a scratch
bead appeared in `bd ready`, was deferred, and disappeared from `ready`
immediately, with no actor involved in its eventual return. **That is a
self-releasing TTL, already implemented and already deployed.** Proposing a new
expiry mechanism was reinvention.

**Self-evaluating release already exists too.** A dependency edge is computed
from real state: when the blocker closes, the block lifts, and nobody has to
remember. The earlier draft correctly identified this as "the model to
generalise" and then proposed a parallel predicate system anyway. Generalising
it means *using it*, not rebuilding it.

**Releaser liveness does not survive the review at all.** Two reasons. First,
if a hold expires, the releaser's liveness stops mattering — the hold lifts
regardless, which is a strictly simpler guarantee than probing a session.
Second, "must resolve to a live session" is probably *wrong*, not merely
excess: sessions sleep and despawn constantly by design, so a releaser asleep
at set-time may be awake an hour later. Refusing a hold on that basis would
generate false refusals to prevent a failure that expiry already prevents.

### So the real gap is smaller than it looked

Both time-bound and condition-bound pauses are already expressible today:

| Pause is bound by | Use | Self-releases? |
|---|---|---|
| a time | `bd update --defer <date>` | yes |
| another bead | `bd dep add <a> <b>` | yes |
| a specific actor | `hold:<value>` label | **no** |
| prose in a mail | *nothing* | **no** |

The bottom two rows are the entire problem. And the incident lived in the last
one.

### The minimal fix

A pause delivered as prose is invisible to every component by construction.
The cheapest thing that would have prevented the incident is a convention, not
a mechanism:

> A mailed instruction to pause is **advisory** unless it is accompanied by a
> `--defer` date or a dependency edge. If you want a worker to stop, record it
> where the system can see it and where it will lift itself.

That is a line in a prompt template. It needs no new storage, no new reader, no
new evaluator, and no code — which matters here specifically, because Gas City
rejects capability flags and a skills system on exactly this reasoning: a
sentence in the prompt is sufficient, and judgment does not belong in Go.

Only if that convention demonstrably fails should anything be built. The next
increment after it — and *only* if needed — is surfacing the fourth row: a
worker that honours a prose hold records it durably, so it becomes visible to
the idle watchdog. That composes with the worker-declared state marker the
watchdog already needs, so it is one artifact serving two purposes rather than
a new subsystem.

### What this does not propose

No new mechanism, no new storage, and no code. No role names in Go — nothing
here requires a Mayor, a DevOps, or any named role to exist. If the convention
proves insufficient, the increment after it is one durable marker that the
idle watchdog already wants for its own reasons, not a hold subsystem.

## Verification criteria

Scaled to the trimmed design. The first two are the whole of it:

- A pause recorded with `--defer` returns to `bd ready` on its own, with no
  actor involved. (Verified live 2026-07-31 on a scratch bead.)
- A pause recorded as a dependency edge lifts when the blocker closes.
- A mailed pause carrying neither is treated as advisory, and a worker that
  keeps working after one is behaving correctly.

Only if the convention fails in practice:

- A prose hold a worker honours becomes visible to at least one other
  component within one watchdog tick.
- N concurrent holds are individually visible; clearing one does not imply the
  others are gone.

## Provenance

`gci-87vx`, filed by devops for architect after the 2026-07-30 GC3 incident.
The generalisation it turns on was written the night before the cause was
known: *a hold point with no named receiver is indistinguishable from a hang;
any human-in-the-loop gate needs a receiver that resolves to something live,
and a deadline after which the wait itself becomes an alert.*

That bead was itself verbally attributed to architect for several hours
without being filed or delivered — so it existed only in one session's
narrative. The same failure this document describes, one layer up.
