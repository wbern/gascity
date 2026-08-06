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
		// Both tests must run against the RESOLVED provider, not the raw spec.
		// A provider that inherits its resume_command via base= has an empty
		// spec.ResumeCommand yet is fully affected, and the flags that actually
		// get appended come from EffectiveDefaults — which layers schema
		// defaults and inherited provider defaults on top of this spec's own
		// OptionDefaults. Gating on the raw map skips providers whose only
		// defaults are declared by their schema or inherited from a builtin
		// base, and those are silently affected (gcw-84kg).
		//
		// completeResumeDefaults is deliberately FALSE. Resolution would
		// otherwise append the flags before we inspect the command, and
		// appending pushes the token count past the $0-placeholder heuristic in
		// ResumeCommandSwallowsAppendedArgs — silently reclassifying the
		// "$@"-without-$0 half-fix as safe. That half-fix is the most dangerous
		// shape there is, because it looks like success while still eating the
		// first flag.
		resolved, err := resolveProviderChain(name, spec, providers, false)
		if err != nil {
			// An unresolvable chain is a config error surfaced by validation;
			// this advisory stays quiet rather than double-reporting it.
			continue
		}
		if !ResumeCommandSwallowsAppendedArgs(resolved.ResumeCommand) {
			continue
		}
		// Report only the options a resumed session GENUINELY loses. Gating on
		// "has any effective default" is too coarse: a wrapper that bakes its
		// flags into the script body loses nothing, and an option whose
		// selected choice carries no FlagArgs contributes nothing. Warning in
		// those cases sends the operator after a flag that is not missing, and
		// acting on the advice would put a DUPLICATE flag on the real command
		// line beside the hardcoded one.
		lost := lostResumeOptionKeys(resolved.ResumeCommand, resolved.OptionsSchema, resolved.EffectiveDefaults)
		if len(lost) == 0 {
			// Nothing would be appended, so nothing can be lost. Stay quiet:
			// an advisory nobody can act on trains operators to ignore them.
			continue
		}
		warnings = append(warnings, fmt.Sprintf(
			"provider %q: resume_command is a shell wrapper that discards the option flags resolution appends to it, so resumed sessions silently lose %s. "+
				"End the script with \"$@\" AND give the wrapper a $0 placeholder argument (e.g. ... {{.SessionKey}} \"$@\"' %s). "+
				"Both are required: with \"$@\" but no $0 placeholder the FIRST flag is still consumed as the script name",
			name, strings.Join(lost, ", "), name))
	}
	return warnings
}

// lostResumeOptionKeys returns the option keys whose flags would be appended to
// an inert resume command AND are not already carried by the command itself, in
// schema order. These are the options a resumed session genuinely loses.
//
// It deliberately asks a different question from missingDefaultArgsForCommand.
// That helper decides what to APPEND, and it tokenizes with shellquote, so for
// a wrapper like
//
//	/bin/sh -c 'exec launcher -- claude --resume KEY --dangerously-skip-permissions'
//
// the whole script body is ONE token and the hardcoded flag is invisible to it —
// the appender re-appends a duplicate, which the shell then discards. Nothing is
// lost in that case: the script applies the flag itself. Reporting it would send
// an operator after a flag that is not missing, and acting on the advice would
// put a duplicate on the real command line.
//
// So the marker is searched in the raw command STRING, quoted script body
// included, rather than in its tokens.
func lostResumeOptionKeys(command string, schema []ProviderOption, effectiveDefaults map[string]string) []string {
	var lost []string
	for _, opt := range schema {
		value := effectiveDefaults[opt.Key]
		if value == "" {
			value = opt.Default
		}
		if value == "" {
			continue
		}
		choice := findChoice(opt.Choices, value)
		if choice == nil || len(choice.FlagArgs) == 0 {
			continue
		}
		if strings.Contains(command, choice.FlagArgs[0]) {
			continue
		}
		lost = append(lost, opt.Key)
	}
	return lost
}
