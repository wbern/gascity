package main

import (
	"github.com/gastownhall/gascity/internal/bdshim"
)

// mistypedMetadataPairRefusal reports whether a `gc bd` invocation carries
// `--set-metadata` pairs bd would silently drop, and the message to print.
//
// gc bd writes exec raw bd rather than the routed fastpath, so the shim
// classifier's guard never sees them; this is that path's copy of the same
// check, sharing one detector and one message with the shim binary.
func mistypedMetadataPairRefusal(bdArgs []string) (string, bool) {
	verb, verbArgs := bdshim.SplitGlobalFlags(bdArgs)
	return bdshim.MistypedMetadataPairRefusal("gc bd", verb, verbArgs)
}
