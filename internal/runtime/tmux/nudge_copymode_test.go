package tmux

import (
	"context"
	"strings"
	"testing"
	"time"
)

// nudgeRoutingExecutor answers tmux queries by inspecting the arguments rather
// than by call position. NudgeSession issues a dozen probes whose order is an
// implementation detail, so a positional fake (fakeExecutor.outs) would pin the
// wrong thing and break on any reordering.
type nudgeRoutingExecutor struct {
	calls    [][]string
	inMode   string // reply to the #{pane_in_mode} probe: "1" parked, "0" not
	provider string // reply to show-environment GC_PROVIDER; "" = unset
	pane     string // reply to capture-pane (the busy/idle footer)
}

func (e *nudgeRoutingExecutor) execute(args []string) (string, error) {
	cp := make([]string, len(args))
	copy(cp, args)
	e.calls = append(e.calls, cp)

	joined := strings.Join(args, "\x00")
	switch {
	case strings.Contains(joined, "#{pane_in_mode}"):
		return e.inMode, nil
	case callHasTokens(args, "show-environment"):
		if e.provider == "" {
			return "", nil // unparseable -> providerEnv reports ""
		}
		return "GC_PROVIDER=" + e.provider, nil
	case callHasTokens(args, "capture-pane"):
		return e.pane, nil
	}
	return "", nil
}

func (e *nudgeRoutingExecutor) executeCtx(_ context.Context, args []string) (string, error) {
	return e.execute(args)
}

// nudgeTestConfig is DefaultConfig pinned to a test socket. NudgeSession needs
// non-zero ready and lock timeouts to get past its preflight at all.
func nudgeTestConfig() Config {
	cfg := DefaultConfig()
	cfg.SocketName = "x"
	return cfg
}

// countCallsWithTokens returns how many recorded calls contain every token.
func countCallsWithTokens(calls [][]string, tokens ...string) int {
	n := 0
	for _, c := range calls {
		if callHasTokens(c, tokens...) {
			n++
		}
	}
	return n
}

// TestNudgeSessionCancelsCopyModeBeforeDelivery pins gcw-3e62 defect 2. A pane
// parked in copy-mode (the ga-c4w WheelUpPane binding, or a human simply
// scrolling up) routes send-keys into copy-mode's key table, where the nudge's
// characters are COMMANDS, not input: the message is executed as navigation and
// then lost. cancelCopyModeIfParked already fixes this for SendKeysDebounced and
// Respond; NudgeSession — the path every order and every piece of mail takes —
// never called it.
//
// Measured live on codex 0.145.0 / tmux 3.6b before the fix: pane in copy-mode,
// nudge text + Enter delivered, composer empty afterwards and the pane still in
// copy-mode. After `send-keys -X cancel` the identical nudge landed.
func TestNudgeSessionCancelsCopyModeBeforeDelivery(t *testing.T) {
	t.Run("parked pane cancels copy-mode before the literal send", func(t *testing.T) {
		fe := &nudgeRoutingExecutor{inMode: "1", provider: "codex"}
		tm := &Tmux{cfg: nudgeTestConfig(), exec: fe}

		if err := tm.NudgeSession("sess", "hello"); err != nil {
			t.Fatalf("NudgeSession: %v", err)
		}

		probe := callIndexWithTokens(fe.calls, "display-message", "#{pane_in_mode}")
		cancel := callIndexWithTokens(fe.calls, "send-keys", "-X", "cancel")
		literal := callIndexWithTokens(fe.calls, "send-keys", "-l", "hello")

		if probe < 0 {
			t.Fatalf("expected a #{pane_in_mode} probe before delivery; calls=%v", fe.calls)
		}
		if cancel < 0 {
			t.Fatalf("parked pane: expected a copy-mode `-X cancel` before delivery; calls=%v", fe.calls)
		}
		if literal < 0 {
			t.Fatalf("nudge text was never delivered; calls=%v", fe.calls)
		}
		if cancel >= literal {
			t.Fatalf("copy-mode cancel (idx %d) must precede the literal send (idx %d); calls=%v", cancel, literal, fe.calls)
		}
	})

	t.Run("unparked pane issues no cancel and delivers normally", func(t *testing.T) {
		fe := &nudgeRoutingExecutor{inMode: "0", provider: "codex"}
		tm := &Tmux{cfg: nudgeTestConfig(), exec: fe}

		if err := tm.NudgeSession("sess", "hello"); err != nil {
			t.Fatalf("NudgeSession: %v", err)
		}

		if cancel := callIndexWithTokens(fe.calls, "send-keys", "-X", "cancel"); cancel >= 0 {
			t.Fatalf("unparked pane: spurious copy-mode cancel at idx %d; calls=%v", cancel, fe.calls)
		}
		if literal := callIndexWithTokens(fe.calls, "send-keys", "-l", "hello"); literal < 0 {
			t.Fatalf("happy path must still deliver the nudge text; calls=%v", fe.calls)
		}
	})
}

// TestNudgePaneCancelsCopyModeBeforeDelivery pins the same defect on the
// pane-targeted twin. NudgePane shares NudgeSession's delivery shape and so
// shares the copy-mode blind spot.
func TestNudgePaneCancelsCopyModeBeforeDelivery(t *testing.T) {
	fe := &nudgeRoutingExecutor{inMode: "1", provider: "codex"}
	tm := &Tmux{cfg: nudgeTestConfig(), exec: fe}

	if err := tm.NudgePane("%9", "hello"); err != nil {
		t.Fatalf("NudgePane: %v", err)
	}

	cancel := callIndexWithTokens(fe.calls, "send-keys", "-X", "cancel")
	literal := callIndexWithTokens(fe.calls, "send-keys", "-l", "hello")
	if cancel < 0 {
		t.Fatalf("parked pane: expected a copy-mode `-X cancel` before delivery; calls=%v", fe.calls)
	}
	if literal < 0 {
		t.Fatalf("nudge text was never delivered; calls=%v", fe.calls)
	}
	if cancel >= literal {
		t.Fatalf("copy-mode cancel (idx %d) must precede the literal send (idx %d); calls=%v", cancel, literal, fe.calls)
	}
}

// TestSubmitVerifyEligibleCoversCodex pins gcw-3e62 defect 1. submitEnterAndConfirm
// exists to survive a submit Enter lost to the paste race or a detached-pane
// wake — the "drafted but not submitted" stall William screenshotted. It was
// gated to the Claude family alone, so codex got a single unverified Enter and
// nothing retried.
//
// Codex qualifies on the gate's own stated criterion — a reliable busy
// indicator. paneContainsBusyIndicator already matches "esc to interrupt" and
// its own comment names Codex as a producer of that footer; captured live from
// codex 0.145.0 during a real turn.
func TestSubmitVerifyEligibleCoversCodex(t *testing.T) {
	tests := []struct {
		name     string
		provider string
		want     bool
	}{
		{name: "codex", provider: "codex", want: true},
		{name: "codex variant", provider: "openai-codex", want: true},
		{name: "claude stays eligible", provider: "claude", want: true},
		{name: "provider without a proven busy signal stays ineligible", provider: "kimi", want: false},
		{name: "grok stays ineligible", provider: "grok", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fe := &nudgeRoutingExecutor{provider: tt.provider}
			tm := &Tmux{cfg: nudgeTestConfig(), exec: fe}
			if got := tm.submitVerifyEligible("sess"); got != tt.want {
				t.Fatalf("submitVerifyEligible(provider=%q) = %v, want %v", tt.provider, got, tt.want)
			}
		})
	}
}

// TestNudgeSessionConfirmsSubmitForCodex proves the widened gate actually
// engages the confirmation loop end to end: against a pane that never reports
// busy, codex must now re-send Enter up to submitEnterMaxSends instead of
// firing one unverified Enter and declaring success.
func TestNudgeSessionConfirmsSubmitForCodex(t *testing.T) {
	fe := &nudgeRoutingExecutor{inMode: "0", provider: "codex", pane: "idle pane, no busy footer"}
	tm := &Tmux{cfg: nudgeTestConfig(), exec: fe}

	if err := tm.NudgeSession("sess", "hello"); err != nil {
		t.Fatalf("NudgeSession: %v", err)
	}

	enters := countCallsWithTokens(fe.calls, "send-keys", "Enter")
	if enters != submitEnterMaxSends {
		t.Fatalf("Enter sends = %d, want %d (confirmation loop must re-send while the pane stays idle); calls=%v",
			enters, submitEnterMaxSends, fe.calls)
	}
	if busyProbes := countCallsWithTokens(fe.calls, "capture-pane"); busyProbes == 0 {
		t.Fatalf("expected the busy indicator to be polled for codex; calls=%v", fe.calls)
	}
}

// TestNudgeBodyTakesBracketedPastePath pins the gcw-3e62 truncation. Codex
// 0.144.4 on Linux reads a send-keys -l burst as individual key events and
// keeps only whole 1024-byte chunks of it, silently. Measured against a live
// codex 0.144.4 TUI over tmux 3.6, composer readout in [Pasted Content N chars]:
//
//	bytes   send-keys -l     paste-buffer -p
//	 1000   1000             1000
//	 1025   1024  (-1)       1025
//	 2048   1024  (-1024)    2048
//	 3000   1024  (-1976)    3000
//	 6000   3072  (-2928)    6000
//
// A real order body is ~3000 bytes, so every order took the lossy path while
// the working one was reserved for bodies over 4096. The gate was backwards.
// pasteLiteralText's own comment already stated the correct intent — "so
// multiline nudges arrive as one paste operation instead of being interpreted
// as individual keypresses by provider TUIs" — the threshold just put it out of
// reach of every message that actually needed it.
func TestNudgeBodyTakesBracketedPastePath(t *testing.T) {
	// A realistic order body. This is the size that was silently truncated.
	body := strings.Repeat("x", 2998)

	fe := &nudgeRoutingExecutor{inMode: "0", provider: "codex"}
	tm := &Tmux{cfg: nudgeTestConfig(), exec: fe}

	if err := tm.sendKeysLiteralWithRetry("%1", body, time.Second); err != nil {
		t.Fatalf("sendKeysLiteralWithRetry: %v", err)
	}

	if idx := callIndexWithTokens(fe.calls, "load-buffer"); idx < 0 {
		t.Fatalf("an order-sized body must be delivered via a paste buffer, not keystrokes; calls=%v", fe.calls)
	}
	paste := callIndexWithTokens(fe.calls, "paste-buffer", "-p")
	if paste < 0 {
		t.Fatalf("paste must be bracketed (-p) so the TUI sees one paste, not N keypresses; calls=%v", fe.calls)
	}
	if idx := callIndexWithTokens(fe.calls, "send-keys", "-l", body); idx >= 0 {
		t.Fatalf("order-sized body still went out as literal keystrokes at idx %d", idx)
	}
}

// TestShortKeystrokePayloadStaysOnSendKeys keeps genuinely keystroke-shaped
// payloads off the paste path: a bracketed paste of a single approval digit is
// not a keypress, and a temp file per keystroke is waste.
func TestShortKeystrokePayloadStaysOnSendKeys(t *testing.T) {
	fe := &nudgeRoutingExecutor{inMode: "0", provider: "codex"}
	tm := &Tmux{cfg: nudgeTestConfig(), exec: fe}

	if err := tm.sendKeysLiteralWithRetry("%1", "yes", time.Second); err != nil {
		t.Fatalf("sendKeysLiteralWithRetry: %v", err)
	}
	if idx := callIndexWithTokens(fe.calls, "send-keys", "-l", "yes"); idx < 0 {
		t.Fatalf("a short payload must stay on send-keys -l; calls=%v", fe.calls)
	}
	if idx := callIndexWithTokens(fe.calls, "load-buffer"); idx >= 0 {
		t.Fatalf("a short payload must not allocate a paste buffer (idx %d); calls=%v", idx, fe.calls)
	}
}
