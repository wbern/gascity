package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/bddispatch"
	"github.com/gastownhall/gascity/internal/bdshim"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/telemetry"
	"github.com/spf13/cobra"
)

// The bd shim (thin client) is a bd-CLI-compatible front end that routes a
// worker's bead operations through the controller's HTTP API, so the controller
// owns the store (the per-class coordrouter Router + the embedded SQLite graph
// store under graph_store=sqlite) and every worker is a thin client. Installed as
// `bd` first on an agent's PATH, it makes both raw `bd` and `gc bd` route
// transparently with no prompt changes (graph-store-rollout-plan.md §C2,
// model A in graph-store-session-handoff.md; the pure-HTTP redirect is
// engdocs/design/bd-shim-http-redirect.md).
//
// Each bd subcommand has one of three dispositions (classifyBdShimVerb):
//
//   - bdRoute       — served by calling the controller's HTTP bead API
//     (dispatchBdShimVerbViaAPI). PURE-HTTP: there is no in-process Router
//     fallback; a routed verb errors when no controller is reachable (ga-2gap48).
//   - bdPassthrough — execed to the real bd (GC_BD_REAL), for ops that provably
//     never touch graph-class data, and for everything in the identity phase
//     (graph_store off → one backend → byte-identical to raw bd).
//   - bdRefuse      — graph-touching ops not yet routed (bd mol / gate / query
//     ephemeral): refused loudly in the split phase rather than silently passed
//     to the work-only bd, where they would drop graph data (§X2). This is the
//     CLOSED-allowlist safety property: passthrough is never a graph-class
//     catch-all.

// realBdEnvVar names the environment variable holding the absolute path of the
// real bd binary. The shim must resolve bd through this, never exec.LookPath,
// because once it is installed as `bd` on PATH a LookPath would resolve back to
// the shim and recurse (graph-store-rollout-plan.md §C2). GC_BD_REAL is
// captured at install time as an absolute path.
const realBdEnvVar = "GC_BD_REAL"

// The bd-shim verb classifier (pure route-vs-passthrough decision logic) now
// lives in internal/bdshim so a standalone bd binary can import it. The aliases
// and thin wrappers below keep cmd/gc's existing call sites unchanged while the
// decision logic lives in one dependency-light place.

// bdShimDisposition aliases bdshim.Disposition (route/passthrough/refuse).
type bdShimDisposition = bdshim.Disposition

const (
	bdPassthrough = bdshim.Passthrough
	bdRoute       = bdshim.Route
	bdRefuse      = bdshim.Refuse
)

// bdQueryRoutingEnabled mirrors bdshim.QueryRoutingEnabled: whether `bd query`
// (ephemeral discovery) routing is compiled in.
const bdQueryRoutingEnabled = bdshim.QueryRoutingEnabled

// classifyBdShimVerb decides how the shim handles a bd subcommand; see
// bdshim.ClassifyVerb.
func classifyBdShimVerb(verb string, args []string, splitPhase bool) bdShimDisposition {
	return bdshim.ClassifyVerb(verb, args, splitPhase)
}

// splitBdGlobalFlags finds the bd subcommand past leading global flags; see
// bdshim.SplitGlobalFlags.
func splitBdGlobalFlags(args []string) (string, []string) {
	return bdshim.SplitGlobalFlags(args)
}

// bdUpdateClaimShape reports whether a `bd update` arg list is the pure-claim
// shape routed to the controller's atomic claim endpoint; see
// bdshim.UpdateClaimShape.
func bdUpdateClaimShape(args []string) bool {
	return bdshim.UpdateClaimShape(args)
}

// resolveRealBdPath returns the absolute path of the real bd binary the shim
// delegates passthrough ops to. It prefers GC_BD_REAL (an install-time absolute
// path) and only falls back to exec.LookPath("bd") when GC_BD_REAL is unset —
// the fallback is unsafe once the shim is installed as bd on PATH, so
// production installs always set GC_BD_REAL.
func resolveRealBdPath() (string, error) {
	if raw := strings.TrimSpace(os.Getenv(realBdEnvVar)); raw != "" {
		if !filepath.IsAbs(raw) {
			return "", fmt.Errorf("%s must be an absolute path, got %q", realBdEnvVar, raw)
		}
		if _, err := os.Stat(raw); err != nil {
			return "", fmt.Errorf("%s=%q: %w", realBdEnvVar, raw, err)
		}
		return raw, nil
	}
	path, err := exec.LookPath("bd")
	if err != nil {
		return "", fmt.Errorf("bd not found: set %s to the real bd binary or put bd on PATH: %w", realBdEnvVar, err)
	}
	return path, nil
}

// execRealBd runs the real bd binary with the given args in dir, streaming its
// stdio and propagating its exit code — preserving bd's exit-code contract. It
// resolves bd via resolveRealBdPath (never a bare LookPath in the shim's own
// dispatch) so it cannot recurse into the shim. A nil env defaults to the
// process environment; passthrough callers pass the projected bd scope env.
func execRealBd(args []string, dir string, env []string, stdin io.Reader, stdout, stderr io.Writer) int {
	bdPath, err := resolveRealBdPath()
	if err != nil {
		fmt.Fprintf(stderr, "gc bd-shim: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	cmd := exec.Command(bdPath, args...)
	if dir != "" {
		cmd.Dir = dir
	}
	cmd.Stdin = stdin
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if env == nil {
		env = os.Environ()
	}
	cmd.Env = env
	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return exitErr.ExitCode()
		}
		fmt.Fprintf(stderr, "gc bd-shim: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	return 0
}

// bdShimAPIClient returns an HTTP client to the controller's bead API for the
// pure-HTTP shim. It prefers a standalone controller (when the city configures an
// [api] port) and otherwise reaches the supervisor-served per-city API. Unlike
// apiClient — used by read-path CLI commands, which keep a local store fallback
// and so deliberately do NOT route a supervisor-managed city through the
// supervisor client — the shim's target is to route through the controller, so
// it falls through to the supervisor client for a supervisor-managed city.
func bdShimAPIClient(cityPath string) *api.Client {
	if disabled, _ := classifyGCNoAPI(os.Getenv("GC_NO_API")); disabled {
		return nil
	}
	if controllerAlive(cityPath) != 0 {
		if c := standaloneControllerClient(cityPath); c != nil {
			return c
		}
	}
	return supervisorCityAPIClient(cityPath)
}

// dispatchBdShimVerbViaAPI serves a routed bd verb by calling the controller's
// HTTP API. The dispatch logic and its arg parsers/output formatters live in the
// dependency-light internal/bddispatch package so a standalone bd binary can
// serve routed verbs without importing package main or cobra.
func dispatchBdShimVerbViaAPI(client *api.Client, verb string, args []string, stdout, stderr io.Writer) int {
	return bddispatch.DispatchViaAPI(client, verb, args, stdout, stderr)
}

// firstBdPositional returns the first non-flag argument (a bead id), or false
// when every argument is a flag; see bddispatch.FirstBdPositional.
func firstBdPositional(args []string) (string, bool) {
	return bddispatch.FirstBdPositional(args)
}

// runBdShim is the bd-compatible thin-client entry point. It resolves the scope
// (rig vs city) exactly as `gc bd` does, classifies the bd subcommand, and then
// either routes it through the in-process Router (graph-aware), passes it
// through to the real bd in the resolved scope with the projected bd env, or
// refuses it (see the package doc above).
func runBdShim(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	cityName, rigName, bdArgs := extractBdScopeFlags(args)

	// Expand the gc-only `heartbeat <id>` verb into the bd write that performs
	// it, then route that write by id — shared with `gc bd`.
	bdArgs, err := rewriteBdHeartbeatArgs(bdArgs)
	if err != nil {
		fmt.Fprintf(stderr, "gc bd-shim: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	if len(bdArgs) == 0 {
		fmt.Fprintln(stderr, "gc bd-shim: missing bd subcommand") //nolint:errcheck // best-effort stderr
		return 1
	}

	cityPath, err := resolveBdCity(cityName)
	if err != nil {
		fmt.Fprintf(stderr, "gc bd-shim: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	cfg, err := loadCityConfig(cityPath, stderr)
	if err != nil {
		fmt.Fprintf(stderr, "gc bd-shim: loading config: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	verb, verbArgs := splitBdGlobalFlags(bdArgs)
	passthrough := func() int {
		// Every real-bd exec — the default passthrough and every documented
		// routing fallback (claim with no actor / no controller / backend can't
		// claim) — flows through here, so recording the disposition in the
		// closure captures a passthrough exactly when a direct Dolt dial happens.
		telemetry.RecordBDShimDisposition(context.Background(), verb, bdPassthrough.String())
		return passthroughRealBd(cfg, cityPath, rigName, cityName, bdArgs, stdin, stdout, stderr)
	}
	switch classifyBdShimVerb(verb, verbArgs, graphStoreSQLiteEnabled(cfg)) {
	case bdRoute:
		// The pure-claim shape is routed to the atomic claim endpoint, but it
		// carries its own fallbacks: it needs an actor to convey, and a claim
		// must still work when the controller is down or its backend cannot
		// claim on behalf of an actor. In those cases fall back to the real bd
		// (correctness preserved; just not warm) rather than hard-failing.
		if verb == "update" && bdUpdateClaimShape(verbArgs) {
			actor := bdShimClaimActor()
			if actor == "" {
				return passthrough()
			}
			id, ok := firstBdPositional(verbArgs)
			if !ok {
				fmt.Fprintln(stderr, "gc bd-shim: usage: update <id> --claim") //nolint:errcheck // best-effort stderr
				return 1
			}
			client := bdShimAPIClient(cityPath)
			if client == nil {
				return passthrough()
			}
			if code, handled := dispatchBdShimClaim(client, id, actor, stdout, stderr); handled {
				telemetry.RecordBDShimDisposition(context.Background(), verb, bdRoute.String())
				return code
			}
			// Backend cannot claim on behalf of an actor (501): fall back.
			return passthrough()
		}
		// Pure-HTTP: route the verb through the controller's HTTP API — the
		// controller owns the store (Router + graph SQLite) and every worker is a
		// thin client. There is no in-process Router fallback; a routed verb errors
		// when no controller is reachable (ga-2gap48). The supervisor publishes a
		// city's beads API before it spawns that city's control-dispatcher and
		// agents, so the shim's consumers always find the API up.
		client := bdShimAPIClient(cityPath)
		if client == nil {
			// Wanted to route but no controller is reachable: the shim neither
			// dialed Dolt directly nor served the verb — record it as a refusal.
			telemetry.RecordBDShimDisposition(context.Background(), verb, bdRefuse.String())
			fmt.Fprintf(stderr, "gc bd-shim: no controller API reachable for %q; the shim routes bead ops through the controller (ga-2gap48 pure-HTTP)\n", verb) //nolint:errcheck // best-effort stderr
			return 1
		}
		return dispatchBdShimVerbViaAPI(client, verb, verbArgs, stdout, stderr)
	case bdRefuse:
		telemetry.RecordBDShimDisposition(context.Background(), verb, bdRefuse.String())
		fmt.Fprintf(stderr, "gc bd-shim: %q reads or mutates graph-class beads but is not yet routed through the graph store; refusing to pass it to the work-only bd while graph_store=sqlite is active (would silently miss graph beads — see graph-store-rollout-plan.md §X2)\n", verb) //nolint:errcheck // best-effort stderr
		return 1
	default: // bdPassthrough
		return passthrough()
	}
}

// passthroughRealBd resolves the local store scope and execs the real bd binary
// for a passthrough op. Scope resolution is paid only here — routed verbs reach
// the bead through the controller's city-scoped API (which federates across
// stores) and never need the local scope target, and scope resolution can cost
// a remote-Dolt owner lookup for id-bearing verbs.
func passthroughRealBd(cfg *config.City, cityPath, rigName, cityName string, bdArgs []string, stdin io.Reader, stdout, stderr io.Writer) int {
	target, err := resolveBdScopeTarget(cfg, cityPath, rigName, bdArgs, cityName != "")
	if err != nil {
		fmt.Fprintf(stderr, "gc bd-shim: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	env, err := bdCommandEnv(cityPath, cfg, target)
	if err != nil {
		fmt.Fprintf(stderr, "gc bd-shim: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	return execRealBd(bdArgs, target.ScopeRoot, workQueryEnvForDir(env, target.ScopeRoot), stdin, stdout, stderr)
}

// bdShimClaimActor resolves the actor a routed claim is made on behalf of, from
// the BEADS_ACTOR env the caller (the agent, or gc hook's claim BdStore) sets.
// An empty actor means the shim cannot faithfully route the claim and must fall
// back to the real bd.
func bdShimClaimActor() string {
	return strings.TrimSpace(os.Getenv("BEADS_ACTOR"))
}

// dispatchBdShimClaim routes an atomic claim to the controller and reproduces
// the `bd update <id> --claim --json` output contract its caller (BdStore.Claim)
// parses: on success it prints the claimed bead JSON (exit 0); on a lost race it
// prints an "already claimed by <holder>" message (exit 1) that
// isBdClaimConflictMessage matches; on not-found it prints a "not found" message
// (exit 1) that isBdNotFound matches. handled=false signals the caller to fall
// back to the real bd (the backend cannot claim on behalf of an actor).
func dispatchBdShimClaim(client *api.Client, id, actor string, stdout, stderr io.Writer) (code int, handled bool) {
	bead, claimed, err := client.ClaimBead(id, actor)
	if err != nil {
		if errors.Is(err, api.ErrClaimRouteUnsupported) {
			return 0, false
		}
		fmt.Fprintf(stderr, "gc bd-shim: claiming %q via API: %v\n", id, err) //nolint:errcheck // best-effort stderr
		return 1, true
	}
	if !claimed {
		holder := strings.TrimSpace(bead.Assignee)
		if holder == "" {
			holder = "another agent"
		}
		fmt.Fprintf(stderr, "gc bd-shim: bead %s already claimed by %s\n", id, holder) //nolint:errcheck // best-effort stderr
		return 1, true
	}
	return writeReadyJSON([]beads.Bead{bead}, stdout, stderr), true
}

// isBdShimInvocation reports whether this gc binary was invoked through the bd
// shim — i.e. argv[0]'s basename is exactly `bd`. The PATH install symlinks
// `bd` -> the gc binary; gc invoked under any other name runs normally.
func isBdShimInvocation(arg0 string) bool {
	return filepath.Base(arg0) == "bd"
}

// ensureRealBdResolvable prepends the directory of GC_BD_REAL to PATH so that a
// bare `bd` exec performed in-process resolves to the real bd binary rather than
// this shim. The in-process work BdStore execs "bd" (and the Router probes each
// backend's Get by id, which runs `bd show`), so without this guard a shim
// installed as `bd` first on PATH would recurse on routed verbs. No-op when
// GC_BD_REAL is unset/relative or its directory already leads PATH.
func ensureRealBdResolvable() {
	raw := strings.TrimSpace(os.Getenv(realBdEnvVar))
	if raw == "" || !filepath.IsAbs(raw) {
		return
	}
	dir := filepath.Dir(raw)
	path := os.Getenv("PATH")
	sep := string(os.PathListSeparator)
	if path == dir || strings.HasPrefix(path, dir+sep) {
		return // already first; don't accumulate duplicate entries
	}
	if path == "" {
		_ = os.Setenv("PATH", dir) //nolint:errcheck // best-effort
		return
	}
	_ = os.Setenv("PATH", dir+sep+path) //nolint:errcheck // best-effort
}

// dispatchBdShimArgv0 runs the bd shim when this gc binary was invoked as `bd`,
// returning (exitCode, true). Otherwise it returns (0, false) and the caller
// proceeds with the normal gc command tree. When invoked as bd without
// GC_BD_REAL set it refuses loudly rather than recursing — a bare bd lookup
// would resolve back to this shim.
func dispatchBdShimArgv0(arg0 string, args []string, stdin io.Reader, stdout, stderr io.Writer) (int, bool) {
	if !isBdShimInvocation(arg0) {
		return 0, false
	}
	if strings.TrimSpace(os.Getenv(realBdEnvVar)) == "" {
		fmt.Fprintf(stderr, "bd (gc shim): %s must point at the real bd binary when gc runs as the bd shim; refusing to run (a bare bd lookup would recurse into the shim)\n", realBdEnvVar) //nolint:errcheck // best-effort stderr
		return 1, true
	}
	ensureRealBdResolvable()
	return runBdShim(args, stdin, stdout, stderr), true
}

// newBdShimCmd registers the hidden `gc bd-shim` subcommand: the bd-compatible
// thin client. It is hidden because operators invoke it as `bd` (via a PATH
// install), not by name; exposing it as a gc subcommand keeps it testable and
// lets the install point a `bd` shim at `gc bd-shim`.
func newBdShimCmd(stdout, stderr io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:                "bd-shim [bd-args...]",
		Short:              "bd-compatible thin client routing graph beads through the in-process Router",
		Hidden:             true,
		DisableFlagParsing: true,
		RunE: func(_ *cobra.Command, args []string) error {
			return exitForCode(runBdShim(args, os.Stdin, stdout, stderr))
		},
	}
}
