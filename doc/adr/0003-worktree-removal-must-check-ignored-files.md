---
status: accepted
date: 2026-08-08
applies_to:
  - "cmd/gc/bead_worktree_reaper.go"
  - "cmd/gc/agent_home_worktree_cleanup.go"
  - "cmd/gc/session_worktree_prune.go"
  - "cmd/gc/stray_worktree_scan.go"
  - "internal/git/**/*.go"
pre_filter:
  - "worktree remove"
  - "WorktreeRemove"
  - "HasUncommittedWork"
  - "porcelain"
---

# 3. Automated worktree removal must check ignored files; `git worktree remove` is not a backstop

## Context

Every worktree-reaping path in this fleet — Go and shell — documents the same
safety claim in its header: that removal uses `git worktree remove` **without**
`--force`, so git's own refusal acts as a final backstop against destroying
work.

**That claim is false for exactly the case that matters.** Measured on
2026-08-08 in a throwaway repo, with a control:

```
worktree holding ONLY an ignored file (.env.local):
    git status --porcelain     -> ''            (looks pristine)
    git worktree remove <wt>   -> EXIT=0, no output, .env.local DESTROYED

CONTROL, same probe, modified TRACKED file:
    git worktree remove <wt2>  -> EXIT=128 "contains modified or untracked
                                  files, use --force to delete it"
```

The probe *can* refuse; it simply cannot see ignored content. Adversarial
review widened the finding: this holds for **every** ignore source — committed
`.gitignore`, nested ignored directories, ignored files under otherwise-tracked
subdirectories, global `core.excludesFile`, and `.git/info/exclude`. It refuses
in only three cases, none credential-related: a co-present non-ignored
untracked file, a locked worktree, and an initialized submodule.

This repo's removal paths inherit the gap. Measured here, with a control:

```
files under cmd/gc matching *worktree*/*reap* that mention --ignored : 0
CONTROL, files repo-wide that mention --ignored                      : 3
```

Three paths remove worktrees — `bead_worktree_reaper.go`,
`agent_home_worktree_cleanup.go`, `session_worktree_prune.go` — and all three
decide safety from a bare `status --porcelain`, which is blind to ignored
files by definition.

The live consequence on 2026-08-08: a worktree holding `.env.local` with
`SUPABASE_SERVICE_ROLE_KEY`, `SUPABASE_JWT_SECRET` and
`STATSIG_SERVER_SECRET_KEY` sat one gate away from deletion by an armed
reaper. It survived only on a borrow-veto and process liveness — two gates that
fail *together*, because the agent stopping is the same event that closes the
beads and ends the liveness. Replicating that exact state in a scratch repo:
both Go probes reported safe, non-force removal returned 0, the file was
destroyed, unrecoverable.

A separate contributing error is worth recording because it shaped the
mistake: a reap performed earlier that day was reported as safe partly on the
grounds that "zero refusals means git agreed with our gates". Git's silence was
not agreement — it was inability to speak. The reassurance converted a single
gate into an imagined double gate, which is exactly when hardening the real one
stops.

## Decision

No automated path may remove a git worktree without first checking for
credential-shaped **ignored** files, using `git status --porcelain --ignored`
(or equivalent). A check keyed on `.gitignore` alone is insufficient, because
`core.excludesFile` and `.git/info/exclude` produce the same invisibility.

That check lives in **one shared helper in `internal/git`**, beside the
existing uncommitted-work predicate — not in each caller. All three removal
paths reach it there.

The absence of `--force` may be described as defence-in-depth. It may **not**
be described, in code comments or documentation, as protection against
destroying ignored or credential-bearing content. Existing headers making that
claim are wrong and should be corrected as they are touched.

## Consequences

Fixing only `bead_worktree_reaper.go` — the path where the risk was first
observed — would leave the other two armed and blind, and would create a fresh
instance of the restated-rule defect class (`gcw-ajoj`) *in the act of fixing
the previous one*. That is the specific reason the gate is placed in
`internal/git` rather than at the call site that happened to surface it.

Implementation is tracked by `gcw-x4xn` (P1) and the criteria live on
`gcw-542j`. Criterion 5 there — "removed with `git worktree remove` (no
`--force`), so git's own refusal is the backstop" — is superseded by this ADR
and must be restated as what it actually does.

A filename-regex gate (`.env|credential|secret|.pem|.key|id_rsa`) is what
exists today and is now single-point rather than belt-and-braces. It will miss
credentials in non-matching files — `.npmrc`, `.netrc`, a token in
`config.json`, `service-account.json`. Strengthening that pattern is in scope
for the shared helper; treating the regex as sufficient is not.

The acceptance test must be a mutation, not a passing run: with the gate
removed, a worktree containing only an ignored `.env.local` must be reported
unsafe. A green suite proves nothing here — this ADR exists because a
non-refusal was read as approval.
