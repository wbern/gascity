#!/bin/sh

# recovery_should_skip_due_to_enospc returns 0 (true) when the recent Dolt
# server log shows disk-exhaustion (ENOSPC) signatures.
#
# Restarting Dolt under ENOSPC does not free disk space, and the recovery cycle
# itself amplifies the failure: every restart triggers a fresh conjoin that can
# drop another partial nbs_table_* file in the backup remote, accelerating the
# disk-full feedback loop. See gastownhall/gascity#2158.
#
# The evidence only justifies refusing while the disk is STILL short, because the
# refusal exists to avoid pointless restarts that cannot free space. A quiet server
# writes few log lines, so the last 1000 can span days: a disk-full that was fixed
# yesterday kept blocking recovery (2026-11-22: a Dolt that had stopped could not
# be auto-recovered, and the refusal pushed an operator into `gc dolt restart
# --force`). So when ENOSPC lines are present, free space on the Dolt volume is
# checked too, and recovery proceeds once it is at least
# GC_DOLT_ENOSPC_CLEAR_FREE_BYTES (default 2 GiB, the warn floor). If free space
# cannot be measured the conservative refusal stands.
#
# Returns 1 (false) when no ENOSPC signature is found in the recent log tail, when
# the disk has recovered, or when LOG_FILE is unset or unreadable.

# _dolt_enospc_free_bytes <path>... prints the smallest free-byte count across the
# existing paths (df -Pk: portable POSIX output, 1K blocks); empty when none could
# be measured.
_dolt_enospc_free_bytes() {
    _de_min=""
    for _de_path in "$@"; do
        [ -n "$_de_path" ] && [ -e "$_de_path" ] || continue
        _de_kb=$(df -Pk -- "$_de_path" 2>/dev/null | awk 'NR==2 && $4 ~ /^[0-9]+$/ {print $4}')
        [ -n "$_de_kb" ] || continue
        _de_bytes=$((_de_kb * 1024))
        if [ -z "$_de_min" ] || [ "$_de_bytes" -lt "$_de_min" ]; then
            _de_min=$_de_bytes
        fi
    done
    printf '%s' "$_de_min"
}

recovery_should_skip_due_to_enospc() {
    [ -n "${LOG_FILE:-}" ] && [ -r "$LOG_FILE" ] || return 1
    # Scan the recent log tail rather than the whole file; an old transient
    # ENOSPC error from a since-resolved disk-pressure event should not block
    # recovery indefinitely.
    tail -n 1000 "$LOG_FILE" 2>/dev/null \
        | grep -qE 'no space left on device|copy_file_range:.*no space|ENOSPC' \
        || return 1

    _de_clear="${GC_DOLT_ENOSPC_CLEAR_FREE_BYTES:-2147483648}"
    case "$_de_clear" in ''|*[!0-9]*) _de_clear=2147483648 ;; esac
    _de_free=$(_dolt_enospc_free_bytes "${GC_DOLT_DATA_DIR:-}" "${DATA_DIR:-}" "$(dirname -- "$LOG_FILE")")
    if [ -n "$_de_free" ] && [ "$_de_free" -ge "$_de_clear" ]; then
        echo "dolt-enospc: ENOSPC lines in the recent log are stale (free ${_de_free} bytes >= ${_de_clear}); not blocking recovery" >&2
        return 1
    fi
    return 0
}
