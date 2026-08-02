package bdshim

import "github.com/gastownhall/gascity/internal/bdflags"

// MistypedMetadataPairs returns the bare key=value tokens sitting in bd's
// positional issue-id slot — the `--set-metadata` pairs bd is about to drop.
//
// The implementation lives in internal/bdflags, which is the declared single
// source of truth for bd's per-subcommand flag names and is upstream-owned; a
// second copy here is how the guard came to use a partial flag table and refuse
// `update <id> --add-label k=v`.
func MistypedMetadataPairs(args []string) []string {
	return bdflags.DroppedMetadataPairs(args)
}

// MistypedMetadataPairRefusal builds the refusal both bd entry points print for
// an update whose metadata pairs would be silently dropped, so the shim binary
// and `gc bd` cannot drift on the wording or on which shapes they reject.
func MistypedMetadataPairRefusal(prefix, verb string, args []string) (string, bool) {
	return bdflags.DroppedMetadataRefusal(prefix, verb, args)
}
