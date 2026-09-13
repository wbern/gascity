package main

import (
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/spf13/cobra"
)

// errContinuityPartial signals that the continuity report completed but at
// least one evidence source was unavailable (store open failure, list
// failure). Distinct from "no history found", which is a complete, empty
// result — see ContinuityReport.Complete. The cobra wrapper maps it to
// errExit without an extra stderr message, since the report itself (already
// written to stdout) already names the unavailable source.
var errContinuityPartial = errors.New("continuity: one or more evidence sources unavailable")

// continuityCmdOptions captures the resolved CLI flags for one invocation of
// `gc analyze continuity`. Mirrors reliabilityCmdOptions's split between
// flag-binding (cobra) and the testable run function.
type continuityCmdOptions struct {
	cityPath string
	rig      string
	agent    string
	since    string
	jsonOut  bool
}

func newAnalyzeContinuityCmd(stdout, stderr io.Writer) *cobra.Command {
	opts := continuityCmdOptions{}
	cmd := &cobra.Command{
		Use:   "continuity --agent <identity>",
		Short: "Discover an agent's current or recent participation without a bead ID",
		Long: `Continuity finds active or recent work for an agent identity, scoped to
exactly one city and (optionally) one rig, without requiring the caller to
already know a bead ID.

It matches candidate beads by current assignee, gc.workspace_owner,
gc.session_name (and any prior aliases recorded on a matching session bead)
within the --since window, then renders the durable evidence already on those
beads: status, owner, route, formula/step, campaign, latest transition,
derivable expected next transition, and any active hold or blocker.

This is a read-only evidence surface. It never dispatches, rescues, re-slings,
restarts, or lifts holds, and it never changes gc hook semantics: gc hook's
empty result still means "no current authorized hook," not "no history."

Missing or unreadable evidence is reported as an explicit unavailable source,
never silently folded into "no history" — see the "complete" field in --json
output. Exit code 0 means the query completed (whether or not it found
results); a non-zero exit means at least one evidence source was unavailable
and the result may be incomplete.

Cross-city use is explicit repetition with a different --city/--rig; this
command never fans out across cities on its own.`,
		RunE: func(_ *cobra.Command, _ []string) error {
			err := runAnalyzeContinuity(opts, stdout, stderr)
			switch {
			case err == nil:
				return nil
			case errors.Is(err, errContinuityPartial):
				return errExit
			case errors.Is(err, errExit):
				return err
			default:
				fmt.Fprintf(stderr, "gc analyze continuity: %v\n", err) //nolint:errcheck
				return errExit
			}
		},
	}
	cmd.Flags().StringVar(&opts.cityPath, "city", "", "city directory (default: discover from cwd)")
	cmd.Flags().StringVar(&opts.rig, "rig", "", "rig name to scope the search (default: auto-detect from cwd, else the city store)")
	cmd.Flags().StringVar(&opts.agent, "agent", "", "agent identity to search for (required)")
	cmd.Flags().StringVar(&opts.since, "since", "30d",
		"how far back to search — duration (1h, 30d) or RFC3339 timestamp")
	cmd.Flags().BoolVar(&opts.jsonOut, "json", false, "emit JSON instead of a table")
	return cmd
}

// continuityStoreOpener resolves a beads.Store for a scope target. Indirected
// so tests can inject a beads.MemStore without touching disk or a real bd
// binary.
var continuityStoreOpener = func(cityPath string, target execStoreTarget) (beads.Store, error) {
	return openStoreAtForCity(target.ScopeRoot, cityPath)
}

// ContinuityReport is the top-level `gc analyze continuity` result.
type ContinuityReport struct {
	SchemaVersion     int                `json:"schema_version"`
	Agent             string             `json:"agent"`
	ObservedAt        time.Time          `json:"observed_at"`
	Since             time.Time          `json:"since,omitempty"`
	City              string             `json:"city"`
	Rig               string             `json:"rig,omitempty"`
	ScopeKind         string             `json:"scope_kind"`
	MatchedIdentities []string           `json:"matched_identities,omitempty"`
	Complete          bool               `json:"complete"`
	Unavailable       []string           `json:"unavailable_sources,omitempty"`
	Results           []ContinuityResult `json:"results"`
}

// ContinuityResult is one bead's worth of continuity evidence.
type ContinuityResult struct {
	BeadID                 string    `json:"bead_id"`
	Title                  string    `json:"title,omitempty"`
	City                   string    `json:"city"`
	Rig                    string    `json:"rig,omitempty"`
	Status                 string    `json:"status"`
	Owner                  string    `json:"owner,omitempty"`
	Assignee               string    `json:"assignee,omitempty"`
	RoutedTo               string    `json:"routed_to,omitempty"`
	Formula                string    `json:"formula,omitempty"`
	StepID                 string    `json:"step_id,omitempty"`
	Campaign               string    `json:"campaign,omitempty"`
	LastTransition         string    `json:"last_transition"`
	LastTransitionAt       time.Time `json:"last_transition_at"`
	ExpectedNextTransition string    `json:"expected_next_transition,omitempty"`
	Hold                   string    `json:"hold,omitempty"`
	LastParticipant        string    `json:"last_participant,omitempty"`
	LastParticipationAt    time.Time `json:"last_participation_at,omitempty"`
	ObservationSource      string    `json:"observation_source"`
	Ended                  bool      `json:"ended"`
}

// runAnalyzeContinuity is the testable core: resolves city/rig scope, opens
// the bounded bead store for that scope, discovers candidate beads by
// identity, and writes the rendered report. Returns errContinuityPartial
// (after writing the report) when an evidence source was unavailable, so the
// caller still sees the partial report on stdout.
func runAnalyzeContinuity(opts continuityCmdOptions, stdout, _ io.Writer) error {
	agent := strings.TrimSpace(opts.agent)
	if agent == "" {
		return fmt.Errorf("--agent is required")
	}

	now := time.Now().UTC()
	var sinceCutoff time.Time
	if strings.TrimSpace(opts.since) != "" {
		var err error
		sinceCutoff, err = parseTimeFlag(opts.since, now)
		if err != nil {
			return fmt.Errorf("--since: %w", err)
		}
	}

	cityPath, cfg, err := resolveContinuityCityAndConfig(opts)
	if err != nil {
		return err
	}
	target, err := resolveContinuityScope(cityPath, cfg, opts.rig)
	if err != nil {
		return err
	}

	report := ContinuityReport{
		SchemaVersion: 1,
		Agent:         agent,
		ObservedAt:    now,
		Since:         sinceCutoff,
		City:          cityPath,
		Rig:           target.RigName,
		ScopeKind:     target.ScopeKind,
		Complete:      true,
		Results:       []ContinuityResult{},
	}

	store, err := continuityStoreOpener(cityPath, target)
	if err != nil {
		report.Complete = false
		report.Unavailable = append(report.Unavailable,
			fmt.Sprintf("bead store (%s): %v", scopeLabel(target), err))
		if wErr := writeContinuityOutput(report, opts.jsonOut, stdout); wErr != nil {
			return wErr
		}
		return errContinuityPartial
	}

	allBeads, err := store.List(beads.ListQuery{
		AllowScan:     true,
		IncludeClosed: true,
		TierMode:      beads.TierBoth,
	})
	if err != nil {
		report.Complete = false
		report.Unavailable = append(report.Unavailable,
			fmt.Sprintf("bead list (%s): %v", scopeLabel(target), err))
		if wErr := writeContinuityOutput(report, opts.jsonOut, stdout); wErr != nil {
			return wErr
		}
		return errContinuityPartial
	}

	identities := resolveContinuityIdentities(agent, allBeads)
	report.MatchedIdentities = identities

	for _, b := range allBeads {
		if b.Type == "session" {
			// Session bookkeeping beads are identity evidence (aliases),
			// consumed by resolveContinuityIdentities above — not work rows.
			continue
		}
		if !continuityIdentityMatches(b, identities) {
			continue
		}
		evidenceAt := continuityEvidenceTime(b)
		if !sinceCutoff.IsZero() && evidenceAt.Before(sinceCutoff) {
			continue
		}
		report.Results = append(report.Results, buildContinuityResult(b, target, cityPath, now))
	}
	sort.Slice(report.Results, func(i, j int) bool {
		return report.Results[i].LastTransitionAt.After(report.Results[j].LastTransitionAt)
	})

	return writeContinuityOutput(report, opts.jsonOut, stdout)
}

// resolveContinuityCityAndConfig resolves the target city directory (via
// --city or cwd discovery, mirroring resolveEventsPath) and loads its config,
// the same pair `gc bd` scope resolution starts from.
func resolveContinuityCityAndConfig(opts continuityCmdOptions) (string, *config.City, error) {
	cityPath := strings.TrimSpace(opts.cityPath)
	var err error
	if cityPath != "" {
		cityPath, err = validateCityPath(cityPath)
		if err != nil {
			return "", nil, fmt.Errorf("--city %s: %w", opts.cityPath, err)
		}
	} else {
		cityPath, err = resolveCity()
		if err != nil {
			return "", nil, fmt.Errorf("could not locate city; pass --city: %w", err)
		}
	}
	cfg, err := config.Load(fsys.OSFS{}, filepath.Join(cityPath, "city.toml"))
	if err != nil {
		return "", nil, fmt.Errorf("loading city config: %w", err)
	}
	return cityPath, cfg, nil
}

// resolveContinuityScope resolves the bounded store-scope target: an
// explicit --rig, else cwd auto-detection (mirroring gc bd's own default),
// else the city store. Reuses the exact scope-target helpers `gc bd`
// commands use (bdRigScopeTarget/bdCityScopeTarget) rather than inventing a
// second scoping mechanism.
func resolveContinuityScope(cityPath string, cfg *config.City, rigName string) (execStoreTarget, error) {
	resolveRigPaths(cityPath, cfg.Rigs)
	rigName = strings.TrimSpace(rigName)
	if rigName != "" {
		rig, ok := rigByName(cfg, rigName)
		if !ok {
			return execStoreTarget{}, fmt.Errorf("rig %q not found", rigName)
		}
		if strings.TrimSpace(rig.Path) == "" {
			return execStoreTarget{}, fmt.Errorf("rig %q is declared but has no path binding", rig.Name)
		}
		return bdRigScopeTarget(cityPath, rig), nil
	}
	if rig, ok, err := bdRigFromCwd(cfg, cityPath); err == nil && ok {
		return bdRigScopeTarget(cityPath, rig), nil
	}
	return bdCityScopeTarget(cityPath, cfg), nil
}

// resolveContinuityIdentities expands the queried identity to include any
// prior aliases recorded on a matching session bead's alias history, so a
// renamed/recycled session's earlier work is still discoverable by its
// current identity. Returns a sorted, deduplicated slice that always
// includes the original queried identity even if no session bead matches.
func resolveContinuityIdentities(agent string, all []beads.Bead) []string {
	identities := map[string]bool{agent: true}
	for _, b := range all {
		if b.Type != "session" {
			continue
		}
		meta := b.Metadata
		alias := strings.TrimSpace(meta["alias"])
		sessionName := strings.TrimSpace(meta[beadmeta.SessionNameMetadataKey])
		if alias != agent && sessionName != agent && b.ID != agent && b.Assignee != agent {
			continue
		}
		if alias != "" {
			identities[alias] = true
		}
		if sessionName != "" {
			identities[sessionName] = true
		}
		for _, prior := range session.AliasHistory(meta) {
			identities[prior] = true
		}
	}
	out := make([]string, 0, len(identities))
	for id := range identities {
		if id != "" {
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out
}

// continuityIdentityMatches reports whether a bead carries current or
// historical participation evidence for any of the resolved identities,
// checking assignee, gc.workspace_owner, gc.session_name (both spellings),
// and gc.deferred_assignee — the identity fields named in the continuity
// spec (gci-56qch Story A).
func continuityIdentityMatches(b beads.Bead, identities []string) bool {
	for _, id := range identities {
		if b.Assignee == id {
			return true
		}
		if b.Metadata == nil {
			continue
		}
		if b.Metadata[beadmeta.WorkspaceOwnerMetadataKey] == id ||
			b.Metadata[beadmeta.SessionNameMetadataKey] == id ||
			b.Metadata[beadmeta.SessionNameCamelMetadataKey] == id ||
			b.Metadata[beadmeta.DeferredAssigneeMetadataKey] == id {
			return true
		}
	}
	return false
}

// continuityEvidenceTime returns the most recent durable timestamp on a
// bead — UpdatedAt when present, falling back to CreatedAt for legacy beads,
// matching beadUpdatedReferenceTime's convention in query.go.
func continuityEvidenceTime(b beads.Bead) time.Time {
	if !b.UpdatedAt.IsZero() {
		return b.UpdatedAt
	}
	return b.CreatedAt
}

// buildContinuityResult renders one bead's durable metadata into the
// minimum recoverable facts the continuity spec requires. Raw source values
// win over prose: every field here is read directly off bead state, not
// inferred beyond the documented ExpectedNextTransition/Hold derivations.
func buildContinuityResult(b beads.Bead, target execStoreTarget, cityPath string, now time.Time) ContinuityResult {
	meta := b.Metadata
	get := func(keys ...string) string {
		for _, k := range keys {
			if v := strings.TrimSpace(meta[k]); v != "" {
				return v
			}
		}
		return ""
	}

	routedTo := get(beadmeta.RoutedToMetadataKey, beadmeta.DeferredRoutedToMetadataKey, beadmeta.ExecutionRoutedToMetadataKey)
	formula := get(beadmeta.FormulaNameMetadataKey, beadmeta.FormulaMetadataKey)
	campaign := get(beadmeta.InputConvoyIDMetadataKey, beadmeta.RootBeadIDMetadataKey)
	lastParticipant := get(beadmeta.WorkspaceOwnerMetadataKey, beadmeta.SessionNameMetadataKey)
	if lastParticipant == "" {
		lastParticipant = b.Assignee
	}

	ended := b.Status == "closed"
	lastTransitionAt := continuityEvidenceTime(b)
	lastTransition := b.Status
	if ended {
		lastTransition = "closed"
	}

	var hold string
	switch {
	case strings.TrimSpace(b.AwaitType) == "human":
		hold = "human gate: " + b.ID
	case strings.TrimSpace(b.AwaitType) != "":
		hold = "await:" + strings.TrimSpace(b.AwaitType)
	case b.DeferUntil != nil && b.DeferUntil.After(now):
		hold = "deferred until " + b.DeferUntil.UTC().Format(time.RFC3339)
	}

	var expectedNext string
	switch {
	case ended:
		expectedNext = ""
	case hold != "":
		expectedNext = "wait-for-gate"
	case routedTo != "":
		expectedNext = "execution by " + routedTo
	}

	return ContinuityResult{
		BeadID:                 b.ID,
		Title:                  b.Title,
		City:                   cityPath,
		Rig:                    target.RigName,
		Status:                 b.Status,
		Owner:                  b.Owner,
		Assignee:               b.Assignee,
		RoutedTo:               routedTo,
		Formula:                formula,
		StepID:                 get(beadmeta.StepIDMetadataKey),
		Campaign:               campaign,
		LastTransition:         lastTransition,
		LastTransitionAt:       lastTransitionAt,
		ExpectedNextTransition: expectedNext,
		Hold:                   hold,
		LastParticipant:        lastParticipant,
		LastParticipationAt:    lastTransitionAt,
		ObservationSource:      "beadstore:" + scopeLabel(target),
		Ended:                  ended,
	}
}

func writeContinuityOutput(report ContinuityReport, jsonOut bool, stdout io.Writer) error {
	if jsonOut {
		return writeCLIJSONLine(stdout, report)
	}
	return formatContinuityTable(report, stdout)
}

func formatContinuityTable(report ContinuityReport, stdout io.Writer) error {
	scope := report.ScopeKind
	if report.Rig != "" {
		scope = fmt.Sprintf("rig %q", report.Rig)
	}
	fmt.Fprintf(stdout, "Continuity for %q — city %s, scope %s\n", report.Agent, report.City, scope) //nolint:errcheck
	if !report.Since.IsZero() {
		fmt.Fprintf(stdout, "Window: since %s\n", report.Since.Format(time.RFC3339)) //nolint:errcheck
	}
	if len(report.MatchedIdentities) > 1 {
		fmt.Fprintf(stdout, "Matched identities: %s\n", strings.Join(report.MatchedIdentities, ", ")) //nolint:errcheck
	}
	if !report.Complete {
		fmt.Fprintf(stdout, "INCOMPLETE — unavailable sources:\n") //nolint:errcheck
		for _, u := range report.Unavailable {
			fmt.Fprintf(stdout, "  - %s\n", u) //nolint:errcheck
		}
	}
	if len(report.Results) == 0 {
		if report.Complete {
			fmt.Fprintf(stdout, "not observed in %s scope (window=%s)\n", scope, sinceLabel(report)) //nolint:errcheck
		}
		return nil
	}
	for _, r := range report.Results {
		fmt.Fprintf(stdout, "\n%s  %s\n", r.BeadID, r.Title)                   //nolint:errcheck
		fmt.Fprintf(stdout, "  status=%s owner=%s assignee=%s routed_to=%s\n", //nolint:errcheck
			r.Status, r.Owner, r.Assignee, r.RoutedTo)
		fmt.Fprintf(stdout, "  formula=%s step=%s campaign=%s\n", r.Formula, r.StepID, r.Campaign)                     //nolint:errcheck
		fmt.Fprintf(stdout, "  last_transition=%s at=%s\n", r.LastTransition, r.LastTransitionAt.Format(time.RFC3339)) //nolint:errcheck
		if r.Hold != "" {
			fmt.Fprintf(stdout, "  hold=%s\n", r.Hold) //nolint:errcheck
		}
		if r.ExpectedNextTransition != "" {
			fmt.Fprintf(stdout, "  expected_next=%s\n", r.ExpectedNextTransition) //nolint:errcheck
		}
		fmt.Fprintf(stdout, "  last_participant=%s observation_source=%s\n", r.LastParticipant, r.ObservationSource) //nolint:errcheck
	}
	return nil
}

func sinceLabel(report ContinuityReport) string {
	if report.Since.IsZero() {
		return "all-time"
	}
	return report.Since.Format(time.RFC3339)
}
