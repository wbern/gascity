package main

import (
	"strings"
	"testing"
	"time"

	sessiontmux "github.com/gastownhall/gascity/internal/runtime/tmux"
)

// TestUnconfirmedDropWarning_NamesTheDroppedNudge pins the observability gap
// left by e22aed815. That commit made ErrNudgeSubmitUnconfirmed terminal, which
// correctly stopped the queue from re-typing a message the agent already had —
// but it made the outcome INVISIBLE. If "unconfirmed" ever means the Enter
// genuinely did not submit, the reminder is now dropped and nothing says so.
//
// A dropped nudge must be observable, so a future regression shows up as a
// rising count rather than as an agent that mysteriously never answered.
func TestUnconfirmedDropWarning_NamesTheDroppedNudge(t *testing.T) {
	item := newQueuedNudgeWithOptions("gas-city-infra/devops", "[idle-mail-nudge] You have 2 unread message(s)", "queue", time.Now(), queuedNudgeOptions{
		ID:        "n-dropped",
		SessionID: "gc2-dxrtn",
	})
	item.LastError = sessiontmux.ErrNudgeSubmitUnconfirmed.Error() + `: session "gc2-dxrtn"`

	got := unconfirmedDropWarning(item)

	if got == "" {
		t.Fatal("no warning emitted for a nudge dropped on an unconfirmed submit: the drop is silent")
	}
	for _, want := range []string{"n-dropped", "gas-city-infra/devops", "not confirmed"} {
		if !strings.Contains(got, want) {
			t.Errorf("warning does not mention %q, so an operator cannot act on it:\n  %s", want, got)
		}
	}
}

// TestUnconfirmedDropWarning_SilentForOtherFailures keeps the signal worth
// reading. A fence mismatch or an exhausted retry budget is an ordinary
// dead-letter, already visible in `gc nudge status`; warning about those too
// would train operators to ignore the line that matters.
func TestUnconfirmedDropWarning_SilentForOtherFailures(t *testing.T) {
	for _, tc := range []struct{ name, lastErr string }{
		{"fence mismatch", "queued nudge session fence mismatch"},
		{"ordinary transient", "tmux: transient write failure"},
		{"empty", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			item := newQueuedNudgeWithOptions("worker", "reminder", "queue", time.Now(), queuedNudgeOptions{ID: "n-other"})
			item.LastError = tc.lastErr
			if got := unconfirmedDropWarning(item); got != "" {
				t.Fatalf("warned about a non-unconfirmed failure (%s): %s", tc.name, got)
			}
		})
	}
}
