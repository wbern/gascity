package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/git"
	"github.com/gastownhall/gascity/internal/worktree"
)

// A test-only observation seam for races at the namespace retirement boundary.
// Production has no external callbacks or policy overrides here.
var archiveRetirementCheckpoint = func(_, _, _ string) {}

func executeArchiveReap(city string, cfg *config.City, scope reapStoreScope, liveDirs []string, req worktreeArchiveRequest, rig string, plan *worktree.ArchivePlan, evidence []byte, report worktreeArchiveReport, stderr io.Writer) worktreeArchiveReport {
	// Nothing has been written before this entrypoint. The operator explicitly
	// requested execution with the digest of an already qualified inventory.
	if err := archiveHandlesClear(req.Path); err != nil {
		return archiveHandleFailure(report, err)
	}
	physicalCity, err := normalizeReapBorrowPath(city)
	if err != nil {
		return archiveReportStop(report, "error", "archive_unavailable", err.Error())
	}
	archiveBase := filepath.Join(physicalCity, ".gc", "artifacts")
	if err := archivePrivateDirectory(archiveBase); err != nil {
		return archiveReportStop(report, "error", "archive_unavailable", err.Error())
	}
	requestJSON, err := json.Marshal(req)
	if err != nil {
		return archiveReportStop(report, "error", "request_encoding", err.Error())
	}
	saved, err := worktree.WritePrivateArchive(plan, filepath.Join(archiveBase, "worktree-retirement"), evidence, requestJSON, containerFreeBytes)
	if err != nil {
		return archiveReportStop(report, "protected", "archive_failed", err.Error())
	}
	report.ArchivePath = saved.Path
	report.ManifestSHA256 = saved.ManifestSHA256
	quarantine := filepath.Join(saved.Path, "checkout")
	archiveRetirementCheckpoint("before_move", req.Path, quarantine)
	if _, err := archiveSubjectIdentity(cfg, req, req.Path); err != nil {
		return archiveReportStop(report, "protected", "source_changed", err.Error())
	}
	if err := worktree.VerifyArchiveSource(plan); err != nil {
		return archiveReportStop(report, "protected", "source_changed", err.Error())
	}
	if err := git.NewInspection(rig).WorktreeMove(req.Path, quarantine); err != nil {
		// Git may have moved before post-operation identity verification failed.
		// Retain any private destination and never touch the original namespace.
		report.Complete = false
		if _, statErr := os.Lstat(quarantine); !os.IsNotExist(statErr) {
			report.QuarantinePath = quarantine
		}
		return archiveReportStop(report, "protected", "quarantine_failed", err.Error())
	}
	report.QuarantinePath = quarantine
	keep := func(code, reason string) worktreeArchiveReport {
		// The original namespace can now belong to another writer. A rollback
		// would race its recreation, so retain the private checkout and archive.
		report.Complete = false
		return archiveReportStop(report, "protected", code, reason)
	}
	if err := plan.RebindRoot(quarantine); err != nil {
		return keep("quarantine_changed", err.Error())
	}
	archiveRetirementCheckpoint("after_move", req.Path, quarantine)
	archiveRetirementCheckpoint("before_remove", req.Path, quarantine)
	// One post-rename descriptor census and one fresh all-store checkpoint.
	// Both occur after the final observation seam; avoid repeating expensive
	// city-wide scans between the same retirement boundaries.
	if err := archiveHandlesClear(quarantine); err != nil {
		failure := archiveHandleFailure(report, err)
		report.Complete = failure.Complete
		return keep(failure.ReasonCode, err.Error())
	}
	complete, reason := archiveSafetyCheck(city, cfg, scope, liveDirs, req, quarantine, stderr)
	report.Complete = complete
	if reason != "" {
		return keep("safety_guard", reason)
	}
	if _, err := archiveSubjectIdentity(cfg, req, quarantine); err != nil {
		return keep("source_changed", err.Error())
	}
	if err := worktree.VerifyArchiveSource(plan); err != nil {
		return keep("source_changed", err.Error())
	}
	status, err := git.NewInspection(quarantine).WorkingTreeStatus()
	if err != nil || len(status.Dirty) != 0 {
		return keep("source_changed", "checkout changed after quarantine")
	}
	patterns := append(append([]string(nil), builtinDisposableIgnored...), cfg.Daemon.AutoReapDisposableIgnored...)
	leaves, err := archiveIgnoredInventory(quarantine, status.Ignored, patterns)
	if err != nil {
		return keep("unsafe_ignored_inventory", err.Error())
	}
	expected := make([]string, 0, len(plan.Entries()))
	for _, entry := range plan.Entries() {
		expected = append(expected, entry.Path)
	}
	if !slices.Equal(leaves, expected) {
		return keep("inventory_changed", "ignored artifact inventory changed after quarantine")
	}
	if err := git.NewInspection(quarantine).VerifyTrackedContent(req.ExpectedHEAD); err != nil {
		return keep("tracked_content_unsafe", err.Error())
	}
	owner, err := reapOwnerLive(scope.RigStores[req.Rig], req.BeadID)
	if err != nil || owner.Status != "closed" || owner.Revision != req.ExpectedBeadRevision {
		return keep("owner_changed", "owner changed before removal")
	}
	if _, err := archiveSubjectIdentity(cfg, req, quarantine); err != nil {
		return keep("source_changed", err.Error())
	}
	// Only the private retired namespace is removed. Original artifact inodes
	// remain linked separately from the immutable archive copy, preserving late
	// writes through preexisting file handles. Noncooperating same-user mutation
	// inside the private quarantine is outside this checkpoint-based protocol.
	if err := git.NewInspection(rig).WorktreeRemove(quarantine, false); err != nil {
		return keep("remove_failed", err.Error())
	}
	report.OK = true
	report.Complete = true
	report.Outcome = "reaped"
	report.QuarantinePath = ""
	return report
}

func archivePrivateDirectory(path string) error {
	parent := filepath.Dir(path)
	physical, err := fsys.ResolveSymlinks(fsys.OSFS{}, parent)
	if err != nil || filepath.Clean(physical) != parent {
		return fmt.Errorf("archive parent must have a stable nonsymlink namespace")
	}
	if err := os.Mkdir(path, 0o700); err != nil && !os.IsExist(err) {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("archive base is not a directory")
	}
	// Existing shared artifacts containers need not be chmodded; the library
	// creates and verifies the dedicated retirement root and every child private.
	return nil
}
