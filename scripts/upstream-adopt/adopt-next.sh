#!/usr/bin/env bash
# adopt-next.sh — take ONE verified-clean upstream commit per tick, as a PR.
#
# Purpose: keep the fork moving toward upstream in small, reviewable, low-risk
# steps while the big merge waits, and shrink that merge's surface as it goes.
#
# This runs unattended, so every dangerous thing it could do is refused rather
# than merely avoided:
#
#   - It never writes to the canonical checkout. All work happens in a scratch
#     worktree it creates and removes. (Pathspec checkouts and force-removals in
#     a shared worktree destroy uncommitted work unrecoverably; see AGENTS.md
#     "Git safety".)
#   - It only ever force-removes a worktree whose path it created itself, matched
#     by exact path. Never by pattern. A loose `git worktree list | grep adopt`
#     also matches an unrelated worktree whose BRANCH merely contains "adopt",
#     and force-removing that destroys a colleague's uncommitted work. That is
#     not hypothetical: it happened while building this script, 2026-09-04.
#   - It never pushes to develop and never merges. It opens a PR and stops; the
#     existing review/merge gate decides.
#   - It keeps WIP at one. If an adoption PR is already open, it refuses.
#   - It halts on first failure instead of retrying. A failed cherry-pick or a
#     red preflight means a human looks, not that the next tick tries again.
#   - Every probe fails CLOSED. If idleness or WIP cannot be established, it
#     refuses rather than assuming the safe answer.
#   - It only ever consumes SHAs from worklist.tsv, which contains only commits
#     already trialled clean. It never picks a commit it chose itself.
#   - --check is side-effect free and is what the order's trigger runs.
#
# Usage:
#   adopt-next.sh --check        exit 0 only if a tick should run now
#   adopt-next.sh --dry-run      do everything except push and open the PR
#   adopt-next.sh                take the next entry and open a PR
set -euo pipefail

REPO="${ADOPT_REPO:-$HOME/Repos/KenDev-AB/gas-city-wbern}"
BASE="${ADOPT_BASE:-develop}"
IDLE_MIN="${ADOPT_IDLE_MIN:-20}"
WORKLIST_REL="scripts/upstream-adopt/worklist.tsv"
BRANCH_PREFIX="upstream-adopt"
MODE="run"

case "${1:-}" in
--check) MODE=check ;;
--dry-run) MODE=dry ;;
"") ;;
*)
  echo "adopt-next: unknown argument $1" >&2
  exit 2
  ;;
esac

log() { printf '%s adopt-next: %s\n' "$(date -u +%H:%M:%SZ)" "$*" >&2; }
refuse() {
  log "REFUSING: $*"
  exit 1
}

command -v gh >/dev/null 2>&1 || refuse "gh is not on PATH"
# A linked worktree's .git is a FILE, not a directory, so test the repo the way
# git does. State lives in the common git dir so every worktree shares one
# adopted-list rather than silently keeping its own and re-adopting.
git -C "$REPO" rev-parse --git-dir >/dev/null 2>&1 || refuse "not a git repo: $REPO"
GIT_COMMON=$(cd "$REPO" && cd "$(git rev-parse --git-common-dir)" && pwd)
STATE_DIR="${ADOPT_STATE_DIR:-$GIT_COMMON/upstream-adopt}"

# --- gate 1: is there anything left to do? -----------------------------------
worklist="$REPO/$WORKLIST_REL"
[ -f "$worklist" ] || refuse "no worklist at $worklist"
mkdir -p "$STATE_DIR"
done_file="$STATE_DIR/adopted.txt"
touch "$done_file"

next_line=""
while IFS= read -r line; do
  case "$line" in '' | '#'*) continue ;; esac
  sha=$(printf '%s' "$line" | cut -f2)
  grep -qxF "$sha" "$done_file" && continue
  next_line="$line"
  break
done <"$worklist"

[ -n "$next_line" ] || refuse "worklist is exhausted — nothing left to adopt"
NEXT_TIER=$(printf '%s' "$next_line" | cut -f1)
NEXT_SHA=$(printf '%s' "$next_line" | cut -f2)
NEXT_SUBJ=$(printf '%s' "$next_line" | cut -f3)

# --- gate 2: WIP is one ------------------------------------------------------
# A pile of open adoption PRs is exactly the friction this is supposed to avoid.
if ! pr_json=$(gh pr list --repo wbern/gascity --state open --json number,headRefName 2>/dev/null); then
  refuse "could not query open PRs; refusing rather than assuming none"
fi
open_prs=$(printf '%s' "$pr_json" | python3 -c '
import json,sys
try: rows=json.loads(sys.stdin.read() or "[]")
except Exception: sys.exit(3)
print(sum(1 for r in rows if str(r.get("headRefName","")).startswith("upstream-adopt/")))
') || refuse "could not parse the open-PR list; refusing rather than assuming none"
case "$open_prs" in '' | *[!0-9]*) refuse "open-PR count was not a number ($open_prs)" ;; esac
if [ "$open_prs" -gt 0 ]; then
  refuse "an adoption PR is already open ($open_prs); land it before taking another"
fi

# --- gate 3: is the city quiet? ----------------------------------------------
# Adoption is background work. It must never compete with William's own session
# or with live agents.
if [ "${ADOPT_SKIP_IDLE:-0}" != "1" ]; then
  # Idleness is "no active session has done anything for IDLE_MIN minutes",
  # matching gc2-idle-mail-nudge.sh — the city's already-proven signal. Session
  # state alone is useless here: an "active" session is merely alive, and most
  # stay active indefinitely. A session with no parseable last_active counts as
  # busy, so an unreadable clock can never green-light a run.
  if ! sessions_json=$(gc session list --json 2>/dev/null); then
    refuse "could not list sessions; refusing rather than assuming the city is idle"
  fi
  recent=$(printf '%s' "$sessions_json" | IDLE_MIN="$IDLE_MIN" python3 -c '
import json,os,sys,datetime
raw=sys.stdin.read().strip()
if not raw: sys.exit(3)
try: d=json.loads(raw)
except Exception: sys.exit(3)
if not d.get("ok", True): sys.exit(3)          # ok:false arrives with exit 0
rows=d.get("sessions")
if not isinstance(rows,list): sys.exit(3)
limit=float(os.environ["IDLE_MIN"])*60
now=datetime.datetime.now(datetime.timezone.utc)
n=0
for s in rows:
    if not isinstance(s,dict) or s.get("state")!="active": continue
    ts=str(s.get("last_active") or "").strip()
    if not ts or ts=="-":
        n+=1; continue                          # unknown clock => treat as busy
    try:
        t=datetime.datetime.fromisoformat(ts.replace("Z","+00:00"))
        if t.tzinfo is None: t=t.replace(tzinfo=datetime.timezone.utc)
    except Exception:
        n+=1; continue
    if (now-t).total_seconds() < limit: n+=1
print(n)
') || refuse "could not evaluate session idleness; refusing rather than assuming the city is idle"
  case "$recent" in '' | *[!0-9]*) refuse "idle probe did not return a number ($recent)" ;; esac
  if [ "$recent" -gt 0 ]; then
    refuse "$recent session(s) active within ${IDLE_MIN}m; adoption waits for a quiet city"
  fi
fi

if [ "$MODE" = check ]; then
  log "READY: next is [$NEXT_TIER] $NEXT_SHA $NEXT_SUBJ"
  exit 0
fi

# --- do the work in a scratch worktree, never the canonical checkout ---------
branch="$BRANCH_PREFIX/${NEXT_SHA}"
scratch=$(mktemp -d -p /var/tmp adopt.XXXXXX)

# Remove ONLY this exact path. See the header note on pattern-matched removal.
cleanup() {
  git -C "$REPO" worktree remove --force "$scratch" >/dev/null 2>&1 || true
  rm -rf "$scratch"
  git -C "$REPO" worktree prune >/dev/null 2>&1 || true
}
trap cleanup EXIT

# A previous tick may have been killed mid-flight (a timeout, a reboot), leaving
# this SHA's branch behind. Reclaim it rather than refusing forever — but only
# when no worktree still holds it, so a live checkout is never yanked.
if git -C "$REPO" show-ref --verify --quiet "refs/heads/$branch"; then
  if git -C "$REPO" worktree list --porcelain | grep -qxF "branch refs/heads/$branch"; then
    refuse "$branch is checked out in another worktree; resolve by hand"
  fi
  log "reclaiming stale branch $branch from an interrupted run"
  git -C "$REPO" branch -D "$branch" >/dev/null 2>&1 || refuse "could not delete stale $branch"
fi

log "[$NEXT_TIER] adopting $NEXT_SHA — $NEXT_SUBJ"
git -C "$REPO" fetch --quiet origin "$BASE" || refuse "fetch origin/$BASE failed"
git -C "$REPO" fetch --quiet upstream || refuse "fetch upstream failed"
git -C "$REPO" worktree add --quiet -b "$branch" "$scratch" "origin/$BASE" ||
  refuse "could not create scratch worktree for $branch"

if ! git -C "$scratch" cherry-pick -x "$NEXT_SHA" >"$scratch/.cp.log" 2>&1; then
  git -C "$scratch" cherry-pick --abort >/dev/null 2>&1 || true
  log "cherry-pick FAILED for $NEXT_SHA — halting so a human can look"
  sed 's/^/    /' "$scratch/.cp.log" >&2 || true
  refuse "cherry-pick conflict on $NEXT_SHA (worklist said clean; base has moved)"
fi

# Preflight, not a gate. This proves the pick is not obviously broken before it
# costs a reviewer anything; the PR's own CI (verify + lint) remains the real
# gate and runs the full suite. Scoping to the touched packages is deliberate:
# a repo-wide `go build ./... && go vet ./...` measured over 10 minutes per tick
# here — far too heavy for an unattended job, and it duplicates what CI does
# properly a moment later.
pkgs=$(git -C "$scratch" show --stat --format='' --name-only "$NEXT_SHA" |
  grep -E '\.go$' | xargs -n1 dirname 2>/dev/null | sort -u | sed 's|^|./|' | tr '\n' ' ')

if [ -z "${pkgs// /}" ]; then
  log "no Go packages touched; skipping preflight (CI still gates the PR)"
else
  log "preflight over touched packages: $pkgs"
  # shellcheck disable=SC2086
  (cd "$scratch" && go build $pkgs >"$scratch/.pre.log" 2>&1) || {
    tail -20 "$scratch/.pre.log" >&2
    refuse "go build failed after $NEXT_SHA"
  }
  # shellcheck disable=SC2086
  (cd "$scratch" && go vet $pkgs >"$scratch/.pre.log" 2>&1) || {
    tail -20 "$scratch/.pre.log" >&2
    refuse "go vet failed after $NEXT_SHA"
  }
  # shellcheck disable=SC2086
  (cd "$scratch" && go test $pkgs -count=1 >"$scratch/.test.log" 2>&1) || {
    tail -30 "$scratch/.test.log" >&2
    refuse "tests failed after $NEXT_SHA"
  }
fi

if [ "$MODE" = dry ]; then
  log "DRY RUN OK — $NEXT_SHA cherry-picks and passes preflight. Nothing pushed."
  exit 0
fi

git -C "$scratch" push --quiet -u origin "$branch" || refuse "push failed for $branch"
gh pr create --repo wbern/gascity --base "$BASE" --head "$branch" \
  --title "chore(upstream): adopt $NEXT_SHA — $NEXT_SUBJ" \
  --body "$(
    cat <<BODY
Automated upstream adoption, one commit per tick. Tier: **$NEXT_TIER**.

Cherry-picks upstream \`$NEXT_SHA\` onto \`$BASE\`:

> $NEXT_SUBJ

**Why this is here.** The fork diverged from \`upstream/main\` on 2026-08-04 and is 493 commits behind. Rather than one large merge, this queue takes verified-clean commits in risk-ascending order, shrinking the eventual merge surface while picking up stability fixes on the way.

**Why this one is low risk.** Its SHA is on \`scripts/upstream-adopt/worklist.tsv\`, meaning it was trialled with \`git cherry-pick -n\` against \`$BASE\` and applied without conflict. Feature-shaped commits (\`feat\`/\`perf\`) are excluded by policy and listed as deferred in that file.

**Preflight before opening:** \`go build\`, \`go vet\` and \`go test\` over the packages this commit touches, in a scratch worktree. That is a smoke check, not a gate — this PR's own CI is the gate.

Opened by \`scripts/upstream-adopt/adopt-next.sh\`, which never merges and never writes to the canonical checkout. Review normally; this is not auto-merged.
BODY
  )" >"$scratch/.pr.url" 2>&1 || refuse "gh pr create failed"

printf '%s\n' "$NEXT_SHA" >>"$done_file"
log "opened $(cat "$scratch/.pr.url") and recorded $NEXT_SHA"
