package core

import (
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRenudgeStaleHumanGatesStopsAtReminderCap(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	scriptDir := filepath.Join(tmp, "scripts")
	binDir := filepath.Join(tmp, "bin")
	stateDir := filepath.Join(tmp, "state")
	for _, dir := range []string{scriptDir, binDir, stateDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}

	for _, name := range []string{"renudge-stale-human-gates.sh", "_bd_trace.sh"} {
		data, err := fs.ReadFile(PackFS, "assets/scripts/"+name)
		if err != nil {
			t.Fatalf("read embedded %s: %v", name, err)
		}
		if err := os.WriteFile(filepath.Join(scriptDir, name), data, 0o755); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}

	mailLog := filepath.Join(tmp, "mail.log")
	fakeGC := `#!/usr/bin/env bash
set -euo pipefail
case "$*" in
  "rig list --json")
    printf '{"rigs":[]}'
    ;;
  "bd gate list --limit 0 --json")
    printf '[{"id":"gate-1","created_at":"2020-01-01T00:00:00Z","await_type":"human","status":"open"}]'
    ;;
  "bd show gate-1 --json")
    printf '[{"id":"gate-1","created_at":"2020-01-01T00:00:00Z","await_type":"human","status":"open","assignee":"human","title":"Choose","description":"Decide"}]'
    ;;
  mail\ send\ human*)
    printf '%s\n' "$*" >>"$MAIL_LOG"
    ;;
  *)
    printf 'unexpected gc argv: %s\n' "$*" >&2
    exit 97
    ;;
esac
`
	if err := os.WriteFile(filepath.Join(binDir, "gc"), []byte(fakeGC), 0o755); err != nil {
		t.Fatalf("write fake gc: %v", err)
	}

	statePath := filepath.Join(stateDir, "renudge-stale-human-gates-state.json")
	state := `{"gate-1":{"last_sent_at":"2020-01-01T00:00:00Z","last_seen_at":"2020-01-01T00:00:00Z","count":3}}`
	if err := os.WriteFile(statePath, []byte(state), 0o600); err != nil {
		t.Fatalf("write state: %v", err)
	}

	runScript := func(maxRenudges string) {
		t.Helper()
		cmd := exec.Command("bash", filepath.Join(scriptDir, "renudge-stale-human-gates.sh"))
		cmd.Env = append(os.Environ(),
			"PATH="+binDir+":"+os.Getenv("PATH"),
			"MAIL_LOG="+mailLog,
			"GC_CITY="+tmp,
			"GC_PACK_STATE_DIR="+stateDir,
			"GC_STALE_GATE_THRESHOLD=0s",
			"GC_STALE_GATE_RENUDGE_INTERVAL=1h",
			"GC_STALE_GATE_MAX_RENUDGES="+maxRenudges,
		)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("renudge script: %v\n%s", err, out)
		}
	}
	runScript("3")
	if data, err := os.ReadFile(mailLog); err == nil && len(strings.TrimSpace(string(data))) > 0 {
		t.Fatalf("capped gate sent another reminder: %s", data)
	} else if err != nil && !os.IsNotExist(err) {
		t.Fatalf("read mail log: %v", err)
	}

	updated, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("read updated state: %v", err)
	}
	if !strings.Contains(string(updated), `"count": 3`) && !strings.Contains(string(updated), `"count":3`) {
		t.Fatalf("capped reminder count changed or lost: %s", updated)
	}
	if strings.Contains(string(updated), `"last_seen_at":"2020-01-01T00:00:00Z"`) {
		t.Fatalf("capped open gate did not refresh last_seen_at: %s", updated)
	}

	// A legacy timestamp proves the gate was already being reminded under the
	// unbounded policy, but cannot recover how many times. Migration therefore
	// saturates the new budget instead of sending four more reminders to a gate
	// that may already have produced hundreds.
	if err := os.Remove(mailLog); err != nil && !os.IsNotExist(err) {
		t.Fatalf("reset mail log before legacy migration: %v", err)
	}
	if err := os.WriteFile(statePath, []byte(`{"gate-1":"2020-01-01T00:00:00Z"}`), 0o600); err != nil {
		t.Fatalf("write legacy state: %v", err)
	}
	runScript("5")
	if data, err := os.ReadFile(mailLog); err == nil && len(strings.TrimSpace(string(data))) > 0 {
		t.Fatalf("legacy migrated gate sent another reminder: %s", data)
	} else if err != nil && !os.IsNotExist(err) {
		t.Fatalf("read legacy migration mail log: %v", err)
	}
	updated, err = os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("read legacy migrated state: %v", err)
	}
	if !strings.Contains(string(updated), `"count": 5`) && !strings.Contains(string(updated), `"count":5`) {
		t.Fatalf("legacy migration did not saturate reminder budget: %s", updated)
	}

	// At count=2 the default exponential curve requires a two-hour gap. A
	// 90-minute-old reminder is suppressed; a three-hour-old reminder fires
	// exactly once and advances the count.
	if err := os.Remove(mailLog); err != nil && !os.IsNotExist(err) {
		t.Fatalf("reset mail log: %v", err)
	}
	recent := time.Now().UTC().Add(-90 * time.Minute).Format(time.RFC3339)
	state = `{"gate-1":{"last_sent_at":"` + recent + `","last_seen_at":"` + recent + `","count":2}}`
	if err := os.WriteFile(statePath, []byte(state), 0o600); err != nil {
		t.Fatalf("write recent backoff state: %v", err)
	}
	runScript("5")
	if data, err := os.ReadFile(mailLog); err == nil && len(strings.TrimSpace(string(data))) > 0 {
		t.Fatalf("backoff sent before two-hour boundary: %s", data)
	} else if err != nil && !os.IsNotExist(err) {
		t.Fatalf("read backoff mail log: %v", err)
	}

	if err := os.Remove(mailLog); err != nil && !os.IsNotExist(err) {
		t.Fatalf("reset mail log after suppression: %v", err)
	}
	due := time.Now().UTC().Add(-3 * time.Hour).Format(time.RFC3339)
	state = `{"gate-1":{"last_sent_at":"` + due + `","last_seen_at":"` + due + `","count":2}}`
	if err := os.WriteFile(statePath, []byte(state), 0o600); err != nil {
		t.Fatalf("write due backoff state: %v", err)
	}
	runScript("5")
	data, err := os.ReadFile(mailLog)
	if err != nil {
		t.Fatalf("read due mail log: %v", err)
	}
	if got := strings.Count(strings.TrimSpace(string(data)), "mail send human"); got != 1 {
		t.Fatalf("due backoff sent %d reminders, want 1: %s", got, data)
	}
	updated, err = os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("read due updated state: %v", err)
	}
	if !strings.Contains(string(updated), `"count": 3`) && !strings.Contains(string(updated), `"count":3`) {
		t.Fatalf("due backoff did not advance count to 3: %s", updated)
	}

	// A malformed existing ledger cannot safely mean "no reminders sent". Fail
	// closed instead of resetting every open gate to an eligible zero count.
	if err := os.Remove(mailLog); err != nil && !os.IsNotExist(err) {
		t.Fatalf("reset mail log before corrupt state: %v", err)
	}
	if err := os.WriteFile(statePath, []byte(`{"gate-1":`), 0o600); err != nil {
		t.Fatalf("write corrupt state: %v", err)
	}
	cmd := exec.Command("bash", filepath.Join(scriptDir, "renudge-stale-human-gates.sh"))
	cmd.Env = append(os.Environ(),
		"PATH="+binDir+":"+os.Getenv("PATH"),
		"MAIL_LOG="+mailLog,
		"GC_CITY="+tmp,
		"GC_PACK_STATE_DIR="+stateDir,
		"GC_STALE_GATE_THRESHOLD=0s",
		"GC_STALE_GATE_RENUDGE_INTERVAL=1h",
		"GC_STALE_GATE_MAX_RENUDGES=5",
	)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("corrupt reminder state did not fail closed: %s", out)
	}
	if !strings.Contains(string(out), "state is corrupt") {
		t.Fatalf("corrupt reminder state failure was not diagnostic: %s", out)
	}
	if data, readErr := os.ReadFile(mailLog); readErr == nil && len(strings.TrimSpace(string(data))) > 0 {
		t.Fatalf("corrupt reminder state sent mail: %s", data)
	} else if readErr != nil && !os.IsNotExist(readErr) {
		t.Fatalf("read corrupt-state mail log: %v", readErr)
	}

	// Valid JSON with an invalid per-gate shape is equally unsafe: treating an
	// unknown count as zero would reopen the reminder budget.
	if err := os.WriteFile(statePath, []byte(`{"gate-1":{"count":"unknown"}}`), 0o600); err != nil {
		t.Fatalf("write invalid entry state: %v", err)
	}
	cmd = exec.Command("bash", filepath.Join(scriptDir, "renudge-stale-human-gates.sh"))
	cmd.Env = append(os.Environ(),
		"PATH="+binDir+":"+os.Getenv("PATH"),
		"MAIL_LOG="+mailLog,
		"GC_CITY="+tmp,
		"GC_PACK_STATE_DIR="+stateDir,
		"GC_STALE_GATE_THRESHOLD=0s",
		"GC_STALE_GATE_RENUDGE_INTERVAL=1h",
		"GC_STALE_GATE_MAX_RENUDGES=5",
	)
	out, err = cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("invalid reminder entry did not fail closed: %s", out)
	}
	if !strings.Contains(string(out), "state is corrupt") {
		t.Fatalf("invalid reminder entry failure was not diagnostic: %s", out)
	}
}
