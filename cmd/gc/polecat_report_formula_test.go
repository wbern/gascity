package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/formula"
)

func coreFormulaDir(t *testing.T) string {
	t.Helper()
	_, filename, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(filename), "..", "..", "internal", "bootstrap", "packs", "core", "formulas")
}

// TestCoreFormulaProgressRecordsUseComments keeps every embedded core formula
// on the controller-safe bridge until typed Bead Notes parity (gcw-9tpw.1) is
// available. A raw Notes mutation would be refused by the managed shim.
func TestCoreFormulaProgressRecordsUseComments(t *testing.T) {
	dir := coreFormulaDir(t)
	for _, name := range []string{
		"mol-do-work.toml",
		"mol-polecat-commit.toml",
		"mol-polecat-report.toml",
		"mol-prompt-synth.toml",
	} {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("ReadFile(%s): %v", name, err)
		}
		body := string(data)
		if strings.Contains(body, "--notes") || strings.Contains(body, "--append-notes") {
			t.Errorf("%s still writes unsupported Bead Notes; use gc bd comment until gcw-9tpw.1 lands", name)
		}
		if !strings.Contains(body, "gc bd comment") {
			t.Errorf("%s does not record progress through gc bd comment", name)
		}
	}
}

func TestCoreWorkSkillDocumentsTemporaryNotesBridge(t *testing.T) {
	_, filename, _, _ := runtime.Caller(0)
	path := filepath.Join(filepath.Dir(filename), "..", "..", "internal", "bootstrap", "packs", "core", "skills", "gc-work", "SKILL.md")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%s): %v", path, err)
	}
	body := string(data)
	for _, want := range []string{
		"gc bd comment <id> --file <path>",
		"gcw-9tpw.1",
		"merged and deployed",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("gc-work skill missing %q", want)
		}
	}
	if strings.Contains(body, `gc bd update <id> --note "progress..."`) {
		t.Error("gc-work skill still teaches the invalid --note progress command")
	}
}

// TestPolecatReportFormulaParsesAndHasNoGHPRCreate verifies that the
// mol-polecat-report formula parses without error, contains a write-report
// terminal step, and never instructs the agent to call gh pr create or
// git push in any step description.
func TestPolecatReportFormulaParsesAndHasNoGHPRCreate(t *testing.T) {
	dir := coreFormulaDir(t)
	path := filepath.Join(dir, "mol-polecat-report.toml")

	parser := formula.NewParser(dir)
	f, err := parser.ParseFile(path)
	if err != nil {
		t.Fatalf("ParseFile mol-polecat-report.toml: %v", err)
	}

	var hasWriteReport, hasWriteComment, hasClose, hasDrainAck, hasImplement bool
	for _, step := range f.Steps {
		if strings.Contains(step.Description, "gh pr create") {
			t.Errorf("step %q must not invoke 'gh pr create'", step.ID)
		}
		if strings.Contains(step.Description, "git push") {
			t.Errorf("step %q must not invoke 'git push'", step.ID)
		}
		if strings.Contains(step.Description, "git checkout -- .") {
			t.Errorf("step %q must not run destructive 'git checkout -- .' in a shared checkout", step.ID)
		}
		if step.ID == "implement" {
			hasImplement = true
			desc := strings.ToLower(step.Description)
			if !strings.Contains(desc, "no code changes") && !strings.Contains(desc, "analysis only") {
				t.Errorf("implement step must forbid code changes (expected 'no code changes' or 'analysis only'), got: %q", step.Description)
			}
		}
		if step.ID == "write-report" {
			hasWriteReport = true
			if strings.Contains(step.Description, `gc bd comment "$WORK_BEAD_ID" --stdin`) {
				hasWriteComment = true
			}
			if strings.Contains(step.Description, `bd close "$WORK_BEAD_ID"`) {
				hasClose = true
			}
			if strings.Contains(step.Description, "gc runtime drain-ack") {
				hasDrainAck = true
			}
		}
	}
	if !hasImplement {
		t.Error("mol-polecat-report formula missing 'implement' step override")
	}
	if !hasWriteReport {
		t.Error("mol-polecat-report formula missing 'write-report' step")
	}
	if !hasWriteComment {
		t.Error(`write-report step must write findings with 'gc bd comment "$WORK_BEAD_ID" --stdin'`)
	}
	if !hasClose {
		t.Error(`write-report step must close the bead with 'bd close "$WORK_BEAD_ID"'`)
	}
	if !hasDrainAck {
		t.Error("write-report step must signal the reconciler with 'gc runtime drain-ack'")
	}
}
