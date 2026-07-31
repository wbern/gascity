---
title: Holds that outlive their releaser
description: Why a correct pause is indistinguishable from a deadlock today, and what a hold must carry to be safe — expiry, a resolvable releaser, and a self-evaluable predicate. Written from the GC3 incident of 2026-07-30.
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

## The design

A hold is safe when it carries three things. Stated as invariants rather than
an implementation, because the mechanism is a later decision:

**1. An expiry.** After it, the hold self-releases or escalates — never
silently persists. The default must be finite. An indefinite hold should be
something you opt into loudly, not what you get by forgetting.

**2. A releaser that resolves to something live.** Not a role name: an
identity the system can probe. If the releaser cannot be resolved, or resolves
to a suspended session, the hold is **already broken at the moment it is set**
and should be refused or immediately escalated — not discovered seven hours
later. This is the same failure as `crm-workspace` escalating to *"a human/mayor
must reconcile"* with no receiver, and the same as a bead being assigned to an
agent that cannot read it.

**3. A machine-readable predicate the worker can re-evaluate itself.** This is
the most valuable of the three, because it removes the releaser from the
critical path entirely. Two of the four workers were waiting on a condition
that had already cleared. A worker that can ask *"is this still true?"* does not
need anyone to come back. The dependency edge already works this way; that is
the model to generalise.

### The part that is genuinely hard

You cannot put a TTL on a sentence in a mail. So one of two things has to be
true, and this is the real decision:

- **(a) A hold is only binding if it is recorded structurally.** Prose asking a
  worker to pause is advisory; the worker records an actual hold (with expiry,
  releaser and predicate) or it keeps working. Puts the burden on the sender
  and makes non-compliance correct behaviour.
- **(b) A worker that accepts a mailed hold must durably declare it.** The
  hold becomes visible the moment it is honoured, and gets its expiry then.
  Puts the burden on the recipient, and composes with the worker-declared state
  marker proposed for the idle watchdog: a durable
  `blocked, waiting on <receiver>, predicate=<x>` record that a watchdog
  **reads** rather than infers, turning an inference into a fact.

**(b) is the better fit for Gas City** — it needs no enforcement against
senders, it is the same artifact the watchdog already needs, and it keeps
judgment out of Go. The system never decides *whether* a pause is legitimate;
it only observes that one was declared, checks whether its clock has run out,
and whether its releaser still exists. Those are mechanical predicates.

### What this does not propose

No role names in Go. The releaser is an opaque, config-supplied identity that
the session layer can resolve — the same shape as any other session reference.
Nothing here requires a Mayor, a DevOps, or any named role to exist.

## Verification criteria

A design is not done until these are demonstrable:

- A hold whose expiry passes releases or escalates without anyone acting.
- A hold naming an unresolvable or suspended releaser is rejected **at set
  time**, not at release time.
- A worker whose predicate has cleared resumes without the releaser.
- N concurrent holds on one worker are individually visible; clearing one does
  not imply the others are gone, and the remaining ones are surfaced.
- A mailed hold that a worker honours is visible to at least one other
  component within one watchdog tick.

## Provenance

`gci-87vx`, filed by devops for architect after the 2026-07-30 GC3 incident.
The generalisation it turns on was written the night before the cause was
known: *a hold point with no named receiver is indistinguishable from a hang;
any human-in-the-loop gate needs a receiver that resolves to something live,
and a deadline after which the wait itself becomes an alert.*

That bead was itself verbally attributed to architect for several hours
without being filed or delivered — so it existed only in one session's
narrative. The same failure this document describes, one layer up.
