package bdshim

import (
	"fmt"
	"strings"
)

// MistypedMetadataPairs returns the bare key=value tokens sitting in bd's
// positional issue-id slot — the `--set-metadata` pairs bd is about to drop.
func MistypedMetadataPairs(args []string) []string {
	var dropped []string
	for _, p := range UpdatePositionals(args) {
		if strings.IndexByte(p, '=') >= 0 {
			dropped = append(dropped, p)
		}
	}
	return dropped
}

// MistypedMetadataPairRefusal builds the refusal both bd entry points print for
// an update whose metadata pairs would be silently dropped, so the shim binary
// and `gc bd` cannot drift on the wording or on which shapes they reject. prefix
// names the entry point (e.g. "gc bd" or "bdshim"). It reports false for any
// verb other than update and for every well-formed invocation.
//
// The message names each dropped pair and the form that works, because the
// caller's own output gives it nothing: bd prints its success line and exits 0
// regardless of how many pairs it discarded.
func MistypedMetadataPairRefusal(prefix, verb string, args []string) (string, bool) {
	if verb != "update" {
		return "", false
	}
	dropped := MistypedMetadataPairs(args)
	if len(dropped) == 0 {
		return "", false
	}
	corrected := make([]string, 0, len(dropped))
	for _, pair := range dropped {
		corrected = append(corrected, "--set-metadata "+pair)
	}
	subject, object := "it", "an issue id"
	if len(dropped) > 1 {
		subject, object = "them", "issue ids"
	}
	return fmt.Sprintf(
		"%s: refusing update: %s would be dropped. --set-metadata takes ONE key=value per flag, so bd reads %s as %s, fails to resolve %s, and still exits 0 after writing only the first pair. Repeat the flag instead: %s\n",
		prefix,
		strings.Join(dropped, ", "),
		strings.Join(dropped, ", "),
		object,
		subject,
		strings.Join(corrected, " "),
	), true
}
