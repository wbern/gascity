package core

import (
	"encoding/json"
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
    if [ "${FAKE_RIG_UNAVAILABLE:-0}" = "1" ]; then
      printf '{"rigs":[{"name":"unavailable"}]}'
    else
      printf '{"rigs":[]}'
    fi
    ;;
  "bd gate list --limit 0 --json")
    printf '[{"id":"gate-1","created_at":"2020-01-01T00:00:00Z","await_type":"human","status":"open"}]'
    ;;
  bd\ --rig\ unavailable\ gate\ list*)
    exit 91
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

	runScript := func(maxRenudges string, unavailableRig ...bool) {
		t.Helper()
		unavailable := "0"
		if len(unavailableRig) > 0 && unavailableRig[0] {
			unavailable = "1"
		}
		cmd := exec.Command("bash", filepath.Join(scriptDir, "renudge-stale-human-gates.sh"))
		cmd.Env = append(os.Environ(),
			"PATH="+binDir+":"+os.Getenv("PATH"),
			"MAIL_LOG="+mailLog,
			"GC_CITY="+tmp,
			"GC_PACK_STATE_DIR="+stateDir,
			"GC_STALE_GATE_THRESHOLD=0s",
			"GC_STALE_GATE_RENUDGE_INTERVAL=1h",
			"GC_STALE_GATE_MAX_RENUDGES="+maxRenudges,
			"FAKE_RIG_UNAVAILABLE="+unavailable,
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
	var cappedState struct {
		Count      int    `json:"count"`
		LastSeenAt string `json:"last_seen_at"`
	}
	if err := json.Unmarshal(updated, &map[string]json.RawMessage{}); err != nil {
		t.Fatalf("updated reminder state is not JSON: %v", err)
	}
	var stateEntries map[string]json.RawMessage
	if err := json.Unmarshal(updated, &stateEntries); err != nil {
		t.Fatalf("decode updated reminder state: %v", err)
	}
	if err := json.Unmarshal(stateEntries["gate-1"], &cappedState); err != nil {
		t.Fatalf("decode capped gate state: %v", err)
	}
	if cappedState.Count != 3 {
		t.Fatalf("capped reminder count = %d, want 3: %s", cappedState.Count, updated)
	}
	if cappedState.LastSeenAt == "2020-01-01T00:00:00Z" || cappedState.LastSeenAt == "" {
		t.Fatalf("capped open gate did not refresh last_seen_at: %s", updated)
	}

	// Zero retention makes a whole-second last_seen_at immediately older than
	// jq's fractional `now`: the first sweep prunes the capped ledger entry and
	// the next sweep starts its reminder budget over. Reject that configuration
	// before either the state file or mailbox can change.
	if err := os.Remove(mailLog); err != nil && !os.IsNotExist(err) {
		t.Fatalf("reset mail log before zero-retention check: %v", err)
	}
	state = `{"gate-1":{"last_sent_at":"2020-01-01T00:00:00Z","last_seen_at":"2020-01-01T00:00:00Z","count":3}}`
	if err := os.WriteFile(statePath, []byte(state), 0o600); err != nil {
		t.Fatalf("write capped zero-retention state: %v", err)
	}
	zeroRetention := func() ([]byte, error) {
		cmd := exec.Command("bash", filepath.Join(scriptDir, "renudge-stale-human-gates.sh"))
		cmd.Env = append(os.Environ(),
			"PATH="+binDir+":"+os.Getenv("PATH"),
			"MAIL_LOG="+mailLog,
			"GC_CITY="+tmp,
			"GC_PACK_STATE_DIR="+stateDir,
			"GC_STALE_GATE_THRESHOLD=0s",
			"GC_STALE_GATE_RENUDGE_INTERVAL=1h",
			"GC_STALE_GATE_MAX_RENUDGES=3",
			"GC_STALE_GATE_STATE_RETENTION=0s",
		)
		return cmd.CombinedOutput()
	}
	firstOut, firstErr := zeroRetention()
	if firstErr == nil {
		secondOut, secondErr := zeroRetention()
		mail, _ := os.ReadFile(mailLog)
		t.Fatalf("zero retention was accepted and can restart a capped gate: first=%s second_err=%v second=%s mail=%s", firstOut, secondErr, secondOut, mail)
	}
	if !strings.Contains(string(firstOut), "retention must be positive") {
		t.Fatalf("zero-retention rejection was not diagnostic: %s", firstOut)
	}
	unchanged, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("read state after zero-retention rejection: %v", err)
	}
	if string(unchanged) != state {
		t.Fatalf("zero-retention rejection mutated capped state: got %s want %s", unchanged, state)
	}
	if mail, err := os.ReadFile(mailLog); err == nil && len(strings.TrimSpace(string(mail))) > 0 {
		t.Fatalf("zero-retention rejection sent mail: %s", mail)
	} else if err != nil && !os.IsNotExist(err) {
		t.Fatalf("read zero-retention mail log: %v", err)
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

	// A structured entry proves the new bounded ledger format is in use. If its
	// count is absent, the prior reminder depth is unknowable; treating that as
	// zero would reopen the budget just like a corrupt count value.
	missingCount := `{"gate-1":{"last_sent_at":"2020-01-01T00:00:00Z","last_seen_at":"2020-01-01T00:00:00Z"}}`
	if err := os.WriteFile(statePath, []byte(missingCount), 0o600); err != nil {
		t.Fatalf("write missing-count state: %v", err)
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
	if err == nil || !strings.Contains(string(out), "state is corrupt") {
		t.Fatalf("missing reminder count did not fail closed: err=%v out=%s", err, out)
	}
	unchanged, err = os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("read missing-count state after rejection: %v", err)
	}
	if string(unchanged) != missingCount {
		t.Fatalf("missing-count rejection rewrote ledger: got %s want %s", unchanged, missingCount)
	}
	if mail, err := os.ReadFile(mailLog); err == nil && len(strings.TrimSpace(string(mail))) > 0 {
		t.Fatalf("missing-count rejection sent mail: %s", mail)
	} else if err != nil && !os.IsNotExist(err) {
		t.Fatalf("read missing-count mail log: %v", err)
	}

	// A capped structured entry without timestamps is also unknowable. If its
	// owning rig is unavailable, the sweep cannot refresh it and pruning would
	// otherwise treat the missing last_seen_at as epoch zero, erase the cap, and
	// restart reminders when the rig returns.
	incompleteCapped := `{"gate-unavailable":{"count":5}}`
	if err := os.WriteFile(statePath, []byte(incompleteCapped), 0o600); err != nil {
		t.Fatalf("write incomplete capped state: %v", err)
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
		"FAKE_RIG_UNAVAILABLE=1",
	)
	out, err = cmd.CombinedOutput()
	if err == nil || !strings.Contains(string(out), "state is corrupt") {
		t.Fatalf("incomplete capped state did not fail closed: err=%v out=%s", err, out)
	}
	unchanged, err = os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("read incomplete capped state after rejection: %v", err)
	}
	if string(unchanged) != incompleteCapped {
		t.Fatalf("incomplete capped rejection rewrote ledger: got %s want %s", unchanged, incompleteCapped)
	}
	if mail, err := os.ReadFile(mailLog); err == nil && len(strings.TrimSpace(string(mail))) > 0 {
		t.Fatalf("incomplete capped rejection sent mail: %s", mail)
	} else if err != nil && !os.IsNotExist(err) {
		t.Fatalf("read incomplete capped mail log: %v", err)
	}

	// Invalid timestamps are dangerous even when the JSON shape is otherwise
	// valid: pruning must not reinterpret them as epoch zero and erase a cap.
	invalidTimestamp := `{"gate-1":{"last_sent_at":"not-a-date","last_seen_at":"not-a-date","count":5}}`
	if err := os.WriteFile(statePath, []byte(invalidTimestamp), 0o600); err != nil {
		t.Fatalf("write invalid timestamp state: %v", err)
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
	if err == nil || !strings.Contains(string(out), "state is corrupt") {
		t.Fatalf("invalid timestamp did not fail closed: err=%v out=%s", err, out)
	}

	// One rig can fail enumeration for a sweep. A recently seen capped gate in
	// that rig must retain its ledger entry rather than reopening the budget.
	recentSeen := time.Now().UTC().Add(-time.Minute).Format(time.RFC3339)
	unavailableState := `{"gate-1":{"last_sent_at":"` + recentSeen + `","last_seen_at":"` + recentSeen + `","count":5},` +
		`"gate-unavailable":{"last_sent_at":"` + recentSeen + `","last_seen_at":"` + recentSeen + `","count":5}}`
	if err := os.WriteFile(statePath, []byte(unavailableState), 0o600); err != nil {
		t.Fatalf("write unavailable-rig state: %v", err)
	}
	runScript("5", true)
	updated, err = os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("read unavailable-rig state: %v", err)
	}
	stateEntries = nil
	if err := json.Unmarshal(updated, &stateEntries); err != nil {
		t.Fatalf("decode unavailable-rig state: %v", err)
	}
	if _, ok := stateEntries["gate-unavailable"]; !ok {
		t.Fatalf("unavailable sweep pruned capped gate ledger: %s", updated)
	}
}
