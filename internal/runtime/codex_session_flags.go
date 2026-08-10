package runtime

import (
	"fmt"
	"reflect"
)

const (
	// CodexSessionFlagsVersion is the Gas City/T3 transport schema version for
	// session-scoped provider configuration.
	CodexSessionFlagsVersion = 1
	// CodexSessionFlagsProvider is the provider discriminator accepted by the
	// v1 session-flags schema.
	CodexSessionFlagsProvider = "codex"
)

// CodexCommandHook is a command handler in the Codex hooks schema.
type CodexCommandHook struct {
	Type    string `json:"type"`
	Command string `json:"command"`
}

// CodexHookEntry groups command handlers under one Codex hook matcher.
type CodexHookEntry struct {
	Matcher string             `json:"matcher"`
	Hooks   []CodexCommandHook `json:"hooks"`
}

// CodexSessionConfig is the typed per-session Codex configuration forwarded to
// thread/start and thread/resume. Dotted JSON keys are Codex app-server config
// overrides, not filesystem paths.
type CodexSessionConfig struct {
	FeaturesHooks    bool             `json:"features.hooks"`
	BypassHookTrust  bool             `json:"bypass_hook_trust"`
	SessionStart     []CodexHookEntry `json:"hooks.SessionStart"`
	PreCompact       []CodexHookEntry `json:"hooks.PreCompact"`
	UserPromptSubmit []CodexHookEntry `json:"hooks.UserPromptSubmit"`
}

// Clone returns a deep copy of the dotted Codex configuration.
func (f CodexSessionConfig) Clone() CodexSessionConfig {
	f.SessionStart = cloneCodexHookEntries(f.SessionStart)
	f.PreCompact = cloneCodexHookEntries(f.PreCompact)
	f.UserPromptSubmit = cloneCodexHookEntries(f.UserPromptSubmit)
	return f
}

func cloneCodexHookEntries(entries []CodexHookEntry) []CodexHookEntry {
	if entries == nil {
		return nil
	}
	out := make([]CodexHookEntry, len(entries))
	for i, entry := range entries {
		out[i] = entry
		out[i].Hooks = append([]CodexCommandHook(nil), entry.Hooks...)
	}
	return out
}

// CodexSessionFlagsPayload is the versioned, provider-specific value carried
// on a T3 thread. Config remains typed so arbitrary metadata cannot become
// Codex app-server configuration.
type CodexSessionFlagsPayload struct {
	Version  int                `json:"version"`
	Provider string             `json:"provider"`
	Config   CodexSessionConfig `json:"config"`
}

// NewCodexSessionFlagsPayload wraps Codex session flags for the T3 transport.
func NewCodexSessionFlagsPayload(flags CodexSessionConfig) CodexSessionFlagsPayload {
	return CodexSessionFlagsPayload{
		Version:  CodexSessionFlagsVersion,
		Provider: CodexSessionFlagsProvider,
		Config:   flags,
	}
}

// Clone returns a deep copy of the versioned payload.
func (p *CodexSessionFlagsPayload) Clone() *CodexSessionFlagsPayload {
	if p == nil {
		return nil
	}
	out := *p
	out.Config = p.Config.Clone()
	return &out
}

// Equal reports whether two payloads describe identical provider session
// configuration.
func (p *CodexSessionFlagsPayload) Equal(other *CodexSessionFlagsPayload) bool {
	return reflect.DeepEqual(p, other)
}

// Validate rejects session-flags payloads that this Gas City/T3 contract does
// not understand.
func (p CodexSessionFlagsPayload) Validate() error {
	if p.Version != CodexSessionFlagsVersion {
		return fmt.Errorf("unsupported session flags version %d (want %d)", p.Version, CodexSessionFlagsVersion)
	}
	if p.Provider != CodexSessionFlagsProvider {
		return fmt.Errorf("unsupported session flags provider %q (want %q)", p.Provider, CodexSessionFlagsProvider)
	}
	return nil
}
