package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/gastownhall/gascity/internal/api"
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

// bdUpdateFlagNeedsValue mirrors bdshim.UpdateFlagNeedsValue — the routable
// update flags that consume the following token as their value — for
// parseBdUpdateOpts.
var bdUpdateFlagNeedsValue = bdshim.UpdateFlagNeedsValue

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

// bdMolRoutable reports whether a `bd mol` arg list is a routable read and
// returns the parsed subcommand/id/json; see bdshim.MolRoutable.
func bdMolRoutable(args []string) (sub, id string, jsonOut, ok bool) {
	return bdshim.MolRoutable(args)
}

// parseBdListOpts maps a routable `bd list` arg list onto api.ListBeadsOpts. The
// default limit mirrors bd's default page size (50). Known v1 caveat: an
// explicit `--limit 0` (bd's "unlimited") maps to the server's default page size
// rather than true-unlimited; no hot-path traffic uses that shape.
func parseBdListOpts(args []string) (api.ListBeadsOpts, error) {
	opts := api.ListBeadsOpts{Limit: 50}
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case (a == "--status" || a == "-s") && i+1 < len(args):
			opts.Status = args[i+1]
			i++
		case strings.HasPrefix(a, "--status="):
			opts.Status = strings.TrimPrefix(a, "--status=")
		case (a == "--assignee" || a == "-a") && i+1 < len(args):
			opts.Assignee = args[i+1]
			i++
		case strings.HasPrefix(a, "--assignee="):
			opts.Assignee = strings.TrimPrefix(a, "--assignee=")
		case (a == "--type" || a == "-t") && i+1 < len(args):
			opts.Type = args[i+1]
			i++
		case strings.HasPrefix(a, "--type="):
			opts.Type = strings.TrimPrefix(a, "--type=")
		case (a == "--label" || a == "-l") && i+1 < len(args):
			opts.Label = args[i+1]
			i++
		case strings.HasPrefix(a, "--label="):
			opts.Label = strings.TrimPrefix(a, "--label=")
		case (a == "--limit" || a == "-n") && i+1 < len(args):
			n, err := strconv.Atoi(args[i+1])
			if err != nil {
				return opts, fmt.Errorf("parse %s %q: %w", a, args[i+1], err)
			}
			opts.Limit = n
			i++
		case strings.HasPrefix(a, "--limit="):
			n, err := strconv.Atoi(strings.TrimPrefix(a, "--limit="))
			if err != nil {
				return opts, fmt.Errorf("parse %q: %w", a, err)
			}
			opts.Limit = n
		case a == "--all":
			opts.All = true
		}
	}
	return opts, nil
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
// HTTP API (the pure-HTTP redirect: the controller owns the store, every worker
// is a thin client). It is the API counterpart of dispatchBdShimVerb — reads
// render the same JSON, mutations map onto the bead write-path client methods.
func dispatchBdShimVerbViaAPI(client *api.Client, verb string, args []string, stdout, stderr io.Writer) int {
	// Every call here is a controller dispatch (the classifier already decided
	// bdRoute) — a warm-pool hit with no direct worker->Dolt dial, whether the
	// dispatch ultimately succeeds or the API returns an error.
	telemetry.RecordBDShimDisposition(context.Background(), verb, bdRoute.String())
	switch verb {
	case "close":
		id, ok := firstBdPositional(args)
		if !ok {
			fmt.Fprintln(stderr, "gc bd-shim: usage: close <id>") //nolint:errcheck // best-effort stderr
			return 1
		}
		if err := client.CloseBead(id); err != nil {
			fmt.Fprintf(stderr, "gc bd-shim: closing %q via API: %v\n", id, err) //nolint:errcheck // best-effort stderr
			return 1
		}
		return 0
	case "reopen":
		id, ok := firstBdPositional(args)
		if !ok {
			fmt.Fprintln(stderr, "gc bd-shim: usage: reopen <id>") //nolint:errcheck // best-effort stderr
			return 1
		}
		if err := client.ReopenBead(id); err != nil {
			fmt.Fprintf(stderr, "gc bd-shim: reopening %q via API: %v\n", id, err) //nolint:errcheck // best-effort stderr
			return 1
		}
		return 0
	case "delete":
		id, ok := firstBdPositional(args)
		if !ok {
			fmt.Fprintln(stderr, "gc bd-shim: usage: delete <id>") //nolint:errcheck // best-effort stderr
			return 1
		}
		if err := client.DeleteBead(id); err != nil {
			fmt.Fprintf(stderr, "gc bd-shim: deleting %q via API: %v\n", id, err) //nolint:errcheck // best-effort stderr
			return 1
		}
		return 0
	case "update":
		id, ok := firstBdPositional(args)
		if !ok {
			fmt.Fprintln(stderr, "gc bd-shim: usage: update <id> [flags]") //nolint:errcheck // best-effort stderr
			return 1
		}
		opts, err := parseBdUpdateOpts(args)
		if err != nil {
			fmt.Fprintf(stderr, "gc bd-shim: update %q: %v\n", id, err) //nolint:errcheck // best-effort stderr
			return 1
		}
		if err := client.UpdateBead(id, opts); err != nil {
			fmt.Fprintf(stderr, "gc bd-shim: updating %q via API: %v\n", id, err) //nolint:errcheck // best-effort stderr
			return 1
		}
		return 0
	case "show":
		id, ok := firstBdPositional(args)
		if !ok {
			fmt.Fprintln(stderr, "gc bd-shim: usage: show <id>") //nolint:errcheck // best-effort stderr
			return 1
		}
		read, err := client.GetBead(id)
		if err != nil {
			if isBdShimAPINotFound(err) {
				// Raw bd prints an empty array (exit 0) for an unknown id; a
				// `bd show ... --json | jq '.[0]'` consumer reads that as absent.
				return writeReadyJSON(nil, stdout, stderr)
			}
			fmt.Fprintf(stderr, "gc bd-shim: show %q via API: %v\n", id, err) //nolint:errcheck // best-effort stderr
			return 1
		}
		return writeReadyJSON([]beads.Bead{read.Body}, stdout, stderr)
	case "ready":
		p, err := parseBdReadyParams(args)
		if err != nil {
			fmt.Fprintf(stderr, "gc bd-shim: %v\n", err) //nolint:errcheck // best-effort stderr
			return 1
		}
		read, err := client.ReadyBeads()
		if err != nil {
			fmt.Fprintf(stderr, "gc bd-shim: ready via API: %v\n", err) //nolint:errcheck // best-effort stderr
			return 1
		}
		// /v0/beads/ready takes no predicates; apply the discovery post-filter
		// (assignee/metadata-field/unassigned/exclude-type/limit) client-side.
		return writeReadyJSON(applyBdReadyParams(read.Body, p), stdout, stderr)
	case "list":
		opts, err := parseBdListOpts(args)
		if err != nil {
			fmt.Fprintf(stderr, "gc bd-shim: list: %v\n", err) //nolint:errcheck // best-effort stderr
			return 1
		}
		read, err := client.ListBeads(opts)
		if err != nil {
			fmt.Fprintf(stderr, "gc bd-shim: list via API: %v\n", err) //nolint:errcheck // best-effort stderr
			return 1
		}
		return writeReadyJSON(read.Body, stdout, stderr)
	case "query":
		opts, ok := parseBdQueryEphemeral(args)
		if !ok {
			fmt.Fprintln(stderr, "gc bd-shim: query: routable only as `ephemeral=true ...` --json") //nolint:errcheck // best-effort stderr
			return 1
		}
		read, err := client.EphemeralBeads(opts)
		if err != nil {
			fmt.Fprintf(stderr, "gc bd-shim: query via API: %v\n", err) //nolint:errcheck // best-effort stderr
			return 1
		}
		return writeReadyJSON(read.Body, stdout, stderr)
	case "mol":
		sub, id, jsonOut, ok := bdMolRoutable(args)
		if !ok {
			fmt.Fprintln(stderr, "gc bd-shim: mol: routable only as `current|progress <id>`") //nolint:errcheck // best-effort stderr
			return 1
		}
		g, err := client.GetBeadGraph(id)
		if err != nil {
			fmt.Fprintf(stderr, "gc bd-shim: mol %s %q via API: %v\n", sub, id, err) //nolint:errcheck // best-effort stderr
			return 1
		}
		return renderBdMol(sub, g, jsonOut, stdout, stderr)
	case "create":
		b, jsonOut, err := parseBdCreateBead(args)
		if err != nil {
			fmt.Fprintf(stderr, "gc bd-shim: create: %v\n", err) //nolint:errcheck // best-effort stderr
			return 1
		}
		created, err := client.CreateBead(b)
		if err != nil {
			fmt.Fprintf(stderr, "gc bd-shim: create via API: %v\n", err) //nolint:errcheck // best-effort stderr
			return 1
		}
		if jsonOut {
			enc, err := json.Marshal(created)
			if err != nil {
				fmt.Fprintf(stderr, "gc bd-shim: create: marshal: %v\n", err) //nolint:errcheck // best-effort stderr
				return 1
			}
			fmt.Fprintln(stdout, string(enc)) //nolint:errcheck // best-effort stdout
			return 0
		}
		fmt.Fprintf(stdout, "Created bead: %s\n", created.ID) //nolint:errcheck // best-effort stdout
		return 0
	default:
		fmt.Fprintf(stderr, "gc bd-shim: no routed API handler for %q\n", verb) //nolint:errcheck // best-effort stderr
		return 1
	}
}

// parseBdQueryEphemeral maps the two in-repo `bd query` ephemeral shapes —
// listEphemeral's multi-clause argv (bdstore.go) and the work_query literal
// `bd query --json 'ephemeral=true AND status=<s>' --limit=N` (config.go) — onto
// EphemeralBeadsOpts. It returns ok=false for any shape it cannot map cleanly,
// so the caller refuses/passes through rather than silently dropping clauses.
func parseBdQueryEphemeral(args []string) (api.EphemeralBeadsOpts, bool) {
	var opts api.EphemeralBeadsOpts
	var predicate string
	var sawJSON, sawPredicate bool
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "query":
			continue
		case a == "--json":
			sawJSON = true
		case a == "--all":
			opts.All = true
		case a == "--limit" || a == "-n":
			if i+1 >= len(args) {
				return opts, false
			}
			n, err := strconv.Atoi(args[i+1])
			if err != nil {
				return opts, false
			}
			opts.Limit = n
			i++
		case strings.HasPrefix(a, "--limit="):
			n, err := strconv.Atoi(strings.TrimPrefix(a, "--limit="))
			if err != nil {
				return opts, false
			}
			opts.Limit = n
		case strings.HasPrefix(a, "-"):
			return opts, false // unknown flag — not faithfully routable
		default:
			if sawPredicate {
				return opts, false // a second positional we can't account for
			}
			predicate = a
			sawPredicate = true
		}
	}
	if !sawJSON || !sawPredicate {
		return opts, false
	}
	if !parseEphemeralPredicate(predicate, &opts) {
		return opts, false
	}
	return opts, true
}

// parseEphemeralPredicate parses an `ephemeral=true [AND key=value]...` predicate
// into opts. The predicate MUST contain ephemeral=true; every other clause must
// be a bare key=value with key in {status,label,type,assignee,parent}.
func parseEphemeralPredicate(predicate string, opts *api.EphemeralBeadsOpts) bool {
	sawEphemeral := false
	for _, clause := range strings.Split(predicate, " AND ") {
		k, v, ok := strings.Cut(strings.TrimSpace(clause), "=")
		if !ok {
			return false
		}
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		if k == "ephemeral" {
			if v != "true" {
				return false
			}
			sawEphemeral = true
			continue
		}
		if !isBareBdQueryValue(v) {
			return false
		}
		switch k {
		case "status":
			opts.Status = v
		case "label":
			opts.Label = v
		case "type":
			opts.Type = v
		case "assignee":
			opts.Assignee = v
		case "parent":
			opts.Parent = v
		default:
			return false
		}
	}
	return sawEphemeral
}

// isBareBdQueryValue reports whether v is a server-routable bare value
// (alphanumerics plus _-:.), mirroring the bd store's isBareBdQueryValue.
func isBareBdQueryValue(v string) bool {
	if v == "" {
		return false
	}
	for _, r := range v {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '_' || r == '-' || r == ':' || r == '.':
		default:
			return false
		}
	}
	return true
}

// renderBdMol renders `bd mol current|progress` from a fetched bead graph. Step
// indicators derive from each bead's status (done=closed, current=in_progress,
// pending=open). The graph endpoint returns parent-child topology but not
// blocking-dep edges, so open steps render as [pending] rather than asserting
// [ready]/[blocked] — exact ready/blocked discrimination is C2a's byte-identity
// work. Routing here (X2) reaches SQLite-resident topology the work-only bd
// cannot see; the text is LLM-facing situational awareness, not a parsed wire.
func renderBdMol(sub string, g api.BeadGraph, jsonOut bool, stdout, stderr io.Writer) int {
	steps := molSteps(g)
	if jsonOut {
		return writeReadyJSON(steps, stdout, stderr)
	}
	done := 0
	for _, b := range steps {
		if b.Status == "closed" {
			done++
		}
	}
	switch sub {
	case "progress":
		pct := 0
		if len(steps) > 0 {
			pct = done * 100 / len(steps)
		}
		fmt.Fprintf(stdout, "%s: %d/%d steps complete (%d%%)\n", g.Root.ID, done, len(steps), pct) //nolint:errcheck // best-effort stdout
	default: // current
		fmt.Fprintf(stdout, "Molecule %s — %s (%d/%d done)\n", g.Root.ID, g.Root.Title, done, len(steps)) //nolint:errcheck // best-effort stdout
		for _, b := range steps {
			fmt.Fprintf(stdout, "  [%s] %s %s\n", molStepIndicator(b), b.ID, b.Title) //nolint:errcheck // best-effort stdout
		}
	}
	return 0
}

// molSteps returns the molecule's step beads (every graph bead except the root),
// preserving the endpoint's order.
func molSteps(g api.BeadGraph) []beads.Bead {
	steps := make([]beads.Bead, 0, len(g.Beads))
	for _, b := range g.Beads {
		if b.ID == g.Root.ID {
			continue
		}
		steps = append(steps, b)
	}
	return steps
}

func molStepIndicator(b beads.Bead) string {
	switch b.Status {
	case "closed":
		return "done"
	case "in_progress":
		return "current"
	default:
		return "pending"
	}
}

// parseBdCreateBead parses the routable `bd create` args (title positional plus
// the create flags in bdCreateRoutableFlags) into a beads.Bead and whether
// --json output was requested. Non-routable flags never reach here
// (classifyBdShimVerb passes those through).
func parseBdCreateBead(args []string) (beads.Bead, bool, error) {
	var b beads.Bead
	jsonOut := false
	gotTitle := false
	needsValue := map[string]bool{
		"--type": true, "--priority": true, "--assignee": true, "--label": true,
		"--description": true, "--parent": true, "--set-metadata": true,
		"--metadata": true, "--defer-until": true,
	}
	for i := 0; i < len(args); i++ {
		a := args[i]
		if !strings.HasPrefix(a, "-") {
			if !gotTitle {
				b.Title = a
				gotTitle = true
			}
			continue
		}
		name := a
		val := ""
		hasVal := false
		if eq := strings.IndexByte(a, '='); eq >= 0 {
			name, val, hasVal = a[:eq], a[eq+1:], true
		}
		if !hasVal && needsValue[name] && i+1 < len(args) {
			val = args[i+1]
			hasVal = true
			i++
		}
		switch name {
		case "--type":
			b.Type = val
		case "--assignee":
			b.Assignee = val
		case "--description":
			b.Description = val
		case "--parent":
			b.ParentID = val
		case "--priority":
			n, err := strconv.Atoi(val)
			if err != nil {
				return b, jsonOut, fmt.Errorf("parse --priority %q: %w", val, err)
			}
			b.Priority = &n
		case "--label":
			b.Labels = append(b.Labels, val)
		case "--set-metadata", "--metadata":
			k, mv, ok := strings.Cut(val, "=")
			if !ok {
				return b, jsonOut, fmt.Errorf("%s expects key=value, got %q", name, val)
			}
			if b.Metadata == nil {
				b.Metadata = map[string]string{}
			}
			b.Metadata[k] = mv
		case "--json":
			jsonOut = true
		}
	}
	if !gotTitle {
		return b, jsonOut, fmt.Errorf("create requires a title")
	}
	return b, jsonOut, nil
}

// isBdShimAPINotFound reports whether an API client error is a not-found, so
// `show` can reproduce raw bd's empty-array-for-unknown-id contract instead of
// failing.
func isBdShimAPINotFound(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "not found") || strings.Contains(msg, "not_found")
}

// firstBdPositional returns the first non-flag argument (a bead id), or false
// when every argument is a flag.
func firstBdPositional(args []string) (string, bool) {
	for _, a := range args {
		if !strings.HasPrefix(a, "-") {
			return a, true
		}
	}
	return "", false
}

// bdReadyParams is a parsed `bd ready` invocation. query carries the predicates
// the Router's ReadyQuery can express (Assignee); the rest are applied as a
// Go-side post-filter over the federated ready set (so they work against the
// SQLite graph backend the ReadyQuery itself cannot describe). limit is applied
// after filtering — it bounds the post-filtered result, matching bd.
type bdReadyParams struct {
	query          beads.ReadyQuery
	metadataEquals map[string]string // --metadata-field k=v (all must match)
	unassigned     bool              // --unassigned
	excludeTypes   map[string]bool   // --exclude-type=T (repeatable)
	limit          int               // --limit / -n
}

// parseBdReadyParams parses the routable `bd ready` flags. --assignee feeds the
// ReadyQuery; --metadata-field/--unassigned/--exclude-type/--limit feed the
// post-filter; --json/--include-ephemeral/--sort are accepted no-ops (output is
// always JSON, tier expansion is the policy wrapper's job, and the federated
// ready set is already created-asc which is bd's "oldest" order). Non-routable
// flags never reach here — classifyBdShimVerb passes those through.
func parseBdReadyParams(args []string) (bdReadyParams, error) {
	p := bdReadyParams{metadataEquals: map[string]string{}, excludeTypes: map[string]bool{}}
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--assignee" && i+1 < len(args):
			p.query.Assignee = args[i+1]
			i++
		case strings.HasPrefix(a, "--assignee="):
			p.query.Assignee = strings.TrimPrefix(a, "--assignee=")
		case a == "--unassigned":
			p.unassigned = true
		case (a == "--metadata-field") && i+1 < len(args):
			if err := addMetadataEquals(p.metadataEquals, args[i+1]); err != nil {
				return p, err
			}
			i++
		case strings.HasPrefix(a, "--metadata-field="):
			if err := addMetadataEquals(p.metadataEquals, strings.TrimPrefix(a, "--metadata-field=")); err != nil {
				return p, err
			}
		case a == "--exclude-type" && i+1 < len(args):
			p.excludeTypes[args[i+1]] = true
			i++
		case strings.HasPrefix(a, "--exclude-type="):
			p.excludeTypes[strings.TrimPrefix(a, "--exclude-type=")] = true
		case (a == "--limit" || a == "-n") && i+1 < len(args):
			n, err := strconv.Atoi(args[i+1])
			if err != nil {
				return p, fmt.Errorf("parse %s %q: %w", a, args[i+1], err)
			}
			p.limit = n
			i++
		case strings.HasPrefix(a, "--limit="):
			n, err := strconv.Atoi(strings.TrimPrefix(a, "--limit="))
			if err != nil {
				return p, fmt.Errorf("parse %q: %w", a, err)
			}
			p.limit = n
		case a == "--sort" && i+1 < len(args):
			i++ // value consumed; federated ready is already created-asc
		}
	}
	return p, nil
}

// addMetadataEquals records a `k=v` --metadata-field predicate.
func addMetadataEquals(into map[string]string, kv string) error {
	k, v, ok := strings.Cut(kv, "=")
	if !ok {
		return fmt.Errorf("--metadata-field expects key=value, got %q", kv)
	}
	into[k] = v
	return nil
}

// applyBdReadyParams filters a federated ready set by the post-filter predicates
// and applies the limit last. The input is assumed created-asc (Router.Ready's
// canonical order), so a `--limit N` after filtering matches `bd ready ... -n N`.
func applyBdReadyParams(in []beads.Bead, p bdReadyParams) []beads.Bead {
	out := make([]beads.Bead, 0, len(in))
	for _, b := range in {
		if p.unassigned && strings.TrimSpace(b.Assignee) != "" {
			continue
		}
		if p.excludeTypes[b.Type] {
			continue
		}
		match := true
		for k, v := range p.metadataEquals {
			if b.Metadata[k] != v {
				match = false
				break
			}
		}
		if !match {
			continue
		}
		out = append(out, b)
	}
	if p.limit > 0 && len(out) > p.limit {
		out = out[:p.limit]
	}
	return out
}

// parseBdUpdateOpts maps the routable `bd update` flags onto a beads.UpdateOpts.
// It ignores the leading id positional; only flags in bdUpdateRoutableFlags
// reach here (classifyBdShimVerb passes the rest through), so an unknown flag is
// silently skipped rather than erroring. --set-metadata is repeatable.
func parseBdUpdateOpts(args []string) (beads.UpdateOpts, error) {
	var opts beads.UpdateOpts
	for i := 0; i < len(args); i++ {
		a := args[i]
		if !strings.HasPrefix(a, "-") {
			continue // the id positional or a consumed value
		}
		name := a
		val := ""
		hasVal := false
		if eq := strings.IndexByte(a, '='); eq >= 0 {
			name, val, hasVal = a[:eq], a[eq+1:], true
		}
		if !hasVal && bdUpdateFlagNeedsValue[name] && i+1 < len(args) {
			val = args[i+1]
			hasVal = true
			i++
		}
		switch name {
		case "--status":
			s := val
			opts.Status = &s
		case "--assignee":
			s := val
			opts.Assignee = &s
		case "--title":
			s := val
			opts.Title = &s
		case "--type":
			s := val
			opts.Type = &s
		case "--description":
			s := val
			opts.Description = &s
		case "--parent":
			s := val
			opts.ParentID = &s
		case "--priority":
			n, err := strconv.Atoi(val)
			if err != nil {
				return opts, fmt.Errorf("parse --priority %q: %w", val, err)
			}
			opts.Priority = &n
		case "--label":
			opts.Labels = append(opts.Labels, val)
		case "--remove-label":
			opts.RemoveLabels = append(opts.RemoveLabels, val)
		case "--set-metadata":
			k, mv, ok := strings.Cut(val, "=")
			if !ok {
				return opts, fmt.Errorf("--set-metadata expects key=value, got %q", val)
			}
			if opts.Metadata == nil {
				opts.Metadata = map[string]string{}
			}
			opts.Metadata[k] = mv
		}
	}
	return opts, nil
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
