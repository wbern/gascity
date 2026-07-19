// Command bdproxy is the tiny bd-compatible thin client. Installed as `bd` first
// on an agent's PATH, it makes a worker's bead calls fast by routing the
// cache-servable verbs through the already-warm controller HTTP API and execing
// the real bd (GC_BD_REAL) for everything else — WITHOUT paying the ~200ms
// cold-start of the 117MB gc binary the fat gc-shim spawns per call.
//
// It is a faithful behavioral clone of cmd/gc's runBdShim (same verb classifier,
// same route/passthrough/claim dispositions, splitPhase pinned false to match
// gc's graphStoreSQLiteEnabled), but built as a ~small standalone binary that
// imports only the dependency-light dispatch/classify packages (internal/bdshim,
// internal/bddispatch → internal/api), never the SDK's config/session/worker
// wiring. Routed verbs hit the same controller endpoints and produce byte-
// identical output; passthrough execs bd.real with the caller's own env and cwd.
//
// Why this is safe: gc's graphStoreSQLiteEnabled is hardcoded false (identity
// phase — one backend), so passthrough to the work-only bd is byte-identical to
// raw bd for every verb. Routing is therefore a pure latency optimization, never
// a correctness requirement.
package main

import (
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/bddispatch"
	"github.com/gastownhall/gascity/internal/bdshim"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

// run is the testable entry point: it classifies the bd invocation and either
// routes it through the controller or execs the real bd, returning bd's exit code.
func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	start := time.Now()

	// Strip the gc-only --city/--rig scope flags (raw bd does not accept them);
	// --city overrides the routed target city, matching extractBdScopeFlags.
	cityOverride, _, bdArgs := extractScopeFlags(args)

	// Expand the gc-only `heartbeat <id>` verb into the bd write that performs it.
	bdArgs, err := rewriteHeartbeatArgs(bdArgs)
	if err != nil {
		fmt.Fprintf(stderr, "bdproxy: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	if len(bdArgs) == 0 {
		fmt.Fprintln(stderr, "bdproxy: missing bd subcommand") //nolint:errcheck // best-effort stderr
		return 1
	}

	verb, verbArgs := bdshim.SplitGlobalFlags(bdArgs)

	passthrough := func() int {
		code := execRealBd(bdArgs, "", nil, stdin, stdout, stderr)
		logDisposition(verb, "passthrough", code, start)
		return code
	}

	// splitPhase is pinned false to match gc's graphStoreSQLiteEnabled (identity
	// phase). Route is therefore always a pure latency choice; passthrough is
	// always byte-identical to raw bd.
	switch bdshim.ClassifyVerb(verb, verbArgs, false) {
	case bdshim.Route:
		// The pure-claim shape routes to the atomic claim endpoint and carries its
		// own fallbacks (needs an actor; must still work when the controller is
		// down or its backend cannot claim on behalf of an actor).
		if verb == "update" && bdshim.UpdateClaimShape(verbArgs) {
			actor := strings.TrimSpace(os.Getenv("BEADS_ACTOR"))
			if actor == "" {
				return passthrough()
			}
			id, ok := bddispatch.FirstBdPositional(verbArgs)
			if !ok {
				fmt.Fprintln(stderr, "bdproxy: usage: update <id> --claim") //nolint:errcheck // best-effort stderr
				return 1
			}
			client := controllerClient(cityOverride)
			if client == nil {
				return passthrough()
			}
			if code, handled := dispatchClaim(client, id, actor, stdout, stderr); handled {
				logDisposition(verb, "route", code, start)
				return code
			}
			// Backend cannot claim on behalf of an actor (501): fall back.
			return passthrough()
		}
		client := controllerClient(cityOverride)
		if client == nil {
			// Wanted to route but no controller reachable. In identity phase
			// passthrough is byte-identical and strictly safer than failing, so
			// fall back rather than refuse (the fat shim's pure-HTTP refusal exists
			// for the split phase, which is off here).
			return passthrough()
		}
		code := bddispatch.DispatchViaAPI(client, verb, verbArgs, stdout, stderr)
		logDisposition(verb, "route", code, start)
		return code
	case bdshim.Refuse:
		// Unreachable while splitPhase is false (ClassifyVerb only returns Refuse
		// under the split phase); fall back to passthrough defensively rather than
		// hard-failing a verb that is byte-identical through raw bd.
		return passthrough()
	default: // bdshim.Passthrough
		return passthrough()
	}
}

// extractScopeFlags strips the gc-only --city/--rig flags from a bd arg list,
// returning the city override, rig override, and the remaining raw-bd args. It
// mirrors cmd/gc's extractBdScopeFlags (minus the persistent cityFlag/rigFlag
// cobra fallbacks, which do not exist in this standalone binary).
func extractScopeFlags(args []string) (cityName, rigName string, rest []string) {
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "--city" && i+1 < len(args):
			cityName = args[i+1]
			i++
		case strings.HasPrefix(args[i], "--city="):
			cityName = strings.TrimPrefix(args[i], "--city=")
		case args[i] == "--rig" && i+1 < len(args):
			rigName = args[i+1]
			i++
		case strings.HasPrefix(args[i], "--rig="):
			rigName = strings.TrimPrefix(args[i], "--rig=")
		default:
			rest = append(rest, args[i])
		}
	}
	return cityName, rigName, rest
}

// heartbeatMetadataKey is beadmeta.LastHeartbeatAtMetadataKey, hardcoded to keep
// this binary free of the heavy beadmeta (go/ast) dependency. TestHeartbeatKeyInSync
// asserts it stays equal to the canonical constant.
const heartbeatMetadataKey = "gc.last_heartbeat_at"

// rewriteHeartbeatArgs expands the gc-only `heartbeat <id>` verb into the
// `update <id> --set-metadata gc.last_heartbeat_at=<now>` write it performs,
// mirroring cmd/gc's rewriteBdHeartbeatArgs.
func rewriteHeartbeatArgs(bdArgs []string) ([]string, error) {
	if len(bdArgs) == 0 || bdArgs[0] != "heartbeat" {
		return bdArgs, nil
	}
	rest := bdArgs[1:]
	if len(rest) != 1 || rest[0] == "" || strings.HasPrefix(rest[0], "-") ||
		strings.IndexFunc(rest[0], func(r rune) bool { return r == ' ' || r == '\t' || r == '\n' }) >= 0 {
		return nil, fmt.Errorf("usage: bd heartbeat <issue-id>")
	}
	stamp := time.Now().UTC().Format(time.RFC3339)
	return []string{"update", rest[0], "--set-metadata", heartbeatMetadataKey + "=" + stamp}, nil
}
