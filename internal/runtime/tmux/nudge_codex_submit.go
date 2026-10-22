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
	for poll := 0; poll < submitDraftRecoverySends*submitConfirmPollsPerSend; poll++ {
		obs, err := observe()
		if err != nil {
			return errors.Join(ErrNudgeSubmitUnconfirmed, err)
		}
		switch obs.composer {
		case codexComposerEmpty:
			if sawDraft && sent {
				return nil
			}
		case codexComposerOther:
			return ErrNudgeSubmitUnconfirmed
		case codexComposerOwned, codexComposerPaste, codexComposerWrapped:
			if (obs.composer == codexComposerPaste || obs.composer == codexComposerWrapped) && !ownPaste {
				return ErrNudgeSubmitUnconfirmed
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
				if err := send(key); err != nil {
					return errors.Join(ErrNudgeSubmitUnconfirmed, err)
				}
				sent = true
			}
		}
		sleep(submitConfirmPollInterval)
	}
	return ErrNudgeSubmitUnconfirmed
}
