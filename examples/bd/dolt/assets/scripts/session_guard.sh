#!/bin/sh
# dolt_refuse_inside_agent_session <command> refuses (returns 77) when called from
# an agent session. A Dolt server started from a session is a descendant of that
# session's process group, so it dies when the session is stopped: on 2026-11-22 a
# session's `gc dolt restart --force` did exactly that and the whole fleet lost
# the bead store and mail for ~14 minutes. Agent sessions are marked by
# GC_SESSION_ID. Operators who really intend it (for example attached to an agent
# pane while following a runbook) can set GC_DOLT_ALLOW_SESSION_CONTROL=1.
dolt_refuse_inside_agent_session() {
  [ -n "${GC_SESSION_ID:-}" ] || return 0
  [ "${GC_DOLT_ALLOW_SESSION_CONTROL:-}" = "1" ] && return 0
  echo "gc dolt $1: refusing to run inside agent session ${GC_SESSION_ID}." >&2
  echo "  A Dolt server managed from here dies when this session is stopped, taking the bead store with it." >&2
  echo "  Diagnose read-only (gc dolt status, the Dolt log) and ask DevOps or the operator to run gc-restart." >&2
  echo "  Operators who intend this: GC_DOLT_ALLOW_SESSION_CONTROL=1 gc dolt $1" >&2
  return 77
}
