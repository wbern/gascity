---
status: accepted
date: 2026-08-20
complexity: standard
diff_context: true
applies_to:
  - "cmd/gc/cmd_handoff.go"
pre_filter:
  - "--recycle"
  - "doHandoffWithRecycleOutcome"
---

# 6. Attended sessions recycle only through an explicit self-handoff opt-in

## Context

An on-demand configured named session is attended: its lifetime belongs to the
human who opened it. Ordinary `gc handoff` therefore writes a durable
continuation and leaves that session running. This prevents an automatic hook
or controller path from unexpectedly ending a user conversation.

That safety boundary did not provide an agent that has deliberately reached its
context limit with a complete operation. The agent could write its continuation
and then had to ask a human to run the existing restart command separately.

## Decision

`gc handoff --recycle` is the explicit self-initiated continuation command. It
writes the handoff mail before requesting the same persisted restart that the
controller recognizes, even when the caller is an attended on-demand named
session. The successor receives the handoff mail through the existing
continuation path.

Plain `gc handoff` retains its non-disruptive attended-session behavior.
`--recycle` is invalid with `--auto` and `--target`; neither a provider hook
nor another session can use it to recycle an attended session.

## Consequences

- Agents can deliberately leave and resume a fresh conversation without human
  intervention.
- The controller still has no new unilateral authority over attended sessions.
- Agents must choose the disruptive behavior explicitly; automatic context
  hooks continue to preserve the active conversation.

## References

- `gcw-jt5rr`
- ADR-0005, which separately records the scope of session-destruction guards.
