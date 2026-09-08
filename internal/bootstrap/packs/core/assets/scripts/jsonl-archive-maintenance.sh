#!/usr/bin/env bash
# Sourced by the JSONL exporter. Packing and export share the same kernel lock;
# no second order, PID file, stale-lock reaper, or background Git maintenance.
# GC_JSONL_PACK_INTERVAL_SEC=0 disables packing (default); e.g. 21600 is six
# hours. GC_JSONL_PACK_MIN_FREE_MB defaults to 1024 above twice object size.
# GC_JSONL_PACK_MIN_LOOSE_MB defaults to 256; tiny/already-packed stores defer.

archive_lock() {
    local archive_real lock_rc=0
    command -v flock >/dev/null 2>&1 || {
        echo 'jsonl-export: flock is required for archive exclusion' >&2
        return 1
    }
    mkdir -p "$ARCHIVE_REPO" || return 1
    archive_real=$(cd "$ARCHIVE_REPO" && pwd -P) || return 1
    # Never unlink the lock inode: another exporter may already have it open.
    exec 8>"${archive_real}.export.lock" || return 1
    flock -n -E 75 8 || lock_rc=$?
    if [ "$lock_rc" = 75 ]; then
        echo 'jsonl-export: archive busy; deferred' >&2
        return 75
    fi
    [ "$lock_rc" = 0 ] || return "$lock_rc"
}

archive_pack_if_due() {
    local interval="${GC_JSONL_PACK_INTERVAL_SEC:-0}" now last status gitdir pre post free refs post_refs log rc rss elapsed start timeout_bin loose updated
    PACK_VERIFY_PENDING=$(read_state_json | jq -r '.packing.pending_export // false') || return 1
    case "$interval" in ''|*[!0-9]*) echo 'jsonl-export: invalid packing interval' >&2; return 1 ;; esac
    [ "$interval" -gt 0 ] || return 0
    [ -d "$ARCHIVE_REPO/.git" ] || return 0
    now=$(date +%s)
    last=$(read_state_json | jq -er '(.packing.last_packed_epoch // 0) | select(type == "number" and . >= 0 and floor == .)') || return 1
    [ "$((now - last))" -ge "$interval" ] || return 0
    local min_loose="${GC_JSONL_PACK_MIN_LOOSE_MB:-256}"
    case "$min_loose" in ''|*[!0-9]*) echo 'jsonl-export: invalid loose-object floor' >&2; return 1 ;; esac
    loose=$(git -C "$ARCHIVE_REPO" count-objects -v | awk '$1 == "size:" {print $2}') || return 1
    case "$loose" in ''|*[!0-9]*) echo 'jsonl-export: invalid object-size measurement' >&2; return 1 ;; esac
    [ "$loose" -ge "$((min_loose * 1024))" ] || return 0
    status=$(GIT_OPTIONAL_LOCKS=0 git -C "$ARCHIVE_REPO" status --porcelain --untracked-files=all) || return 1
    if [ -n "$status" ]; then
        echo 'jsonl-export: packing deferred; archive has uncommitted exports' >&2
        return 0
    fi
    refs=$(git -C "$ARCHIVE_REPO" show-ref) || {
        echo 'jsonl-export: cannot read archive references' >&2
        return 1
    }
    timeout_bin=$(command -v timeout || command -v gtimeout) || {
        echo 'jsonl-export: packing requires timeout or gtimeout' >&2
        return 1
    }
    gitdir=$(git -C "$ARCHIVE_REPO" rev-parse --absolute-git-dir) || return 1
    pre=$(du -sk "$gitdir/objects" | awk '{print $1}') || return 1
    free=$(df -Pk "$gitdir" | awk 'END {print $4}') || return 1
    local min_free="${GC_JSONL_PACK_MIN_FREE_MB:-1024}"
    case "$min_free" in ''|*[!0-9]*) echo 'jsonl-export: invalid packing free-space floor' >&2; return 1 ;; esac
    if [ "$free" -lt "$((pre * 2 + min_free * 1024))" ]; then
        echo 'jsonl-export: packing deferred; insufficient temporary disk space' >&2
        return 0
    fi
    "$timeout_bin" 60 git -C "$ARCHIVE_REPO" fsck --full --no-dangling || {
        echo 'jsonl-export: pre-packing connectivity failed' >&2
        return 1
    }
    log=$(mktemp "${TMPDIR:-/tmp}/jsonl-pack.XXXXXX") || return 1
    start=$(date +%s)
    local time_args=(-v)
    [ "$(uname -s)" != Darwin ] || time_args=(-l)
    rc=0
    # Fixed, reviewed CPU/window/cache bounds. gc.auto stays disabled. Preserve
    # reflogs and worktree references too; --no-prune alone does not do that.
    "$timeout_bin" --kill-after=10 300 /usr/bin/time "${time_args[@]}" \
        git -C "$ARCHIVE_REPO" -c maintenance.auto=false -c gc.auto=0 \
        -c pack.threads=2 -c pack.windowMemory=64m -c pack.deltaCacheSize=32m \
        -c gc.reflogExpire=never -c gc.reflogExpireUnreachable=never \
        -c gc.worktreePruneExpire=never gc --no-prune >"$log" 2>&1 || rc=$?
    elapsed=$(($(date +%s) - start))
    rss=$(awk '/maximum resident set size/ {print int($1 / 1024)} /Maximum resident set size/ {print $NF}' "$log")
    if [ "$rc" != 0 ]; then
        echo "jsonl-export: packing failed rc=$rc elapsed=${elapsed}s" >&2
        tail -8 "$log" >&2
        rm -f "$log"
        return "$rc"
    fi
    rm -f "$log"
    "$timeout_bin" 60 git -C "$ARCHIVE_REPO" fsck --full --no-dangling || {
        echo 'jsonl-export: post-packing connectivity failed' >&2
        return 1
    }
    post_refs=$(git -C "$ARCHIVE_REPO" show-ref) || return 1
    [ "$post_refs" = "$refs" ] || {
        echo 'jsonl-export: refs changed during packing' >&2
        return 1
    }
    post=$(du -sk "$gitdir/objects" | awk '{print $1}') || return 1
    updated=$(read_state_json | jq -c --argjson at "$now" --argjson pre "$pre" --argjson post "$post" \
        --argjson elapsed "$elapsed" --argjson rss "${rss:-null}" \
        '.packing = {last_packed_epoch:$at,pre_kib:$pre,post_kib:$post,elapsed_sec:$elapsed,max_rss_kib:$rss,pending_export:true}') || return 1
    write_state_json "$updated" || return 1
    PACK_VERIFY_PENDING=true
    echo "jsonl-export: packed ${pre}KiB -> ${post}KiB elapsed=${elapsed}s max_rss_kib=${rss:-unknown}; next export verification pending" >&2
}

archive_pack_verify_export() {
    local updated
    [ "${PACK_VERIFY_PENDING:-false}" = true ] || return 0
    if [ "${TOTAL_DBS:-0}" = 0 ] || [ "${FAILED_DB_COUNT:-0}" != 0 ] || [ "${HALTED:-0}" != 0 ]; then
        echo 'jsonl-export: post-packing export incomplete; verification pending' >&2
        return 1
    fi
    # A no-change export still needs an explicit push proof after packing.
    # This is the existing exporter retry/ownership path, never a new pusher.
    if should_attempt_push; then
        case "${PUSH_STATUS:-}" in
            ok) : ;;
            failed) return 1 ;;
            *) push_archive_main || return 1 ;;
        esac
    fi
    updated=$(read_state_json | jq -c --arg at "$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
        --arg mode "$ARCHIVE_MODE" \
        '.packing.pending_export = false | .packing.export_verified_at = $at | .packing.archive_mode = $mode') || return 1
    write_state_json "$updated" || return 1
    PACK_VERIFY_PENDING=false
}
