---
status: accepted
date: 2026-10-22
applies_to:
  - "cmd/gc/bead_worktree_reaper.go"
  - "cmd/gc/bead_worktree_reaper_ignored.go"
  - "internal/git/**/*.go"
pre_filter:
  - "nonDisposableIgnored"
  - "WorkingTreeStatus"
  - "auto_reap_disposable_ignored"
---

# 10. The closed-bead reaper releases only provably disposable ignored content

Refines [ADR-0003](0003-worktree-removal-must-check-ignored-files.md); does not
supersede it.

## Context

ADR-0003 made every automated removal path refuse a worktree that holds any
ignored file, because a non-force `git worktree remove` destroys ignored
content without complaint. That rule holds. Applied as "any ignored file at
all", though, it stopped the closed-bead reaper from removing anything: gc
writes ignored files into every task worktree it creates (`.beads/redirect`,
`.gc/settings.json`, `.codex/hooks.json`), and package installs leave
`node_modules/`.

Measured on gc2 on 2026-10-22, with the host disk at 97%: the reaper had
removed nothing since 2026-10-16. A live dry run kept all 42 closed-bead
candidates. 38 were held by the ignored-file gate, and 27 of those held
**only** ignored content.

An adversarial review of the first proposal also found:
- git collapses a wholly-ignored directory to a single `!! dir/` line, so a
  decision keyed on git's paths cannot see a nested linked worktree under
  `.gc/`. A non-force `git worktree remove` deletes it with exit 0 (reproduced
  in a scratch repo; live debris of that shape exists under three task
  worktrees);
- real regular-file `.env.local` copies exist in the fleet next to symlinked
  ones, so the two must be told apart.

## Decision

The closed-bead reaper classifies ignored content **on the filesystem**,
default-deny:

1. A symlink is disposable and never followed. Removal unlinks it; the target
   survives.
2. A path matching the disposable set is disposable. The built-in set holds
   only files gc itself writes and fully owns, plus `node_modules/` and
   `__pycache__/` at any depth. Files gc merges into while preserving other
   content (`.codex/hooks.json`, `.codex/config.toml`, `.mcp.json`) are not in
   it. Operators extend it with `daemon.auto_reap_disposable_ignored`; Go
   carries no project-specific build-output names.
3. A matched **directory** is walked without following symlinks. Any entry
   named `.git` keeps the worktree, and so does any walk error.
4. Everything else keeps the worktree: an unmatched file, a regular
   `.env.local`, `.npmrc`, `service-account.json`, an unreadable path. The
   reason, and `non_disposable_ignored` in `gc worktree reap --json`, name the
   paths.

This is a strictly narrower release than ADR-0003's filename-regex fallback
would allow. The regex is not consulted: a credential in a non-matching file
is kept because the file is not on the allowlist, not because its name looks
dangerous.

The other removal paths (agent-home cleanup, session prune, stray scan) keep
ADR-0003's whole-tree `HasUncommittedOrIgnoredWork` gate unchanged. The
`internal/git` side of the split is `WorkingTreeStatus`, which separates real
dirt from ignored roots and fails closed: an unparseable status record counts
as dirt.

## Consequences

The reaper can reclaim task worktrees again. On the 2026-10-22 dry run that
was 5 with the built-in set alone; most of the rest name crm and vibe-coding
build output (`report/`, `dist/`, `.vercel/`, `.husky/_/`) that a city must
opt in to explicitly.

Entries added to the built-in set must be files gc materializes or caches that
are rebuilt on demand, never project output. A pattern that matches something
irreplaceable deletes it on the next tick.

Acceptance stays a mutation, per ADR-0003: with `nonDisposableIgnored`
short-circuited to "all disposable",
`TestReapClosedBeadWorktrees_ProtectsIgnoredOnlyWorktree` (a regular ignored
`.env.local`) must fail.

Cost: a worktree kept by its first non-disposable path never walks its matched
directories. Only trees that are otherwise reapable pay for the nested-`.git`
walk, so classifying every candidate on every controller tick stays cheap.
