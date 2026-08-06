package config

import (
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/gastownhall/gascity/internal/shellquote"
)

// shellWrapperNames are the shells whose `-c <script>` form takes the script as
// a single argument, so anything after it becomes a POSITIONAL PARAMETER of the
// script rather than an argument of the program the script runs.
var shellWrapperNames = map[string]bool{
	"sh": true, "bash": true, "zsh": true, "dash": true, "ksh": true, "ash": true,
}

// scriptConsumesPositionalArgs matches a script that actually reads the
// positional parameters it is handed: "$@", $@, "$*", $*, or an explicit $1..$9.
// A script that references none of these cannot observe appended args at all.
var scriptConsumesPositionalArgs = regexp.MustCompile(`\$[@*1-9]`)

// ResumeCommandSwallowsAppendedArgs reports whether appending schema-managed
// flag args to command would be INERT — the args would be accepted by the shell
// and then never reach the program the operator intended to configure.
//
// completeResumeCommandDefaults appends missing option flags at the end of the
// token list. For a bare command ("claude --resume KEY") that is correct. For a
// shell wrapper ("/bin/sh -c '<script>' ...") it is not: the script is ONE
// token, so appended flags land in the script's positional parameters. Unless
// the script expands them, they vanish silently.
//
// Two distinct ways to lose them, and this reports both:
//
//  1. The script never references $@/$*/$N. Every appended arg is discarded.
//  2. The script does reference them, but no $0 placeholder follows the script.
//     `sh -c 'script' A B` binds A to $0 — the script NAME — so "$@" starts at
//     B and the FIRST appended flag is still lost. This is the dangerous
//     half-fix: adding "$@" alone makes --effort/--model work while
//     --dangerously-skip-permissions stays broken, which looks like success.
//
// Returns false for anything it cannot confidently call a finding — an empty or
// unparseable command, or a non-wrapper invocation — so this can gate a warning
// without producing noise. Detection is static and side-effect free.
//
// See gcw-bdmt: on gc2 this silently stripped --dangerously-skip-permissions,
// --effort and --model from every resumed Claude agent, fleet-wide, for weeks.
func ResumeCommandSwallowsAppendedArgs(command string) bool {
	if strings.TrimSpace(command) == "" {
		return false
	}
	// Split is the same tokenizer completeResumeCommandDefaults uses, so this
	// sees exactly the token boundaries the insertion will see. It is total:
	// an unterminated quote yields whatever it could read rather than an error,
	// which is why the length and shape checks below carry the safety.
	tokens := shellquote.Split(command)
	if len(tokens) < 3 {
		// Too short to be `<shell> -c <script>`. A bare command takes appended
		// args correctly, so this is not a finding.
		return false
	}
	if !shellWrapperNames[filepath.Base(tokens[0])] || tokens[1] != "-c" {
		return false
	}
	script := tokens[2]
	if !scriptConsumesPositionalArgs.MatchString(script) {
		// Case 1: the script cannot observe positional parameters at all.
		return true
	}
	// Case 2: the script reads positionals, but the slot that would become $0
	// is not reserved, so the first appended arg is consumed as the script name.
	hasArg0Placeholder := len(tokens) > 3
	return !hasArg0Placeholder
}

// ResumeCommandWarnings returns advisory warnings for providers whose explicit
// resume_command would silently swallow the schema-managed option flags that
// provider resolution appends to it.
//
// This is deliberately advisory rather than fatal: the command is valid shell
// and the session still starts, it just starts without the options the operator
// declared. Blocking startup over it would be worse than the defect. But the
// defect is invisible without this — a resumed agent looks healthy while
// missing its permission mode, effort tier and model — so it must be said out
// loud at least once per start.
//
// Providers are reported in sorted order so the output is stable across runs.
func ResumeCommandWarnings(providers map[string]ProviderSpec) []string {
	if len(providers) == 0 {
		return nil
	}
	names := make([]string, 0, len(providers))
	for name := range providers {
		names = append(names, name)
	}
	sort.Strings(names)

	var warnings []string
	for _, name := range names {
		spec := providers[name]
		if !ResumeCommandSwallowsAppendedArgs(spec.ResumeCommand) {
			continue
		}
		if len(spec.OptionDefaults) == 0 {
			// Nothing would be appended, so nothing can be lost. Stay quiet:
			// an advisory nobody can act on trains operators to ignore them.
			continue
		}
		warnings = append(warnings, fmt.Sprintf(
			"provider %q: resume_command is a shell wrapper that discards the option flags resolution appends to it, so resumed sessions silently lose %s. "+
				"End the script with \"$@\" AND give the wrapper a $0 placeholder argument (e.g. ... {{.SessionKey}} \"$@\"' %s). "+
				"Both are required: with \"$@\" but no $0 placeholder the FIRST flag is still consumed as the script name",
			name, optionDefaultKeyList(spec.OptionDefaults), name))
	}
	return warnings
}

// optionDefaultKeyList renders the option keys at risk, sorted, for the warning.
func optionDefaultKeyList(defaults map[string]string) string {
	keys := make([]string, 0, len(defaults))
	for k := range defaults {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return strings.Join(keys, ", ")
}
