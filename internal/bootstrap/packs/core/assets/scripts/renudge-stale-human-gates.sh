#!/usr/bin/env bash
# renudge-stale-human-gates — re-mail + re-nudge the addressee of a human gate
# that has stayed OPEN past a staleness threshold, repeating on an interval.
#
# notify-on-human-gate-creation notifies the addressee ONCE, at creation. A
# human gate that is created, notified, and then left unresolved gets no
# further reminder: the creation mail scrolls off, the human forgets, and the
# only gate watcher (`gc bd gate check`) skips human gates entirely. Doctrine
# papers over the gap by hand ("a human gate open past a threshold gets
# re-nudged, repeating on the interval"); this order ships that reflex, and it
# is also the safety net for a creation notify that was undeliverable beyond
# its short lookback window.
#
# Runs as a cooldown sweep. Each run:
#   1. Enumerates OPEN gates across HQ and every rig (`gc bd gate list` is
#      open-only by default), keeping only await_type == "human" gates.
#   2. For each human gate whose age exceeds GC_STALE_GATE_THRESHOLD and whose
#      last re-nudge is older than GC_STALE_GATE_RENUDGE_INTERVAL, re-fetches
#      the gate (the list projection omits assignee/metadata), re-verifies it
#      is still an open human gate, resolves the addressee and re-notifies.
#
# Addressee resolution (first non-empty wins), identical to the creation notify
# so a gate is always re-nudged at the same address it was first notified:
#   1. the gate's assignee
#   2. gc.deferred_assignee metadata (formula/molecule gates strip the
#      assignee here at create time, molecule.go stripDeferredAssignee)
#   3. $GC_ESCALATION_RECIPIENT (default "human")
#
# Notification rides `gc mail send --notify`, which mails the addressee and
# nudges them when they are a real session — and deliberately skips the
# tmux-nudge for the "human" recipient (humans have no session to poke;
# cmd_mail.go guards `to != "human"`).
#
# Loud-fail (gastownhall/gascity#4543): an undeliverable send surfaces to the
# controller log (stderr) and is NOT recorded, so the next sweep retries it. It
# never silently evaporates.
#
# Dedup / cadence: per-gate reminder state lives in
# $GC_PACK_STATE_DIR/renudge-stale-human-gates-state.json (city- and
# pack-scoped). Each entry records last_sent_at, last_seen_at and count. Repeat
# intervals back off exponentially and stop at GC_STALE_GATE_MAX_RENUDGES. Every
# observed open gate refreshes last_seen_at even after reaching the cap, so
# retention can never erase a capped live gate and restart the reminder storm.
# A resolved or unreachable gate stops refreshing and is pruned after
# GC_STALE_GATE_STATE_RETENTION; the retention window still cushions transient
# per-rig enumeration failures.
#
# Cross-rig: gates are enumerated per scope (HQ + each non-HQ rig), so the
# owning rig is known without a prefix lookup; the re-fetch is scoped with
# `--rig` (a gc flag, not a bd flag, so it routes through `gc bd`). Mail send is
# city-scoped: recipients (mayor / human / coordinators) are city-level
# identities.
#
# Runs as an exec order (no LLM, no agent, no wisp).
set -euo pipefail

__SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck disable=SC1091
. "$__SCRIPT_DIR/_bd_trace.sh" "renudge-stale-human-gates"

# jq is a hard dependency: it decodes the gate list and the re-fetched gate
# record. Without it every re-nudge would be silently skipped. Fail loud.
if ! command -v jq >/dev/null 2>&1; then
    echo "renudge-stale-human-gates: jq is required but not found in PATH" >&2
    exit 1
fi

CITY="${GC_CITY:-.}"
# A human gate must be open at least this long before its FIRST staleness
# re-nudge. Below this the creation notify already covered it; this avoids
# double-notifying a freshly created gate.
THRESHOLD="${GC_STALE_GATE_THRESHOLD:-1h}"
# Base time between successive re-nudges of the same gate. Later intervals
# expand from this value under the backoff policy below.
RENUDGE_INTERVAL="${GC_STALE_GATE_RENUDGE_INTERVAL:-1h}"
# Successful reminders back off by this multiplier. With the defaults the
# repeat gaps are 1h, 2h, 4h and 8h before the gate reaches its hard cap.
BACKOFF_MULTIPLIER="${GC_STALE_GATE_BACKOFF_MULTIPLIER:-2}"
# The creation notification is separate and is not counted here.
MAX_RENUDGES="${GC_STALE_GATE_MAX_RENUDGES:-5}"
# Bound exponential arithmetic even when an operator raises MAX_RENUDGES.
MAX_RENUDGE_INTERVAL="${GC_STALE_GATE_MAX_RENUDGE_INTERVAL:-24h}"
# Dedup entries older than this are pruned so the state file stays bounded.
# Must exceed RENUDGE_INTERVAL (a live gate's entry is refreshed each re-nudge,
# so it never ages past this; only a resolved gate's entry reaches it).
RETENTION="${GC_STALE_GATE_STATE_RETENTION:-24h}"
# Human channel for gates with no resolvable assignee. escalate.sh and the
# creation notify use the same default, keeping the "notify the human" address
# consistent across all three.
ESCALATION_RECIPIENT="${GC_ESCALATION_RECIPIENT:-human}"

PACK_STATE_DIR="${GC_PACK_STATE_DIR:-${GC_CITY_RUNTIME_DIR:-$CITY/.gc/runtime}/packs/core}"
STATE_FILE="$PACK_STATE_DIR/renudge-stale-human-gates-state.json"
mkdir -p "$PACK_STATE_DIR"

# Convert a simple Go-style duration (Ns/Nm/Nh/Nd) to whole seconds.
duration_to_seconds() {
    case "$1" in
        *d) echo $(( ${1%d} * 86400 )) ;;
        *h) echo $(( ${1%h} * 3600 )) ;;
        *m) echo $(( ${1%m} * 60 )) ;;
        *s) echo "${1%s}" ;;
        *)  echo "$1" ;;
    esac
}

# Parse an ISO-8601 UTC timestamp (e.g. 2026-07-22T13:54:16Z) to epoch seconds.
# Empty on failure so callers can skip an unparseable gate rather than misage it.
# Portable across GNU and BSD/macOS date, matching wisp-compact.sh: GNU `date -d`
# first, then BSD `date -ju -f` (forcing UTC to match GNU), with a no-Z layout
# for older timestamps. Without the BSD fallbacks every gate would be skipped on
# macOS (BSD date rejects -d), silently disabling the whole sweep.
iso_to_epoch() {
    [ -n "$1" ] || { echo ""; return 0; }
    date -u -d "$1" +%s 2>/dev/null || \
        date -ju -f "%Y-%m-%dT%H:%M:%SZ" "$1" +%s 2>/dev/null || \
        date -ju -f "%Y-%m-%dT%H:%M:%S" "$1" +%s 2>/dev/null || \
        echo ""
}

THRESHOLD_S="$(duration_to_seconds "$THRESHOLD")"
RENUDGE_INTERVAL_S="$(duration_to_seconds "$RENUDGE_INTERVAL")"
MAX_RENUDGE_INTERVAL_S="$(duration_to_seconds "$MAX_RENUDGE_INTERVAL")"
RETENTION_S="$(duration_to_seconds "$RETENTION")"
NOW_EPOCH="$(date -u +%s)"
NOW_ISO="$(date -u +%Y-%m-%dT%H:%M:%SZ)"

for duration_policy in "$THRESHOLD_S" "$RENUDGE_INTERVAL_S" "$MAX_RENUDGE_INTERVAL_S" "$RETENTION_S"; do
    case "$duration_policy" in
        ''|*[!0-9]*)
            echo "renudge-stale-human-gates: invalid duration policy" >&2
            exit 1
            ;;
    esac
done
for integer_policy in "$BACKOFF_MULTIPLIER" "$MAX_RENUDGES"; do
    case "$integer_policy" in
        ''|*[!0-9]*)
            echo "renudge-stale-human-gates: invalid integer policy: $integer_policy" >&2
            exit 1
            ;;
    esac
done
[ "$BACKOFF_MULTIPLIER" -ge 1 ] && [ "$MAX_RENUDGES" -ge 1 ] || {
    echo "renudge-stale-human-gates: backoff multiplier and max reminders must be positive" >&2
    exit 1
}
[ "$RENUDGE_INTERVAL_S" -gt 0 ] && [ "$MAX_RENUDGE_INTERVAL_S" -gt 0 ] || {
    echo "renudge-stale-human-gates: reminder intervals must be positive" >&2
    exit 1
}

# Build the list of scopes to sweep: HQ (empty scope, bare gc bd) plus every
# non-HQ rig. `gc bd gate list` without --rig is HQ-scoped from the city cwd,
# so per-rig gates are invisible to a bare query — walk each rig explicitly.
# The HQ entry is excluded (gc rig list reports the city root as an hq=true
# pseudo-rig that `gc --rig <cityName>` cannot resolve), matching orphan-sweep.
SCOPES_FILE="$(mktemp "$PACK_STATE_DIR/.renudge-scopes.XXXXXX")"
trap 'rm -f "$SCOPES_FILE"' EXIT
printf '\n' > "$SCOPES_FILE" # HQ scope: an empty line
RIGS_JSON="$(gc rig list --json 2>/dev/null || true)"
if [ -n "$RIGS_JSON" ]; then
    printf '%s' "$RIGS_JSON" \
        | jq -r '(.rigs // [])[] | select(.hq != true) | .name' 2>/dev/null \
        >> "$SCOPES_FILE" || true
fi

# Load reminder state. Legacy versions stored a bare ISO timestamp per gate and
# did not retain a count; saturate those gates at the new cap because resetting
# an unknown prior count would immediately resume the old reminder storm.
STATE="$(cat "$STATE_FILE" 2>/dev/null || true)"
if [ -z "$STATE" ] && [ ! -e "$STATE_FILE" ]; then
    STATE='{}'
elif ! printf '%s' "$STATE" | jq -e '
    type == "object" and all(.[];
        if type == "string" then true
        elif type == "object" then
            (((.last_sent_at // "") | type) == "string") and
            (((.last_seen_at // "") | type) == "string") and
            ((.count // 0) as $count |
                ($count | type) == "number" and
                $count >= 0 and ($count | floor) == $count)
        else false
        end
    )
' >/dev/null 2>&1; then
    echo "renudge-stale-human-gates: state is corrupt; refusing to reset reminder counts: $STATE_FILE" >&2
    exit 1
fi
STATE="$(printf '%s' "$STATE" | jq --argjson max "$MAX_RENUDGES" '
    with_entries(
        .value as $v
        | .value = (
            if ($v | type) == "string" then
                {last_sent_at:$v, last_seen_at:$v, count:$max}
            elif ($v | type) == "object" then
                {
                    last_sent_at: ($v.last_sent_at // ""),
                    last_seen_at: ($v.last_seen_at // $v.last_sent_at // ""),
                    count: ($v.count // 0)
                }
            else
                {last_sent_at:"", last_seen_at:"", count:0}
            end
        )
    )
')"

# Atomic write of $STATE to disk: temp file in the same dir, then rename.
# Called after EVERY successful send (not just once at exit) so a process
# death mid-sweep loses at most the gate in flight, never the whole ledger.
write_state() {
    __write_state_tmp="$(mktemp "$PACK_STATE_DIR/.renudge-stale-human-gates-state.XXXXXX")"
    printf '%s\n' "$STATE" > "$__write_state_tmp"
    mv -f "$__write_state_tmp" "$STATE_FILE"
}

RENUDGED=0
FAILED=0
while IFS= read -r scope; do
    RIG_ARG1=""
    RIG_ARG2=""
    if [ -n "$scope" ]; then
        RIG_ARG1="--rig"
        RIG_ARG2="$scope"
    fi

    # List OPEN gates in this scope (open-only by default). --limit 0 =
    # unlimited so a busy rig past the default 50 is not silently truncated.
    # Best-effort: a read failure (API down, unreachable rig) must not crash
    # the controller's order loop — skip this scope and continue.
    GATES_JSON="$(gc bd gate list ${RIG_ARG1:+"$RIG_ARG1" "$RIG_ARG2"} --limit 0 --json 2>/dev/null)" || continue
    [ -n "$GATES_JSON" ] && [ "$GATES_JSON" != "null" ] || continue

    # Keep only human gates, emit "<id>\t<created_at>". Non-human gates (timer,
    # gh, bead, and the legacy await_type=null workflow gates) are dropped here.
    HUMAN_GATES="$(printf '%s' "$GATES_JSON" \
        | jq -r '(if type == "array" then . else [.] end)[]
                 | select(.await_type == "human" and .status == "open")
                 | "\(.id)\t\(.created_at // "")"' 2>/dev/null)" || HUMAN_GATES=""
    [ -n "$HUMAN_GATES" ] || continue

    while IFS="$(printf '\t')" read -r gate_id created_at; do
        [ -n "$gate_id" ] || continue

        # Seeing an open gate is itself durable cadence evidence. Capped gates
        # keep refreshing last_seen_at, so retention can never erase their cap
        # and restart the reminder sequence.
        STATE="$(printf '%s' "$STATE" | jq --arg k "$gate_id" --arg now "$NOW_ISO" '
            .[$k] = ((.[$k] // {last_sent_at:"", count:0}) + {last_seen_at:$now})
        ')"
        reminder_count="$(printf '%s' "$STATE" | jq -r --arg k "$gate_id" '.[$k].count // 0')"
        case "$reminder_count" in ''|*[!0-9]*) reminder_count=0 ;; esac
        [ "$reminder_count" -lt "$MAX_RENUDGES" ] || continue

        # Age gate: only gates open past the staleness threshold.
        created_epoch="$(iso_to_epoch "$created_at")"
        [ -n "$created_epoch" ] || continue
        age=$(( NOW_EPOCH - created_epoch ))
        [ "$age" -ge "$THRESHOLD_S" ] || continue

        # Cadence gate: each successful reminder doubles the next interval,
        # bounded by MAX_RENUDGE_INTERVAL. A never-reminded gate is eligible
        # immediately once past the threshold.
        effective_interval_s="$RENUDGE_INTERVAL_S"
        if [ "$effective_interval_s" -gt "$MAX_RENUDGE_INTERVAL_S" ]; then
            effective_interval_s="$MAX_RENUDGE_INTERVAL_S"
        fi
        backoff_step=1
        while [ "$backoff_step" -lt "$reminder_count" ]; do
            if [ "$effective_interval_s" -ge $((MAX_RENUDGE_INTERVAL_S / BACKOFF_MULTIPLIER)) ]; then
                effective_interval_s="$MAX_RENUDGE_INTERVAL_S"
                break
            fi
            effective_interval_s=$((effective_interval_s * BACKOFF_MULTIPLIER))
            backoff_step=$((backoff_step + 1))
        done
        last_iso="$(printf '%s' "$STATE" | jq -r --arg k "$gate_id" '.[$k].last_sent_at // ""' 2>/dev/null)"
        if [ -n "$last_iso" ]; then
            last_epoch="$(iso_to_epoch "$last_iso")"
            if [ -n "$last_epoch" ] && [ $(( NOW_EPOCH - last_epoch )) -lt "$effective_interval_s" ]; then
                continue
            fi
        fi

        # Re-fetch: the list projection omits assignee/metadata, and re-reading
        # closes the tiny window where the gate resolved since the list. Confirm
        # it is still an open human gate before sending.
        GATE_JSON="$(gc bd show "$gate_id" ${RIG_ARG1:+"$RIG_ARG1" "$RIG_ARG2"} --json 2>/dev/null \
            | jq -c 'if type == "array" then .[0] else . end' 2>/dev/null)" || continue
        [ -n "$GATE_JSON" ] && [ "$GATE_JSON" != "null" ] || continue
        AWAIT_TYPE="$(printf '%s' "$GATE_JSON" | jq -r '.await_type // ""' 2>/dev/null)"
        STATUS="$(printf '%s' "$GATE_JSON" | jq -r '.status // ""' 2>/dev/null)"
        [ "$AWAIT_TYPE" = "human" ] || continue
        [ "$STATUS" = "open" ] || continue

        # Addressee: assignee -> gc.deferred_assignee -> escalation recipient.
        # Both null and empty-string count as "unset" (a stripped assignee can
        # land as "" rather than null).
        ADDRESSEE="$(printf '%s' "$GATE_JSON" | jq -r \
            '[.assignee, .metadata."gc.deferred_assignee"]
             | map(select(. != null and . != "")) | (.[0] // "")' 2>/dev/null)"
        [ -n "$ADDRESSEE" ] || ADDRESSEE="$ESCALATION_RECIPIENT"

        TITLE="$(printf '%s' "$GATE_JSON" | jq -r '.title // ""' 2>/dev/null)"
        DESC="$(printf '%s' "$GATE_JSON" | jq -r '.description // ""' 2>/dev/null)"
        age_h=$(( age / 3600 ))
        age_m=$(( (age % 3600) / 60 ))

        SUBJECT="Reminder — human gate still open: $gate_id"
        BODY="Human gate $gate_id has been open and unresolved for ${age_h}h${age_m}m and still awaits you."
        [ -n "$TITLE" ] && BODY="$BODY
Title: $TITLE"
        [ -n "$DESC" ] && BODY="$BODY
$DESC"
        BODY="$BODY
Resolve with: gc bd gate resolve $gate_id"

        # Loud-fail: record the re-nudge only on a delivered send, so an
        # undeliverable one surfaces and retries next sweep.
        if gc mail send "$ADDRESSEE" -s "$SUBJECT" -m "$BODY" --notify >/dev/null 2>&1; then
            reminder_count=$((reminder_count + 1))
            STATE="$(printf '%s' "$STATE" | jq \
                --arg k "$gate_id" --arg now "$NOW_ISO" --argjson count "$reminder_count" \
                '.[$k] = ((.[$k] // {}) + {last_sent_at:$now,last_seen_at:$now,count:$count})')"
            write_state
            RENUDGED=$((RENUDGED + 1))
        else
            echo "renudge-stale-human-gates: FAILED to re-notify addressee '$ADDRESSEE' of stale human gate $gate_id (will retry next sweep)" >&2
            FAILED=$((FAILED + 1))
        fi
    done <<INNER
$HUMAN_GATES
INNER
done < "$SCOPES_FILE"

# Prune entries older than RETENTION so the state file stays bounded.
STATE="$(echo "$STATE" | jq --argjson keep "$RETENTION_S" \
    'with_entries(select(
        (now - ((.value.last_seen_at // .value.last_sent_at // "1970-01-01T00:00:00Z")
            | fromdateiso8601? // 0)) <= $keep
    ))')" || true

write_state

if [ "$RENUDGED" -gt 0 ]; then
    echo "renudge-stale-human-gates: re-notified $RENUDGED stale human gate addressee(s)"
fi

# Loud-fail: state has been written (successful re-nudges are deduped), so a
# non-zero exit now surfaces the per-gate failure lines above to the controller
# log without losing the recorded successes. exit 0 would swallow them (#4543).
if [ "$FAILED" -gt 0 ]; then
    echo "renudge-stale-human-gates: $FAILED stale human gate addressee(s) failed to re-notify (see above; will retry next sweep)" >&2
    exit 1
fi
