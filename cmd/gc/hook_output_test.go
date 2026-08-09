package main

import (
	"bytes"
	"encoding/json"
	"testing"
)

func TestWriteProviderHookContextGemini(t *testing.T) {
	var out bytes.Buffer
	err := writeProviderHookContextForEvent(&out, "gemini", "", "<system-reminder>\nhello\n</system-reminder>\n")
	if err != nil {
		t.Fatalf("writeProviderHookContextForEvent: %v", err)
	}

	var payload struct {
		HookSpecificOutput struct {
			AdditionalContext string `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal(out.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal output: %v\n%s", err, out.String())
	}
	if got, want := payload.HookSpecificOutput.AdditionalContext, "<system-reminder>\nhello\n</system-reminder>"; got != want {
		t.Fatalf("additionalContext = %q, want %q", got, want)
	}
}

func TestWriteProviderHookContextAntigravity(t *testing.T) {
	var out bytes.Buffer
	err := writeProviderHookContextForEvent(&out, "antigravity", "", "<system-reminder>\nhello\n</system-reminder>\n")
	if err != nil {
		t.Fatalf("writeProviderHookContextForEvent: %v", err)
	}

	var payload struct {
		InjectSteps []struct {
			EphemeralMessage string `json:"ephemeralMessage"`
		} `json:"injectSteps"`
	}
	if err := json.Unmarshal(out.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal output: %v\n%s", err, out.String())
	}
	if got, want := len(payload.InjectSteps), 1; got != want {
		t.Fatalf("len(injectSteps) = %d, want %d", got, want)
	}
	if got, want := payload.InjectSteps[0].EphemeralMessage, "<system-reminder>\nhello\n</system-reminder>"; got != want {
		t.Fatalf("ephemeralMessage = %q, want %q", got, want)
	}
}

func TestWriteProviderHookContextCodex(t *testing.T) {
	var out bytes.Buffer
	err := writeProviderHookContextForEvent(&out, "codex", "Stop", "<system-reminder>\nhello\n</system-reminder>\n")
	if err != nil {
		t.Fatalf("writeProviderHookContextForEvent: %v", err)
	}

	var payload struct {
		Decision string `json:"decision"`
		Reason   string `json:"reason"`
	}
	if err := json.Unmarshal(out.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal output: %v\n%s", err, out.String())
	}
	if got, want := payload.Decision, "block"; got != want {
		t.Fatalf("decision = %q, want %q", got, want)
	}
	if got, want := payload.Reason, "<system-reminder>\nhello\n</system-reminder>"; got != want {
		t.Fatalf("reason = %q, want %q", got, want)
	}
}

func TestWriteProviderHookContextCodexAdditionalContext(t *testing.T) {
	var out bytes.Buffer
	err := writeProviderHookContextForEvent(&out, "codex", "UserPromptSubmit", "<system-reminder>\nhello\n</system-reminder>\n")
	if err != nil {
		t.Fatalf("writeProviderHookContextForEvent: %v", err)
	}

	var payload struct {
		HookSpecificOutput struct {
			HookEventName     string `json:"hookEventName"`
			AdditionalContext string `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal(out.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal output: %v\n%s", err, out.String())
	}
	if got, want := payload.HookSpecificOutput.HookEventName, "UserPromptSubmit"; got != want {
		t.Fatalf("hookEventName = %q, want %q", got, want)
	}
	if got, want := payload.HookSpecificOutput.AdditionalContext, "<system-reminder>\nhello\n</system-reminder>"; got != want {
		t.Fatalf("additionalContext = %q, want %q", got, want)
	}
}

func TestWriteProviderHookContextCodexDefaultsSessionStartFromEnv(t *testing.T) {
	t.Setenv("GC_HOOK_EVENT_NAME", "SessionStart")

	var out bytes.Buffer
	err := writeProviderHookContextForEvent(&out, "codex", "", "<system-reminder>\nhello\n</system-reminder>\n")
	if err != nil {
		t.Fatalf("writeProviderHookContextForEvent: %v", err)
	}

	var payload struct {
		HookSpecificOutput struct {
			HookEventName     string `json:"hookEventName"`
			AdditionalContext string `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal(out.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal output: %v\n%s", err, out.String())
	}
	if got, want := payload.HookSpecificOutput.HookEventName, "SessionStart"; got != want {
		t.Fatalf("hookEventName = %q, want %q", got, want)
	}
	if got, want := payload.HookSpecificOutput.AdditionalContext, "<system-reminder>\nhello\n</system-reminder>"; got != want {
		t.Fatalf("additionalContext = %q, want %q", got, want)
	}
}

// codexPreCompactCommandOutputWire mirrors Codex 0.146's
// PreCompactCommandOutputWire: only universal output fields are accepted and
// unknown fields are rejected. Decoding with DisallowUnknownFields reproduces
// the rejection of hookSpecificOutput.
type codexPreCompactCommandOutputWire struct {
	Continue       *bool   `json:"continue"`
	StopReason     *string `json:"stopReason"`
	SuppressOutput *bool   `json:"suppressOutput"`
	SystemMessage  *string `json:"systemMessage"`
}

func TestWriteProviderHookContextCodexPreCompactUniversalOnly(t *testing.T) {
	for _, tc := range []struct {
		name      string
		eventName string
		envEvent  string
	}{
		{name: "explicit event", eventName: "PreCompact"},
		{name: "event from environment", envEvent: "PreCompact"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("GC_HOOK_EVENT_NAME", tc.envEvent)

			var out bytes.Buffer
			err := writeProviderHookContextForEvent(&out, "codex", tc.eventName, "Handoff: sent auto mail gc-abc12 (restart skipped).\n")
			if err != nil {
				t.Fatalf("writeProviderHookContextForEvent: %v", err)
			}

			dec := json.NewDecoder(bytes.NewReader(out.Bytes()))
			dec.DisallowUnknownFields()
			var payload codexPreCompactCommandOutputWire
			if err := dec.Decode(&payload); err != nil {
				t.Fatalf("PreCompact output rejected by Codex wire contract: %v\n%s", err, out.String())
			}
			if payload.SystemMessage == nil {
				t.Fatalf("systemMessage missing; handoff reference not preserved:\n%s", out.String())
			}
			if got, want := *payload.SystemMessage, "Handoff: sent auto mail gc-abc12 (restart skipped)."; got != want {
				t.Fatalf("systemMessage = %q, want %q", got, want)
			}
			if payload.Continue != nil && !*payload.Continue {
				t.Fatalf("PreCompact output set continue=false:\n%s", out.String())
			}
			if payload.StopReason != nil {
				t.Fatalf("PreCompact output set stopReason:\n%s", out.String())
			}
		})
	}
}

func TestWriteProviderHookContextPlain(t *testing.T) {
	var out bytes.Buffer
	err := writeProviderHookContextForEvent(&out, "", "", "<system-reminder>\nhello\n</system-reminder>\n")
	if err != nil {
		t.Fatalf("writeProviderHookContextForEvent: %v", err)
	}
	if got, want := out.String(), "<system-reminder>\nhello\n</system-reminder>\n"; got != want {
		t.Fatalf("output = %q, want %q", got, want)
	}
}
