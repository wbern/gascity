package main

import (
	"encoding/json"
	"io"
)

// worktreeJSONSchemaVersion is the version constant both worktree result
// schemas pin. Bump it only alongside the checked-in schemas under
// schemas/worktree/.
const worktreeJSONSchemaVersion = "1"

// worktreeScanJSON is the machine-readable result of `gc worktree scan`.
// Matches schemas/worktree/scan/result.schema.json.
type worktreeScanJSON struct {
	SchemaVersion string              `json:"schema_version"`
	OK            bool                `json:"ok"`
	Strays        []worktreeStrayJSON `json:"strays"`
}

// worktreeStrayJSON is one stray checkout in a scan result.
type worktreeStrayJSON struct {
	Path        string `json:"path"`
	Reclaimable bool   `json:"reclaimable"`
	Reason      string `json:"reason,omitempty"`
	Warning     string `json:"warning,omitempty"`
}

// worktreeReapJSON is the machine-readable result of `gc worktree reap`.
// Matches schemas/worktree/reap/result.schema.json.
type worktreeReapJSON struct {
	SchemaVersion string                 `json:"schema_version"`
	OK            bool                   `json:"ok"`
	DryRun        bool                   `json:"dry_run"`
	Summary       worktreeReapSummary    `json:"summary"`
	Reaped        []worktreeDecisionJSON `json:"reaped"`
	Protected     []worktreeDecisionJSON `json:"protected"`
}

// worktreeReapSummary carries the counts a caller would otherwise have to
// re-derive by parsing the human summary line. HoldingUnlandedWork is broken
// out because it is the only protection class whose accumulation can cost
// something: every other reason describes a tree that reproduces from a remote.
type worktreeReapSummary struct {
	Reaped              int `json:"reaped"`
	Kept                int `json:"kept"`
	HoldingUnlandedWork int `json:"holding_unlanded_work"`
}

// worktreeDecisionJSON is one reap decision.
type worktreeDecisionJSON struct {
	BeadID            string `json:"bead_id"`
	Rig               string `json:"rig"`
	Branch            string `json:"branch,omitempty"`
	Path              string `json:"path"`
	Reason            string `json:"reason,omitempty"`
	Warning           string `json:"warning,omitempty"`
	HoldsUnlandedWork bool   `json:"holds_unlanded_work"`
}

// newWorktreeReapJSON projects a reapReport onto its wire shape.
func newWorktreeReapJSON(report reapReport) worktreeReapJSON {
	return worktreeReapJSON{
		SchemaVersion: worktreeJSONSchemaVersion,
		OK:            true,
		DryRun:        report.DryRun,
		Summary: worktreeReapSummary{
			Reaped:              len(report.Reaped),
			Kept:                len(report.Protected),
			HoldingUnlandedWork: countHoldingUnlandedWork(report.Protected),
		},
		Reaped:    worktreeDecisionsJSON(report.Reaped),
		Protected: worktreeDecisionsJSON(report.Protected),
	}
}

// worktreeDecisionsJSON converts decisions to their wire shape, always
// returning a non-nil slice so the payload carries [] rather than null.
func worktreeDecisionsJSON(decisions []reapDecision) []worktreeDecisionJSON {
	out := make([]worktreeDecisionJSON, 0, len(decisions))
	for _, d := range decisions {
		out = append(out, worktreeDecisionJSON{
			BeadID:            d.BeadID,
			Rig:               d.Rig,
			Branch:            d.Branch,
			Path:              d.Path,
			Reason:            d.Reason,
			Warning:           d.Warning,
			HoldsUnlandedWork: d.HoldsUnlandedWork,
		})
	}
	return out
}

// encodeWorktreeJSON writes an indented payload, matching the other JSON
// commands' output style.
func encodeWorktreeJSON(stdout io.Writer, payload any) error {
	enc := json.NewEncoder(stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(payload)
}
