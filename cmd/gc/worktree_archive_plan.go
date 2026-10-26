package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/git"
	"github.com/gastownhall/gascity/internal/worktree"
)

func archiveSHA(data []byte) string { sum := sha256.Sum256(data); return hex.EncodeToString(sum[:]) }

func archiveReportStop(report worktreeArchiveReport, outcome, code, reason string) worktreeArchiveReport {
	report.Outcome = outcome
	report.OK = outcome == "protected"
	report.ReasonCode = code
	report.Reasons = []string{reason}
	return report
}

func archiveArtifactPath(path string) bool {
	if path == "" || filepath.IsAbs(path) || strings.ContainsAny(path, "\\\x00\r\n") || filepath.ToSlash(filepath.Clean(path)) != path {
		return false
	}
	for _, part := range strings.Split(path, "/") {
		if part == "." || part == ".." || strings.EqualFold(part, ".git") {
			return false
		}
		switch strings.ToLower(part) {
		case "src", "source", "scratch", ".scratch":
			return false
		}
	}
	switch strings.ToLower(filepath.Ext(path)) {
	case ".go", ".rs", ".py", ".pyc", ".js", ".jsx", ".ts", ".tsx", ".c", ".cc", ".cpp", ".h", ".sh", ".bash", ".zsh", ".java", ".swift", ".rb", ".php", ".sql", ".patch", ".diff", ".ipynb":
		return false
	}
	return true
}

// archiveIgnoredInventory inspects every ignored leaf, including descendants of
// disposable caches for repository/special-file hazards. Unknown leaves are
// never omitted merely because the caller supplied an approved subset.
func archiveIgnoredInventory(root string, ignored, patterns []string) ([]string, error) {
	var out []string
	seen := make(map[string]bool)
	visited := 0
	var visit func(string, bool) error
	visit = func(rel string, disposable bool) error {
		visited++
		if visited > 250000 {
			return fmt.Errorf("ignored inventory exceeds inspection budget")
		}
		if rel == "" || filepath.IsAbs(rel) || rel == ".." || strings.HasPrefix(rel, "../") {
			return fmt.Errorf("invalid ignored path")
		}
		abs := filepath.Join(root, filepath.FromSlash(rel))
		info, err := os.Lstat(abs)
		if err != nil {
			return err
		}
		if strings.EqualFold(filepath.Base(rel), ".git") {
			return fmt.Errorf("nested repository at %s", rel)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return nil
		}
		if info.IsDir() {
			if looksLikeBareRepository(abs) {
				return fmt.Errorf("nested bare repository at %s", rel)
			}
			disposable = disposable || matchesDisposable(rel, true, patterns)
			children, err := os.ReadDir(abs)
			if err != nil {
				return err
			}
			for _, child := range children {
				if err := visit(rel+"/"+child.Name(), disposable); err != nil {
					return err
				}
			}
			return nil
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("special ignored file at %s", rel)
		}
		if !disposable && !matchesDisposable(rel, false, patterns) && !seen[rel] {
			out = append(out, rel)
			seen[rel] = true
		}
		return nil
	}
	for _, name := range ignored {
		if err := visit(strings.TrimSuffix(filepath.ToSlash(name), "/"), false); err != nil {
			return out, err
		}
	}
	sort.Strings(out)
	return out, nil
}

func archiveSubjectIdentity(cfg *config.City, req worktreeArchiveRequest, current string) (string, error) {
	rig := rigRootByName(cfg, req.Rig)
	if rig == "" {
		return "", fmt.Errorf("rig is not bound")
	}
	boundRoot, err := normalizeReapBorrowPath(rig)
	if err != nil || boundRoot == current {
		return "", fmt.Errorf("subject is the configured rig checkout")
	}
	g := git.NewInspection(current)
	common, err := g.CommonDir()
	if err != nil {
		return "", err
	}
	rigCommon, err := git.NewInspection(rig).CommonDir()
	if err != nil {
		return "", err
	}
	common, err = normalizeReapBorrowPath(common)
	if err != nil {
		return "", err
	}
	rigCommon, err = normalizeReapBorrowPath(rigCommon)
	if err != nil || common != rigCommon {
		return "", fmt.Errorf("worktree belongs to another repository")
	}
	if _, err := os.Lstat(filepath.Join(common, "info", "grafts")); err == nil || !os.IsNotExist(err) {
		return "", fmt.Errorf("legacy grafts present or unreadable")
	}
	root, err := g.TopLevel()
	if err != nil {
		return "", err
	}
	root, err = normalizeReapBorrowPath(root)
	if err != nil || root != current {
		return "", fmt.Errorf("subject is not the exact worktree root")
	}
	registered, err := git.NewInspection(rig).WorktreeList()
	if err != nil {
		return "", err
	}
	found := false
	for _, wt := range registered {
		p, err := normalizeReapBorrowPath(wt.Path)
		if err == nil && p == current {
			found = true
			break
		}
	}
	if !found {
		return "", fmt.Errorf("subject is not a registered rig worktree")
	}
	head, err := g.RevParseVerifyCommit("HEAD")
	if err != nil || head != req.ExpectedHEAD {
		return "", fmt.Errorf("worktree HEAD changed")
	}
	branch, err := g.HeadSymbolicRef()
	if err != nil || branch != req.ExpectedBranch {
		return "", fmt.Errorf("worktree branch changed")
	}
	admin, err := g.GitDir()
	if err != nil {
		return "", err
	}
	admin, err = normalizeReapBorrowPath(admin)
	if err != nil || admin != req.ExpectedGitDir {
		return "", fmt.Errorf("private git directory changed")
	}
	_, info, err := fsys.ReadRegularFileStableBounded(fsys.OSFS{}, filepath.Join(current, ".git"), 16<<10)
	if err != nil {
		return "", err
	}
	device, inode, ok := fsys.FileIdentity(info)
	if !ok || device != req.ExpectedPointerDevice || inode != req.ExpectedPointerInode {
		return "", fmt.Errorf("worktree pointer identity changed")
	}
	back, _, err := fsys.ReadRegularFileStableBounded(fsys.OSFS{}, filepath.Join(admin, "gitdir"), 16<<10)
	if err != nil {
		return "", err
	}
	backPath := strings.TrimSpace(string(back))
	if !filepath.IsAbs(backPath) {
		backPath = filepath.Join(admin, backPath)
	}
	backPath, err = normalizeReapBorrowPath(backPath)
	if err != nil || backPath != filepath.Join(current, ".git") {
		return "", fmt.Errorf("worktree backlink mismatch")
	}
	if _, err := os.Lstat(filepath.Join(admin, "locked")); err == nil || !os.IsNotExist(err) {
		return "", fmt.Errorf("worktree is locked or lock state unreadable")
	}
	production, err := g.RevParseVerifyCommit(req.ProductionRef)
	if err != nil || production != req.ExpectedProductionHEAD {
		return "", fmt.Errorf("production ref changed")
	}
	ancestor, err := g.IsAncestor(req.IntegratedCommit, production)
	if err != nil || !ancestor {
		return "", fmt.Errorf("integration commit is not on pinned production history")
	}
	return rig, nil
}

func archiveSafetyCheck(city string, cfg *config.City, scope reapStoreScope, liveDirs []string, req worktreeArchiveRequest, current string, stderr io.Writer) (bool, string) {
	refs, incomplete := collectReapBorrowScope(city, cfg, scope, stderr)
	if len(incomplete) > 0 {
		return false, "incomplete borrow scopes: " + strings.Join(incomplete, ", ")
	}
	for _, path := range []string{req.Path, current} {
		borrowers, err := reapBorrowersUnder(refs, path)
		if err != nil {
			return false, err.Error()
		}
		if len(borrowers) > 0 {
			return true, "borrow-veto: " + strings.Join(borrowers, ", ")
		}
	}
	owner, err := reapOwnerLive(scope.RigStores[req.Rig], req.BeadID)
	if err != nil {
		return false, "owner read failed: " + err.Error()
	}
	if owner.Status != "closed" || owner.Revision != req.ExpectedBeadRevision {
		return true, "owner is not closed at the expected revision"
	}
	if owner.Metadata["gc.user_paused"] == "true" {
		return true, "owner is user-paused"
	}
	bound, err := normalizeReapBorrowPath(owner.Metadata[beadmeta.TaskWorktreeMetadataKey])
	if err != nil || bound != req.Path {
		return true, "owner task-worktree binding mismatch"
	}
	if strings.TrimPrefix(req.ExpectedBranch, "refs/heads/") != owner.Metadata[beadmeta.WorkBranchMetadataKey] {
		return true, "owner branch binding mismatch"
	}
	live := collectLiveWorktreeStateFn()
	if !live.scanned {
		return false, "liveness scan unavailable"
	}
	for _, path := range []string{req.Path, current} {
		if yes, reason := worktreeIsLive(path, live, nil); yes {
			return true, reason
		}
		for _, dir := range liveDirs {
			p, err := normalizeReapBorrowPath(dir)
			if err != nil {
				return false, "session work-dir unresolved"
			}
			if pathAtOrUnder(path, p) {
				return true, "active session directory"
			}
		}
	}
	g := git.NewInspection(current)
	state, err := g.WorkingTreeStatus()
	if err != nil {
		return false, err.Error()
	}
	if len(state.Dirty) > 0 {
		return true, "uncommitted tracked or untracked work"
	}
	unlanded, err := g.HasUnlandedCommitsResult()
	if err != nil {
		return false, err.Error()
	}
	unpreserved, err := g.HasUnpreservedCommitsResult()
	if err != nil {
		return false, err.Error()
	}
	if unlanded && unpreserved {
		return true, "unpreserved commits"
	}
	return true, ""
}

func archiveReapWorktree(city string, cfg *config.City, scope reapStoreScope, liveDirs []string, req worktreeArchiveRequest, execute bool, stderr io.Writer) worktreeArchiveReport {
	report := worktreeArchiveReport{SchemaVersion: worktreeArchiveSchemaVersion, Artifacts: []worktreeArchiveArtifact{}}
	if stderr == nil {
		stderr = io.Discard
	}
	if cfg == nil || req.SchemaVersion != "1" || req.Rig == "" || req.BeadID == "" || req.ExpectedHEAD == "" || !strings.HasPrefix(req.ExpectedBranch, "refs/heads/") || !strings.HasPrefix(req.ProductionRef, "refs/") || strings.HasPrefix(req.ProductionRef, "refs/replace/") || req.ExpectedProductionHEAD == "" || req.IntegratedCommit == "" || req.EvidenceSHA256 == "" || req.ExpectedPointerInode == 0 {
		return archiveReportStop(report, "refused", "invalid_request", "missing or unsupported source expectation")
	}
	if !archiveObjectID(req.ExpectedHEAD) || !archiveObjectID(req.ExpectedProductionHEAD) || !archiveObjectID(req.IntegratedCommit) || !archiveFullRef(req.ProductionRef) || !archiveFullRef(req.ExpectedBranch) || len(req.EvidenceSHA256) != 64 || strings.ContainsAny(req.Path, "\x00\r\n\t") {
		return archiveReportStop(report, "refused", "invalid_request", "invalid object, ref or path expectation")
	}
	var err error
	req.Path, err = normalizeReapBorrowPath(req.Path)
	if err != nil {
		return archiveReportStop(report, "refused", "invalid_path", err.Error())
	}
	req.ExpectedGitDir, err = normalizeReapBorrowPath(req.ExpectedGitDir)
	if err != nil {
		return archiveReportStop(report, "refused", "invalid_git_dir", err.Error())
	}
	rigTree, err := normalizeReapBorrowPath(filepath.Join(city, ".gc", "worktrees", req.Rig))
	if err != nil {
		return archiveReportStop(report, "refused", "invalid_city_path", err.Error())
	}
	if !archiveManagedTaskPath(rigTree, req.Path, req.BeadID) {
		return archiveReportStop(report, "refused", "ownership_mismatch", "not an exact managed task for this bead")
	}
	if execute {
		rigRoot := rigRootByName(cfg, req.Rig)
		if rigRoot == "" {
			return archiveReportStop(report, "refused", "unknown_rig", "rig is not bound")
		}
		unlock, lockErr := worktree.TryLockRetirement(rigRoot, req.Path)
		if lockErr != nil {
			return archiveReportStop(report, "protected", "workspace_locked", lockErr.Error())
		}
		defer unlock()
	}
	rig, err := archiveSubjectIdentity(cfg, req, req.Path)
	if err != nil {
		return archiveReportStop(report, "refused", "source_changed", err.Error())
	}
	age, known := computeWorktreeAge(req.Path)
	if !known || age < cfg.Daemon.AutoReapClosedBeadWorktreesMinAge() {
		return archiveReportStop(report, "protected", "freshness", "worktree freshness quarantine")
	}
	complete, reason := archiveSafetyCheck(city, cfg, scope, liveDirs, req, req.Path, stderr)
	report.Complete = complete
	if reason != "" {
		return archiveReportStop(report, "protected", "safety_guard", reason)
	}
	evidence, _, err := fsys.ReadRegularFileStableBounded(fsys.OSFS{}, req.EvidencePath, 64<<10)
	if err != nil || !json.Valid(evidence) || archiveSHA(evidence) != req.EvidenceSHA256 {
		return archiveReportStop(report, "refused", "evidence_changed", "completion evidence unreadable or digest mismatch")
	}
	status, err := git.NewInspection(req.Path).WorkingTreeStatus()
	if err != nil {
		return archiveReportStop(report, "error", "status_failed", err.Error())
	}
	patterns := append(append([]string(nil), builtinDisposableIgnored...), cfg.Daemon.AutoReapDisposableIgnored...)
	leaves, err := archiveIgnoredInventory(req.Path, status.Ignored, patterns)
	if err != nil {
		return archiveReportStop(report, "protected", "unsafe_ignored_inventory", err.Error())
	}
	allowed := make(map[string]bool, len(req.ArtifactPaths))
	for _, path := range req.ArtifactPaths {
		if !archiveArtifactPath(path) || allowed[path] {
			return archiveReportStop(report, "refused", "invalid_artifact_policy", "invalid, duplicate or source/scratch artifact path")
		}
		allowed[path] = true
	}
	for _, path := range leaves {
		report.Artifacts = append(report.Artifacts, worktreeArchiveArtifact{Path: path})
	}
	for _, path := range leaves {
		if !archiveArtifactPath(path) || !allowed[path] {
			return archiveReportStop(report, "protected", "unapproved_artifact", "unapproved ignored leaf: "+path)
		}
	}
	if err := archiveHandlesClear(req.Path); err != nil {
		return archiveHandleFailure(report, err)
	}
	if err := git.NewInspection(req.Path).VerifyTrackedContent(req.ExpectedHEAD); err != nil {
		return archiveReportStop(report, "protected", "tracked_content_unsafe", err.Error())
	}
	plan, err := worktree.PrepareArchive(req.Path, leaves, defaultWorktreeArchiveBounds())
	if err != nil {
		return archiveReportStop(report, "protected", "artifact_snapshot_failed", err.Error())
	}
	defer plan.Close() //nolint:errcheck // read-only source descriptor
	report.InventorySHA256 = plan.Digest()
	report.Artifacts = []worktreeArchiveArtifact{}
	for _, entry := range plan.Entries() {
		report.Artifacts = append(report.Artifacts, worktreeArchiveArtifact{Path: entry.Path, SHA256: entry.SHA256, Size: entry.Size, Mode: uint32(entry.Mode)})
	}
	report.Files = len(leaves)
	report.Bytes = plan.TotalBytes()
	if req.ExpectedInventorySHA256 != "" && req.ExpectedInventorySHA256 != plan.Digest() {
		return archiveReportStop(report, "refused", "inventory_changed", "artifact inventory changed since qualification")
	}
	if !execute {
		report.OK = true
		report.Outcome = "would_reap"
		return report
	}
	if req.ExpectedInventorySHA256 == "" {
		return archiveReportStop(report, "refused", "missing_inventory_expectation", "execute requires the qualified plan digest")
	}
	return executeArchiveReap(city, cfg, scope, liveDirs, req, rig, plan, evidence, report, stderr)
}

// Only task namespaces are eligible. Legacy direct children still require the
// fresh authoritative gc.task_worktree and gc.work_branch bindings above.
func archiveManagedTaskPath(rigRoot, path, id string) bool {
	if id == "" || filepath.Base(id) != id || id == "." || id == ".." {
		return false
	}
	relative, err := filepath.Rel(rigRoot, path)
	if err != nil {
		return false
	}
	parts := strings.Split(relative, string(filepath.Separator))
	if len(parts) == 2 && parts[0] == "tasks" {
		return parts[1] == id || strings.HasPrefix(parts[1], id+"-")
	}
	return len(parts) == 1 && parts[0] == id
}

func archiveObjectID(value string) bool {
	if len(value) != 40 && len(value) != 64 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil && strings.ToLower(value) == value
}

func archiveFullRef(value string) bool {
	if !strings.HasPrefix(value, "refs/") || strings.ContainsAny(value, " ~^:?*[\\\x00\r\n\t") || strings.Contains(value, "..") || strings.Contains(value, "@{") || strings.HasSuffix(value, "/") || strings.HasSuffix(value, ".") {
		return false
	}
	for _, part := range strings.Split(value, "/") {
		if part == "" || strings.HasPrefix(part, ".") || strings.HasSuffix(part, ".lock") {
			return false
		}
	}
	return true
}
