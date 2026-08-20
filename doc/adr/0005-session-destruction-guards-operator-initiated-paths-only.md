---
status: accepted
date: 2026-08-20
complexity: standard
diff_context: true
# enforced_by is deliberately ABSENT. Per doc/adr/templates/template.md, setting
# it marks the ADR as covered by external tooling so the model stops re-checking
# it — and no linter can detect "someone wired a guard into the self-handoff
# path". Model review is the only enforcement this decision has; suppressing it
# would defeat the ADR. adr-lint reports the omission; that is a considered
# trade, not an oversight.
applies_to:
  - "cmd/gc/subagent_kill_guard.go"
  - "cmd/gc/cmd_handoff.go"
  - "cmd/gc/cmd_session.go"
  - "internal/worker/subagent_guard.go"
pre_filter:
  - "refuseKillForLiveSubagents"
  - "InFlightBackgroundSubagents"
  - "doHandoffRemote"
  - "cmdSessionKillWithForce"
---

# 5. Session-destruction guards cover operator-initiated paths only; self-handoff is deliberately unguarded

## Context

Destroying a session destroys the provider process, and coding-agent subagents
run **in-process**. Every path that kills a session therefore destroys the
target's in-flight background subagents, leaving no artifact beyond an orphaned
transcript.

This was not theoretical. Reconstructed second-by-second from four independent
artifacts on 2026-08-02:

```
09:00:40.121  background subagent spawned
09:15:46.012  subagent issues a tool_use — actively working
09:15:55.778  another agent runs `gc handoff --target <session>`
09:15:57.982  subagent receives its tool_result — its LAST entry, mid-loop
09:15:58.218  CLI prints "killed session (reconciler will restart)"
09:18:45.246  successor reports "No completion record was found for background agent…"
```

The victim had no say: the kill came from a different agent.

Measured across 763 transcripts on this machine:

```
real `gc handoff` SELF-executions          268
background subagent spawns                 256
self-handoffs colliding with a live subagent  0
```

## Decision

The guard (`refuseKillForLiveSubagents`) is wired into the two
**operator-initiated** destruction paths only:

| Path | Guarded | Initiator |
| --- | --- | --- |
| `gc handoff --target` | yes | another agent |
| `gc session kill` | yes | an operator |
| `gc handoff` (self) | **no** | the agent itself |
| controller/reconciler kills | no — observe only | the controller |

Self-handoff stays unguarded **by design**, for three reasons:

1. **It is the agent's own decision.** Refusing it in Go would put a judgment
   call in the framework, which this codebase forbids. The agent knows whether
   its subagents matter; the framework does not.
2. **It has never once collided** — 0 of 268 real self-handoffs ran while a
   background subagent was live.
3. **Refusing would have no escape hatch that helps.** A guard the agent must
   then override with `--force` is a prompt with extra steps.

The correct lever for the self-handoff path is the **context-advisory message**,
which is operator-authored configuration (`[context_advisory]` tiers), not Go.
That is why the advisory wording was deliberately *not* hardcoded.

Autonomous/controller paths (convoy dispatch, session beads, wake) **observe and
report** rather than refuse: the controller has no operator to pass `--force`,
so refusing there could wedge a convoy or leave a session permanently
unrecyclable — a worse failure than the one being prevented.

Detection is **fail-open**: an absent, unreadable or corrupt transcript proceeds
exactly as before. A detection failure must never block a legitimate kill.

## Consequences

- A future reader will notice self-handoff is unguarded and may file it as a
  bug. It is not. Cite this ADR.
- If the 0/268 figure ever stops holding, the fix is still the advisory message
  first; only reconsider a Go-side guard if configuration demonstrably fails.
- Detection must not be "simplified" into a heuristic. Four cheaper approaches
  were measured and rejected: the process table is blind (subagents are
  in-process); `tool_use`/`tool_result` pairing fails because a background spawn
  returns its acknowledgement in ~9ms; `inferAgentStatus` keyed on a
  `type:"result"` entry absent in 476/476 real transcripts; and activity
  inference on subagent transcripts scored 14% precision. The shipped join is
  exact: `agent-*.meta.json` → `toolUseId` → parent `Agent` `tool_use` →
  terminal task-notification by `task-id`.
- Only an explicit `run_in_background: false` counts as synchronous. The field
  is omitted far more often than set; treating "absent" as synchronous silently
  drops ~78% of background subagents.

## Related

- Upstream PR gastownhall/gascity#5448 proposes the operator-initiated half.
- Upstream #1029 (session reaped mid-work) is the same loss class from a
  different trigger.
