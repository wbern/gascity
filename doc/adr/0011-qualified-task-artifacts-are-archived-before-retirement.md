---
status: accepted
date: 2026-10-25
applies_to:
  - "cmd/gc/worktree_archive*.go"
  - "cmd/gc/cmd_worktree_archive.go"
  - "cmd/gc/bead_worktree_reaper*.go"
  - "internal/worktree/archive*.go"
  - "internal/worktree/retirement_lock*.go"
  - "cmd/gc/session_worktree_prune*.go"
  - "cmd/gc/city_runtime.go"
pre_filter:
  - "Archive"
  - "archive"
  - "collectReapBorrowScope"
  - "reapOwnerLive"
  - "workerDirRetry"
  - "recordDeferredWorkerDirPrune"
---

# 11. Qualified task artifacts are archived before checkout retirement

Refines [ADR-0003](0003-worktree-removal-must-check-ignored-files.md) and
[ADR-0010](0010-reaper-releases-only-provably-disposable-ignored-content.md).

## Context

Provider configuration merges preserve user content. Treating those files as
rebuildable would discard edits; keeping every checkout indefinitely prevents
reclamation even after its PR is merged. The operator chose to preserve bounded
configuration and handoff artifacts in a private archive before reclamation.

## Decision

Ordinary scheduled reaping remains default-deny for non-disposable ignored
content. The separate `worktree archive-reap` protocol requires explicit exact
source expectations and reviewed artifact leaves. Its default plan creates no
archive or checkout files. Execute additionally requires the plan inventory hash.

The configured caller qualifies remote PR completion and repository identity,
and pins the production commit locally. Native code is provider-independent:
its ancestry check alone cannot connect a squash merge to a topic commit. The
copied completion evidence is an audit record, not an authentication mechanism.

Native qualification requires exact registered task ownership, owning common Git
directory, private pointer identity and backlink, HEAD and branch, fresh closed
bead revision and metadata binding, no live process or session, and no borrower
at or beneath the checkout in any required city, rig or relocated class store.
Every scope is live-read through the canonical census resolver; incomplete scope
protects all candidates.
Before removal, actual tracked bytes must match committed blob identities through
a bounded streaming read independent of Git's stat/index cache. Hidden index
flags and weak current status settings are refused early; a past transient
setting can poison the cache, so absence of such flags is not byte proof. Source changes, hidden Git index flags, weakened Git status settings, nested
repositories (including bare repositories), unreadable/special files, and unapproved ignored leaves protect.

Execute holds the same canonical path lock as Ensure/Cleanup. It copies bounded
artifacts into private directories, verifies hashes and an independent restore,
and retains original artifact inodes in a separate private namespace. Immutable
copies remain independent of those links. Evidence, request and manifest are
included in the archive; its completion marker is published only after checks.

The checkout is moved without force into a private quarantine outside ordinary
reaper discovery. The original path may be recreated by its writer and is never
removed by retirement. A bounded, complete current-user file/directory descriptor
scan and fresh source/ownership/borrower checks precede non-force removal of only
the quarantine. Uncertainty retains the private quarantine and archive; it never moves back into
the original namespace or forces deletion over newly created content. Retained inode links preserve late
writes through artifact file descriptors.

This protocol coordinates supported writers and canonical workspace operations.
It does not claim atomic exclusion against a hostile same-user process that
finds and mutates the private quarantine after the final checks. Archive
retention and restoration are explicit operational responsibilities; archives
are not automatically garbage-collected by the worktree reaper.

## Deferred closed-session retirement

A refused session-retirement attempt records an immutable intent under the
city's private `.gc/runtime/worker-dir-retirement/` directory. The controller
creates these records only for actual retirement refusals; it does not infer
ownership from an arbitrary stale directory or create an inline
`.worktree-stale` file. Existing user-authored markers remain ordinary content.

The record binds the closed session, rig, linked-worktree pointer incarnation,
private/common Git directories, exact physical Git root, and branch. It does
not bind an old HEAD: preserving or committing previously dirty work may make
the same retired checkout safe. Each retry rechecks current closed ownership,
all required live borrower scopes, session directories, process cwd and file
handles, current Git identity, ignored/uncommitted content, actual tracked
bytes, and commit preservation under the canonical workspace lock. Removal
uses non-force Git and retains the branch. This retry requires a fully clean
checkout with no ignored content; the separate task-artifact archive protocol
above handles explicitly approved configuration and handoff leftovers.

The periodic consumer runs at most once per minute after the preceding pass
finishes, rotates at most eight records per pass, and shares a ten-second
cooperative budget across phases. Existing synchronous store/Git probes retain
their own bounds; this is not a hard interruption guarantee for every filesystem
call. Existing load, pacing, and removal limits also apply. The private registry
has a bounded entry count, and its cursor advances only past actually attempted
records, so retained work cannot indefinitely starve the unattempted batch tail.

A crash after publishing a record can leave its reserved `.pending-*` hardlink.
Recovery first validates the complete record's JSON, filename digest, private
ownership/mode, inode, and every kernel link. Only matching reserved temporary
aliases inside that same pinned registry may be removed. Unknown names,
external links, symlinks, corrupt content, or changed evidence remain protected;
no user marker or checkout artifact is removed during metadata recovery. The
ordinary single-link reader must accept the same bytes after recovery before
any retirement decision. Interrupted alias cleanup is safe to repeat. Incomplete prepublication
`.pending-*` files and unknown aliases are retained; this is not a general
metadata garbage sweep.

## Consequences

Legacy and newly materialized configuration use the same archival path. Active work and source dirt
remain protected. Full scoped safety reads and a bounded full descriptor scan
cost more than ordinary ignored-file classification, so the configured caller
uses bounded candidate batches and cooldown rather than per-controller-tick
archive attempts.

Tests cover actual materializers, content-changing squash merges, closed/open
owners, complete and failed borrow scopes, private byte restoration, namespace
recreation, late artifact writes and changed quarantine contents. Mutation tests
must demonstrate that source-change verification rejects real changed bytes.
