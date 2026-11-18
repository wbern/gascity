package workrecord

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/citylayout"
)

// ViolationLogName is the append-only JSONL file, under <city>/.gc/logs/, that
// records every close the gate evaluated as violating the contract. Warn-only
// closes otherwise leave only a stderr/log line that is lost with the process,
// so this file is the durable data for deciding whether to enable enforcement.
const ViolationLogName = "work-record-violations.jsonl"

const (
	// ViolationLogMaxBytes is the size past which the log is rotated once to
	// "<name>.1" (replacing any previous rotation). Total disk use is bounded
	// to about twice this.
	ViolationLogMaxBytes = 4 << 20

	maxLoggedReasons    = 16
	maxLoggedFieldBytes = 512
)

// Door names the plane a close arrived through.
const (
	DoorCLI  = "cli"
	DoorHTTP = "http"
)

type violationRecord struct {
	TS      string   `json:"ts"`
	Door    string   `json:"door"`
	Bead    string   `json:"bead"`
	Outcome string   `json:"gc.work_outcome"`
	Commit  string   `json:"commit"`
	Branch  string   `json:"branch"`
	Mode    string   `json:"mode"`
	Blocked bool     `json:"blocked"`
	Reasons []string `json:"reasons"`
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// ViolationLogPath returns the log path for a city ("" when cityPath is empty).
func ViolationLogPath(cityPath string) string {
	if cityPath == "" {
		return ""
	}
	return citylayout.RuntimePath(cityPath, "logs", ViolationLogName)
}

// RecordViolations appends one JSON line describing a close that violated the
// contract. It is strictly best-effort: it does nothing when there are no
// violations or no city path, swallows every error, and recovers from panics,
// so it can never change a close decision. Each line is written with a single
// O_APPEND write so concurrent writers (CLI processes and the supervisor) do
// not interleave. enforce reflects the mode; blocked is whether the close was
// refused (enforce && violations).
func RecordViolations(cityPath, door string, bead beads.Bead, enforce bool, violations []string) {
	if len(violations) == 0 || cityPath == "" {
		return
	}
	defer func() { _ = recover() }()

	mode := "warn-only"
	if enforce {
		mode = "enforce"
	}
	reasons := make([]string, 0, len(violations))
	for i, v := range violations {
		if i == maxLoggedReasons {
			break
		}
		reasons = append(reasons, truncate(v, maxLoggedFieldBytes))
	}
	rec := violationRecord{
		TS:      time.Now().UTC().Format(time.RFC3339),
		Door:    door,
		Bead:    truncate(bead.ID, maxLoggedFieldBytes),
		Outcome: truncate(bead.Metadata[beadmeta.WorkOutcomeMetadataKey], maxLoggedFieldBytes),
		Commit:  truncate(bead.Metadata[beadmeta.WorkCommitMetadataKey], maxLoggedFieldBytes),
		Branch:  truncate(bead.Metadata[beadmeta.WorkBranchMetadataKey], maxLoggedFieldBytes),
		Mode:    mode,
		Blocked: enforce,
		Reasons: reasons,
	}
	line, err := json.Marshal(rec)
	if err != nil {
		return
	}
	line = append(line, '\n')

	path := ViolationLogPath(cityPath)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return
	}
	if fi, err := os.Stat(path); err == nil && fi.Size() >= ViolationLogMaxBytes {
		_ = os.Rename(path, path+".1")
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	_, _ = f.Write(line)
	_ = f.Close()
}
