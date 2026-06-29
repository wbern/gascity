# Upstream issue — POSTED

Filed as **[gastownhall/gascity#3810](https://github.com/gastownhall/gascity/issues/3810)**
on 2026-06-29. Companion design:
[`context-advisory-pluggable.md`](./context-advisory-pluggable.md). This file is
the source text as posted.

---

**Title:** Make the context-pressure advisory a pluggable trigger (configurable levels, message, and audience)

**Labels (suggested):** `kind/feature`, `priority/p2`

---

## Summary

The context advisory from #3371 is useful and we agree with its framing — the PR
calls it "the trigger signal for canonical handoffs." Our ask: make that trigger
configurable per city and per agent, instead of fixed in Go.

## What we're seeing

Running persisted agents, two recurring patterns:

1. **They defer work and recycle early** once the advisory fires — which is the
   documented intent (the PR notes the live line "told the authoring agent to
   defer a newly-dispatched workstream to a fresh session — the intended
   behavior"). That's exactly why it should be tunable: different roles want
   different thresholds, not one fixed number for the whole fleet.
2. **They punt the recycle to the human** instead of doing it themselves. We
   suspect the wording is part of this — the urgent tier suggests
   `gc session reset` (no continuation note, not attended-safe) where
   `gc handoff` is the right per-session-type action. A one-line wording fix
   helps our defaults, but it surfaced the bigger question.

## The ask

The advisory is a trigger — "at **level X**, tell **agent Y** to do **Z**" — but
X/Y/Z are all hardwired (env-only thresholds, every-agent audience, one message).
That's more Gas Town than Gas City. Make it config-driven at global + per-agent
scope (per-agent wins), env vars kept as a back-compat override:

- enable / disable per scope (this is the audience knob)
- advisory + urgent thresholds, and a context-window override
- the message text per tier, operator-authored

Zero-config keeps today's behavior. We'd fold in a fix for the under-report case
(#3604) while reworking the read path.

## Open questions

- `text/template` for the message (consistent with prompt templates), or
  something lighter?
- Per-agent config home: the existing agent-defaults axis, or wait for PackV2
  agent-defaults (#903)?
- Is per-agent enable enough for "audience," or do you want richer targeting?

## References

#3371 (the feature) · #3604 (under-report bug) · #903 (PackV2 agent-defaults) ·
#1465 (per-step recycle policy)
