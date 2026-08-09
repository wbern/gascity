package main

import (
	"encoding/json"
	"io"
	"os"
	"strings"
)

const (
	hookOutputFormatAntigravity = "antigravity"
	hookOutputFormatCodex       = "codex"
	hookOutputFormatGemini      = "gemini"
)

func writeProviderHookContextForEvent(stdout io.Writer, format, eventName, content string) error {
	if content == "" {
		return nil
	}
	switch strings.ToLower(strings.TrimSpace(format)) {
	case hookOutputFormatAntigravity:
		return json.NewEncoder(stdout).Encode(antigravityHookAdditionalContext(content))
	case hookOutputFormatCodex:
		return json.NewEncoder(stdout).Encode(codexHookOutput(eventName, content))
	case hookOutputFormatGemini:
		return json.NewEncoder(stdout).Encode(geminiHookAdditionalContext(content))
	}
	_, err := io.WriteString(stdout, content)
	return err
}

func antigravityHookAdditionalContext(content string) map[string]any {
	return map[string]any{
		"injectSteps": []map[string]any{
			{"ephemeralMessage": strings.TrimRight(content, "\n")},
		},
	}
}

func codexHookOutput(eventName, content string) map[string]any {
	eventName = codexHookEventName(eventName)
	if strings.EqualFold(eventName, "Stop") {
		return map[string]any{
			"decision": "block",
			"reason":   strings.TrimRight(content, "\n"),
		}
	}
	if strings.EqualFold(eventName, "PreCompact") {
		return codexPreCompactOutput(content)
	}
	return codexHookAdditionalContext(eventName, content)
}

// codexPreCompactOutput builds a Codex PreCompact hook payload using only the
// universal fields Codex 0.146 accepts. Its PreCompact wire type rejects
// hookSpecificOutput, unlike SessionStart and UserPromptSubmit. The handoff
// reference travels in systemMessage; the durable continuation is the
// persisted auto-handoff mail.
func codexPreCompactOutput(content string) map[string]any {
	msg := strings.TrimRight(content, "\n")
	if msg == "" {
		return map[string]any{}
	}
	return map[string]any{
		"systemMessage": msg,
	}
}

func codexHookAdditionalContext(eventName, content string) map[string]any {
	eventName = codexHookEventName(eventName)
	return map[string]any{
		"hookSpecificOutput": map[string]any{
			"hookEventName":     eventName,
			"additionalContext": strings.TrimRight(content, "\n"),
		},
	}
}

func codexHookEventName(eventName string) string {
	eventName = strings.TrimSpace(eventName)
	if eventName == "" {
		eventName = strings.TrimSpace(os.Getenv("GC_HOOK_EVENT_NAME"))
	}
	if eventName == "" {
		eventName = "SessionStart"
	}
	return eventName
}

func geminiHookAdditionalContext(content string) map[string]any {
	return map[string]any{
		"hookSpecificOutput": map[string]any{
			"additionalContext": strings.TrimRight(content, "\n"),
		},
	}
}
