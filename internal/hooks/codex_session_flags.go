package hooks

import (
	"encoding/json"
	"fmt"
	iofs "io/fs"
	"path"

	"github.com/gastownhall/gascity/internal/bootstrap/packs/core"
	"github.com/gastownhall/gascity/internal/runtime"
)

const managedCodexHooksPath = "overlay/per-provider/codex/.codex/hooks.json"

type codexHookDocument struct {
	Hooks codexHookEvents `json:"hooks"`
}

type codexHookEvents struct {
	SessionStart     []runtime.CodexHookEntry `json:"SessionStart"`
	PreCompact       []runtime.CodexHookEntry `json:"PreCompact"`
	UserPromptSubmit []runtime.CodexHookEntry `json:"UserPromptSubmit"`
}

// ManagedCodexSessionFlags renders the embedded Gas City Codex lifecycle
// hooks as typed app-server session configuration bound to cityDir.
func ManagedCodexSessionFlags(cityDir string) (runtime.CodexSessionFlagsPayload, error) {
	data, err := iofs.ReadFile(core.PackFS, path.Clean(managedCodexHooksPath))
	if err != nil {
		return runtime.CodexSessionFlagsPayload{}, fmt.Errorf("reading managed Codex hooks: %w", err)
	}
	normalized, _, err := normalizeCodexHookCommands(data, cityDir)
	if err != nil {
		return runtime.CodexSessionFlagsPayload{}, fmt.Errorf("binding managed Codex hooks to city: %w", err)
	}
	var document codexHookDocument
	if err := json.Unmarshal(normalized, &document); err != nil {
		return runtime.CodexSessionFlagsPayload{}, fmt.Errorf("decoding managed Codex hooks: %w", err)
	}
	payload := runtime.NewCodexSessionFlagsPayload(runtime.CodexSessionConfig{
		FeaturesHooks:    true,
		BypassHookTrust:  true,
		SessionStart:     document.Hooks.SessionStart,
		PreCompact:       document.Hooks.PreCompact,
		UserPromptSubmit: document.Hooks.UserPromptSubmit,
	})
	if err := validateManagedCodexSessionFlags(payload); err != nil {
		return runtime.CodexSessionFlagsPayload{}, err
	}
	return payload, nil
}

func validateManagedCodexSessionFlags(payload runtime.CodexSessionFlagsPayload) error {
	if err := payload.Validate(); err != nil {
		return fmt.Errorf("validating managed Codex session flags: %w", err)
	}
	if !payload.Config.FeaturesHooks || !payload.Config.BypassHookTrust {
		return fmt.Errorf("validating managed Codex session flags: hooks feature and trust bypass must both be enabled")
	}

	counts := map[string]int{}
	events := []struct {
		name        string
		wantMatcher string
		entries     []runtime.CodexHookEntry
	}{
		{name: "SessionStart", wantMatcher: "startup", entries: payload.Config.SessionStart},
		{name: "PreCompact", wantMatcher: "", entries: payload.Config.PreCompact},
		{name: "UserPromptSubmit", wantMatcher: "", entries: payload.Config.UserPromptSubmit},
	}
	for _, event := range events {
		for _, entry := range event.entries {
			if entry.Matcher != event.wantMatcher {
				return fmt.Errorf("validating managed Codex session flags: %s matcher %q (want %q)", event.name, entry.Matcher, event.wantMatcher)
			}
			for _, hook := range entry.Hooks {
				if hook.Type != "command" {
					return fmt.Errorf("validating managed Codex session flags: %s handler type %q (want command)", event.name, hook.Type)
				}
				behavior := codexManagedBehavior(event.name, hook.Command)
				if behavior == "" {
					return fmt.Errorf("validating managed Codex session flags: unrecognized %s command %q", event.name, hook.Command)
				}
				counts[behavior]++
			}
		}
	}
	for _, behavior := range []string{"session-start", "pre-compact", "nudge", "mail"} {
		if counts[behavior] != 1 {
			return fmt.Errorf("validating managed Codex session flags: behavior %s appears %d times (want exactly once)", behavior, counts[behavior])
		}
	}
	return nil
}
