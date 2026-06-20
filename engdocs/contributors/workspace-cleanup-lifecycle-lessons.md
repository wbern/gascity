# Workspace Cleanup Lifecycle Lessons

Status: upstream report for follow-up design

Owner context: `gcw-ffp`

Date: 2026-06-20

## Problem Statement

Gas City already has an opt-in closed-bead worktree reaper via
`[daemon].auto_reap_closed_bead_worktrees`. That reaper is intentionally
small: when enabled, the controller removes per-bead worktrees for closed
beads if the worktree is clean, has no unpushed commits, and has no stashes.

Local gc2 operations found that production-like agent cities need a richer
workspace cleanup lifecycle before this should become a default operational
cleanup surface. The local incidents were not just "closed bead leaves a
directory behind"; they included task worktrees with branch provenance,
patch-equivalent changes, rejected or deferred work, repo-wide stashes, local
snapshot exclusions, and disk pressure from local service state.

The upstream decision needed is whether Gas City should promote these lessons
into a first-class workspace cleanup design instead of growing ad hoc local
janitors per city.

## Source Evidence

The source evidence was inspected from the local `gas-city-infra` rig. Runtime
logs and machine-specific measurements are intentionally not copied here.

- `gas-city-infra/docs/local-operations-runbook.md` - persistence rules,
  worktree cleanup measurements, local snapshot exclusions, and local
  Supabase/OrbStack cleanup rules.
- `gas-city-infra/scripts/gc-worktree-janitor-report.sh` - conservative
  dry-run/apply classifier for Gas City-managed bead task worktrees.
- `gas-city-infra/scripts/gc-worktree-janitor-order.sh` - dry-run order wrapper
  that emits one compact JSONL summary instead of persisting full reports.
- `gas-city-infra/scripts/check-crm-workspace.sh` - fixture coverage for task
  worktree ownership, rejected-branch resume, and workspace state telemetry.

The local runbook explicitly separates durable decisions from telemetry:
source docs keep policy, scripts keep reusable behavior, `.gc/` JSONL keeps
high-volume per-item measurements, and Beads keep actionable follow-up or
rollups.

## Current Upstream State

As of upstream `main` on 2026-06-20:

- `cmd/gc/bead_worktree_reaper.go` implements
  `reapClosedBeadWorktrees(...)`.
- `internal/config/config.go` exposes
  `[daemon].auto_reap_closed_bead_worktrees`, defaulting to false.
- `cmd/gc/city_runtime.go` calls the reaper during controller ticks only when
  the config field is enabled.
- `internal/events/payloads.go` registers `bead.worktree.reaped` and
  `bead.worktree.reap_skipped` payloads.
- `docs/reference/config.md` documents the opt-in knob.

The current reaper is a good safety baseline, but it does not yet classify
closed work by merge metadata, patch equivalence, intentional non-merge
metadata, task-worktree path ownership, or telemetry sinks. It also relies on a
repo-wide stash check as a hard blocker, while the local janitor treats stashes
as hygiene warnings because Git stashes are repository-wide, not proof that a
specific task worktree is unsafe.

## Design Guidance

### Treat Workspace Cleanup As A Bead Lifecycle Projection

The cleanup unit should be a Gas City-managed workspace derived from bead and
session state, not an arbitrary directory. A first-class implementation should
classify at least these workspace kinds:

- Session home worktrees: never remove as closed-bead task cleanup.
- Pool worker directories: keep under the existing worker-dir pruning contract.
- Bead task worktrees: eligible only when the path matches the city-owned task
  workspace convention for that bead.
- Unknown worktrees: report only; do not apply cleanup automatically.

The local janitor's stricter path rule is the useful upstream default:
automatic removal should apply only to a path shaped like a managed task
worktree for the same rig and bead.

### Preserve Branches And Provenance

Workspace cleanup should remove only the worktree checkout. It should not
delete local or remote branches. Branches are low-volume provenance compared to
worktree directories and are useful when a human later needs to audit or
recover the decision.

Cleanup decisions should read standardized bead metadata when present:

- `branch`
- `target`
- `merge_result`
- `merged_sha`
- `merged_target`
- `rejection_reason`
- close reason or notes containing an intentional non-merge decision

The metadata contract does not need to be a new primitive. It is bead
metadata used by a controller-owned projection.

### Classify Before Removing

The local classifier found a better decision shape than a single clean/dirty
gate:

- `keep-active`: an active session still owns the work directory.
- `keep-open`: the bead is not closed.
- `keep-untracked`: no bead ID can be resolved for the worktree.
- `review-dirty`: the checkout has uncommitted changes.
- `clean-merged-metadata`: the bead records a merge and the merged SHA is on
  the target.
- `clean-patch-equivalent`: `git cherry` shows every branch patch is already
  present on the target.
- `clean-intentional-nonmerge`: the bead records rejection, deferral,
  abandonment, or another explicit non-merge close.
- `review-closed-unclassified`: the bead is closed but unique patches or
  missing provenance make automatic cleanup unsafe.

Patch equivalence is the key lesson. A branch can be safe to remove as a
worktree even when it was not merged by exact SHA, because its changes were
applied elsewhere. Without patch equivalence, cleanup either leaks safe closed
work forever or pushes humans toward unsafe manual deletion.

### Keep Per-Item Telemetry Out Of Beads By Default

The default sink split should be:

- Event bus / local runtime JSONL: per-item cleanup events, dry-run summaries,
  candidate sizes, dirty counts, upstream ahead/behind state, and timing.
- Beads: actionable anomalies, review-required closed worktrees, skipped
  policy decisions, and periodic rollups.
- Source docs: durable safety rules and commands.

Do not append routine per-worktree disk measurements or full cleanup reports to
Beads. That turns the task store into a telemetry dump and makes meaningful
follow-up harder to find.

The existing `bead.worktree.reaped` and `bead.worktree.reap_skipped` events are
the right observation surface. A future slice can expand the payload or add a
summary event with fields such as classification, mode, candidate count,
review-required count, estimated footprint, actual free-space delta for apply
runs, and output log path.

### Ship Dry-Run First

Automatic apply should remain opt-in. The next upstream-friendly step should be
a dry-run command or controller phase that produces:

- one machine-readable summary,
- one human-readable table,
- no destructive action,
- explicit review-required rows,
- a stable event payload for dashboards or orders.

Apply mode should be a separate choice and should remove only
clean-classified, city-owned task worktrees.

### Include Disk-Pressure Runbooks But Keep Them Separate

The gc2 incident also involved local snapshot exclusions and large local
Supabase/OrbStack state. Those are related operational lessons, not the same
cleanup mechanism.

Upstream docs should keep a safe disk-pressure runbook adjacent to workspace
cleanup:

- Preserve source checkouts, `.beads`, rig bead stores, and useful audit state
  in backups.
- Exclude regenerable runtime/cache/build outputs where appropriate.
- Treat local container volumes as local disk pressure, not worktree cleanup.
- Identify Docker/Supabase ownership before deleting anything.
- Ask for explicit approval before destructive cleanup unless the scope has
  already been approved.
- Stop local services cleanly when possible.
- Remove whole local service state through service or Docker commands.
- Do not delete individual database relation files or remote/prod resources.

This avoids overloading the worktree reaper with service-state policy while
still giving operators one place to start during disk incidents.

## Recommended Upstream Slices

1. Document the current reaper limits and this lifecycle target.

   Acceptance: contributors can tell that
   `auto_reap_closed_bead_worktrees` is safe but intentionally limited, and
   that richer task-worktree cleanup is a follow-up design.

2. Add a non-destructive `gc worktree cleanup --dry-run --json` or equivalent
   controller-owned report surface.

   Acceptance: it classifies city-owned task worktrees without deleting files,
   emits typed summary data, and has tests for active, open, dirty, merged,
   patch-equivalent, intentional non-merge, and unclassified closed cases.

3. Standardize task worktree provenance metadata.

   Acceptance: dispatch/worker paths consistently record branch, target, merge
   result, merged SHA, and intentional non-merge metadata where those facts are
   known. Missing metadata remains review-required, not delete-required.

4. Add apply mode behind the existing opt-in posture.

   Acceptance: apply removes only clean-classified, city-owned task worktrees;
   preserves branches; records typed events; and reports actual filesystem
   free-space delta after removal.

5. Add a disk-pressure runbook.

   Acceptance: the runbook covers backup exclusions, generated outputs, local
   service volumes, approval boundaries, and explicit "do not touch" resources.

## Rollback Shape

All destructive behavior should stay behind an explicit opt-in config or flag.
If apply-mode classification proves too broad, operators can disable the config
or stop using the apply flag and keep dry-run telemetry. Because branch deletion
is out of scope, rollback is normally recovering from a preserved branch or
recreating the task worktree from Git plus bead metadata.

## Verification Plan

For docs-only slices:

- `git diff --check`
- link/path review for referenced source docs and scripts

For implementation slices:

- unit tests for classification decisions,
- integration tests for worktree removal under `t.TempDir()`,
- event payload registration tests,
- config default tests proving destructive cleanup remains opt-in,
- manual dry-run on a city with known closed/open/dirty task worktrees before
  any apply run.
