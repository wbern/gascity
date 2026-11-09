---
name: upstream-contribution
description: Propose a fork-local Gas City change UPSTREAM to gastownhall/gascity as a clean, humble, reviewer-friendly PR. Use when a fix or improvement that lives on the wbern fork is worth contributing upstream. Covers isolation, fork-pollution scrubbing, rebasing onto current upstream/main, evidence-based + BDD PR descriptions, observability, and the hold-for-human-approval gate before anything outward-facing.
---

# Upstream Contribution (gastownhall/gascity)

Turn a fork-local change into an upstream PR that a maintainer can accept with
minimal effort. Two audiences are served at once: the maintainer (who must
parse and trust it fast) and future fork merges (which must rebase cleanly).

This is for **upstream** PRs to `gastownhall/gascity`. Fork-internal work goes
to the fork's `develop`; do not confuse the two. To hand execution to a
crewmate, use [[gas-city-handoff]]. Verify worktree/branch with
[[gas-city-worktree-safety]] before editing.

## Hard rules (non-negotiable)

1. **Hold the outward-facing step for explicit human approval.** Prepare
   everything locally — branch, clean commit, green tests, PR body — then STOP.
   Never force-push a branch that updates an open upstream PR, and never open a
   new upstream PR, without the human saying go. Preparing is autonomous;
   publishing is gated.
2. **Keep upstream pristine — zero fork pollution.** No fork bead IDs
   (`gcw-*`, `gci-*`, `ga-*`), no fork/city names (`gc2`, city dir names), no
   agent names (`furiosa`, `nux`, `slit`, …), no `.gitignore`/config/fork-only
   files. Reference upstream `#issues`/`#PRs` instead of fork beads. Run a final
   grep sweep for these before pushing.
3. **Never commit from the canonical or planning worktree.** Work in a
   dedicated worktree off `upstream/main`. Preparing a PR must not disturb a
   live dev checkout.
4. **Don't burden the maintainer.** Be humble and neutral. No conversational
   narrative ("we debugged and found…"), no fork-internal backstory, no
   unnecessary detail. Anecdotal facts are fine only if relevant and neutral
   (e.g. a measured number, a reproduced trigger) — never the story of how you
   got there.

## Workflow

**1. Confirm it belongs upstream.** It must be a general improvement, not
fork-specific behavior. If it hardcodes fork assumptions, it isn't ready.

**2. Check prior art.** `git merge-base --is-ancestor <commit> upstream/main`
(is it already upstream?), and `gh pr/issue list --repo gastownhall/gascity
--search …`. Find the predecessor/sibling issues — they become "Related work".

**3. Isolated worktree off CURRENT upstream/main.**
```sh
git fetch upstream
git worktree add -b upstream-pr/<slug> <path> upstream/main
```
Rebasing/cherry-picking onto the *current* tip avoids stale-base lint failures
(golangci-lint `SA5011` etc. that only fire against an old baseline).

**4. Bring the change over and make history clean.** Cherry-pick or rebase the
fork commits onto `upstream/main`; squash to one (or a few) self-contained,
logically-cohesive commit(s). Rewrite the message to stand alone — no fork IDs.
End every commit message with the `Co-Authored-By:` trailer per repo policy.

**5. Scrub fork-local references** from code comments, identifiers, and the
commit message. Rename fork-flavored symbols to generic ones
(`furiosaHybridCodexHooks` → `driftedCodexHooks`). Reframe `(gcw-xxxx)` notes to
the mechanism or the relevant upstream `#PR`. Then sweep:
```sh
git show HEAD | grep -inE 'gcw-|gci-|ga-|gc2|<city>|<agent-names>|furiosa'   # expect only the Author line
```

**6. Green before proposing.** `go build`, `go vet`, and the touched packages'
tests must pass. On macOS with ICU/CGO, `export CGO_CPPFLAGS="-I$(brew --prefix
icu4c)/include"` or use `make`. **Never** `go clean -cache` (`-testcache` is ok).

**7. Make new behavior observable.** If the change adds a new state or path,
surface it (a typed diagnostic field, a status flag) — do not introduce a
*silent* behavior. A silent auto-upgrade/auto-fallback is a review smell and a
future debugging trap.

**8. Write the PR description to a high bar:**
   - **Lead with the value** — the footgun it removes / why it helps, in a
     TL;DR a maintainer reads in ten seconds.
   - **Evidence-based trigger** — exactly what conditions cause the behavior,
     with measured numbers where they exist (latency, counts). Neutral facts.
   - **BDD Given/When/Then** for the behavior change — one scenario per path
     (the fix; the unchanged-conservative case; the unchanged-safe case).
     Match them to the tests.
   - **Why it's safe** — the guard's boundary; what it does *not* change.
   - **Related work** — a short list linking the predecessor/sibling
     `#issues`/`#PRs`, one line each. This is the handover: it lets the
     maintainer place the change in context without you narrating.
   - **Scope** — the files touched.
   - Easily parseable (headings, tables, code spans). Humble. No fork story.

**9. Push (gated).** With human approval:
```sh
git push --no-verify --force-with-lease origin upstream-pr/<slug>   # rebased branch
```
`--no-verify` avoids the pre-push full-suite hook (which has taken a live city
down); CI gates the PR. `--force-with-lease` for rebased history.

**10. Open / update the PR (gated).** `gh pr create --repo gastownhall/gascity
--base main --head <fork>:<branch>` or `gh pr edit <n> --body-file …`. Confirm
the head SHA and that CI re-ran.

**11. Aftercare.** Record the upstream PR number on the fork tracking bead.
Delete stale duplicate branches. If a fix already merged to the fork's
`develop` under a fork ID, the upstream commit is the clean sibling — don't
re-derive it.

## Design facts worth not re-litigating

- **A Gas City city root is not a git repo, by design** — `git init` setup and
  city registration are separate concerns (unlike Gas Town's `gt git-init`).
  Rigs are the git repos. Do not "fix" a non-git city root by `git init`-ing it
  (it nests every rig/worktree repo under the root). Fix the *assumption* that
  leaked into a subsystem instead.
