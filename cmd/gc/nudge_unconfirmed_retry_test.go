package main

import (
	"fmt"
	"testing"
	"time"

	sessiontmux "github.com/gastownhall/gascity/internal/runtime/tmux"
)

// TestFailedQueuedNudge_DoesNotRetryUnconfirmedSubmit pins the live incident of
// 2026-08-06: after ErrNudgeSubmitUnconfirmed was introduced (upstream #5012,
// cherry-picked as f19fc0c85 and deployed at 21:38 CEST), queued reminders were
// re-delivered up to defaultQueuedNudgeMaxAttempts times to agents that had
// already received them — including agents that were actively working, because
// the replayed text is a snapshot of an "idle" reminder.
//
// The retry is never a remedy for this error. The keystrokes DID reach tmux
// ("submit Enter delivered to tmux but not confirmed"); only the busy-state
// confirmation was not observed. Re-sending types the same text at the pane
// again, so a retry can only ever duplicate a message the agent already has —
// it cannot recover a lost one. Contrast the two errors that DO warrant a
// retry, both handled elsewhere: runtime.ErrSessionNotFound (released for a
// later pass) and a runtime that declines without an error (!result.Delivered,
// claims released).
//
// tmux/adapter.go:1324 already encodes exactly this reasoning for the startup
// nudge — it tolerates ErrNudgeSubmitUnconfirmed rather than failing the start.
// The queued-delivery path was simply never given the same treatment.
func TestFailedQueuedNudge_DoesNotRetryUnconfirmedSubmit(t *testing.T) {
	item := newQueuedNudgeWithOptions("worker", "[idle-mail-nudge] You have 2 unread message(s)", "queue", time.Now(), queuedNudgeOptions{
		ID:        "n-unconfirmed",
		SessionID: "gc-1",
	})

	updated, dead := failedQueuedNudge(item, sessiontmux.ErrNudgeSubmitUnconfirmed, time.Now())

	if !dead {
		t.Fatalf("dead = false, want true: an unconfirmed submit must be terminal, not retried (attempts=%d would replay the same text at the agent)", updated.Attempts)
	}
	if updated.DeadAt.IsZero() {
		t.Fatal("DeadAt is zero, want a terminal timestamp")
	}
	// Deliberately NOT asserting DeliverAfter here. It is stamped at creation,
	// and the fence-mismatch path leaves it set too; terminality is carried by
	// DeadAt plus the returned flag, which is what moves the item onto the Dead
	// list instead of back onto Pending. An earlier draft of this test asserted
	// a zero DeliverAfter and failed — the assertion was wrong, not the code.
	if updated.Attempts != 1 {
		t.Fatalf("Attempts = %d, want 1: a terminal outcome must not consume the retry budget", updated.Attempts)
	}
}

// TestFailedQueuedNudge_UnconfirmedIsTerminalWhenWrapped pins that the check
// survives error wrapping. NudgeSession returns the sentinel wrapped with the
// session name (tmux.go:2002 does `fmt.Errorf("%w: session %q", ...)`), so a
// non-errors.Is comparison would miss every real occurrence while passing a
// naive test that used the bare sentinel.
func TestFailedQueuedNudge_UnconfirmedIsTerminalWhenWrapped(t *testing.T) {
	item := newQueuedNudgeWithOptions("worker", "reminder", "queue", time.Now(), queuedNudgeOptions{
		ID:        "n-wrapped",
		SessionID: "gc-1",
	})
	wrapped := fmt.Errorf("%w: session %q", sessiontmux.ErrNudgeSubmitUnconfirmed, "gc-1")

	_, dead := failedQueuedNudge(item, wrapped, time.Now())

	if !dead {
		t.Fatal("dead = false for a WRAPPED ErrNudgeSubmitUnconfirmed: the check must use errors.Is, not equality")
	}
}

// TestFailedQueuedNudge_StillRetriesOrdinaryFailures guards the other side.
// Making unconfirmed terminal must not turn every transient delivery error into
// a one-shot: an ordinary failure still gets its retry budget.
func TestFailedQueuedNudge_StillRetriesOrdinaryFailures(t *testing.T) {
	item := newQueuedNudgeWithOptions("worker", "reminder", "queue", time.Now(), queuedNudgeOptions{
		ID:        "n-transient",
		SessionID: "gc-1",
	})

	updated, dead := failedQueuedNudge(item, fmt.Errorf("tmux: transient write failure"), time.Now())

	if dead {
		t.Fatal("dead = true for an ordinary transient error, want it retried")
	}
	if updated.DeliverAfter.IsZero() {
		t.Fatal("DeliverAfter is zero, want the item rescheduled for retry")
	}
	if updated.Attempts != 1 {
		t.Fatalf("Attempts = %d, want 1", updated.Attempts)
	}
}
