package main

import "github.com/gastownhall/gascity/internal/worktree"

const worktreeArchiveSchemaVersion = "1"

// Remote completion qualification belongs to the configured caller. The native
// command fences the exact local subject and preserves its approved artifacts;
// evidence is copied into the archive for audit, not interpreted as authority.
type worktreeArchiveRequest struct {
	SchemaVersion           string   `json:"schema_version"`
	Rig                     string   `json:"rig"`
	BeadID                  string   `json:"bead_id"`
	Path                    string   `json:"path"`
	ExpectedHEAD            string   `json:"expected_head"`
	ExpectedBranch          string   `json:"expected_branch"`
	ExpectedBeadRevision    int64    `json:"expected_bead_revision"`
	ExpectedGitDir          string   `json:"expected_git_dir"`
	ExpectedPointerDevice   uint64   `json:"expected_pointer_device"`
	ExpectedPointerInode    uint64   `json:"expected_pointer_inode"`
	ProductionRef           string   `json:"production_ref"`
	ExpectedProductionHEAD  string   `json:"expected_production_head"`
	IntegratedCommit        string   `json:"integrated_commit"`
	EvidencePath            string   `json:"evidence_path"`
	EvidenceSHA256          string   `json:"evidence_sha256"`
	ArtifactPaths           []string `json:"artifact_paths"`
	ExpectedInventorySHA256 string   `json:"expected_inventory_sha256,omitempty"`
}

type worktreeArchiveArtifact struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
	Mode   uint32 `json:"mode"`
}

type worktreeArchiveReport struct {
	SchemaVersion   string                    `json:"schema_version"`
	OK              bool                      `json:"ok"`
	Complete        bool                      `json:"complete"`
	Outcome         string                    `json:"outcome"`
	ReasonCode      string                    `json:"reason_code,omitempty"`
	Reasons         []string                  `json:"reasons,omitempty"`
	InventorySHA256 string                    `json:"inventory_sha256,omitempty"`
	Artifacts       []worktreeArchiveArtifact `json:"artifacts"`
	ArchivePath     string                    `json:"archive_path,omitempty"`
	ManifestSHA256  string                    `json:"manifest_sha256,omitempty"`
	QuarantinePath  string                    `json:"quarantine_path,omitempty"`
	Files           int                       `json:"files"`
	Bytes           int64                     `json:"bytes"`
}

type worktreeArchiveCapabilities struct {
	SchemaVersion        string `json:"schema_version"`
	OK                   bool   `json:"ok"`
	Operation            string `json:"operation"`
	RequestSchemaVersion string `json:"request_schema_version"`
	ReportSchemaVersion  string `json:"report_schema_version"`
	PlanNoMutation       bool   `json:"plan_no_mutation"`
}

func defaultWorktreeArchiveBounds() worktree.ArchiveBounds {
	return worktree.ArchiveBounds{MaxFiles: 128, MaxFileBytes: 8 << 20, MaxTotalBytes: 32 << 20, MinFreeBytes: 1 << 30}
}
