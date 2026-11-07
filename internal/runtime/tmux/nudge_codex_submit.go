package tmux

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// ErrNudgeInputNotReady means no input was injected because the live composer
// did not expose a safe submit action. The caller may retry at a later boundary.
var ErrNudgeInputNotReady = errors.New("nudge: composer is not ready; no input injected")

type codexComposerState uint8

const (
	codexComposerUnknown codexComposerState = iota
	codexComposerEmpty
	codexComposerOwned
	codexComposerPaste
	codexComposerWrapped
	codexComposerOther
)

type codexSubmitObservation struct {
	composer codexComposerState
	busy     bool
	queue    bool
}

// codexSubmitFailure reports only mechanical state, never the payload, captured
// pane, or raw transport error. The cause remains available through errors.Is.
type codexSubmitFailure struct {
	phase         string
	observation   codexSubmitObservation
	keysAttempted int
	keysDelivered int
	lastKey       string
	cause         error
}

// Error describes tmux key-call outcomes, not provider acceptance.
func (e *codexSubmitFailure) Error() string {
	return fmt.Sprintf("nudge: submit unconfirmed (adapter=codex_draft key_transport=tmux phase=%s composer=%s busy=%t queue=%t keys_attempted=%d keys_delivered=%d last_key=%s)",
		e.phase, codexComposerName(e.observation.composer), e.observation.busy, e.observation.queue, e.keysAttempted, e.keysDelivered, e.lastKey)
}

// Unwrap retains the queue's existing classification and the transport cause.
func (e *codexSubmitFailure) Unwrap() []error {
	if e.cause != nil {
		return []error{ErrNudgeSubmitUnconfirmed, e.cause}
	}
	return []error{ErrNudgeSubmitUnconfirmed}
}

func codexComposerName(state codexComposerState) string {
	switch state {
	case codexComposerEmpty:
		return "empty"
	case codexComposerOwned:
		return "owned"
	case codexComposerPaste:
		return "paste"
	case codexComposerWrapped:
		return "wrapped"
	case codexComposerOther:
		return "other"
	default:
		return "unknown"
	}
}

var (
	codexSGR     = regexp.MustCompile("\x1b\\[[0-9;]*m")
	codexDimText = regexp.MustCompile("\x1b\\[2m([^\x1b]*)(?:\x1b\\[(?:0|22)m|$)")
)

// captureCodexComposer retains terminal styling to distinguish a dim input
// placeholder from identical words typed by a human, and rejoins soft wraps.
func (t *Tmux) captureCodexComposer(target string) ([]string, error) {
	raw, err := t.run("capture-pane", "-p", "-e", "-J", "-t", paneTarget(target), "-S", fmt.Sprintf("-%d", promptObservationLines))
	if err != nil {
		return nil, err
	}
	return strings.Split(raw, "\n"), nil
}

func codexPastePlaceholder(content string) bool {
	content = strings.TrimRight(content, " ")
	if !strings.HasPrefix(content, "[Pasted Content ") || !strings.HasSuffix(content, " chars]") {
		return false
	}
	count, err := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(content, "[Pasted Content "), " chars]"))
	return err == nil && count > 0
}

// readCodexComposer reads only the last live composer, including wrapped lines.
// A missing or cropped composer cannot establish consumption or input ownership.
func readCodexComposer(lines []string, message string) codexSubmitObservation {
	rawLines := lines
	lines = make([]string, len(rawLines))
	for i, raw := range rawLines {
		lines[i] = codexSGR.ReplaceAllString(raw, "")
	}
	obs := codexSubmitObservation{}
	start := -1
	var text []string
	for i, line := range lines {
		if strings.HasPrefix(line, "› ") || line == "›" {
			start = i
		}
	}
	if start < 0 {
		return obs
	}
	// Only the status adjacent to the live composer can describe its turn.
	// Draft text and older transcript output are not busy-state evidence.
	for i := start - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if line == "" {
			continue
		}
		obs.busy = (strings.HasPrefix(line, "• Working") || strings.HasPrefix(line, "Working (")) && strings.Contains(line, "esc to interrupt")
		break
	}
	// Footer controls belong to the trailing footer block, never to an
	// arbitrary line within the draft. Keep hard newlines and input spaces;
	// capture-pane -J already joins terminal soft wraps.
	end := len(lines)
	for end > start+1 && strings.TrimSpace(lines[end-1]) == "" {
		end--
	}
	footer := end
	for footer > start+1 && codexFooterLine(strings.TrimSpace(lines[footer-1])) {
		footer--
	}
	if footer < end {
		// Consume exactly one separator. Earlier blank lines remain input;
		// a draft may itself contain footer-like text or trailing newlines.
		if footer <= start+1 || strings.TrimSpace(lines[footer-1]) != "" {
			return obs
		}
		for _, line := range lines[footer:end] {
			if strings.HasPrefix(strings.TrimSpace(line), "tab to queue message") {
				obs.queue, obs.busy = true, true
			}
		}
		end = footer - 1
	}
	first := strings.TrimPrefix(lines[start], "› ")
	if lines[start] == "›" {
		first = ""
	}
	text = append(text, first)
	renderedBlankRows := false
	for _, line := range lines[start+1 : end] {
		if line == " " {
			// Styled joined captures render an empty input row with one padding
			// space. This is ambiguous, so only the current paste may own it.
			text = append(text, "")
			renderedBlankRows = true
			continue
		}
		if !strings.HasPrefix(line, "  ") && line != "" {
			return obs
		}
		text = append(text, strings.TrimPrefix(line, "  "))
	}
	content := strings.Join(text, "\n")
	expected := message
	switch {
	case content == "":
		obs.composer = codexComposerEmpty
	case codexDimPlaceholder(rawLines[start], content):
		obs.composer = codexComposerEmpty
	case codexPastePlaceholder(content):
		obs.composer = codexComposerPaste
	case expected != "" && content == expected && !renderedBlankRows:
		obs.composer = codexComposerOwned
	case codexRenderedDraftMatches(text, expected):
		obs.composer = codexComposerWrapped
	default:
		obs.composer = codexComposerOther
	}
	return obs
}

// codexRenderedDraftMatches recognizes TUI word wrapping without collapsing
// input whitespace. This rendering is ambiguous with a human's hard newlines,
// so it establishes ownership only for a draft pasted by the current attempt.
func codexRenderedDraftMatches(lines []string, message string) bool {
	remaining := message
	for i, line := range lines {
		if !strings.HasPrefix(remaining, line) {
			return false
		}
		remaining = remaining[len(line):]
		if i == len(lines)-1 {
			return remaining == ""
		}
		switch {
		case strings.HasPrefix(remaining, "\n"):
			remaining = remaining[1:]
		case line != "" && strings.HasPrefix(remaining, " "):
			remaining = remaining[1:]
		case line == "" || remaining == "":
			return false
		default:
			// The TUI also breaks long words and punctuation-delimited tokens
			// without inserting whitespace. Leave remaining intact so the next
			// rendered line must match its exact prefix.
		}
	}
	return false
}

func codexFooterLine(line string) bool {
	return strings.HasPrefix(line, "tab to queue message") ||
		strings.HasPrefix(strings.ToLower(line), "gpt-") ||
		strings.Contains(line, "% context left") || strings.Contains(line, "? for shortcuts")
}

func codexDimPlaceholder(raw, content string) bool {
	for _, match := range codexDimText.FindAllStringSubmatch(raw, -1) {
		if strings.TrimRight(content, " ") == match[1] {
			return true
		}
	}
	return false
}

// submitCodexDraft confirms consumption of this injection, never an existing
// busy turn. Only this call's paste may own an anonymous paste placeholder.
func submitCodexDraft(ownPaste bool, observe func() (codexSubmitObservation, error), send func(string) error, sleep func(time.Duration)) error {
	sawDraft := false
	sent := false
	failure := &codexSubmitFailure{phase: "budget", lastKey: "none"}
	for poll := 0; poll < submitDraftRecoverySends*submitConfirmPollsPerSend; poll++ {
		failure.phase = "budget"
		obs, err := observe()
		failure.observation = obs
		if err != nil {
			failure.phase, failure.cause = "observe", err
			return failure
		}
		switch obs.composer {
		case codexComposerEmpty:
			if sawDraft && sent {
				return nil
			}
		case codexComposerOther:
			failure.phase = "ownership"
			return failure
		case codexComposerOwned, codexComposerPaste, codexComposerWrapped:
			if (obs.composer == codexComposerPaste || obs.composer == codexComposerWrapped) && !ownPaste {
				failure.phase = "ownership"
				return failure
			}
			sawDraft = true
			if poll%submitConfirmPollsPerSend == 0 {
				key := "Enter"
				if obs.busy {
					if !obs.queue {
						sleep(submitConfirmPollInterval)
						continue
					}
					key = "Tab"
				}
				failure.keysAttempted++
				failure.lastKey = key
				if err := send(key); err != nil {
					failure.phase, failure.cause = "submit", err
					return failure
				}
				failure.keysDelivered++
				sent = true
			}
		}
		if obs.composer == codexComposerUnknown {
			// Retain the existing observation budget; this label does not
			// authorize a key or alter the delivery decision.
			failure.phase = "ownership"
		}
		sleep(submitConfirmPollInterval)
	}
	return failure
}
