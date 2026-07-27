package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/beads"
)

// bdFastpathEnv opts an invocation into the read fast path. It is off by
// default: the fast path answers from the controller's bead projection, which
// is a strict subset of the fields the bd CLI emits (pinned by
// TestEarlyBdShowOmitsFieldsGascityDoesNotModel), so enabling it is a
// deliberate choice by an operator who consumes only the modeled fields.
const bdFastpathEnv = "GC_BD_FASTPATH"

// canonicalDoltHostEnv and canonicalDoltPortEnv carry the provenance of a
// Dolt endpoint that gc itself projected. A managed session always inherits
// GC_DOLT_HOST/GC_DOLT_PORT, so the fast path cannot treat their mere presence
// as a foreign endpoint without disabling itself everywhere it matters.
// Stamping the values gc projected lets the gate tell its own projection apart
// from an operator override that points bd at a different store.
const (
	canonicalDoltHostEnv = "GC_BD_FASTPATH_CANONICAL_DOLT_HOST"
	canonicalDoltPortEnv = "GC_BD_FASTPATH_CANONICAL_DOLT_PORT"
)

// earlyBdAPIClient is indirected so tests can exercise the fast path without a
// running supervisor.
var earlyBdAPIClient = supervisorCityAPIClient

// bdFastpathEnabled reports whether the fast path is opted in. Unknown values
// fail closed to the ordinary path rather than enabling unexpectedly.
func bdFastpathEnabled(raw string) bool {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "1", "true", "yes":
		return true
	default:
		return false
	}
}

// tryEarlyBdRead answers `gc bd show <id> --json` from the city controller
// before telemetry initialization and Cobra command construction, which
// dominate the cost of an otherwise trivial point lookup.
//
// It is deliberately all-or-nothing: it either writes the complete response and
// reports handled, or it writes nothing and reports handled=false so the
// ordinary gc bd path runs unchanged. Every guard below — the opt-in, the
// argument shape, the endpoint provenance, the city context, the controller
// route, and the lookup itself — falls through on any doubt. No other verb, no
// mutation, and no explicitly scoped invocation reaches this path.
func tryEarlyBdRead(args []string, stdout, stderr io.Writer) (code int, handled bool) {
	if !bdFastpathEnabled(os.Getenv(bdFastpathEnv)) {
		return 0, false
	}
	id, ok := earlyBdShowID(args)
	if !ok {
		return 0, false
	}
	if hasForeignDoltEndpoint() {
		return 0, false
	}
	cityPath := earlyBdCityPath()
	if cityPath == "" || !servesCityBeadScope(cityPath) {
		return 0, false
	}
	client := earlyBdAPIClient(cityPath)
	if client == nil {
		return 0, false
	}
	return writeEarlyBdShow(client, id, stdout, stderr)
}

// writeEarlyBdShow renders the bead exactly as the bd read passthrough does for
// a successful lookup: a single-element JSON array on one line. The payload is
// buffered first so a lookup or encoding failure can still fall through with an
// untouched stdout.
func writeEarlyBdShow(client *api.Client, id string, stdout, stderr io.Writer) (int, bool) {
	read, err := client.GetBead(id)
	if err != nil {
		return 0, false
	}
	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode([]beads.Bead{read.Body}); err != nil {
		return 0, false
	}
	if _, err := stdout.Write(buf.Bytes()); err != nil {
		fmt.Fprintf(stderr, "gc bd: writing bead %q: %v\n", id, err) //nolint:errcheck // best-effort stderr
		return 1, true
	}
	return 0, true
}

// earlyBdShowID accepts only `bd show <id> --json` with a bare bead ID. Any
// other verb, flag, or ordering — including the scope flags gc validates before
// it prepares a bd child — keeps the ordinary path.
func earlyBdShowID(args []string) (string, bool) {
	if len(args) != 4 || args[0] != "bd" || args[1] != "show" || args[3] != "--json" {
		return "", false
	}
	id := args[2]
	if id == "" || strings.TrimSpace(id) != id || strings.HasPrefix(id, "-") {
		return "", false
	}
	if strings.IndexFunc(id, unicode.IsSpace) >= 0 {
		return "", false
	}
	return id, true
}

// hasForeignDoltEndpoint reports whether an inherited Dolt endpoint differs
// from the one gc projected. A caller that points bd at another store must keep
// the ordinary path, which resolves and validates that target; only gc's own
// projection describes the store the controller serves.
func hasForeignDoltEndpoint() bool {
	host := strings.TrimSpace(os.Getenv("GC_DOLT_HOST"))
	port := strings.TrimSpace(os.Getenv("GC_DOLT_PORT"))
	if host == "" && port == "" {
		return false
	}
	return host != strings.TrimSpace(os.Getenv(canonicalDoltHostEnv)) ||
		port != strings.TrimSpace(os.Getenv(canonicalDoltPortEnv))
}

// servesCityBeadScope reports whether this invocation reads the city's own bead
// scope, which is the only scope the fast path can answer faithfully.
//
// The bead read endpoint is city-scoped and resolves an ID across every store
// the city federates. The ordinary path resolves a rig-scoped invocation
// against that rig's store alone, so it reports an ID held by a sibling rig as
// absent while the controller would return it. Requiring a projected scope root
// that is the city itself keeps the two answers over the same set of beads;
// an unset scope root proves nothing, because the ordinary path also derives
// rig scope from the working directory, so it declines too.
func servesCityBeadScope(cityPath string) bool {
	scopeRoot := strings.TrimSpace(os.Getenv("GC_BEADS_SCOPE_ROOT"))
	if scopeRoot == "" {
		return false
	}
	return normalizePathForCompare(scopeRoot) == normalizePathForCompare(cityPath)
}

// earlyBdCityPath resolves the city from the environment alone. Discovering a
// city from the working directory, or resolving GC_CITY as a registered name,
// needs the configuration loading this path exists to avoid, so an invocation
// without an absolute city path in its environment keeps the ordinary path.
func earlyBdCityPath() string {
	if cityPath := strings.TrimSpace(os.Getenv("GC_CITY_PATH")); filepath.IsAbs(cityPath) {
		return cityPath
	}
	if cityPath := strings.TrimSpace(os.Getenv("GC_CITY")); filepath.IsAbs(cityPath) {
		return cityPath
	}
	return ""
}
