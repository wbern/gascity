package tmux

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestCodexComposerReadsExactInjection(t *testing.T) {
	for _, tc := range []struct {
		name    string
		lines   []string
		message string
		state   codexComposerState
		queue   bool
	}{
		{"raw multiline busy", []string{"• Working (esc to interrupt)", "› <system-reminder>", "  do work", "  </system-reminder>", "", "  tab to queue message   75% context left"}, "<system-reminder>\ndo work\n</system-reminder>", codexComposerOwned, true},
		{"placeholder with terminal padding", []string{"› [Pasted Content 4414 chars]                                                        ", "", "tab to queue message"}, "message", codexComposerPaste, true},
		{"anonymous marker collides with payload", []string{"› [Pasted Content 12 chars]", "", "GPT-6 low"}, "[Pasted Content 12 chars]", codexComposerPaste, false},
		{"placeholder busy", []string{"› [Pasted Content 2048 chars]", "", "tab to queue message"}, "message", codexComposerPaste, true},
		{"hard newline is not soft wrap", []string{"› first line with", "  wrapping", "", "gpt-6 low"}, "first line with wrapping", codexComposerWrapped, false},
		{"TUI wraps after slash without whitespace", []string{"› Delivery diagnostic. Mail gas-city-wbern/", "  architect now.", "", "GPT-6.1-Sol low"}, "Delivery diagnostic. Mail gas-city-wbern/architect now.", codexComposerWrapped, false},
		{"TUI wraps inside long token", []string{"› nonce_abcdef", "  ghijkl", "", "GPT-6.1-Sol low"}, "nonce_abcdefghijkl", codexComposerWrapped, false},
		{"inserted blank line is not token wrap", []string{"› nonce_abcdef", "  ", "  ghijkl", "", "GPT-6.1-Sol low"}, "nonce_abcdefghijkl", codexComposerOther, false},
		{"wrapped token changed", []string{"› nonce_abcdef", "  ghijXl", "", "GPT-6.1-Sol low"}, "nonce_abcdefghijkl", codexComposerOther, false},
		{"joined soft wrap", []string{"› first line with wrapping", "", "gpt-6 low"}, "first line with wrapping", codexComposerOwned, false},
		{"unexpected trailing newline", []string{"› hello", "  ", "", "gpt-6 low"}, "hello", codexComposerOther, false},
		{"trailing newline", []string{"› hello", "  ", "", "gpt-6 low"}, "hello\n", codexComposerOwned, false},
		{"exact input spaces", []string{"› hello  world", "", "gpt-6 low"}, "hello world", codexComposerOther, false},
		{"human prefix collision", []string{"› same prefix but human text", "", "gpt-6 low"}, "same prefix", codexComposerOther, false},
		{"placeholder with human suffix", []string{"› [Pasted Content 12 chars] human addition"}, "message", codexComposerOther, false},
		{"empty busy", []string{"• Working (esc to interrupt)", "› ", "", "tab to queue message"}, "message", codexComposerEmpty, true},
		{"idle hint with terminal padding", []string{"› \x1b[2mAsk Codex to do anything\x1b[0m                                                       ", "", "GPT-6 low"}, "message", codexComposerEmpty, false},
		{"idle hint", []string{"› \x1b[2mAsk Codex to do anything\x1b[0m", "", "GPT-6.1-Sol low"}, "message", codexComposerEmpty, false},
		{"dim reset deferred to next line", []string{"› \x1b[2mExplain this codebase", "", "\x1b[0m  tab to queue message", "gpt-5.5 low"}, "message", codexComposerEmpty, true},
		{"human types placeholder words", []string{"› Ask Codex to do anything", "", "GPT-6 low"}, "message", codexComposerOther, false},
		{"malformed placeholder", []string{"› [Pasted Content 12 chars] plus [human]"}, "message", codexComposerOther, false},
		{"cropped", []string{"• Working (esc to interrupt)"}, "message", codexComposerUnknown, false},
		{"last composer only", []string{"› message", "• completed", "› human draft"}, "message", codexComposerOther, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			obs := readCodexComposer(tc.lines, tc.message)
			if obs.composer != tc.state || obs.queue != tc.queue {
				t.Fatalf("observation=%+v want state=%v queue=%v", obs, tc.state, tc.queue)
			}
		})
	}
}

func TestCodexSubmitRequiresDraftConsumption(t *testing.T) {
	for _, tc := range []struct {
		name         string
		ownPaste     bool
		observations []codexSubmitObservation
		wantKey      string
		wantSuccess  bool
		wantSends    int
		probeErr     bool
	}{
		{"already busy raw remains", false, []codexSubmitObservation{{composer: codexComposerOwned, busy: true, queue: true}}, "Tab", false, submitDraftRecoverySends, false},
		{"already busy placeholder remains", true, []codexSubmitObservation{{composer: codexComposerPaste, busy: true, queue: true}}, "Tab", false, submitDraftRecoverySends, false},
		{"busy queued and consumed", false, []codexSubmitObservation{{composer: codexComposerOwned, busy: true, queue: true}, {composer: codexComposerEmpty, busy: true, queue: true}}, "Tab", true, 1, false},
		{"idle consumed before busy renders", false, []codexSubmitObservation{{composer: codexComposerOwned}, {composer: codexComposerEmpty}}, "Enter", true, 1, false},
		{"own placeholder consumed", true, []codexSubmitObservation{{composer: codexComposerPaste}, {composer: codexComposerEmpty}}, "Enter", true, 1, false},
		{"unowned placeholder", false, []codexSubmitObservation{{composer: codexComposerPaste, busy: true, queue: true}}, "", false, 0, false},
		{"busy without queue hint", true, []codexSubmitObservation{{composer: codexComposerOwned, busy: true}}, "", false, 0, false},
		{"unknown", true, []codexSubmitObservation{{composer: codexComposerUnknown, busy: true}}, "", false, 0, false},
		{"empty before own draft observed", true, []codexSubmitObservation{{composer: codexComposerEmpty, busy: true}}, "", false, 0, false},
		{"human replaces own draft", true, []codexSubmitObservation{{composer: codexComposerOwned}, {composer: codexComposerOther, busy: true}}, "Enter", false, 1, false},
		{"probe failure", true, nil, "", false, 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reads := 0
			var keys []string
			err := submitCodexDraft(tc.ownPaste, func() (codexSubmitObservation, error) {
				if tc.probeErr {
					return codexSubmitObservation{}, errors.New("capture failed")
				}
				i := reads
				reads++
				if i >= len(tc.observations) {
					i = len(tc.observations) - 1
				}
				return tc.observations[i], nil
			}, func(key string) error { keys = append(keys, key); return nil }, noSleep)
			if (err == nil) != tc.wantSuccess || len(keys) != tc.wantSends {
				t.Fatalf("err=%v keys=%v", err, keys)
			}
			for _, key := range keys {
				if key != tc.wantKey || key == "Escape" {
					t.Fatalf("unsafe submit key %q", key)
				}
			}
		})
	}
}

func TestCodexNudgeQueuesOwnDraftWithoutEscape(t *testing.T) {
	for _, attached := range []string{"0", "1"} {
		for _, placeholder := range []bool{false, true} {
			t.Run(fmt.Sprintf("attached=%s/placeholder=%v", attached, placeholder), func(t *testing.T) {
				fe := &nudgeRoutingExecutor{inMode: "0", provider: "codex", attached: attached, pane: "• Working (esc to interrupt)\n› \x1b[2mAsk Codex to do anything\x1b[0m\n\ntab to queue message"}
				fe.errFor = func(args []string) error {
					if callHasTokens(args, "send-keys", "Tab") {
						fe.pane = "• Working (esc to interrupt)\n› \x1b[2mAsk Codex to do anything\x1b[0m\n\ntab to queue message"
					}
					return nil
				}
				tm := &Tmux{cfg: nudgeTestConfig(), exec: fe}
				message := "<system-reminder>\nwork the exact bead\n</system-reminder>"
				pastes := 0
				err := tm.nudgeSession("sess", message, func(_ string, text string, _ time.Duration) error {
					pastes++
					if text != message {
						t.Fatal("payload changed")
					}
					if placeholder {
						fe.pane = "• Working (esc to interrupt)\n› [Pasted Content 80 chars]\n\ntab to queue message"
					} else {
						fe.pane = "• Working (esc to interrupt)\n› " + strings.ReplaceAll(message, "\n", "\n  ") + "\n\ntab to queue message"
					}
					return nil
				}, func(string) bool { return true }, func(string) []string { return []string{"Escape", "Enter"} })
				if err != nil || pastes != 1 {
					t.Fatalf("err=%v pastes=%d", err, pastes)
				}
				if got := countCallsWithTokens(fe.calls, "send-keys", "Tab"); got != 1 {
					t.Fatalf("Tab sends=%d", got)
				}
				for _, key := range []string{"Escape", "C-u", "Enter"} {
					if countCallsWithTokens(fe.calls, "send-keys", key) != 0 {
						t.Fatalf("unexpected %s", key)
					}
				}
			})
		}
	}
}

func TestCodexNudgePreservesAttachedHumanDraft(t *testing.T) {
	fe := &nudgeRoutingExecutor{inMode: "0", provider: "codex", attached: "1", pane: "› human draft\n\nGPT-6 low"}
	tm := &Tmux{cfg: nudgeTestConfig(), exec: fe}
	pasted := false
	err := tm.nudgeSession("sess", "injected reminder", func(string, string, time.Duration) error { pasted = true; return nil }, func(string) bool { return false }, func(string) []string { return []string{"Escape", "Enter"} })
	if !errors.Is(err, ErrNudgeSubmitUnconfirmed) || pasted {
		t.Fatalf("err=%v pasted=%v", err, pasted)
	}
	if got := countCallsWithTokens(fe.calls, "send-keys"); got != 0 {
		t.Fatalf("human draft received keys: %v", fe.calls)
	}
}

func TestCodexSubmitStopsAfterSingleConsumption(t *testing.T) {
	keys := []string{}
	err := submitCodexDraft(true, func() (codexSubmitObservation, error) {
		if len(keys) > 0 {
			return codexSubmitObservation{composer: codexComposerEmpty, busy: true}, nil
		}
		return codexSubmitObservation{composer: codexComposerOwned, busy: true, queue: true}, nil
	}, func(key string) error { keys = append(keys, key); return nil }, noSleep)
	if err != nil || !reflect.DeepEqual(keys, []string{"Tab"}) {
		t.Fatalf("err=%v keys=%v", err, keys)
	}
}

func TestCodexComposerRejectsFooterLikeHumanDraft(t *testing.T) {
	for _, body := range [][]string{
		{"› hello", "  gpt-human addition", "", "GPT-6 low"},
		{"› human draft", "  › hello", "", "GPT-6 low"},
		{"› ›", "", "GPT-6 low"},
		{"› hello", "", "  gpt-human addition", "", "GPT-6 low"},
		{"› ", "  tab to queue message", "  private addition", "", "GPT-6 low"},
		{"› hello", "  75% context left", "  private addition", "", "GPT-6 low"},
		{"› hello", "  ? for shortcuts", "  private addition", "", "GPT-6 low"},
		{"› delete", "  everything", "", "GPT-6 low"},
	} {
		message := "hello"
		if strings.HasPrefix(body[0], "› delete") {
			message = "delete everything"
		}
		obs := readCodexComposer(body, message)
		if obs.composer == codexComposerOwned || obs.composer == codexComposerEmpty {
			t.Errorf("foreign input classified as owned/empty: %q %+v", body, obs)
		}
		fe := &nudgeRoutingExecutor{inMode: "0", provider: "codex", attached: "1", pane: strings.Join(body, "\n")}
		tm := &Tmux{cfg: nudgeTestConfig(), exec: fe}
		pasted := false
		err := tm.nudgeSession("sess", message, func(string, string, time.Duration) error {
			pasted = true
			return nil
		}, func(string) bool { return false }, func(string) []string { return []string{"Escape", "Enter"} })
		if !errors.Is(err, ErrNudgeSubmitUnconfirmed) || pasted || countCallsWithTokens(fe.calls, "send-keys") != 0 {
			t.Errorf("foreign draft received input: err=%v pasted=%v calls=%v", err, pasted, fe.calls)
		}
	}
}

func TestCodexComposerDoesNotReadDraftAsBusy(t *testing.T) {
	message := "explain esc to interrupt"
	obs := readCodexComposer([]string{"› " + message, "", "gpt-6 low"}, message)
	if obs.busy || obs.composer != codexComposerOwned {
		t.Fatalf("draft was interpreted as live status: %+v", obs)
	}
}

func TestCodexComposerRecognizesLivePlainBusyStatus(t *testing.T) {
	obs := readCodexComposer([]string{"Working (3m 41s • esc to interrupt) · 1 background terminal running", "", "› reminder", "", "GPT-6.1-Sol low"}, "reminder")
	if !obs.busy || obs.queue || obs.composer != codexComposerOwned {
		t.Fatalf("live busy status lost before queue hint renders: %+v", obs)
	}
}

func TestCodexNudgeHandlesOnlyFreshlyPastedRenderedWraps(t *testing.T) {
	for _, tc := range []struct{ message, rendered string }{
		{"first line with wrapping", "first line with\n  wrapping"},
		{"mail gas-city-wbern/architect", "mail gas-city-wbern/\n  architect"},
		{"nonce_abcdefghijkl", "nonce_abcdef\n  ghijkl"},
	} {
		for _, existing := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/existing=%v", tc.message, existing), func(t *testing.T) {
				empty := "Working (1s • esc to interrupt)\n› \x1b[2mAsk Codex to do anything\x1b[0m\n\ntab to queue message"
				wrapped := "Working (1s • esc to interrupt)\n› " + tc.rendered + "\n\ntab to queue message"
				fe := &nudgeRoutingExecutor{inMode: "0", provider: "codex", attached: "1", pane: empty}
				if existing {
					fe.pane = wrapped
				}
				fe.errFor = func(args []string) error {
					if callHasTokens(args, "send-keys", "Tab") {
						fe.pane = empty
					}
					return nil
				}
				tm := &Tmux{cfg: nudgeTestConfig(), exec: fe}
				pastes := 0
				err := tm.nudgeSession("sess", tc.message, func(string, string, time.Duration) error {
					pastes++
					fe.pane = wrapped
					return nil
				}, func(string) bool { return false }, func(string) []string { return []string{"Enter"} })
				if existing {
					if !errors.Is(err, ErrNudgeSubmitUnconfirmed) || pastes != 0 || countCallsWithTokens(fe.calls, "send-keys") != 0 {
						t.Fatalf("existing ambiguous input received keys: err=%v pastes=%d calls=%v", err, pastes, fe.calls)
					}
				} else if err != nil || pastes != 1 || countCallsWithTokens(fe.calls, "send-keys", "Tab") != 1 {
					t.Fatalf("own rendered draft not queued: err=%v pastes=%d calls=%v", err, pastes, fe.calls)
				}
			})
		}
	}
}

func TestCodexBusyWithoutQueueActionDoesNotStageReminder(t *testing.T) {
	fe := &nudgeRoutingExecutor{inMode: "0", provider: "codex", attached: "1", pane: "Working (18s • esc to interrupt)\n\n› \x1b[2mAsk Codex to do anything\x1b[0m\n\nGPT-6.1-Sol … Goal achieved (2m)    ⚠ 1 warning · f2 to view"}
	tm := &Tmux{cfg: nudgeTestConfig(), exec: fe}
	message := "<system-reminder>\nYou have a deferred reminder that was queued until a safe boundary:\n\n- [session] narrow reminder\n\nHandle them after this turn.\n</system-reminder>\n"
	pastes := 0
	err := tm.nudgeSession("sess", message, func(_ string, text string, _ time.Duration) error {
		pastes++
		fe.pane = "Working (18s • esc to interrupt)\n\n› " + strings.ReplaceAll(text, "\n", "\n  ") + "\n\nGPT-6.1-Sol … Goal achieved (2m)    ⚠ 1 warning · f2 to view"
		return nil
	}, func(string) bool { return false }, func(string) []string { return []string{"Enter"} })
	if !errors.Is(err, ErrNudgeInputNotReady) || pastes != 0 || countCallsWithTokens(fe.calls, "send-keys", "Enter") != 0 || countCallsWithTokens(fe.calls, "send-keys", "Tab") != 0 {
		t.Fatalf("busy unsafe boundary mutated input: err=%v pastes=%d calls=%v", err, pastes, fe.calls)
	}
}
