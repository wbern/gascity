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
	cityOverride, rigOverride, rawBDArgs := extractScopeFlags(args)
	rawVerb, _ := bdshim.SplitGlobalFlags(rawBDArgs)

	// --rig selects a DIFFERENT rig's bead store, and this shim has no way to
	// honor that: it answers from the rig it was invoked in. Stripping the flag
	// and proceeding returned the wrong store's beads with exit 0, so a
	// cross-rig query looked like it had succeeded. That breaks the invariant
	// this whole binary rests on — passthrough is a latency choice, never a
	// semantic one — and it does so silently, which is the worst direction for
	// a wrapper to fail in. Raw bd rejects --rig outright; refuse likewise, and
	// name the form that actually works.
	if rigOverride != "" {
		logDisposition(rawVerb, rawBDArgs, "refuse", 1, start)
		fmt.Fprintf(stderr, //nolint:errcheck // best-effort stderr
			"bdshim: --rig %s is not supported: bd reads only its own rig's store, so answering here would return the wrong beads. Use `gc bd` instead, which resolves across rigs.\n",
			rigOverride)
		return 1
	}

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

	// A caller that pinned a NON-CITY store scope has already chosen the store,
	// and this shim cannot represent that choice in a controller request — the
	// dispatch is city-scoped by construction, which is exactly why a bare
	// `bd --rig` is refused above. gc pins one on every `gc bd --rig <rig>`
	// invocation: it resolves the rig itself, sets BEADS_DIR to that rig's store,
	// and announces it with GC_STORE_SCOPE/GC_STORE_ROOT. Routing anyway silently
	// answered from the city instead. Measured live on gc2 before this guard:
	//
	//	gc bd --rig statusline ready --json -> {gc2:16, crm:4}, ZERO statusline
	//	gc bd --rig gas-city-wbern create   -> bead created in the HQ store
	//
	// Both reads and writes, both silently wrong, both looking like success.
	// Passthrough is the correct answer and is available precisely because gc
	// already set BEADS_DIR: real bd honors it and reaches the pinned store.
	if pinnedNonCityStoreScope() {
		return passthrough()
	}

	// A GitHub PR gate is a cross-bead invariant, not a plain store mutation:
	// the target bead may not wait on the PR it owns. Route this one existing bd
	// spelling through gc's exact-store guard before real bd performs the write.
	// This check must follow pinnedNonCityStoreScope: gc's guarded child call is
	// pinned and must pass through to real bd rather than recurse back into gc.
	if _, match, err := bdshim.ParsePRGateCreateArgs(bdArgs); err != nil {
		logDisposition("gate", rawBDArgs, "refuse", 1, start)
		fmt.Fprintf(stderr, "bdshim: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	} else if match {
		code := executeGCBD(args, stdin, stdout, stderr)
		logDisposition("gate", rawBDArgs, "guard", code, start)
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
			if msg, mistyped := bdshim.MistypedMetadataPairRefusal("bdshim", verb, verbArgs); mistyped {
				logDisposition(verb, rawBDArgs, "refuse", 1, start)
				fmt.Fprint(stderr, msg) //nolint:errcheck // best-effort stderr
				return 1
			}
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

// storeScopeEnv is the store scope gc resolved for this invocation. gc sets it
// (alongside GC_STORE_ROOT) in bdCommandEnv for every `gc bd` child.
const storeScopeEnv = "GC_STORE_SCOPE"

// pinnedNonCityStoreScope reports whether the caller pinned a store scope this
// shim cannot express in a city-scoped controller request. Only "city" (and an
// unset value, meaning the caller expressed no opinion) are representable; any
// other scope — "rig" today — must pass through to real bd, which honors the
// BEADS_DIR the caller set.
//
// It fails CLOSED on an unrecognized scope: a future scope kind is treated as
// unrepresentable and passes through, which is byte-identical to raw bd, rather
// than being routed to the wrong store.
func pinnedNonCityStoreScope() bool {
	scope := strings.TrimSpace(os.Getenv(storeScopeEnv))
	if scope == "" || scope == "city" {
		return false
	}
	return true
}
