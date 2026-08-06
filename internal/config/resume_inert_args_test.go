package config

import "testing"

// These cases are the gcw-bdmt defect: completeResumeCommandDefaults appends
// schema-managed flag args at the end of the token list, which is correct for a
// bare command but inert for a `sh -c '<script>' [args...]` wrapper — the script
// is a single token, and appended args become positional parameters that a
// script without "$@" never reads. On gc2 this silently stripped
// --dangerously-skip-permissions, --effort and --model from every resumed Claude
// agent for weeks, with no diagnostic.
func TestResumeCommandSwallowsAppendedArgs(t *testing.T) {
	// The real broken template from gc2 city.toml (claude-opus-5-kenneth et al).
	brokenLive := `/bin/sh -c 'city=${GC_CITY_PATH:-${GC_CITY:?GC_CITY not set}}; exec "$city/assets/scripts/claude-account-launch.sh" "$city/.local/claude-kenneth.env.toml" -- claude --resume {{.SessionKey}}'`
	// The real WORKING launch template from the same file: script ends in "$@"
	// and a $0 placeholder follows the script, so appended args reach claude.
	fixedLive := `/bin/sh -c 'city=${GC_CITY_PATH:-${GC_CITY:?GC_CITY not set}}; exec "$city/assets/scripts/claude-account-launch.sh" "$city/.local/claude-kenneth.env.toml" -- claude --resume {{.SessionKey}} "$@"' claude-account-launch`

	for _, tc := range []struct {
		name    string
		command string
		want    bool
	}{
		{"live gc2 broken resume template", brokenLive, true},
		{"live gc2 template with \"$@\" and a $0 placeholder", fixedLive, false},

		{"bare command is always fine", `claude --resume {{.SessionKey}}`, false},
		{"bare command with flags is fine", `claude --resume {{.SessionKey}} --dangerously-skip-permissions`, false},

		{"sh -c without $@ swallows args", `/bin/sh -c 'exec claude --resume {{.SessionKey}}'`, true},
		{"sh -c with \"$@\" but NO $0 placeholder still loses the first arg", `/bin/sh -c 'exec claude --resume {{.SessionKey}} "$@"'`, true},
		{"sh -c with \"$@\" and a $0 placeholder is fine", `/bin/sh -c 'exec claude --resume {{.SessionKey}} "$@"' sh0`, false},
		{"unquoted $@ also consumes args", `/bin/sh -c 'exec claude --resume {{.SessionKey}} $@' sh0`, false},
		{"$* counts as consuming", `/bin/sh -c 'exec claude --resume {{.SessionKey}} $*' sh0`, false},
		{"explicit $1 counts as consuming", `/bin/sh -c 'exec claude --resume {{.SessionKey}} "$1"' sh0`, false},

		{"bash -c behaves like sh -c", `/bin/bash -c 'exec claude --resume {{.SessionKey}}'`, true},
		{"zsh -c behaves like sh -c", `/usr/bin/zsh -c 'exec claude --resume {{.SessionKey}}'`, true},
		{"plain sh on PATH", `sh -c 'exec claude --resume {{.SessionKey}}'`, true},

		{"empty command is not a finding", ``, false},
		{"a shell with no -c is not a wrapper", `/bin/sh script.sh`, false},
		// An unterminated quote is still a finding, and deliberately so: the
		// tokenizer is total and hands completeResumeCommandDefaults the same
		// three tokens, so the insertion really would be inert. Agreeing with
		// what the insertion actually does matters more than being cautious
		// about malformed input.
		{"unterminated quote is still a wrapper the insertion would break", `sh -c 'unbalanced`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ResumeCommandSwallowsAppendedArgs(tc.command); got != tc.want {
				t.Errorf("ResumeCommandSwallowsAppendedArgs() = %v, want %v\ncommand: %s", got, tc.want, tc.command)
			}
		})
	}
}
