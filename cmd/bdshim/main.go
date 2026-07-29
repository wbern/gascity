// Command bdshim is the tiny bd-compatible thin client. Installed as `bd` first
// on an agent's PATH, it makes a worker's bead calls fast by routing the
// cache-servable verbs through the already-warm controller HTTP API and execing
// the real bd (GC_BD_REAL) for everything else — WITHOUT paying the cold-start
// of the ~117MiB gc binary, which it undercuts at ~7MiB by importing only the
// dependency-light dispatch/classify packages (internal/bdshim,
// internal/bddispatch → internal/api) and never the SDK's config/session/worker
// wiring.
//
// Routed verbs use the controller's typed contract; compatibility reads whose bd
// output has no complete controller projection delegate to bd.real with the
// caller's own env and cwd.
//
// This binary does not reimplement the routing decision. It shares one
// classifier — internal/bdshim.ClassifyVerb — with the in-process fastpath in
// cmd/gc/bd_fastpath.go, so the two entry points cannot drift on which verbs
// route, pass through, or refuse.
//
// Why passthrough is safe: ClassifyVerb's splitPhase argument is false at every
// call site, because no split graph store is wired — the city runs a single
// beads backend, so passthrough to the work-only bd is byte-identical to raw bd
// for every verb. The split-phase branches exist for the graph_store=sqlite
// deployment shape, where a distinct graph backend makes a bd fallback able to
// omit graph-resident beads and the classifier refuses loudly instead. In the
// current identity phase routing is therefore a pure latency optimization, never
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
	"github.com/gastownhall/gascity/internal/beadclient"
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
	cityOverride, _, rawBDArgs := extractScopeFlags(args)
	rawVerb, _ := bdshim.SplitGlobalFlags(rawBDArgs)

	// Expand the gc-only `heartbeat <id>` verb into the bd write that performs it.
	bdArgs, err := rewriteHeartbeatArgs(rawBDArgs)
	if err != nil {
		logDisposition(rawVerb, rawBDArgs, "refuse", 1, start)
		fmt.Fprintf(stderr, "bdshim: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	if len(bdArgs) == 0 {
		logDisposition(rawVerb, rawBDArgs, "refuse", 1, start)
		fmt.Fprintln(stderr, "bdshim: missing bd subcommand") //nolint:errcheck // best-effort stderr
		return 1
	}

	verb, verbArgs := bdshim.SplitGlobalFlags(bdArgs)

	passthrough := func() int {
		code := execRealBd(bdArgs, nil, stdin, stdout, stderr)
		logDisposition(verb, rawBDArgs, "passthrough", code, start)
		return code
	}

	// splitPhase is pinned false: no split graph store is wired, so the city is
	// in the identity phase. Route is therefore always a pure latency choice;
	// passthrough is always byte-identical to raw bd.
	switch bdshim.ClassifyVerb(verb, verbArgs, false) {
	case bdshim.Route:
		city := resolveCityName(cityOverride)
		if city == "" {
			// No city resolvable (non-agent context): can't route, passthrough is
			// best-effort and byte-identical in the identity phase.
			return passthrough()
		}
		base := controllerBaseURL()
		// The pure-claim shape routes to the atomic claim endpoint and carries its
		// own fallbacks (needs an actor; must still work when the controller is
		// down — bd.real's atomic claim is a correct fallback — or its backend
		// cannot claim on behalf of an actor).
		if verb == "update" && bdshim.UpdateClaimShape(verbArgs) {
			actor := strings.TrimSpace(os.Getenv("BEADS_ACTOR"))
			if actor == "" {
				return passthrough()
			}
			id, ok := bddispatch.FirstBdPositional(verbArgs)
			if !ok {
				logDisposition(verb, rawBDArgs, "refuse", 1, start)
				fmt.Fprintln(stderr, "bdshim: usage: update <id> --claim") //nolint:errcheck // best-effort stderr
				return 1
			}
			if !controllerReachable(base) {
				return passthrough()
			}
			client := beadclient.NewCityScopedClient(base, city)
			if code, handled := dispatchClaim(client, id, actor, stdout, stderr); handled {
				logDisposition(verb, rawBDArgs, "route", code, start)
				return code
			}
			// Backend cannot claim on behalf of an actor (501): fall back.
			return passthrough()
		}
		// Routed read/write: dispatch through the controller. When the controller
		// is unreachable this fails loudly (rc=1) rather than silently passing
		// through to the work-only bd, whose cwd scope cannot answer a city-wide
		// read — matching the fat shim's pure-HTTP contract. No liveness probe on
		// this hot path (a probe can spuriously trip under load and mis-route).
		client := beadclient.NewCityScopedClient(base, city)
		code := bddispatch.DispatchViaAPI(client, verb, verbArgs, stdout, stderr)
		logDisposition(verb, rawBDArgs, "route", code, start)
		return code
	case bdshim.Refuse:
		if verb == "update" {
			if flag, unsupported := bdshim.UnsupportedUpdateMutationFlag(verbArgs); unsupported {
				logDisposition(verb, rawBDArgs, "refuse", 1, start)
				fmt.Fprintf(stderr, "bdshim: refusing update %s: this body/notes mutation is not routed to the controller\n", flag) //nolint:errcheck // best-effort stderr
				return 1
			}
		}
		logDisposition(verb, rawBDArgs, "refuse", 1, start)
		fmt.Fprintf(stderr, "bdshim: refusing unsupported %s command\n", verb) //nolint:errcheck // best-effort stderr
		return 1
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
