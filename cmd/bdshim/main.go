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
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/bddispatch"
	"github.com/gastownhall/gascity/internal/bdflags"
	"github.com/gastownhall/gascity/internal/bdguard"
	"github.com/gastownhall/gascity/internal/bdshim"
	"github.com/gastownhall/gascity/internal/beadclient"
	"github.com/gastownhall/gascity/internal/pathutil"
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
	rawBDArgs, allowUnbounded := stripAllowUnbounded(rawBDArgs)
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
	if msg, refuse := bareBDHQGuardRefusal(rawBDArgs, rawVerb); refuse {
		logDisposition(rawVerb, rawBDArgs, "refuse", 1, start)
		fmt.Fprintf(stderr, "bdshim: %s\n", msg) //nolint:errcheck // best-effort stderr
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
	realBdArgs := stripShimPrivateFlags(bdArgs)
	if summaryJSONRequested(verbArgs) && !hasJSONOutput(verbArgs) {
		logDisposition(verb, rawBDArgs, "refuse", 1, start)
		fmt.Fprintf(stderr, "bdshim: %s --summary-json requires --json\n", verb) //nolint:errcheck // best-effort stderr
		return 1
	}

	passthrough := func() int {
		if summaryJSONRequested(verbArgs) && (verb == "ready" || verb == "list") {
			var staged bytes.Buffer
			code := execRealBd(realBdArgs, nil, stdin, &staged, stderr)
			if code != 0 {
				_, _ = stdout.Write(staged.Bytes())
				logDisposition(verb, rawBDArgs, "passthrough", code, start)
				return code
			}
			if summaryCode := bddispatch.WriteBeadSummaryOutput(verb, staged.Bytes(), stdout, stderr); summaryCode != 0 {
				logDisposition(verb, rawBDArgs, "passthrough", summaryCode, start)
				return summaryCode
			}
			logDisposition(verb, rawBDArgs, "passthrough", 0, start)
			return 0
		}
		if !allowUnbounded && bddispatch.ManagedOutputFirewallActive(verb) && managedPassthroughReadVerb(verb, verbArgs) {
			var staged bytes.Buffer
			code := execRealBd(realBdArgs, nil, stdin, &staged, stderr)
			if verb == "show" && hasJSONOutput(verbArgs) {
				if firewallCode := bddispatch.WriteManagedShowOutput(staged.Bytes(), stdout, stderr); firewallCode != 0 {
					logDisposition(verb, rawBDArgs, "passthrough", firewallCode, start)
					return firewallCode
				}
				logDisposition(verb, rawBDArgs, "passthrough", code, start)
				return code
			}
			if firewallCode := bddispatch.WriteManagedOutput(context.Background(), "managed_bd_passthrough", verb, staged.Bytes(), hasJSONOutput(verbArgs), stdout, stderr); firewallCode != 0 {
				logDisposition(verb, rawBDArgs, "passthrough", firewallCode, start)
				return firewallCode
			}
			logDisposition(verb, rawBDArgs, "passthrough", code, start)
			return code
		}
		code := execRealBd(realBdArgs, nil, stdin, stdout, stderr)
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

func stripAllowUnbounded(args []string) ([]string, bool) {
	result := make([]string, 0, len(args))
	allow := false
	for _, arg := range args {
		if arg == "--allow-unbounded" {
			allow = true
			continue
		}
		result = append(result, arg)
	}
	return result, allow
}

// shimPrivateFlags are understood by this wrapper but not by the standalone bd
// binary used for passthrough. Keep the set centralized so every passthrough
// path remains compatible when the shim gains a new private instruction.
var shimPrivateFlags = map[string]struct{}{
	"--summary-json": {},
}

func stripShimPrivateFlags(args []string) []string {
	result := make([]string, 0, len(args))
	for _, arg := range args {
		if _, private := shimPrivateFlags[arg]; private {
			continue
		}
		result = append(result, arg)
	}
	return result
}

func managedPassthroughReadVerb(verb string, args []string) bool {
	switch verb {
	case "show", "ready", "list", "query":
		return true
	case "mol":
		positionals := bdflags.Positionals("mol current", args)
		return len(positionals) > 0 && (positionals[0] == "current" || positionals[0] == "progress")
	default:
		return false
	}
}

func hasJSONOutput(args []string) bool {
	for _, arg := range args {
		if arg == "--json" || strings.HasPrefix(arg, "--format=json") {
			return true
		}
	}
	return false
}

func summaryJSONRequested(args []string) bool {
	for _, arg := range args {
		if arg == "--summary-json" {
			return true
		}
	}
	return false
}

// bareBDHQGuardRefusal applies the same managed-session HQ fence to the `bd`
// PATH shim as `gc bd`. Otherwise an agent could bypass the operator's policy
// merely by dropping the `gc` prefix.
func bareBDHQGuardRefusal(args []string, verb string) (string, bool) {
	if strings.TrimSpace(os.Getenv(bdguard.MarkerEnv)) != "1" ||
		strings.TrimSpace(os.Getenv(bdguard.AccessEnv)) == "1" {
		return "", false
	}
	city := pathutil.NormalizePathForCompare(strings.TrimSpace(os.Getenv(bdguard.CityEnv)))
	if city == "" {
		return "managed-session HQ guard has no city path; refusing an unverified bare bd target", true
	}

	target, explicitDirectory := extractBareBDDirectory(args)
	target = strings.TrimSpace(target)
	store := ""
	if explicitDirectory {
		store = nearestBareBDStore(target)
	} else if beadsDir := strings.TrimSpace(os.Getenv("BEADS_DIR")); beadsDir != "" {
		// BEADS_DIR names the store itself; unlike -C and cwd, bd does not
		// search its ancestors for another .beads entry.
		store = pathutil.NormalizePathForCompare(beadsDir)
	} else if cwd, err := os.Getwd(); err == nil {
		store = nearestBareBDStore(cwd)
	} else {
		return "managed-session HQ guard could not determine the bare bd target; refusing unverified access", true
	}
	rigRoot := pathutil.NormalizePathForCompare(strings.TrimSpace(os.Getenv("GC_RIG_ROOT")))
	cityStore := pathutil.NormalizePathForCompare(filepath.Join(city, ".beads"))
	if store != city && store != cityStore {
		return "", false
	}

	identity := strings.TrimSpace(os.Getenv("GC_ALIAS"))
	if identity == "" {
		identity = strings.TrimSpace(os.Getenv("GC_AGENT"))
	}
	if identity == "" {
		identity = "unknown"
	}
	rig := strings.TrimSpace(os.Getenv("GC_RIG"))
	if rig == "" {
		rig = "unknown"
	}
	if rigRoot == "" {
		rigRoot = "unknown"
	}
	if verb == "" {
		verb = "<verb>"
	}
	return fmt.Sprintf(
		"managed agent %q (agent rig %q, rig store %q) was denied HQ store %q; you may have meant `gc bd %s --rig %s ...`; ask the operator if HQ access is required",
		identity,
		rig,
		rigRoot,
		city,
		verb,
		rig,
	), true
}

// nearestBareBDStore mirrors bd's workspace discovery closely enough for the
// HQ fence: -C and cwd select a workspace, then bd walks toward the filesystem
// root until it finds .beads. The starting path need not exist.
func nearestBareBDStore(start string) string {
	current := pathutil.NormalizePathForCompare(strings.TrimSpace(start))
	for current != "" {
		if filepath.Base(current) == ".beads" {
			if _, err := os.Lstat(current); err == nil {
				return current
			}
		}
		candidate := filepath.Join(current, ".beads")
		if _, err := os.Lstat(candidate); err == nil {
			return pathutil.NormalizePathForCompare(candidate)
		}
		parent := filepath.Dir(current)
		if parent == current {
			break
		}
		current = parent
	}
	return ""
}

func extractBareBDDirectory(args []string) (string, bool) {
	for i := 0; i < len(args); i++ {
		switch {
		case (args[i] == "-C" || args[i] == "--directory") && i+1 < len(args):
			return args[i+1], true
		case strings.HasPrefix(args[i], "--directory="):
			return strings.TrimPrefix(args[i], "--directory="), true
		}
	}
	return "", false
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
