package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/contract"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/git"
	"github.com/gastownhall/gascity/internal/pathutil"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// gitProbe is the slice of internal/git.Git used by the worker-dir
// auto-prune path. Defined as an interface so tests can inject a fake
// without standing up real git worktrees.
type gitProbe interface {
	IsRepo() bool
	CurrentBranch() (string, error)
	HasUncommittedWork() bool
	HasUnpushedCommitsResult() (bool, error)
	HasStashesResult() (bool, error)
	WorktreeRemove(path string, force bool) error
}

// newGitProbe returns a gitProbe scoped to the given directory. Indirected
// through a package-level var so tests can stub the git invocations.
var newGitProbe = func(workDir string) gitProbe { return git.New(workDir) }

// resolveWorkerDirForPrune applies the eligibility gates that precede any git
// safety probe and returns the probe to continue with. When the worker_dir is
// not eligible it returns a non-empty reason instead, so no skip is silent:
// asked why the reconciler prunes nothing, an operator can read the answer
// rather than infer it from the absence of output.
//
// Shared by both entry points below. They differ only in how they read the
// worker_dir and resolve the rig root; every gate here is identical, and a
// safety gate that exists in one copy but not the other is precisely the bug
// this shape prevents.
//
// Config-disabled is deliberately not handled here: that is a standing operator
// choice, not an anomaly, and announcing it on every session close would be
// noise.
func resolveWorkerDirForPrune(workerDir, cityPath string) (gp gitProbe, skip string) {
	if workerDir == "" {
		return nil, "session has no worker_dir metadata"
	}
	if !filepath.IsAbs(workerDir) {
		return nil, "worker_dir is not an absolute path"
	}

	wtRoot := filepath.Join(cityPath, ".gc", "worktrees")
	if !pathutil.PathWithin(wtRoot, workerDir) || pathutil.SamePath(wtRoot, workerDir) {
		return nil, fmt.Sprintf("worker_dir is not nested under the city's worktrees tree %s", wtRoot)
	}

	if _, err := os.Stat(filepath.Join(workerDir, ".git")); err != nil {
		return nil, "worker_dir has no .git pointer (already reclaimed, or never a worktree)"
	}

	gp = newGitProbe(workerDir)
	if !gp.IsRepo() {
		return nil, "worker_dir is not a readable git repo"
	}
	return gp, ""
}

// workerDirGitStateSkip applies the git-state safety gates to a prune candidate
// and returns a non-empty skip reason when the worktree holds work its removal
// would destroy: uncommitted changes in its own working files and index, or
// commits on its branch that no remote carries. Both fail closed. It also
// logs — but does not return — a non-blocking warning for anything worth an
// operator's attention that does not make removal unsafe. Neither caller has a
// report to attach it to, so the log line is the surface.
//
// Shared by both entry points so a gate cannot exist in one copy and not the
// other — the hazard that made this file's duplicated chain worth removing.
//
// Stashes are deliberately NOT a gate. refs/stash lives in the repository's
// common git dir, so `git stash list` inside a linked worktree reports every
// stash in the repo, none of which this worktree owns and none of which its
// removal can destroy (internal/git's TestWorktreeRemove_PreservesStashes).
// Gating on it blocked every prune in any repository that had ever stashed —
// crm alone holds 58 — and wrote a ".worktree-stale reason=stashed-work" marker
// that agent_home_worktree_cleanup.go then read as a real signal. Both are gone:
// the stash is a warning, and no marker is written for it.
func workerDirGitStateSkip(gp gitProbe, workerDir string, stderr io.Writer) (skip string) {
	if gp.HasUncommittedWork() {
		fmt.Fprintf(stderr, "session reconciler: not pruning worker_dir %s: has uncommitted changes\n", workerDir) //nolint:errcheck
		writeWorktreeStaleMarker(gp, workerDir, "uncommitted-work", stderr)
		return "has uncommitted changes"
	}
	hasUnpushed, err := gp.HasUnpushedCommitsResult()
	if err != nil {
		fmt.Fprintf(stderr, "session reconciler: not pruning worker_dir %s: unpushed probe failed: %v\n", workerDir, err) //nolint:errcheck
		return "unpushed probe failed"
	}
	if hasUnpushed {
		fmt.Fprintf(stderr, "session reconciler: not pruning worker_dir %s: has unpushed commits\n", workerDir) //nolint:errcheck
		writeWorktreeStaleMarker(gp, workerDir, "unpushed-commits", stderr)
		return "has unpushed commits"
	}
	warning := ""
	if hasStashes, stashErr := gp.HasStashesResult(); stashErr != nil {
		warning = fmt.Sprintf("repo-wide stash probe failed: %v", stashErr)
	} else if hasStashes {
		warning = "repository holds stashed work (repo-wide; not owned by this worktree and not destroyed by its removal)"
	}
	if warning != "" {
		fmt.Fprintf(stderr, "session reconciler: pruning worker_dir %s: warning: %s\n", workerDir, warning) //nolint:errcheck
	}
	return ""
}

// writeWorktreeStaleMarker records why workerDir was left in place instead of
// pruned, so cleanupClosedBeadAgentHomeWorktrees (agent_home_worktree_cleanup.go)
// can later detect when it's safe to reclaim. Best-effort: write failures are
// logged but never alter the caller's control flow.
func writeWorktreeStaleMarker(gp gitProbe, workerDir, reason string, stderr io.Writer) {
	branch, err := gp.CurrentBranch()
	if err != nil {
		branch = ""
	}
	content := fmt.Sprintf("branch=%s\nreason=%s\n", branch, reason)
	if err := os.WriteFile(filepath.Join(workerDir, worktreeStaleFileName), []byte(content), 0o644); err != nil {
		fmt.Fprintf(stderr, "session reconciler: writing %s marker for %s: %v\n", worktreeStaleFileName, workerDir, err) //nolint:errcheck
	}
}

// pruneAgentHomeWorktreeIfSafe removes the worktree at the closed session's
// worker_dir, after applying the same safety gates as doctor's
// NestedWorktreePruneCheck. Returns true when the removal actually
// happened.
//
// The decision is mechanical, never role-coupled: any pool-managed agent
// worktree that lives under the city's .gc/worktrees/ tree, is a git
// worktree, and probes clean is safe to reclaim. Pool sessions are
// transient by design — their worktrees were never meant to outlive the
// session bead.
//
// No-op when:
//   - cfg.Daemon.AutoPruneWorkerDir is false (the only silent skip: a standing
//     operator choice, not an anomaly)
//   - the session bead has no worker_dir metadata
//   - the worker_dir does not live under cityPath/.gc/worktrees/
//   - the worker_dir is missing on disk or has no .git pointer
//   - the worktree has uncommitted changes or unpushed commits (a repo-wide
//     stash is only a warning — see workerDirGitStateSkip)
//   - the rig that owns the session cannot be resolved to a filesystem path
//
// Every skip but the first reports its reason on stderr. They used to return
// silently, which made "the prune never logs" indistinguishable from "the prune
// never ran" without reading this source.
//
// Removal failures are logged but never surfaced — an orphaned worktree
// still shows up via `gc doctor` later, which is the operator's existing
// reclaim path.
func pruneAgentHomeWorktreeIfSafe(session beads.Bead, cityPath string, cfg *config.City, stderr io.Writer) bool {
	if cfg == nil || !cfg.Daemon.AutoPruneWorkerDirEnabled() {
		return false
	}
	workerDir := strings.TrimSpace(contract.WorkerDirFromMetadata(session.Metadata))
	gp, skip := resolveWorkerDirForPrune(workerDir, cityPath)
	if skip != "" {
		fmt.Fprintf(stderr, "session reconciler: not pruning worker_dir %q: %s\n", workerDir, skip) //nolint:errcheck
		return false
	}
	if skip := workerDirGitStateSkip(gp, workerDir, stderr); skip != "" {
		return false
	}

	// Removal is NON-FORCE, matching the closed-bead reaper. The probes above
	// already refuse a worktree with uncommitted or unpushed work, so --force
	// would only ever apply between that check and this call — and in that window
	// git's own refusal ("contains modified or untracked files") is the last thing
	// standing between a race and deleted work. Verified across 309 live
	// worktrees: none was locked, which is the only other case --force covers.
	//
	// Run `git worktree remove` from the rig root rather than from the
	// worktree being removed: git refuses to remove a worktree whose path
	// equals cwd in some configurations, and operating from cwd of a
	// directory we are about to delete is fragile in general.
	rigRoot := lookupRigRootForSession(session, cfg)
	if rigRoot == "" {
		fmt.Fprintf(stderr, "session reconciler: not pruning worker_dir %s: rig path unresolved\n", workerDir) //nolint:errcheck
		return false
	}
	if err := newGitProbe(rigRoot).WorktreeRemove(workerDir, false); err != nil {
		fmt.Fprintf(stderr, "session reconciler: pruning worker_dir %s: %v\n", workerDir, err) //nolint:errcheck
		return false
	}
	fmt.Fprintf(stderr, "session reconciler: pruned worker_dir %s (session %s)\n", workerDir, session.Metadata["session_name"]) //nolint:errcheck
	return true
}

// pruneAgentHomeWorktreeIfSafeInfo is the session.Info form of
// pruneAgentHomeWorktreeIfSafe: the worker_dir read routes through
// session.WorkerDirFromInfo (the canonical→legacy Info fallback equivalent to
// contract.WorkerDirFromMetadata), the rig-root lookup reads Info.Template via
// lookupRigRootForSessionInfo, and the log line reads Info.SessionNameMetadata —
// every safety gate and the removal itself are unchanged. The eligibility gates
// are now literally the same code (resolveWorkerDirForPrune) rather than a
// duplicated copy; the git-state gates below are still parallel prose in both
// forms. The raw form survives for its test callers.
func pruneAgentHomeWorktreeIfSafeInfo(info sessionpkg.Info, cityPath string, cfg *config.City, stderr io.Writer) {
	if cfg == nil || !cfg.Daemon.AutoPruneWorkerDirEnabled() {
		return
	}
	workerDir := strings.TrimSpace(sessionpkg.WorkerDirFromInfo(info))
	gp, skip := resolveWorkerDirForPrune(workerDir, cityPath)
	if skip != "" {
		fmt.Fprintf(stderr, "session reconciler: not pruning worker_dir %q: %s\n", workerDir, skip) //nolint:errcheck
		return
	}
	if skip := workerDirGitStateSkip(gp, workerDir, stderr); skip != "" {
		return
	}

	rigRoot := lookupRigRootForSessionInfo(info, cfg)
	if rigRoot == "" {
		fmt.Fprintf(stderr, "session reconciler: not pruning worker_dir %s: rig path unresolved\n", workerDir) //nolint:errcheck
		return
	}
	if err := newGitProbe(rigRoot).WorktreeRemove(workerDir, false); err != nil {
		fmt.Fprintf(stderr, "session reconciler: pruning worker_dir %s: %v\n", workerDir, err) //nolint:errcheck
		return
	}
	fmt.Fprintf(stderr, "session reconciler: pruned worker_dir %s (session %s)\n", workerDir, info.SessionNameMetadata) //nolint:errcheck
}

// lookupRigRootForSession returns the filesystem path of the rig that owns
// the given session bead, derived from the qualified template metadata
// ("<rig>/<template>"). Returns "" when the rig cannot be identified or
// has no configured path.
func lookupRigRootForSession(session beads.Bead, cfg *config.City) string {
	qt := strings.TrimSpace(session.Metadata["template"])
	slash := strings.IndexByte(qt, '/')
	if slash <= 0 {
		return ""
	}
	rigName := qt[:slash]
	for i := range cfg.Rigs {
		if cfg.Rigs[i].Name == rigName {
			return strings.TrimSpace(cfg.Rigs[i].Path)
		}
	}
	return ""
}

// lookupRigRootForSessionInfo is the session.Info form of
// lookupRigRootForSession: it reads the qualified template off Info.Template (the
// verbatim raw mirror of b.Metadata["template"]), so the rig resolution is
// byte-identical to the raw form.
func lookupRigRootForSessionInfo(info sessionpkg.Info, cfg *config.City) string {
	qt := strings.TrimSpace(info.Template)
	slash := strings.IndexByte(qt, '/')
	if slash <= 0 {
		return ""
	}
	rigName := qt[:slash]
	for i := range cfg.Rigs {
		if cfg.Rigs[i].Name == rigName {
			return strings.TrimSpace(cfg.Rigs[i].Path)
		}
	}
	return ""
}
