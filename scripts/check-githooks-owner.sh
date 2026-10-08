#!/usr/bin/env bash
# Verify .githooks is this clone's active core.hooksPath.
#
# The gates in .githooks cannot report their own absence: when another installer
# claims core.hooksPath (beads points it at .beads/hooks), git stops invoking
# them entirely and every commit looks clean while formatting, lint-changed, the
# codegen+stage steps and make vet are all skipped. That is how spec-derived
# drift reached the mainline. This check is the external detector; run it
# with `make check-hooks`.
set -euo pipefail

repo_root="$(git rev-parse --show-toplevel)"
configured="$(git config --get core.hooksPath || true)"

expected="$repo_root/.githooks"

if [ ! -d "$expected" ]; then
	echo "ERROR: $expected does not exist — this does not look like a gascity clone." >&2
	exit 1
fi

candidate=""
if [ -z "$configured" ]; then
	actual=""
else
	# core.hooksPath may be relative to the repo root or absolute. Resolve both
	# spellings to a canonical path before comparing.
	case "$configured" in
	/*) candidate="$configured" ;;
	*) candidate="$repo_root/$configured" ;;
	esac
	actual="$(cd "$candidate" 2>/dev/null && pwd -P || true)"
fi

if [ "$actual" = "$(cd "$expected" && pwd -P)" ]; then
	exit 0
fi

# Managed worktrees use pack-owned guards composed with the SDK hooks. Only
# the known small forwarding wrappers qualify; helper policy belongs to the pack.
# Compare the complete bytes so comments or unreachable forwards cannot pass.
composed_hook_body() {
    case "$1" in
    post-checkout) cat <<'GC_HOOK_BODY'
#!/bin/sh
# Gas City post-checkout composition (installed by worktree-setup.sh; do not edit).
[ -z "${GC_CANONICAL_GUARD:-}" ] || exit 0
hook_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd -P) || exit 1
"$hook_dir/gci-post-checkout-policy" "$@" || exit $?
top=$(git rev-parse --show-toplevel 2>/dev/null) || exit 1
if [ -x "$top/.githooks/post-checkout" ]; then
    exec "$top/.githooks/post-checkout" "$@"
fi
exit 0
GC_HOOK_BODY
        ;;
    post-merge) cat <<'GC_HOOK_BODY'
#!/bin/sh
# Gas City post-merge composition (installed by worktree-setup.sh; do not edit).
top=$(git rev-parse --show-toplevel 2>/dev/null) || exit 1
if [ -x "$top/.githooks/post-merge" ]; then
    exec "$top/.githooks/post-merge" "$@"
fi
exit 0
GC_HOOK_BODY
        ;;
    pre-commit) cat <<'GC_HOOK_BODY'
#!/bin/sh
# Gas City pre-commit composition (installed by worktree-setup.sh; do not edit).
hook_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd -P) || exit 1
"$hook_dir/gci-pre-commit-policy" "$@" || exit $?
top=$(git rev-parse --show-toplevel 2>/dev/null) || exit 1
if [ -x "$top/.githooks/pre-commit" ]; then
    exec "$top/.githooks/pre-commit" "$@"
fi
exit 0
GC_HOOK_BODY
        ;;
    pre-push) cat <<'GC_HOOK_BODY'
#!/bin/sh
# Gas City pre-push composition (installed by worktree-setup.sh; do not edit).
hook_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd -P) || exit 1
top=$(git rev-parse --show-toplevel 2>/dev/null) || exit 1
input=$(umask 077; mktemp "${TMPDIR:-/tmp}/gci-pre-push.XXXXXX") || exit 1
trap 'rm -f "$input"' EXIT
trap 'exit 1' HUP INT TERM
cat > "$input" || exit 1
"$hook_dir/gci-pre-push-policy" "$@" < "$input" || exit $?
if [ -x "$top/.githooks/pre-push" ]; then
    "$top/.githooks/pre-push" "$@" < "$input" || exit $?
fi
exit 0
GC_HOOK_BODY
        ;;
    prepare-commit-msg) cat <<'GC_HOOK_BODY'
#!/bin/sh
# Gas City prepare-commit-msg composition (installed by worktree-setup.sh; do not edit).
top=$(git rev-parse --show-toplevel 2>/dev/null) || exit 1
if [ -x "$top/.githooks/prepare-commit-msg" ]; then
    exec "$top/.githooks/prepare-commit-msg" "$@"
fi
exit 0
GC_HOOK_BODY
        ;;
    *) return 1 ;;
    esac
}

# Git reports relative common-dir paths against its invocation directory.
common_dir="$(git -C "$repo_root" rev-parse --git-common-dir)"
case "$common_dir" in
/*) ;;
*) common_dir="$repo_root/$common_dir" ;;
esac
common_dir="$(cd "$common_dir" && pwd -P)"
composed_dir="$common_dir/gci-hooks"
# Recognize a managed installation even when its hook directory is missing.
managed_candidate="$actual"
if [ -z "$managed_candidate" ] && [ -n "$candidate" ]; then
    parent="$(cd "$(dirname "$candidate")" 2>/dev/null && pwd -P || true)"
    if [ -n "$parent" ]; then
        managed_candidate="$parent/$(basename "$candidate")"
    fi
fi
if [ "$managed_candidate" = "$composed_dir" ]; then
    failure=""
    if [ -n "${GC_CANONICAL_GUARD:-}" ]; then
        failure="GC_CANONICAL_GUARD is active"
    elif [ ! -d "$composed_dir" ]; then
        failure="managed hook installation is missing: $composed_dir"
    else
        for hook in post-checkout post-merge pre-commit pre-push prepare-commit-msg; do
            if [ ! -f "$actual/$hook" ] || [ ! -x "$actual/$hook" ]; then
                failure="wrapper $hook is not an executable regular file"
                break
            elif [ ! -f "$expected/$hook" ] || [ ! -x "$expected/$hook" ]; then
                failure="tracked delegate $hook is not an executable regular file"
                break
            elif ! cmp -s "$actual/$hook" <(composed_hook_body "$hook"); then
                failure="wrapper $hook does not match the supported composition"
                break
            fi
        done
        if [ -z "$failure" ]; then
            for helper in gci-post-checkout-policy gci-pre-commit-policy gci-pre-push-policy; do
                if [ ! -f "$actual/$helper" ] || [ ! -x "$actual/$helper" ]; then
                    failure="helper $helper is not an executable regular file"
                    break
                fi
            done
        fi
    fi
    if [ -z "$failure" ]; then
        exit 0
    fi
    echo "ERROR: managed hook composition failed: $failure." >&2
    echo "Ask the pack owner to repair the managed hook installation at $composed_dir." >&2
    exit 1
fi

{
	echo "ERROR: .githooks is not this clone's git hooks directory."
	echo "  core.hooksPath: ${configured:-<unset — git defaults to .git/hooks>}"
	echo "  expected:       $expected"
	echo
	echo "Every gate in .githooks (staged-Go formatting, lint-changed, the OpenAPI"
	echo "spec/client/schema codegen+stage steps, make vet, the push-time suite) is"
	echo "silently skipped while another directory owns core.hooksPath."
	echo
	echo "Fix it with:  make setup"
	echo
	echo "Beads is not lost by doing this: .githooks chains every beads-managed hook"
	echo "through .githooks/lib/beads-chain.sh. If beads reclaims core.hooksPath"
	echo "later (its installer does), re-run 'make setup'."
} >&2
exit 1
