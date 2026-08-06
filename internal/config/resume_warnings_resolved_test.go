package config

import (
	"strings"
	"testing"
)

// brokenResumeWrapper has no "$@" and no $0 placeholder, so every appended
// option flag is discarded by the shell.
const brokenResumeWrapper = `/bin/sh -c 'exec launcher -- claude --resume {{.SessionKey}}'`

// halfFixedResumeWrapper has "$@" but NO $0 placeholder, so the FIRST appended
// flag is still eaten as the script name. This is the shape the detector must
// keep reporting — it reads as success while still losing a flag.
const halfFixedResumeWrapper = `/bin/sh -c 'exec launcher -- claude --resume {{.SessionKey}} "$@"'`

func warnedProviders(t *testing.T, providers map[string]ProviderSpec) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	for _, w := range ResumeCommandWarnings(providers) {
		for name := range providers {
			if strings.Contains(w, `provider "`+name+`"`) {
				out[name] = true
			}
		}
	}
	return out
}

// TestResumeWarningsSeeInheritedResumeCommand pins gcw-84kg's core defect: the
// warning loop must evaluate the RESOLVED provider, not the raw spec. A derived
// provider inherits the broken resume_command from its base and is genuinely
// affected, so it must be named.
func TestResumeWarningsSeeInheritedResumeCommand(t *testing.T) {
	base := "base-claude"
	providers := map[string]ProviderSpec{
		"base-claude": {
			Command:        "claude",
			ResumeCommand:  brokenResumeWrapper,
			OptionDefaults: map[string]string{"effort": "medium"},
			OptionsSchema:  []ProviderOption{{Key: "effort", Type: "select", Choices: []OptionChoice{{Value: "medium", FlagArgs: []string{"--effort", "medium"}}, {Value: "high", FlagArgs: []string{"--effort", "high"}}}}},
		},
		"derived-high": {
			Base:           &base,
			OptionDefaults: map[string]string{"effort": "high"},
		},
	}
	got := warnedProviders(t, providers)
	if !got["derived-high"] {
		t.Errorf("derived provider inheriting a broken resume_command was not warned; warned=%v", got)
	}
	if !got["base-claude"] {
		t.Errorf("base provider was not warned; warned=%v", got)
	}
}

// TestResumeWarningsTotalSilenceShape is the shape that makes gcw-84kg a
// detection failure rather than an enumeration undercount. The base declares a
// broken resume_command but NO option_defaults of its own — yet its SCHEMA
// declares a default, so ComputeEffectiveDefaults still produces flags that get
// appended and discarded. Gating on the raw OptionDefaults map skips it, and
// nothing warns at all.
func TestResumeWarningsTotalSilenceShape(t *testing.T) {
	providers := map[string]ProviderSpec{
		"schema-default-only": {
			Command:       "claude",
			ResumeCommand: brokenResumeWrapper,
			// No OptionDefaults. The schema default alone is enough for
			// resolution to append a flag.
			OptionsSchema: []ProviderOption{{
				Key:     "permission_mode",
				Type:    "select",
				Default: "unrestricted",
				Choices: []OptionChoice{{Value: "unrestricted", FlagArgs: []string{"--dangerously-skip-permissions"}}},
			}},
		},
	}
	got := warnedProviders(t, providers)
	if !got["schema-default-only"] {
		t.Error("provider whose only defaults come from its schema was not warned: total silence on a genuinely affected provider")
	}
}

// TestResumeWarningsStillFlagHalfFix guards the trap in the obvious fix.
// Resolving the chain with resume-default completion ENABLED would append the
// flags before detection, pushing the token count past the $0-placeholder
// heuristic and silently reclassifying the half-fix as safe. The half-fix must
// still be reported.
func TestResumeWarningsStillFlagHalfFix(t *testing.T) {
	providers := map[string]ProviderSpec{
		"half-fixed": {
			Command:        "claude",
			ResumeCommand:  halfFixedResumeWrapper,
			OptionDefaults: map[string]string{"effort": "high"},
			OptionsSchema:  []ProviderOption{{Key: "effort", Type: "select", Choices: []OptionChoice{{Value: "high", FlagArgs: []string{"--effort", "high"}}}}},
		},
	}
	got := warnedProviders(t, providers)
	if !got["half-fixed"] {
		t.Error(`the "$@"-without-$0 half-fix was not warned: detection ran against the post-append command`)
	}
}

// TestResumeWarningsSilentWhenNothingWouldBeAppended keeps the existing
// restraint: a provider that would have nothing appended must stay quiet, so
// the advisory does not train operators to ignore it.
func TestResumeWarningsSilentWhenNothingWouldBeAppended(t *testing.T) {
	providers := map[string]ProviderSpec{
		"no-options": {
			Command:       "claude",
			ResumeCommand: brokenResumeWrapper,
			// No schema, no defaults -> nothing is ever appended.
		},
	}
	if w := ResumeCommandWarnings(providers); len(w) != 0 {
		t.Errorf("warned about a provider with nothing to append: %v", w)
	}
}

// TestResumeWarningsQuietWhenScriptAlreadyCarriesTheFlags pins that the gate
// asks the appender's exact question. A wrapper that bakes its flags into the
// script body loses nothing: the appender skips options already present in the
// command (commandContainsOption), so nothing is appended and nothing can be
// discarded. Warning here is a false positive, and acting on the advice would
// make a DUPLICATE flag reach the binary alongside the hardcoded one.
func TestResumeWarningsQuietWhenScriptAlreadyCarriesTheFlags(t *testing.T) {
	providers := map[string]ProviderSpec{
		"baked-in": {
			Command:       "claude",
			ResumeCommand: `/bin/sh -c 'exec launcher -- claude --resume {{.SessionKey}} --dangerously-skip-permissions'`,
			OptionsSchema: []ProviderOption{{
				Key:     "permission_mode",
				Type:    "select",
				Default: "unrestricted",
				Choices: []OptionChoice{{Value: "unrestricted", FlagArgs: []string{"--dangerously-skip-permissions"}}},
			}},
		},
	}
	if w := ResumeCommandWarnings(providers); len(w) != 0 {
		t.Errorf("warned about a wrapper that already carries its flags: %v", w)
	}
}

// TestResumeWarningsNamesOnlyLostKeys pins that the message lists the keys that
// are actually discarded, not every effective default. Naming a key the script
// already applies sends the operator after a flag that is not missing.
func TestResumeWarningsNamesOnlyLostKeys(t *testing.T) {
	providers := map[string]ProviderSpec{
		"partial": {
			Command:       "claude",
			ResumeCommand: `/bin/sh -c 'exec launcher -- claude --resume {{.SessionKey}} --dangerously-skip-permissions'`,
			OptionsSchema: []ProviderOption{
				{
					Key:     "permission_mode",
					Type:    "select",
					Default: "unrestricted",
					Choices: []OptionChoice{{Value: "unrestricted", FlagArgs: []string{"--dangerously-skip-permissions"}}},
				},
				{
					Key:     "effort",
					Type:    "select",
					Default: "high",
					Choices: []OptionChoice{{Value: "high", FlagArgs: []string{"--effort", "high"}}},
				},
			},
		},
	}
	w := ResumeCommandWarnings(providers)
	if len(w) != 1 {
		t.Fatalf("want exactly 1 warning, got %d: %v", len(w), w)
	}
	if !strings.Contains(w[0], "effort") {
		t.Errorf("warning does not name the genuinely lost key %q: %s", "effort", w[0])
	}
	if strings.Contains(w[0], "permission_mode") {
		t.Errorf("warning names %q, which the script already applies and does not lose: %s", "permission_mode", w[0])
	}
}

// TestResumeWarningsFixedShapeStaysQuiet pins that a correctly fixed wrapper
// produces no warning, for both the base and anything inheriting it.
func TestResumeWarningsFixedShapeStaysQuiet(t *testing.T) {
	base := "base-fixed"
	providers := map[string]ProviderSpec{
		"base-fixed": {
			Command:        "claude",
			ResumeCommand:  `/bin/sh -c 'exec launcher -- claude --resume {{.SessionKey}} "$@"' launcher`,
			OptionDefaults: map[string]string{"effort": "medium"},
			OptionsSchema:  []ProviderOption{{Key: "effort", Type: "select", Choices: []OptionChoice{{Value: "medium", FlagArgs: []string{"--effort", "medium"}}}}},
		},
		"derived-fixed": {
			Base:           &base,
			OptionDefaults: map[string]string{"effort": "high"},
		},
	}
	if w := ResumeCommandWarnings(providers); len(w) != 0 {
		t.Errorf("warned about a correctly fixed resume_command: %v", w)
	}
}
