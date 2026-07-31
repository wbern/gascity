package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	convoycore "github.com/gastownhall/gascity/internal/convoy"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/git"
	"github.com/gastownhall/gascity/internal/pathutil"
	"github.com/gastownhall/gascity/internal/sling"
)

// reapDecision records one worktree the reaper acted on or declined to act on,
// for the dry-run report and event stream.
type reapDecision struct {
	BeadID string
	Path   string
	Rig    string
	Branch string
	// Reason explains a protected decision (why the worktree was left in
	// place). Empty for a reap/would-reap decision.
	Reason string
	// HoldsUnlandedWork marks the one protection whose subject is
	// irreplaceable: commits whose content is carried by no remote. Every
	// other reason describes a tree that can be rebuilt from the remote, so
	// this is the only one whose accumulation silently risks losing work.
	HoldsUnlandedWork bool
	// Warning records a non-blocking observation about a reaped worktree —
	// a signal worth an operator's attention that does not endanger the
	// removal. Empty when there is nothing to note.
	Warning string
}

// reapReport is the outcome of one reapClosedBeadWorktrees pass. Reaped holds
// the worktrees removed (or, in dry-run, the ones that would be removed);
// Protected holds worktrees left in place with the reason (too young/quarantined,
// referenced by a non-terminal bead in another molecule, live process, active
// session, unsafe git state, or an indeterminate age/liveness/borrow-veto scan).
type reapReport struct {
	Reaped    []reapDecision
	Protected []reapDecision
	DryRun    bool
}

// reapClosedBeadWorktrees discovers per-bead git worktrees under
// cityPath/.gc/worktrees/<rig>/ and removes any whose associated bead is closed
// and that pass every safety gate. It returns a reapReport describing what was
// reaped and what was protected.
//
// Discovery is authoritative at any nesting depth: for each rig it runs
// `git worktree list --porcelain` from the rig's own repository (the repo that
// owns these worktrees, per worktree-setup.sh's `git -C <rig> worktree add`),
// rather than a single-level directory scan. Per-bead worktrees are nested
// under agent-home directories (depth-2, sometimes deeper); the old
// os.ReadDir(.gc/worktrees/<rig>/) scan saw only the agent homes and reaped
// nothing (gastownhall/gascity#4492 root cause A).
//
// Safety gates, in order, all fail closed toward keeping the worktree:
//  1. Named agent-home directories are never removed.
//  2. The bead named by the worktree must exist and be closed.
//  3. Freshness quarantine: a worktree younger than
//     cfg.Daemon.AutoReapClosedBeadWorktreesMinAge is exempt, protecting
//     against the race between worktree creation and its owning bead's
//     work-dir metadata being stamped by the next reconcile pass. An
//     indeterminate age (the ".git" pointer file cannot be stat'd) protects.
//  4. Borrow-veto scan: batched once per rig per tick, this finds any
//     non-terminal bead — in any molecule — whose gc.work_dir/work_dir
//     metadata still points at the worktree's path and protects it if so.
//     A query error protects every remaining candidate in that rig's tick.
//  5. Liveness: no live process cwd and no active-session working directory may
//     sit at or beneath the worktree. If the liveness scan is indeterminate
//     (no /proc), NOTHING is reaped this pass — the reaper cannot prove any
//     tree is idle (root cause B: closed-bead != end-of-use).
//  6. Git state: no uncommitted changes and no unpushed commits — the two
//     forms of work that live inside this worktree and that its removal would
//     destroy. Either probe failing protects the tree.
//
// Stashes are deliberately not a gate. refs/stash lives in the repository's
// common git dir, so `git stash list` run inside a linked worktree reports
// every stash in the repository — none of which the worktree owns, and none of
// which its removal can destroy (internal/git's
// TestWorktreeRemove_PreservesStashes proves the property). Treating that
// signal as unsafe state blocked every candidate in any repository that had
// ever stashed: measured live at 0 reaps out of 61 closed-bead candidates
// against 58 repo-wide stashes, on a fleet 90% of whose bead worktrees served
// closed beads. Per-worktree stash detection is not implementable, so a stash
// is recorded as a warning on the decision instead of blocking it.
//
// When dryRun is true the reaper performs all discovery and classification and
// emits bead.worktree.reap_skipped events describing what it would reap and
// what it protected, but removes nothing. liveSessionDirs is the active-session
// working-directory set the liveness gate cross-checks against, alongside the
// authoritative /proc cwd scan.
func reapClosedBeadWorktrees(
	cityPath string,
	cfg *config.City,
	rigBeadStores map[string]beads.Store,
	liveSessionDirs []string,
	dryRun bool,
	rec events.Recorder,
	stderr io.Writer,
) reapReport {
	report := reapReport{DryRun: dryRun}
	if stderr == nil {
		stderr = io.Discard
	}
	if rec == nil {
		rec = events.Discard
	}
	if cfg == nil || len(rigBeadStores) == 0 {
		return report
	}

	// Build a guard set of session home names so agent template directories
	// are never touched.
	sessionHomes := make(map[string]bool, len(cfg.Agents))
	for i := range cfg.Agents {
		if name := cfg.Agents[i].BindingQualifiedName(); name != "" {
			sessionHomes[name] = true
		}
	}

	// Authoritative liveness signal, gathered once for the whole pass. When the
	// scan is indeterminate the reaper protects every candidate (fail closed).
	live := collectLiveWorktreeStateFn()
	if live.scanned && live.source != "" && live.source != liveScanSourceProc {
		// Name the mechanism when it is not the primary one, so a reap decision
		// made on a fallback scan is not indistinguishable from one made on
		// /proc.
		fmt.Fprintf(stderr, "reapClosedBeadWorktrees: liveness scanned via %s (/proc unavailable)\n", live.source) //nolint:errcheck
	}

	// Removal throttle, resolved once and applied across all rigs: the cap
	// bounds a whole pass, not each rig, so a single tick's total git I/O is
	// what stays bounded.
	// Load guard: yield the whole pass while the host is busy, so reclamation
	// never competes with real work. Skipping costs nothing — the next tick
	// reconsiders every candidate — which is why an UNREADABLE load PROCEEDS
	// rather than throttling. A load probe that fails closed would disable the
	// reaper on any host it cannot measure, which is the failure mode this
	// feature is meant to avoid rather than reproduce.
	if pct := cfg.Daemon.AutoReapClosedBeadWorktreesMaxLoadPercent(); pct > 0 {
		if load, err := reapLoadAverageFn(); err == nil {
			ceiling := float64(runtime.NumCPU()) * float64(pct) / 100
			if load > ceiling {
				fmt.Fprintf(stderr, //nolint:errcheck
					"reapClosedBeadWorktrees: skipping pass, load %.2f exceeds %d%% of %d CPUs (%.2f)\n",
					load, pct, runtime.NumCPU(), ceiling,
				)
				return report
			}
		}
	}

	maxPerPass := cfg.Daemon.AutoReapClosedBeadWorktreesMaxPerPass()
	pace := cfg.Daemon.AutoReapClosedBeadWorktreesPace()
	removedThisPass := 0

	wtRoot := filepath.Join(cityPath, ".gc", "worktrees")

	for rigName, store := range rigBeadStores {
		if store == nil {
			continue
		}
		rigRoot := rigRootByName(cfg, rigName)
		if rigRoot == "" {
			// No configured filesystem path for this rig — cannot resolve the
			// owning repository, so we cannot safely enumerate or remove.
			continue
		}
		rigWorktreeDir := filepath.Join(wtRoot, rigName)

		worktrees, err := git.New(rigRoot).WorktreeList()
		if err != nil {
			fmt.Fprintf(stderr, "reapClosedBeadWorktrees: listing worktrees for rig %s (%s): %v\n", rigName, rigRoot, err) //nolint:errcheck
			continue
		}

		// Pass 1: discover reap-eligible candidates — closed bead, and old
		// enough to be past the freshness quarantine (FR-5). Every other gate
		// (borrow-veto, liveness, git safety) is deferred to pass 2 so the
		// borrow-veto scan below can run as a single batched query per rig
		// (FR-3) instead of once per worktree.
		var candidates []reapCandidate
		for _, wt := range worktrees {
			worktreePath := wt.Path

			// Only per-bead worktrees under this rig's .gc/worktrees/<rig>/
			// subtree are in scope. This excludes the rig's main working tree
			// and any worktree checked out elsewhere.
			if !pathutil.PathWithin(rigWorktreeDir, worktreePath) || pathutil.SamePath(rigWorktreeDir, worktreePath) {
				continue
			}
			// Defense in depth: never act on a path that is not strictly under
			// the city worktree root.
			if !isStrictlyUnderDir(wtRoot, worktreePath) {
				continue
			}

			base := filepath.Base(worktreePath)

			// Session home guard: never touch agent template directories.
			if sessionHomes[base] {
				continue
			}

			// Extract a bead ID candidate from the worktree's leaf name.
			beadID := extractBeadIDFromWorktreeName(cfg, base)
			if beadID == "" {
				continue
			}

			// Confirm the bead exists and is closed in this rig's store.
			bead, err := store.Get(beadID)
			if err != nil || bead.Status != "closed" {
				// ErrNotFound, transient error, or bead not yet closed — skip.
				continue
			}

			// Freshness quarantine (FR-5): a worktree younger than the
			// configured minimum age is exempt from reaping, protecting
			// against the race between worktree creation and its owning
			// bead's work-dir metadata being stamped by the next reconcile
			// pass. Age is fail-closed — an indeterminate age protects.
			minAge := cfg.Daemon.AutoReapClosedBeadWorktreesMinAge()
			age, ok := computeWorktreeAge(worktreePath)
			reason := ""
			switch {
			case !ok:
				reason = "worktree age indeterminate (failing closed)"
			case minAge > 0 && age < minAge:
				reason = fmt.Sprintf("worktree too young to reap (quarantine): age=%s min_age=%s", age.Round(time.Second), minAge)
			}
			if reason != "" {
				branch, _ := git.New(worktreePath).CurrentBranch()
				fmt.Fprintf(stderr, //nolint:errcheck
					"reapClosedBeadWorktrees: protecting %s (bead %s closed but %s)\n",
					worktreePath, beadID, reason,
				)
				recordReapSkipped(rec, beadID, worktreePath, rigName, reason)
				report.Protected = append(report.Protected, reapDecision{
					BeadID: beadID, Path: worktreePath, Rig: rigName, Branch: branch, Reason: reason,
				})
				continue
			}

			candidates = append(candidates, reapCandidate{beadID: beadID, worktreePath: worktreePath})
		}

		if len(candidates) == 0 {
			continue
		}

		// Borrow-veto scan (FR-1/FR-2/FR-3): one batched query for every
		// surviving candidate in this rig instead of one query per candidate.
		// A query error fails closed — every remaining candidate in this
		// rig's tick is protected (NFR-1).
		referencingBeads, listErr := scanBorrowVetoReferences(store, candidates)
		if listErr != nil {
			reason := fmt.Sprintf("borrow-veto scan failed (failing closed): %v", listErr)
			for _, c := range candidates {
				branch, _ := git.New(c.worktreePath).CurrentBranch()
				fmt.Fprintf(stderr, //nolint:errcheck
					"reapClosedBeadWorktrees: protecting %s (bead %s closed but %s)\n",
					c.worktreePath, c.beadID, reason,
				)
				recordReapSkipped(rec, c.beadID, c.worktreePath, rigName, reason)
				report.Protected = append(report.Protected, reapDecision{
					BeadID: c.beadID, Path: c.worktreePath, Rig: rigName, Branch: branch, Reason: reason,
				})
			}
			continue
		}

		// Pass 2: apply the borrow-veto verdict, then the existing
		// liveness/git-safety gates, to each surviving candidate.
		for _, c := range candidates {
			worktreePath := c.worktreePath
			beadID := c.beadID

			// Borrow-veto (FR-1/FR-2/FR-7): protect when any non-terminal
			// bead — regardless of molecule — still references this path via
			// work-dir metadata.
			reason := ""
			if refs := referencingBeads[worktreePath]; len(refs) > 0 {
				reason = fmt.Sprintf("borrow-veto: referenced by non-terminal bead(s) %s", strings.Join(refs, ", "))
			}

			// Liveness gate (fail closed). Protect the tree when a live process
			// or active session is working in it, or when liveness could not be
			// determined at all.
			if reason == "" {
				switch {
				case !live.scanned:
					reason = "liveness scan unavailable (failing closed, protecting all)"
				default:
					if isLive, why := worktreeIsLive(worktreePath, live, liveSessionDirs); isLive {
						reason = "live: " + why
					}
				}
			}

			// Git safety gates, only if not already protected. A worktree is
			// unsafe to remove when it holds work removal would destroy:
			// uncommitted changes in its own working files and index, or
			// commits whose content is carried nowhere else. Both fail closed —
			// an unreadable repository is never assumed clean.
			//
			// The commit gate is patch-equivalence, not plain reachability:
			// squash- and rebase-merges rewrite the SHA, so an integrated
			// commit is unreachable from every remote forever and reachability
			// alone would hold the worktree permanently.
			//
			// Stashes are deliberately NOT a gate; they are recorded as a
			// warning below. See the stash note on this function.
			wg := newGitProbe(worktreePath)
			holdsUnlandedWork := false
			if reason == "" {
				hasUncommitted := wg.HasUncommittedWork()
				hasUnlanded, unlandedErr := wg.HasUnlandedCommitsResult()
				switch {
				case unlandedErr != nil:
					reason = fmt.Sprintf("unsafe git state: unlanded probe failed: %v", unlandedErr)
				case hasUncommitted || hasUnlanded:
					reason = fmt.Sprintf("unsafe git state: uncommitted=%v unlanded=%v", hasUncommitted, hasUnlanded)
					holdsUnlandedWork = hasUnlanded
				}
			}

			// Repo-wide stash observation. Not a gate (removal cannot destroy a
			// stash), but worth an operator's attention, so it rides along with
			// the decision and is logged. A failed probe is likewise only a
			// warning: a signal that could not endanger this worktree even when
			// readable cannot endanger it when unreadable.
			warning := ""
			if reason == "" {
				switch hasStashes, stashErr := wg.HasStashesResult(); {
				case stashErr != nil:
					warning = fmt.Sprintf("repo-wide stash probe failed: %v", stashErr)
				case hasStashes:
					warning = "repository holds stashed work (repo-wide; not owned by this worktree and not destroyed by its removal)"
				}
			}

			branch, _ := wg.CurrentBranch()

			if reason != "" {
				fmt.Fprintf(stderr, //nolint:errcheck
					"reapClosedBeadWorktrees: protecting %s (bead %s closed but %s)\n",
					worktreePath, beadID, reason,
				)
				recordReapSkipped(rec, beadID, worktreePath, rigName, reason)
				report.Protected = append(report.Protected, reapDecision{
					BeadID: beadID, Path: worktreePath, Rig: rigName, Branch: branch,
					Reason: reason, HoldsUnlandedWork: holdsUnlandedWork,
				})
				continue
			}

			// Per-pass cap. Removals are paced and bounded so one tick cannot
			// issue hundreds of `git worktree remove` calls back to back; the
			// remainder is reported as deferred and reconsidered next pass
			// rather than silently dropped, because "kept" must not read as
			// "unsafe" when it means "not yet". Dry-run is exempt: it removes
			// nothing, and capping it would understate the backlog an operator
			// is running the dry-run to size.
			if !dryRun && maxPerPass > 0 && removedThisPass >= maxPerPass {
				deferred := fmt.Sprintf("deferred: per-pass reap cap of %d reached", maxPerPass)
				fmt.Fprintf(stderr, //nolint:errcheck
					"reapClosedBeadWorktrees: %s, deferring %s (bead %s) to a later pass\n",
					deferred, worktreePath, beadID,
				)
				recordReapSkipped(rec, beadID, worktreePath, rigName, deferred)
				report.Protected = append(report.Protected, reapDecision{
					BeadID: beadID, Path: worktreePath, Rig: rigName, Branch: branch, Reason: deferred,
				})
				continue
			}

			if warning != "" {
				fmt.Fprintf(stderr, //nolint:errcheck
					"reapClosedBeadWorktrees: %s (bead %s): warning: %s\n",
					worktreePath, beadID, warning,
				)
			}

			if dryRun {
				const whatIf = "dry-run: would reap (closed bead, clean tree, no live process)"
				fmt.Fprintf(stderr, //nolint:errcheck
					"reapClosedBeadWorktrees: %s: %s for closed bead %s\n",
					whatIf, worktreePath, beadID,
				)
				recordReapSkipped(rec, beadID, worktreePath, rigName, whatIf)
				report.Reaped = append(report.Reaped, reapDecision{
					BeadID: beadID, Path: worktreePath, Rig: rigName, Branch: branch, Warning: warning,
				})
				continue
			}

			// Remove the worktree from the OWNING rig repository. git worktree
			// remove must be run from the main repo root, not from within the
			// worktree being removed.
			if err := git.New(rigRoot).WorktreeRemove(worktreePath, false); err != nil {
				fmt.Fprintf(stderr, "reapClosedBeadWorktrees: removing %s: %v\n", worktreePath, err) //nolint:errcheck
				continue
			}
			fmt.Fprintf(stderr, //nolint:errcheck
				"reapClosedBeadWorktrees: removed worktree %s for closed bead %s\n",
				worktreePath, beadID,
			)
			if raw, err := json.Marshal(events.BeadWorktreeReapedPayload{
				BeadID: beadID,
				Path:   worktreePath,
				Rig:    rigName,
				Branch: branch,
			}); err == nil {
				rec.Record(events.Event{
					Type:    events.BeadWorktreeReaped,
					Actor:   "gc",
					Subject: beadID,
					Payload: raw,
				})
			}
			report.Reaped = append(report.Reaped, reapDecision{
				BeadID: beadID, Path: worktreePath, Rig: rigName, Branch: branch, Warning: warning,
			})
			removedThisPass++
			if pace > 0 {
				reapPaceFn(pace)
			}
		}
	}
	return report
}

// reapLoadAverageFn is the host load probe the load guard reads, indirected so
// tests can drive the threshold without loading the machine.
var reapLoadAverageFn = oneMinuteLoadAverage

// reapPaceFn is the pause taken after each removal, indirected through a var so
// tests can assert the pacing is wired without spending real time on it.
var reapPaceFn = time.Sleep

// reapCandidate is a worktree that survived the closed-bead check and the
// freshness quarantine in pass 1, awaiting the batched borrow-veto scan and
// the remaining safety gates in pass 2.
type reapCandidate struct {
	beadID       string
	worktreePath string
}

// computeWorktreeAge returns how long ago worktreePath was created, using the
// mtime of its ".git" pointer file (written once by `git worktree add` and not
// rewritten during normal use) as a creation-time proxy. Worktree structs carry
// no timestamp of their own. ok is false when the file cannot be stat'd, so the
// caller can fail closed instead of treating an indeterminate age as zero.
func computeWorktreeAge(worktreePath string) (age time.Duration, ok bool) {
	info, err := os.Stat(filepath.Join(worktreePath, ".git"))
	if err != nil {
		return 0, false
	}
	return time.Since(info.ModTime()), true
}

// scanBorrowVetoReferences issues one batched beads.Store.List query and
// returns, for each candidate's worktree path, the IDs of any non-terminal
// beads — in any molecule — whose gc.work_dir or legacy work_dir metadata
// still points at that path (FR-1/FR-2/FR-3). Terminal status is decided by
// convoycore.IsTerminalStatus, not a bare "!= closed" check, so a tombstoned
// reference does not veto. Path matching is symlink/alias-normalized on both
// sides via pathutil.NormalizePathForCompare, matching the liveness gate, so a
// metadata path recorded in a different-but-equivalent form still vetoes.
// A query error is returned as-is; the caller must fail closed and protect
// every candidate in the rig (NFR-1).
func scanBorrowVetoReferences(store beads.Store, candidates []reapCandidate) (map[string][]string, error) {
	// The query excludes closed beads at the store level (IsTerminalStatus
	// would discard them anyway) and skips label hydration this scan never
	// reads. TierBoth is explicit so the reaper's safety contract does not
	// depend on a wrapping store expanding the default tier for it.
	all, err := store.List(beads.ListQuery{AllowScan: true, SkipLabels: true, TierMode: beads.TierBoth})
	if err != nil {
		return nil, err
	}
	byNorm := make(map[string]string, len(candidates)) // normalized -> raw candidate path
	for _, c := range candidates {
		byNorm[pathutil.NormalizePathForCompare(c.worktreePath)] = c.worktreePath
	}
	refs := make(map[string][]string)
	for _, b := range all {
		if convoycore.IsTerminalStatus(b.Status) {
			continue
		}
		for _, key := range [...]string{beadmeta.WorkDirMetadataKey, beadmeta.LegacyWorkDirMetadataKey} {
			p := strings.TrimSpace(b.Metadata[key])
			if p == "" {
				continue
			}
			if raw, hit := byNorm[pathutil.NormalizePathForCompare(p)]; hit {
				refs[raw] = append(refs[raw], b.ID)
				break
			}
		}
	}
	return refs, nil
}

// recordReapSkipped emits a bead.worktree.reap_skipped event carrying the
// reason a worktree was protected or (in dry-run) flagged as would-reap.
func recordReapSkipped(rec events.Recorder, beadID, path, rig, reason string) {
	raw, err := json.Marshal(events.BeadWorktreeReapSkippedPayload{
		BeadID: beadID,
		Path:   path,
		Rig:    rig,
		Reason: reason,
	})
	if err != nil {
		return
	}
	rec.Record(events.Event{
		Type:    events.BeadWorktreeReapSkipped,
		Actor:   "gc",
		Subject: beadID,
		Payload: raw,
	})
}

// rigRootByName returns the configured filesystem path of the rig with the
// given name, or "" when the rig is unknown or has no path. This is the
// repository that owns the rig's per-bead worktrees.
func rigRootByName(cfg *config.City, rigName string) string {
	if cfg == nil {
		return ""
	}
	for i := range cfg.Rigs {
		if cfg.Rigs[i].Name == rigName {
			return strings.TrimSpace(cfg.Rigs[i].Path)
		}
	}
	return ""
}

// extractBeadIDFromWorktreeName scans consecutive dash-separated segment pairs
// in name for one that LooksLikeConfiguredBeadID. Returns the first match, or
// "" if none. Handles names like "builder-ga-34q3ss-pr2738" → "ga-34q3ss" and
// bare "ga-06kfi6" → "ga-06kfi6".
func extractBeadIDFromWorktreeName(cfg *config.City, name string) string {
	if name == "" || cfg == nil {
		return ""
	}
	parts := strings.Split(name, "-")
	for i := 0; i+1 < len(parts); i++ {
		candidate := parts[i] + "-" + parts[i+1]
		if sling.LooksLikeConfiguredBeadID(cfg, candidate) {
			return candidate
		}
	}
	return ""
}

// isStrictlyUnderDir reports whether path is strictly contained within dir
// (i.e., it is not dir itself and has dir as a prefix component).
//
// Both sides are normalized through pathutil before comparison. A raw
// filepath.Rel is not sufficient: git reports worktree paths fully resolved,
// so on macOS a city under /var/folders/... is reported as
// /private/var/folders/..., and Rel between the two spellings yields a "../"
// path. That made this defense-in-depth check reject every real candidate and
// silently disabled the whole reaper on Darwin.
func isStrictlyUnderDir(dir, path string) bool {
	// Normalize both sides. git worktree list reports canonical paths, while
	// dir is derived from the configured city path, which may still contain a
	// symlinked ancestor (on macOS every $TMPDIR path does, via /var ->
	// private/var). Comparing the two raw forms makes filepath.Rel return a
	// "../.." escape for a worktree that is plainly inside the city, so this
	// defense-in-depth check silently drops every reap candidate. The
	// PathWithin gate directly above already compares normalized.
	dir = pathutil.NormalizePathForCompare(dir)
	path = pathutil.NormalizePathForCompare(path)
	rel, err := filepath.Rel(dir, path)
	if err != nil {
		return false
	}
	return rel != "." && !strings.HasPrefix(rel, "..")
}
