package dolt_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestCompactScriptStalePendingPushMarkerDoesNotRemailEveryCycle pins the
// "too loud" half of gcw-vmm00.39: a stale pending-push marker that stays
// unresolved across repeated compact cycles must not page the operator on
// every single cycle. Previously ensure_remote_push_retry_fresh had no
// dedup at all, so an unresolved marker sent one identical mail per compact
// invocation. The event still fires every cycle so automation keeps
// observing each check; only the mail is gated.
func TestCompactScriptStalePendingPushMarkerDoesNotRemailEveryCycle(t *testing.T) {
	fixture := newCompactScriptFixture(t)
	firstOut, err := fixture.run(t, "remote_push_failure", "GC_DOLT_COMPACT_THRESHOLD_COMMITS=500")
	if err != nil {
		t.Fatalf("first compact should succeed locally despite remote push failure: %v\n%s", err, firstOut)
	}
	pendingPush := filepath.Join(fixture.cityPath, ".gc", "runtime", "packs", "dolt", "compact-pending-push", "beads")
	replaceCompactMarkerCreatedAt(t, pendingPush, "1970-01-01T00:00:00Z")
	resetCompactGCLog(t, fixture)

	secondOut, err := fixture.run(t, "remote_success", "GC_DOLT_COMPACT_THRESHOLD_COMMITS=500")
	if err == nil {
		t.Fatalf("stale pending-push retry succeeded without manual review:\n%s", secondOut)
	}
	thirdOut, err := fixture.run(t, "remote_success", "GC_DOLT_COMPACT_THRESHOLD_COMMITS=500")
	if err == nil {
		t.Fatalf("stale pending-push retry succeeded without manual review:\n%s", thirdOut)
	}

	log := readCompactGCLog(t, fixture)
	mailLines := compactGCLogLinesWithPrefix(log, "gc mail send ")
	if len(mailLines) != 1 {
		t.Fatalf("two stale-pending-push retry cycles over an unresolved marker should send exactly one operator mail, got %d\nlog:\n%s", len(mailLines), log)
	}
	if !strings.Contains(mailLines[0], "seen=") {
		t.Fatalf("stale-marker alert should carry dynamic context (e.g. seen count) so operators can gauge how long it has persisted\nline:\n%s\nlog:\n%s", mailLines[0], log)
	}
	eventLines := compactGCLogLinesWithPrefix(log, "gc event emit dolt.compact.quarantine")
	if len(eventLines) != 2 {
		t.Fatalf("each stale-pending-push retry cycle should still emit an event even when the mail is suppressed, got %d\nlog:\n%s", len(eventLines), log)
	}
}

// TestCompactScriptQuarantineRenotifiesAfterBackstopElapses pins the "too
// quiet" half of gcw-vmm00.39: an unresolved quarantine with an UNCHANGED
// reason must still page again once the renotify backstop elapses.
// Previously quarantine_should_notify (now marker_should_notify) deduped on
// exact reason match with no time backstop, so a real, still-unresolved
// integrity failure was reported exactly once and then silently ignored
// forever. This must not regress TestCompactScriptQuarantineReasonChangeReMails
// (a changed reason still re-mails immediately, independent of the backstop)
// or TestCompactScriptExistingQuarantineMarkerAlertsOnceAcrossRepeatedCycles
// (an unchanged reason within the backstop window still dedups).
func TestCompactScriptQuarantineRenotifiesAfterBackstopElapses(t *testing.T) {
	fixture := newCompactScriptFixture(t)
	firstOut, err := fixture.run(t, "row_count_decreases", "GC_DOLT_COMPACT_THRESHOLD_COMMITS=500")
	if err == nil {
		t.Fatalf("first compact succeeded despite row-count decrease:\n%s", firstOut)
	}
	secondOut, err := fixture.run(t, "below_threshold")
	if err == nil {
		t.Fatalf("second compact succeeded despite quarantine:\n%s", secondOut)
	}

	log := readCompactGCLog(t, fixture)
	if mailLines := compactGCLogLinesWithPrefix(log, "gc mail send "); len(mailLines) != 1 {
		t.Fatalf("dedup should be established after two cycles, want 1 mail, got %d\nlog:\n%s", len(mailLines), log)
	}

	// Force the backstop to have elapsed by backdating the marker's own
	// notify bookkeeping, matching the file's existing convention of aging
	// marker fields directly rather than faking the clock.
	marker := filepath.Join(fixture.cityPath, ".gc", "runtime", "packs", "dolt", "compact-quarantine", "beads")
	replaceCompactMarkerField(t, marker, "last_notified_ts", "2000-01-01T00:00:00Z")

	thirdOut, err := fixture.run(t, "below_threshold")
	if err == nil {
		t.Fatalf("third compact succeeded despite quarantine:\n%s", thirdOut)
	}

	log = readCompactGCLog(t, fixture)
	mailLines := compactGCLogLinesWithPrefix(log, "gc mail send ")
	if len(mailLines) != 2 {
		t.Fatalf("quarantine must re-notify once the renotify backstop elapses, want 2 mails, got %d\nlog:\n%s", len(mailLines), log)
	}
	if !strings.Contains(mailLines[1], "reason=post-flatten row count decreased") {
		t.Fatalf("backstop re-notify should still carry the original, unchanged reason\nline:\n%s\nlog:\n%s", mailLines[1], log)
	}
	if !strings.Contains(mailLines[1], "seen=") {
		t.Fatalf("repeat notification should carry dynamic context (e.g. seen count) distinguishing it from the first alert\nline:\n%s\nlog:\n%s", mailLines[1], log)
	}
	eventLines := compactGCLogLinesWithPrefix(log, "gc event emit dolt.compact.quarantine")
	if len(eventLines) != 3 {
		t.Fatalf("each compact cycle should still emit a dolt.compact.quarantine event even when the mail is suppressed, got %d\nlog:\n%s", len(eventLines), log)
	}
}

// TestCompactScriptBareGCExistingQuarantineDoesNotRemailEveryCycle pins the
// bare-GC half of gcw-vmm00.39 that the original port missed: bare_gc_database
// called the ungated send_compact_quarantine_alert wrapper directly, which has
// no marker_should_notify dedup and no record_marker_notify_state bookkeeping.
// Because --bare-gc and flatten are mutually exclusive run modes, a
// --bare-gc-only deployment against an unresolved quarantine mailed the mayor
// on every single cycle forever, with seen_count/notify_count frozen since
// they were never bumped. Measured on GC3: 57 mails over 31 hours from one
// marker. bare_gc_database must now share report_existing_quarantine with
// flatten_database so it dedups and its bookkeeping actually advances.
func TestCompactScriptBareGCExistingQuarantineDoesNotRemailEveryCycle(t *testing.T) {
	fixture := newCompactScriptFixture(t)
	marker := filepath.Join(fixture.cityPath, ".gc", "runtime", "packs", "dolt", "compact-quarantine", "beads")
	if err := os.MkdirAll(filepath.Dir(marker), 0o755); err != nil {
		t.Fatalf("mkdir quarantine dir: %v", err)
	}
	const reason = "manual repair pending"
	if err := os.WriteFile(marker, []byte("db=beads\nreason="+reason+"\ncreated_at=2026-05-01T00:00:00Z\n"), 0o600); err != nil {
		t.Fatalf("write quarantine marker: %v", err)
	}

	firstOut, err := fixture.run(t, "success", "GC_DOLT_COMPACT_BARE_GC=1")
	if err == nil {
		t.Fatalf("bare-gc must fail when quarantine marker exists:\n%s", firstOut)
	}
	secondOut, err := fixture.run(t, "success", "GC_DOLT_COMPACT_BARE_GC=1")
	if err == nil {
		t.Fatalf("bare-gc must fail when quarantine marker exists:\n%s", secondOut)
	}

	log := readCompactGCLog(t, fixture)
	if mailLines := compactGCLogLinesWithPrefix(log, "gc mail send "); len(mailLines) != 1 {
		t.Fatalf("two --bare-gc cycles over an unresolved marker within the backstop window should send exactly one operator mail, got %d\nlog:\n%s", len(mailLines), log)
	}
	eventLines := compactGCLogLinesWithPrefix(log, "gc event emit dolt.compact.quarantine")
	if len(eventLines) != 2 {
		t.Fatalf("each --bare-gc cycle should still emit an event even when the mail is suppressed, got %d\nlog:\n%s", len(eventLines), log)
	}

	if seenCount := compactMarkerValue(t, marker, "seen_count"); seenCount != "2" {
		t.Fatalf("bookkeeping must advance on every --bare-gc cycle, not stay frozen: seen_count = %q, want 2", seenCount)
	}
	if notifyCount := compactMarkerValue(t, marker, "notify_count"); notifyCount != "1" {
		t.Fatalf("notify_count must advance on the emitted cycle, not stay frozen: notify_count = %q, want 1", notifyCount)
	}
	if lastNotifiedTS := compactMarkerValue(t, marker, "last_notified_ts"); lastNotifiedTS == "" {
		t.Fatal("last_notified_ts must be recorded once bare-gc's alert fires")
	}
}
