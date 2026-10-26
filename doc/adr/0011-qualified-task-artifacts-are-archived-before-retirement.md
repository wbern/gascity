---
status: accepted
date: 2026-10-25
applies_to:
  - "cmd/gc/worktree_archive*.go"
  - "cmd/gc/cmd_worktree_archive.go"
  - "cmd/gc/bead_worktree_reaper*.go"
  - "internal/worktree/archive*.go"
  - "internal/worktree/retirement_lock*.go"
pre_filter:
  - "Archive"
  - "archive"
  - "collectReapBorrowScope"
  - "reapOwnerLive"
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
