// Package bdshim holds the bd-shim verb classifier: the pure route-vs-passthrough
// decision logic that decides, for a given bd subcommand and its arguments,
// whether the shim serves the op via the controller's HTTP bead API (Route),
// execs the real bd binary unchanged (Passthrough), or refuses it loudly rather
// than silently bypassing the graph store (Refuse).
//
// This package is deliberately dependency-light: it imports only stdlib and is
// safe to import from a tiny standalone bd binary as well as from cmd/gc. It
// contains no I/O, no cobra, and no internal/api coupling — the api-populating
// parse paths (parseBdListOpts, parseBdReadyParams, parseBdQueryEphemeral, …)
// stay in cmd/gc where the api types live.
package bdshim

import (
	"strconv"
	"strings"
)

// Disposition is how the shim handles one bd subcommand.
type Disposition int

const (
	// Passthrough execs the real bd binary unchanged.
	Passthrough Disposition = iota
	// Route serves the verb via the controller's HTTP bead API.
	Route
	// Refuse rejects the verb rather than silently bypassing the graph store.
	Refuse
)

// String renders a Disposition as the lowercase token used in telemetry.
func (d Disposition) String() string {
	switch d {
	case Route:
		return "route"
	case Refuse:
		return "refuse"
	default:
		return "passthrough"
	}
}

// RoutedVerbs are bd subcommands the shim translates to in-process Router
// store ops so graph beads in the embedded SQLite store are seen and mutated,
// not just Dolt work beads. Grown incrementally. Show and list deliberately do
// not appear here: bd's JSON projections carry backend-computed IssueDetails
// and IssueWithCounts fields that the controller Bead does not retain. They
// stay on the real CLI until a typed compatibility projection owns those fields.
var RoutedVerbs = map[string]bool{
	"close":  true,
	"ready":  true,
	"update": true,
	"reopen": true,
	"delete": true,
	"create": true,
}

// CreateRoutableFlags are the `bd create` flags that map cleanly onto the
// create API body. A create carrying any OTHER flag (--ephemeral, --no-history,
// --from, ...) passes through to the real bd rather than silently dropping the
// unmapped effect.
var CreateRoutableFlags = map[string]bool{
	"--type":         true,
	"--priority":     true,
	"--assignee":     true,
	"--label":        true,
	"--description":  true,
	"--parent":       true,
	"--set-metadata": true,
	"--metadata":     true,
	"--defer-until":  true,
	"--json":         true,
}

// CreateRoutable reports whether a `bd create` arg list uses only flags that
// map onto the create API body, so the shim can serve it in-process.
func CreateRoutable(args []string) bool {
	for _, a := range args {
		if !strings.HasPrefix(a, "-") {
			continue // the title positional or a space-separated flag value
		}
		name := a
		if i := strings.IndexByte(a, '='); i >= 0 {
			name = a[:i]
		}
		if !CreateRoutableFlags[name] {
			return false
		}
	}
	return true
}

// UpdateRoutableFlags are the `bd update` flags that map cleanly onto
// beads.UpdateOpts. An update whose body or notes mutation has no faithful
// translation is refused loudly; other unsupported flags pass through to real
// bd in the identity phase rather than being silently dropped.
var UpdateRoutableFlags = map[string]bool{
	"--status":       true,
	"--set-metadata": true,
	"--assignee":     true,
	"--label":        true,
	"--remove-label": true,
	"--title":        true,
	"--type":         true,
	"--priority":     true,
	"--description":  true,
	"-d":             true,
	"--parent":       true,
	"--json":         true,
}

// UpdateFlagNeedsValue is the subset of routable update flags that consume the
// following token as their value when written space-separated (--flag value).
var UpdateFlagNeedsValue = map[string]bool{
	"--status":       true,
	"--set-metadata": true,
	"--assignee":     true,
	"--label":        true,
	"--remove-label": true,
	"--title":        true,
	"--type":         true,
	"--priority":     true,
	"--description":  true,
	"-d":             true,
	"--parent":       true,
}

// UnsupportedUpdateMutationFlag reports body-like update flags that the shim
// cannot faithfully translate to the controller API. They must be refused, not
// passed through to bd.real: in a fastpath city that write can target a
// different Dolt working set while the next shim read returns stale data.
func UnsupportedUpdateMutationFlag(args []string) (string, bool) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if !strings.HasPrefix(a, "-") {
			continue
		}
		name := a
		hasInlineValue := false
		if eq := strings.IndexByte(a, '='); eq >= 0 {
			name = a[:eq]
			hasInlineValue = true
		}
		switch name {
		case "--allow-empty-description", "--body-file", "--stdin", "--append-notes", "--notes", "--design", "--design-file", "--acceptance":
			return name, true
		}
		if !hasInlineValue && UpdateFlagNeedsValue[name] && i+1 < len(args) {
			i++
		}
	}
	return "", false
}

// UpdatePositionals returns the positional arguments of a `bd update` arg list —
// the issue ids — skipping every token consumed as a flag's value. It shares
// UpdateFlagNeedsValue with UpdateRoutable and ParseUpdateOpts, so the three
// agree on which tokens are values and which are ids.
//
// Reading the first non-flag token as the id instead (the earlier rule) picked
// `a=1` out of `--set-metadata a=1 <id>`: cobra accepts flags before positionals
// and raw bd honors that ordering, so the routed write targeted a bead named
// after the metadata pair.
func UpdatePositionals(args []string) []string {
	var positionals []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if !strings.HasPrefix(a, "-") {
			positionals = append(positionals, a)
			continue
		}
		name := a
		hasInlineValue := strings.IndexByte(a, '=') >= 0
		if hasInlineValue {
			name = a[:strings.IndexByte(a, '=')]
		}
		if !hasInlineValue && UpdateFlagNeedsValue[name] && i+1 < len(args) {
			i++ // consume the space-separated value
		}
	}
	return positionals
}

// UpdateMistypedMetadataPair reports whether a `bd update` arg list carries a
// bare `key=value` token in bd's positional issue-id slot — the signature of a
// dropped `--set-metadata` pair.
//
// `--set-metadata` is a repeatable stringArray that takes ONE pair per flag, so
// in `--set-metadata a=1 b=2 c=3` only `a=1` is the flag's value; `b=2` and `c=3`
// become positional issue ids. Raw bd tries to resolve them, fails, prints the
// failures to stderr, still prints its success line for the id that DID resolve,
// and exits 0 — leaving a caller unable to distinguish a full write from a
// 1-of-N write by any means it has. Every `|| exit` guard in a fleet is blind to
// it (measured 2026-08-01; upstream beads v1.1.2 is unchanged).
//
// The guard is safe because no bead id contains '='. Such a positional never
// resolved under raw bd either, so refusing it cannot break an invocation that
// previously succeeded — it only replaces a silent partial write with a loud
// refusal issued BEFORE anything is written.
func UpdateMistypedMetadataPair(args []string) bool {
	for _, p := range UpdatePositionals(args) {
		if strings.IndexByte(p, '=') >= 0 {
			return true
		}
	}
	return false
}

// UpdateClaimShape reports whether a `bd update` arg list is the pure-claim
// shape the shim routes to the controller's atomic claim endpoint: it carries
// --claim and no other flag except --json (the id positional is allowed).
// A claim combined with any other mutation flag has no atomic claim-route
// translation and is left to UpdateRoutable / passthrough.
func UpdateClaimShape(args []string) bool {
	sawClaim := false
	for _, a := range args {
		if !strings.HasPrefix(a, "-") {
			continue // the id positional
		}
		name := a
		if i := strings.IndexByte(a, '='); i >= 0 {
			name = a[:i]
		}
		switch name {
		case "--claim":
			sawClaim = true
		case "--json":
			// allowed alongside --claim
		default:
			return false
		}
	}
	return sawClaim
}

// UpdateRoutable reports whether a `bd update` arg list uses only flags that
// map onto beads.UpdateOpts, so the shim can serve it in-process.
func UpdateRoutable(args []string) bool {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if !strings.HasPrefix(a, "-") {
			continue // the id positional or a space-separated flag value
		}
		name := a
		hasInlineValue := false
		if eq := strings.IndexByte(a, '='); eq >= 0 {
			name = a[:eq]
			hasInlineValue = true
		}
		if !UpdateRoutableFlags[name] {
			return false
		}
		if !hasInlineValue && UpdateFlagNeedsValue[name] && i+1 < len(args) {
			i++
		}
	}
	return true
}

// ReadyRoutableFlags are the `bd ready` flags Router.Ready replicates exactly
// (Assignee/Limit, plus output/tier flags that are no-ops here). A ready
// invocation carrying any OTHER flag — the pool-demand predicates
// (--metadata-field, --unassigned, --exclude-type, --sort, --label, ...) — is
// not yet federated (predicate parity is C3/ga-2gap48.11), so it passes through
// to the real bd (byte-identical in the identity phase) rather than silently
// dropping the filter.
var ReadyRoutableFlags = map[string]bool{
	"--assignee":          true,
	"--limit":             true,
	"-n":                  true,
	"--json":              true,
	"--include-ephemeral": true,
	// Discovery predicates the controller's serve loop and the pool-demand probe
	// use. The Router's ReadyQuery cannot express these, so the shim federates
	// store.Ready() and applies them as a Go-side post-filter (parseBdReadyParams
	// / applyBdReadyParams). This is what lets a graph control bead in SQLite be
	// discovered through `bd ready` (the deployed graph_store=sqlite crux).
	"--metadata-field": true,
	"--unassigned":     true,
	"--exclude-type":   true,
	"--sort":           true,
}

// ReadyRoutable reports whether a `bd ready` arg list uses only flags the shim
// can replicate (directly via ReadyQuery or via the discovery post-filter).
func ReadyRoutable(args []string) bool {
	for _, a := range args {
		if !strings.HasPrefix(a, "-") {
			continue // a bare value (e.g. a space-separated flag arg) — not a gate
		}
		name := a
		if i := strings.IndexByte(a, '='); i >= 0 {
			name = a[:i]
		}
		if !ReadyRoutableFlags[name] {
			return false
		}
	}
	return true
}

// ListRoutableFlags defines the `bd list` grammar an eventual typed
// compatibility projection would need to support. List currently always
// delegates to real bd because its IssueWithCounts JSON cannot be produced from
// the controller's Bead; this allowlist is retained as the candidate API shape,
// not as permission to route a list today.
var ListRoutableFlags = map[string]bool{
	"--status":   true,
	"-s":         true,
	"--assignee": true,
	"-a":         true,
	"--type":     true,
	"-t":         true,
	"--label":    true,
	"-l":         true,
	"--limit":    true,
	"-n":         true,
	"--all":      true,
	"--json":     true,
}

// ListRoutable reports whether a `bd list` arg list fits the retained candidate
// API grammar. ClassifyVerb deliberately does not route a true result until the
// controller provides the complete typed IssueWithCounts projection.
func ListRoutable(args []string) bool {
	hasJSON := false
	for _, a := range args {
		if !strings.HasPrefix(a, "-") {
			continue // a bare value (e.g. a space-separated flag arg) — not a gate
		}
		name := a
		if i := strings.IndexByte(a, '='); i >= 0 {
			name = a[:i]
		}
		if name == "--json" {
			hasJSON = true
		}
		if !ListRoutableFlags[name] {
			return false
		}
	}
	return hasJSON
}

// ListHasMetadataPredicate reports whether a `bd list` arg list carries a
// metadata predicate (--metadata-field / --has-metadata-key). It is retained
// for the eventual typed list projection; current list invocations all delegate
// to real bd regardless of this predicate.
func ListHasMetadataPredicate(args []string) bool {
	for _, a := range args {
		name := a
		if i := strings.IndexByte(a, '='); i >= 0 {
			name = a[:i]
		}
		if name == "--metadata-field" || name == "--has-metadata-key" {
			return true
		}
	}
	return false
}

// SplitGlobalFlags finds the bd subcommand past any leading global flags. bd
// accepts global flags before the subcommand (e.g. `bd --readonly --sandbox
// ready ...`, the controller's discovery form), so the verb is not always
// args[0]. It returns the verb and the args that follow it; leading global flags
// are dropped (they govern bd's execution mode, irrelevant to in-process Router
// reads). Returns ("", nil) when there is no subcommand.
func SplitGlobalFlags(args []string) (string, []string) {
	for i, a := range args {
		if !strings.HasPrefix(a, "-") {
			return a, args[i+1:]
		}
	}
	return "", nil
}

// GraphTouchingUnroutedVerbs are bd subcommands that read or mutate
// graph/wisp data but are not yet translated to Router ops. Passing them to the
// real (work-only) bd is byte-identical and safe while graph storage is off
// (the identity phase), but would SILENTLY miss graph beads once
// graph_store=sqlite is on — so in the split phase the shim refuses them loudly
// rather than dropping graph data (graph-store-rollout-plan.md §X2).
var GraphTouchingUnroutedVerbs = map[string]bool{
	"gate": true, // bd gate check --escalate — a mutation on gate beads
	// "query" and "mol" are now handled in ClassifyVerb: the ephemeral
	// discovery shape maps to GET /beads/ephemeral and mol current|progress to
	// GET /beads/graph/{root}, both reaching the SQLite graph store via the Router.
}

// QueryRoutingEnabled gates routing of `bd query` (ephemeral discovery) to the
// controller. It is true now that GET /beads/ephemeral and the EphemeralBeads
// client method are present on this fork: the mappable ephemeral shape
// (`--json 'ephemeral=true AND <bare clauses>'`) routes to the warm controller,
// reaching wisps resident in the SQLite graph backend through the Router. An
// unmappable query still refuses under the split phase rather than silently
// missing SQLite-resident wisps.
const QueryRoutingEnabled = true

// ClassifyVerb decides how the shim handles a bd subcommand given whether the
// city is in the split phase (graph_store=sqlite active, so a distinct graph
// backend exists). See the Disposition docs above.
func ClassifyVerb(verb string, args []string, splitPhase bool) Disposition {
	// bd show and bd list have richer output contracts than the controller's
	// Bead: show emits IssueDetails (including computed relation/comment counts)
	// and list emits IssueWithCounts. Passing them through is therefore the only
	// exact behavior in the identity phase. Once graph storage is split, a real
	// bd fallback could omit graph-resident beads, so refuse loudly until a typed
	// controller projection implements the complete contract.
	if verb == "show" || verb == "list" {
		if splitPhase {
			return Refuse
		}
		return Passthrough
	}

	// `bd query` (ephemeral discovery) routes when it is the mappable ephemeral
	// shape (`--json 'ephemeral=true AND <bare clauses>'`). An unmappable query
	// under the split phase must REFUSE rather than passthrough: passing it to the
	// work-only bd would silently miss SQLite-resident wisps (the §X2 hazard).
	if verb == "query" {
		if QueryRoutingEnabled && QueryRoutable(args) {
			return Route
		}
		if splitPhase {
			return Refuse
		}
		return Passthrough
	}
	// `bd mol current|progress <id>` routes to the graph endpoint; other mol
	// subcommands (pour/wisp/bond/...) and id-omitted/flag-heavy forms are not
	// faithfully routable and must refuse under the split phase (they would miss
	// SQLite-resident molecule topology), passthrough otherwise.
	if verb == "mol" {
		if MolRoutableArgs(args) {
			return Route
		}
		if splitPhase {
			return Refuse
		}
		return Passthrough
	}
	if RoutedVerbs[verb] {
		switch verb {
		case "ready":
			if !ReadyRoutable(args) {
				return Passthrough
			}
		case "update":
			if _, unsupported := UnsupportedUpdateMutationFlag(args); unsupported {
				return Refuse
			}
			// A bare key=value in the id slot is a dropped --set-metadata pair.
			// Refuse before any write rather than letting bd apply the subset and
			// exit 0.
			if UpdateMistypedMetadataPair(args) {
				return Refuse
			}
			// The routed write carries exactly one id, so a multi-id update belongs
			// to real bd, which applies it to EVERY id; routing served only the
			// first and reported success, silently leaving the rest unwritten.
			// (The id-LESS form keeps its existing loud refusal downstream rather
			// than inheriting bd's last-touched fallback — a separate, deliberate
			// choice this guard does not revisit.)
			if len(UpdatePositionals(args)) > 1 {
				return Passthrough
			}
			// The pure-claim shape routes to the atomic claim endpoint; the
			// actor gate and fallback live in the shim's caller (cmd/bdshim's
			// BEADS_ACTOR gate — env-dependent, kept out of this pure
			// classifier).
			if UpdateClaimShape(args) {
				return Route
			}
			if !UpdateRoutable(args) {
				return Passthrough
			}
		case "create":
			if !CreateRoutable(args) {
				return Passthrough
			}
		case "list":
			if !ListRoutable(args) {
				return Passthrough
			}
		}
		return Route
	}
	if splitPhase && GraphTouchingUnroutedVerbs[verb] {
		return Refuse
	}
	return Passthrough
}

// QueryRoutable reports whether a `bd query` arg list is the ephemeral
// discovery shape the shim can faithfully route: a --json query whose predicate
// is `ephemeral=true` optionally AND-joined with bare status/label/type/
// assignee/parent clauses. Anything else (non-ephemeral predicate, non-bare
// value, unknown flag, missing --json) is not routable.
//
// This is the pure routability half of cmd/gc's parseBdQueryEphemeral: it
// returns the identical boolean decision without populating the api.Ephemeral
// BeadsOpts struct (which keeps this package free of the internal/api dep). The
// two must stay in lockstep on the accepted shape; the arg-loop and predicate
// rules below mirror parseBdQueryEphemeral / parseEphemeralPredicate exactly.
func QueryRoutable(args []string) bool {
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
			// --all is a routable option (opts.All in the full parse); no gate here.
		case a == "--limit" || a == "-n":
			if i+1 >= len(args) {
				return false
			}
			if _, err := strconv.Atoi(args[i+1]); err != nil {
				return false
			}
			i++
		case strings.HasPrefix(a, "--limit="):
			if _, err := strconv.Atoi(strings.TrimPrefix(a, "--limit=")); err != nil {
				return false
			}
		case strings.HasPrefix(a, "-"):
			return false // unknown flag — not faithfully routable
		default:
			if sawPredicate {
				return false // a second positional we can't account for
			}
			predicate = a
			sawPredicate = true
		}
	}
	if !sawJSON || !sawPredicate {
		return false
	}
	return ephemeralPredicateRoutable(predicate)
}

// ephemeralPredicateRoutable reports whether an `ephemeral=true [AND key=value]...`
// predicate is routable. The predicate MUST contain ephemeral=true; every other
// clause must be a bare key=value with key in {status,label,type,assignee,parent}.
// It mirrors cmd/gc's parseEphemeralPredicate minus the opts population.
func ephemeralPredicateRoutable(predicate string) bool {
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
		if !isBareQueryValue(v) {
			return false
		}
		switch k {
		case "status", "label", "type", "assignee", "parent":
		default:
			return false
		}
	}
	return sawEphemeral
}

// isBareQueryValue reports whether v is a server-routable bare value
// (alphanumerics plus _-:.), mirroring the bd store's isBareBdQueryValue.
func isBareQueryValue(v string) bool {
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

// MolRoutable reports whether a `bd mol` arg list is a routable read —
// `current` or `progress` with an explicit molecule id and at most --json — and
// returns the parsed subcommand/id/json. Other subcommands (pour/wisp/bond/...),
// an omitted id (bd infers it from in_progress issues, which the rooted graph
// endpoint cannot express), or view flags (--for/--limit/--range) are not
// faithfully routable.
func MolRoutable(args []string) (sub, id string, jsonOut, ok bool) {
	if len(args) < 2 {
		return "", "", false, false
	}
	sub = args[0]
	if sub != "current" && sub != "progress" {
		return "", "", false, false
	}
	for _, a := range args[1:] {
		switch {
		case a == "--json":
			jsonOut = true
		case strings.HasPrefix(a, "-"):
			return "", "", false, false // --for/--limit/--range: not routable
		default:
			if id != "" {
				return "", "", false, false
			}
			id = a
		}
	}
	if id == "" {
		return "", "", false, false
	}
	return sub, id, jsonOut, true
}

// MolRoutableArgs reports whether a `bd mol` arg list is a routable read, the
// boolean projection of MolRoutable used by the classifier.
func MolRoutableArgs(args []string) bool {
	_, _, _, ok := MolRoutable(args)
	return ok
}
