package runtime

import "strings"

// interactivePromptMarkers are strong textual signals that a terminal pane is
// showing a confirmation or approval prompt whose default action is bound to
// Enter. Matching any of these means a stray Enter keystroke would be consumed
// as the operator's answer.
var interactivePromptMarkers = []string{
	"Enter to confirm",
	"This command requires approval",
	"Approve edits?",
}

// IsAtInteractivePrompt reports whether a terminal pane capture shows an
// interactive selection or confirmation prompt that would consume a stray
// Enter keystroke as a menu choice — e.g. Claude Code's AskUserQuestion option
// menu, an MCP/trust selector, or a command-approval prompt.
//
// It is deliberately conservative to stay wake-safe: a bare shell/agent input
// prompt and ordinary numbered lists in assistant output return false, so a
// nudge to a genuinely idle session is never downgraded. Callers use it to
// defer a live nudge (queue it as a deferred reminder) instead of injecting
// text+Enter into an open prompt. See upstream gascity#2892.
func IsAtInteractivePrompt(paneText string) bool {
	if strings.TrimSpace(paneText) == "" {
		return false
	}
	for _, marker := range interactivePromptMarkers {
		if strings.Contains(paneText, marker) {
			return true
		}
	}
	// A cursor-selected numbered menu row (e.g. "❯ 1. Option") means the
	// selection cursor sits on a numbered choice, so Enter submits it. A bare
	// cursor prompt (no numbered row) and prose numbered lists (no cursor) are
	// intentionally excluded to avoid downgrading delivery to idle sessions.
	for _, line := range strings.Split(paneText, "\n") {
		trimmed := strings.ReplaceAll(line, " ", " ")
		trimmed = strings.TrimRight(trimmed, " \t")
		trimmed = stripLeadingBoxBorder(trimmed)
		if trimmed == "" {
			continue
		}
		for _, cursor := range []string{"❯", "›"} {
			if rest, ok := strings.CutPrefix(trimmed, cursor+" "); ok && isNumberedMenuRow(rest) {
				return true
			}
		}
	}
	return false
}
