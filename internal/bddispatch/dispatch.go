// Package bddispatch serves routed bd-shim verbs by calling the controller's
// HTTP bead API (the pure-HTTP redirect: the controller owns the store, every
// worker is a thin client). It is deliberately dependency-light — it imports
// only the HTTP client (internal/api), the bead types (internal/beads), the
// verb classifier (internal/bdshim), and telemetry (internal/telemetry) — so a
// standalone bd binary can serve routed verbs without importing package main,
// cobra, or the SDK's config/session wiring.
package bddispatch

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/bdshim"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/telemetry"
)

// DispatchViaAPI serves a routed bd verb by calling the controller's HTTP API
// (the pure-HTTP redirect: the controller owns the store, every worker is a
// thin client). Reads render the same JSON raw bd emits; mutations map onto the
// bead write-path client methods.
func DispatchViaAPI(client *api.Client, verb string, args []string, stdout, stderr io.Writer) int {
	// Every call here is a controller dispatch (the classifier already decided
	// bdshim.Route) — a warm-pool hit with no direct worker->Dolt dial, whether
	// the dispatch ultimately succeeds or the API returns an error.
	telemetry.RecordBDShimDisposition(context.Background(), verb, bdshim.Route.String())
	switch verb {
	case "close":
		id, ok := FirstBdPositional(args)
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
		id, ok := FirstBdPositional(args)
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
		id, ok := FirstBdPositional(args)
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
		id, ok := FirstBdPositional(args)
		if !ok {
			fmt.Fprintln(stderr, "gc bd-shim: usage: update <id> [flags]") //nolint:errcheck // best-effort stderr
			return 1
		}
		opts, err := ParseUpdateOpts(args)
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
		id, ok := FirstBdPositional(args)
		if !ok {
			fmt.Fprintln(stderr, "gc bd-shim: usage: show <id>") //nolint:errcheck // best-effort stderr
			return 1
		}
		read, err := client.GetBead(id)
		if err != nil {
			if isAPINotFound(err) {
				// Raw bd prints an empty array (exit 0) for an unknown id; a
				// `bd show ... --json | jq '.[0]'` consumer reads that as absent.
				return WriteReadyJSON(nil, stdout, stderr)
			}
			fmt.Fprintf(stderr, "gc bd-shim: show %q via API: %v\n", id, err) //nolint:errcheck // best-effort stderr
			return 1
		}
		return WriteReadyJSON([]beads.Bead{read.Body}, stdout, stderr)
	case "ready":
		p, err := ParseReadyParams(args)
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
		return WriteReadyJSON(applyReadyParams(read.Body, p), stdout, stderr)
	case "list":
		opts, err := ParseListOpts(args)
		if err != nil {
			fmt.Fprintf(stderr, "gc bd-shim: list: %v\n", err) //nolint:errcheck // best-effort stderr
			return 1
		}
		read, err := client.ListBeads(opts)
		if err != nil {
			fmt.Fprintf(stderr, "gc bd-shim: list via API: %v\n", err) //nolint:errcheck // best-effort stderr
			return 1
		}
		return WriteReadyJSON(read.Body, stdout, stderr)
	case "query":
		opts, ok := ParseQueryEphemeral(args)
		if !ok {
			fmt.Fprintln(stderr, "gc bd-shim: query: routable only as `ephemeral=true ...` --json") //nolint:errcheck // best-effort stderr
			return 1
		}
		read, err := client.EphemeralBeads(opts)
		if err != nil {
			fmt.Fprintf(stderr, "gc bd-shim: query via API: %v\n", err) //nolint:errcheck // best-effort stderr
			return 1
		}
		return WriteReadyJSON(read.Body, stdout, stderr)
	case "mol":
		sub, id, jsonOut, ok := bdshim.MolRoutable(args)
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
		b, jsonOut, err := ParseCreateBead(args)
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

// ParseListOpts maps a routable `bd list` arg list onto api.ListBeadsOpts. The
// default limit mirrors bd's default page size (50). Known v1 caveat: an
// explicit `--limit 0` (bd's "unlimited") maps to the server's default page size
// rather than true-unlimited; no hot-path traffic uses that shape.
func ParseListOpts(args []string) (api.ListBeadsOpts, error) {
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

// ParseQueryEphemeral maps the two in-repo `bd query` ephemeral shapes —
// listEphemeral's multi-clause argv (bdstore.go) and the work_query literal
// `bd query --json 'ephemeral=true AND status=<s>' --limit=N` (config.go) — onto
// EphemeralBeadsOpts. It returns ok=false for any shape it cannot map cleanly,
// so the caller refuses/passes through rather than silently dropping clauses.
func ParseQueryEphemeral(args []string) (api.EphemeralBeadsOpts, bool) {
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
		return WriteReadyJSON(steps, stdout, stderr)
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

// molStepIndicator maps a step bead's status onto its molecule indicator label.
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

// ParseCreateBead parses the routable `bd create` args (title positional plus
// the create flags) into a beads.Bead and whether --json output was requested.
// Non-routable flags never reach here (the classifier passes those through).
func ParseCreateBead(args []string) (beads.Bead, bool, error) {
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

// isAPINotFound reports whether an API client error is a not-found, so `show`
// can reproduce raw bd's empty-array-for-unknown-id contract instead of failing.
func isAPINotFound(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "not found") || strings.Contains(msg, "not_found")
}

// FirstBdPositional returns the first non-flag argument (a bead id), or false
// when every argument is a flag.
func FirstBdPositional(args []string) (string, bool) {
	for _, a := range args {
		if !strings.HasPrefix(a, "-") {
			return a, true
		}
	}
	return "", false
}

// ReadyParams is a parsed `bd ready` invocation. query carries the predicates
// the Router's ReadyQuery can express (Assignee); the rest are applied as a
// Go-side post-filter over the federated ready set (so they work against the
// SQLite graph backend the ReadyQuery itself cannot describe). limit is applied
// after filtering — it bounds the post-filtered result, matching bd.
type ReadyParams struct {
	query          beads.ReadyQuery
	metadataEquals map[string]string // --metadata-field k=v (all must match)
	unassigned     bool              // --unassigned
	excludeTypes   map[string]bool   // --exclude-type=T (repeatable)
	limit          int               // --limit / -n
}

// ParseReadyParams parses the routable `bd ready` flags. --assignee feeds the
// ReadyQuery; --metadata-field/--unassigned/--exclude-type/--limit feed the
// post-filter; --json/--include-ephemeral/--sort are accepted no-ops (output is
// always JSON, tier expansion is the policy wrapper's job, and the federated
// ready set is already created-asc which is bd's "oldest" order). Non-routable
// flags never reach here — the classifier passes those through.
func ParseReadyParams(args []string) (ReadyParams, error) {
	p := ReadyParams{metadataEquals: map[string]string{}, excludeTypes: map[string]bool{}}
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

// applyReadyParams filters a federated ready set by the post-filter predicates
// and applies the limit last. The input is assumed created-asc (Router.Ready's
// canonical order), so a `--limit N` after filtering matches `bd ready ... -n N`.
func applyReadyParams(in []beads.Bead, p ReadyParams) []beads.Bead {
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

// ParseUpdateOpts maps the routable `bd update` flags onto a beads.UpdateOpts.
// It ignores the leading id positional; only routable update flags reach here
// (the classifier passes the rest through), so an unknown flag is silently
// skipped rather than erroring. --set-metadata is repeatable.
func ParseUpdateOpts(args []string) (beads.UpdateOpts, error) {
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
		if !hasVal && bdshim.UpdateFlagNeedsValue[name] && i+1 < len(args) {
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

// WriteReadyJSON encodes the ready beads as a JSON array — never null, so a
// work_query consumer that unmarshals into []beads.Bead always sees valid JSON.
// (v1 extract of the upstream cmd_ready.go helper; the full `gc ready` command
// rides the graph-store Router and is intentionally not ported in v1.)
func WriteReadyJSON(out []beads.Bead, stdout, stderr io.Writer) int {
	if out == nil {
		out = []beads.Bead{}
	}
	if err := json.NewEncoder(stdout).Encode(out); err != nil {
		fmt.Fprintf(stderr, "gc bd-shim: encoding: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	return 0
}
