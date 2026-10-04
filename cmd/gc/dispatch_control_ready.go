package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/gastownhall/gascity/internal/bddispatch"
	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/shellquote"
)

// This file cuts the gc->bd read-storm documented on ga-ak6rt1: the
// control-dispatcher's per-tick readiness scan (workflowServeControlReadyQueryForBeads,
// dispatch_runtime.go) builds a shell script that fork-execs up to ~9
// bd/jq processes per agent per tick. Wire that same readiness evaluation to
// answer from an in-process CachingStore snapshot first, falling back to
// exactly one batched `bd ready --json` call when the snapshot can't answer,
// instead of the shell script's N separate `bd` invocations.
//
// Why this hooks into nextWorkflowServeBeads (the default workflowServeList
// implementation) rather than drainWorkflowServeWork: workflowServeList is a
// package var every existing serve-loop test overrides wholesale to fake the
// ready queue, so changing drainWorkflowServeWork's call site to bypass it
// for control-dispatcher agents would silently stop exercising ~25 existing
// tests' fakes. nextWorkflowServeBeads is never called directly by any
// existing test (they all replace workflowServeList outright), so extending
// its body here is additive: the exact query-string shape from
// workflowServeControlReadyQueryForBeads is unchanged (still asserted upon by
// TestWorkflowServeControlReadyQuery* tests), and any non-control-ready query
// -- or any failure standing up the cache -- falls straight through to the
// original shell exec, unchanged.

// controlReadyQueryMarkerPrefix identifies a workQuery produced by
// workflowServeControlReadyQueryForBeads. That function always writes this
// exact literal prefix (BD_EXPORT_AUTO=false plus a non-empty
// GC_CONTROL_TARGET, dispatch_runtime.go:788); no other work_query shape
// produces it.
const controlReadyQueryMarkerPrefix = "BD_EXPORT_AUTO=false GC_CONTROL_TARGET="

// controlReadyExcludeType mirrors the shell script's --exclude-type=epic.
const controlReadyExcludeType = "epic"

// controlReadyFallbackLimit bounds the single batched bd ready call issued
// when the cache can't answer. It must be generous enough that per-candidate/
// per-route filtering in Go (each capped at workflowServeScanLimit) is never
// starved by an earlier truncation at the bd layer -- unlike the shell script
// this replaces (which ran each candidate/route's own independently-capped bd
// call), this single batched call's cap is shared across every candidate and
// route, so it must hold a whole city's ready set even during the write
// bursts that make the cache dirty in the first place. It costs one bd call
// regardless of value, so err on the generous side; controlReadyFallbackReady
// also logs if a response ever comes back exactly at this limit, so silent
// truncation is at least observable.
const controlReadyFallbackLimit = 5000

// controlReadyCacheTTL bounds how long a primed control-ready snapshot is
// reused before the next tick re-primes it. A fresh CachingStore is built
// per drain invocation's first tick and reused for every ready bead
// processed in that invocation without any further bd calls; the TTL just
// caps how stale that snapshot can get across invocations (e.g. across the
// --follow loop's wake cycles) without needing a persistent, event-fed cache
// for the life of the process.
const controlReadyCacheTTL = 3 * time.Second

// controlReadyCacheFailureBackoff prevents a malformed or unavailable shim
// summary from turning the controller's readiness tick into a subprocess
// storm. The failure remains loud to the caller throughout the bounded pause.
const controlReadyCacheFailureBackoff = 30 * time.Second

// controlReadyFallbackMinTTL is the shortest time a scoped fallback round
// (taken when the whole-set summary prime fails integrity on a large rig) is
// reused before the next round. One round is several scoped bd ready calls;
// on a ~100k-issue rig each takes seconds, so reusing it for only
// controlReadyCacheTTL ran rounds back to back and saturated Dolt (gcw-qg49y).
// Control steps on such a rig are picked up within this bound.
const controlReadyFallbackMinTTL = 30 * time.Second

// controlReadyFallbackTTL returns how long a scoped fallback round that took
// lastRound is reused: at least controlReadyFallbackMinTTL, and twice the round
// when rounds are slow, so the fallback never spends more than about a third
// of the time querying.
func controlReadyFallbackTTL(lastRound time.Duration) time.Duration {
	if ttl := 2 * lastRound; ttl > controlReadyFallbackMinTTL {
		return ttl
	}
	return controlReadyFallbackMinTTL
}

// parsedControlReadyQuery holds the values workflowServeControlReadyQueryForBeads
// bakes into its generated shell command as env-var prefix assignments.
type parsedControlReadyQuery struct {
	target             string
	controlSessionName string
	legacyTarget       string
	bareTarget         string
	includeEphemeral   bool
}

// parseControlReadyQuery recognizes a workQuery built by
// workflowServeControlReadyQueryForBeads and recovers the values it encoded
// as shell-quoted env-var prefix assignments, using shellquote.Split (the
// same package the query was built with) rather than hand-rolled parsing.
func parseControlReadyQuery(workQuery string) (parsedControlReadyQuery, bool) {
	if !strings.HasPrefix(workQuery, controlReadyQueryMarkerPrefix) {
		return parsedControlReadyQuery{}, false
	}
	parsed := parsedControlReadyQuery{
		includeEphemeral: strings.Contains(workQuery, "--include-ephemeral"),
	}
	for _, tok := range shellquote.Split(workQuery) {
		if tok == "sh" {
			break
		}
		switch {
		case strings.HasPrefix(tok, "GC_CONTROL_TARGET="):
			parsed.target = strings.TrimPrefix(tok, "GC_CONTROL_TARGET=")
		case strings.HasPrefix(tok, "GC_CONTROL_SESSION_NAME="):
			parsed.controlSessionName = strings.TrimPrefix(tok, "GC_CONTROL_SESSION_NAME=")
		case strings.HasPrefix(tok, "GC_CONTROL_LEGACY_TARGET="):
			parsed.legacyTarget = strings.TrimPrefix(tok, "GC_CONTROL_LEGACY_TARGET=")
		case strings.HasPrefix(tok, "GC_CONTROL_BARE_TARGET="):
			parsed.bareTarget = strings.TrimPrefix(tok, "GC_CONTROL_BARE_TARGET=")
		}
	}
	return parsed, parsed.target != ""
}

// envListValue looks up key in a KEY=VALUE environment list such as the one
// mergeRuntimeEnv produces, preferring the last match (matching os/exec's own
// last-wins semantics for duplicate keys).
func envListValue(environ []string, key string) string {
	prefix := key + "="
	for i := len(environ) - 1; i >= 0; i-- {
		if v, ok := strings.CutPrefix(environ[i], prefix); ok {
			return v
		}
	}
	return ""
}

// candidateLegacyVariant mirrors the shell loop's per-candidate legacy
// expansion: `case "$id" in *control-dispatcher) legacy="${id%control-dispatcher}workflow-control";; esac`.
// This is a plain suffix rewrite of whatever raw session/alias/id string is
// being checked, distinct from workflowServeLegacyControlRoute (which only
// matches a qualified-name-shaped target).
func candidateLegacyVariant(id string) string {
	const suffix = "control-dispatcher"
	if !strings.HasSuffix(id, suffix) {
		return ""
	}
	return strings.TrimSuffix(id, suffix) + "workflow-control"
}

// controlReadyCandidates returns the deduped, precedence-ordered assignee
// candidates the shell script would have checked: GC_CONTROL_SESSION_NAME,
// GC_SESSION_NAME, GC_ALIAS, GC_CONTROL_TARGET, GC_SESSION_ID, each paired
// with its control-dispatcher -> workflow-control legacy variant.
func controlReadyCandidates(parsed parsedControlReadyQuery, envList []string) []string {
	sources := []string{
		parsed.controlSessionName,
		envListValue(envList, "GC_SESSION_NAME"),
		envListValue(envList, "GC_ALIAS"),
		parsed.target,
		envListValue(envList, "GC_SESSION_ID"),
	}

	seen := make(map[string]struct{}, len(sources)*2)
	var candidates []string
	add := func(id string) {
		id = strings.TrimSpace(id)
		if id == "" {
			return
		}
		if _, ok := seen[id]; ok {
			return
		}
		seen[id] = struct{}{}
		candidates = append(candidates, id)
	}
	for _, id := range sources {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		add(id)
		add(candidateLegacyVariant(id))
	}
	return candidates
}

// controlReadyRoutes returns the routes routed_ready would have checked, in
// order: the target itself, its legacy alias, its bare alias.
func controlReadyRoutes(parsed parsedControlReadyQuery) []string {
	var routes []string
	for _, route := range []string{parsed.target, parsed.legacyTarget, parsed.bareTarget} {
		route = strings.TrimSpace(route)
		if route != "" {
			routes = append(routes, route)
		}
	}
	return routes
}

// controlReadyScopedQueryKey identifies the exact sequence of bounded
// assignee and route queries. A cache entry for another dispatcher, or for a
// changed session identity, is not a valid readiness snapshot.
func controlReadyScopedQueryKey(parsed parsedControlReadyQuery, envList []string) string {
	return strings.Join([]string{
		strings.Join(controlReadyCandidates(parsed, envList), "\x00"),
		strings.Join(controlReadyRoutes(parsed), "\x00"),
		fmt.Sprintf("ephemeral=%t", parsed.includeEphemeral),
		"backend=" + envListValue(envList, "GC_BEADS"),
		"scope=" + envListValue(envList, "GC_STORE_SCOPE"),
		"shim=" + envListValue(envList, citylayout.RealBdEnvVar),
	}, "\x01")
}

// filterReadyByAssignee mirrors `bd ready --assignee=$cand --exclude-type=epic --limit=N`.
// ready is expected to already be in canonical ready order (CachedReady/
// SortBeadsReadyOrder), matching bd's own default (no --sort) ready order.
func filterReadyByAssignee(ready []beads.Bead, assignee string, limit int) []beads.Bead {
	var out []beads.Bead
	for _, b := range ready {
		if b.Assignee != assignee || b.Type == controlReadyExcludeType {
			continue
		}
		out = append(out, b)
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out
}

// filterReadyByRoute mirrors `bd ready --metadata-field $metadataKey=$route --unassigned --exclude-type=epic --exclude-label "hold:mayor" --exclude-label "hold:external" --sort oldest --limit=N`.
// This is a route-scoped, unassigned tier (Tier 3 pool-demand/control-dispatcher
// routing), so held beads must be excluded (ga-5736js): filterReadyByAssignee
// (Tier 1/2, assignee-scoped) stays hold-transparent by design and must not
// gain this filter.
func filterReadyByRoute(ready []beads.Bead, metadataKey, route string) []beads.Bead {
	var matched []beads.Bead
	for _, b := range ready {
		if b.Assignee != "" || b.Type == controlReadyExcludeType {
			continue
		}
		if b.Metadata[metadataKey] != route {
			continue
		}
		held := false
		for _, label := range beadmeta.DispatchHoldLabels {
			if beadLabelsContain(b.Labels, label) {
				held = true
				break
			}
		}
		if held {
			continue
		}
		matched = append(matched, b)
	}
	beads.SortBeads(matched, beads.SortCreatedAsc)
	if len(matched) > workflowServeScanLimit {
		matched = matched[:workflowServeScanLimit]
	}
	return matched
}

// mergeControlReadyGroups flattens the per-candidate/per-route result groups
// in the order they were checked, dropping beads still mid-instantiation and
// deduping by ID on first occurrence -- mirroring the shell script's closing
// `jq -s 'reduce add[] as $item (...)'` filter exactly, including its
// specific quirk: an instantiating-tagged occurrence of an ID is skipped
// WITHOUT being marked seen, so a later non-instantiating occurrence of the
// same ID still gets admitted.
func mergeControlReadyGroups(groups ...[]beads.Bead) []beads.Bead {
	seen := make(map[string]struct{})
	var merged []beads.Bead
	for _, group := range groups {
		for _, b := range group {
			if _, ok := seen[b.ID]; ok {
				continue
			}
			if strings.TrimSpace(b.Metadata[beadmeta.InstantiatingMetadataKey]) != "" {
				continue
			}
			seen[b.ID] = struct{}{}
			merged = append(merged, b)
		}
	}
	return merged
}

// evaluateControlReady answers a control-dispatcher readiness scan against an
// already-fetched ready set (from CachedReady or the single batched
// fallback), applying the exact candidate precedence, legacy/bare route
// aliasing, and instantiating-metadata dedup that
// workflowServeControlReadyQueryForBeads encodes as shell.
func evaluateControlReady(ready []beads.Bead, parsed parsedControlReadyQuery, envList []string) []beads.Bead {
	var groups [][]beads.Bead
	for _, cand := range controlReadyCandidates(parsed, envList) {
		groups = append(groups, filterReadyByAssignee(ready, cand, workflowServeScanLimit))
	}
	for _, route := range controlReadyRoutes(parsed) {
		groups = append(groups, filterReadyByRoute(ready, beadmeta.RunTargetMetadataKey, route))
		groups = append(groups, filterReadyByRoute(ready, beadmeta.RoutedToMetadataKey, route))
	}
	return mergeControlReadyGroups(groups...)
}

func beadsToHookBeads(items []beads.Bead) []hookBead {
	out := make([]hookBead, 0, len(items))
	for _, b := range items {
		out = append(out, hookBead{ID: b.ID, Metadata: hookBeadMetadata(b.Metadata)})
	}
	return out
}

// controlReadyFallbackReady answers the batched ready scan the in-process cache
// could not: dirty, still priming, or a bd compatibility mode that requires
// --include-ephemeral (a tier CachedReady can't serve).
//
// It reads whichever ledger(s) the control dispatcher will actually dispatch
// against, which controlGraphBinding and controlGraphExtraLeg answer between
// them:
//
//   - A CITY scope whose graph class relocated reads the binding INSTEAD of its
//     own store. `bd` in dir speaks to the work store, and the control beads
//     there are the copies the migration retained, which no longer receive the
//     workflow's mutations. Enumerating those would hand the drain loop a queue
//     of ids the dispatch then no-ops on forever.
//   - A RIG scope on that same city reads its own store AND the binding. Its
//     queue is split across both — its own workflows minted control beads
//     locally, and city-scoped molecules minted theirs in the city-keyed binding
//     and routed them here by name — so a scope-only scan answers `[]` for a
//     queue that is not empty.
//
// Every leg fails LOUD, matching `gc ready`: a work query has nowhere to say
// "this answer is short", so a leg that errors must not degrade to a partial
// array that reads as "no work".
func controlReadyFallbackReady(dir, cityPath string, env map[string]string, includeEphemeral bool) ([]beads.Bead, error) {
	if binding, relocated := controlGraphBinding(cityPath, dir); relocated {
		return controlReadyBindingReady(dir, binding, includeEphemeral)
	}
	scoped, err := controlReadyScopeShellReady(dir, env, includeEphemeral)
	if err != nil {
		return nil, err
	}
	binding, federated := controlGraphExtraLeg(cityPath, dir)
	if !federated {
		return scoped, nil
	}
	graphRows, err := controlReadyBindingReady(dir, binding, includeEphemeral)
	if err != nil {
		return nil, err
	}
	return mergeControlReadyLegs(scoped, graphRows), nil
}

// mergeControlReadyLegs unions the legs in order, first leg winning on a
// duplicate id, then restores canonical ready order over the whole set.
//
// The global re-sort is a deliberate divergence from `gc ready`'s federation,
// which preserves per-leg order. evaluateControlReady's inputs are documented as
// canonical (see filterReadyByAssignee), and its assignee tiers cap at
// workflowServeScanLimit by truncating the head of that order — so leaving the
// graph leg's rows appended after the scope leg's would let leg membership, not
// readiness, decide which beads survive the cap.
func mergeControlReadyLegs(legs ...[]beads.Bead) []beads.Bead {
	var merged []beads.Bead
	seen := make(map[string]struct{})
	for _, leg := range legs {
		for _, b := range leg {
			if _, ok := seen[b.ID]; ok {
				continue
			}
			seen[b.ID] = struct{}{}
			merged = append(merged, b)
		}
	}
	beads.SortBeadsReadyOrder(merged)
	return merged
}

// controlReadyScopeShellReady is the scope leg: one batched `bd ready --json`
// call covering the whole active ready set (no --assignee/--metadata-field
// filter), for evaluateControlReady to filter in Go. It runs through the
// current gc executable's `gc bd` front door, and asks a bdshim-fronted
// worker environment for the bounded --summary-json projection.
func controlReadyScopeShellReady(dir string, env map[string]string, includeEphemeral bool) ([]beads.Bead, error) {
	args := []string{"bd", "--readonly", "--sandbox", "ready", "--json", "--exclude-type=" + controlReadyExcludeType, fmt.Sprintf("--limit=%d", controlReadyFallbackLimit)}
	if includeEphemeral {
		args = append(args, "--include-ephemeral")
	}
	runtimeEnv := mergeRuntimeEnv(os.Environ(), env)
	if controlReadyUsesSummary(env) {
		return controlReadyFallbackQuery(append(args, "--summary-json"), dir, runtimeEnv, true, true)
	}
	return controlReadyFallbackQuery(args, dir, runtimeEnv, false, true)
}

// controlReadyBindingReady is the relocated-graph arm of the fallback: the same
// batched ready scan, taken in-process against the binding instead of by
// shelling `bd` in a directory that no longer holds the class.
//
// It reproduces the shell arm's three filters rather than approximating them:
// --include-ephemeral is the TierBoth/TierIssues split BdStore.Ready itself
// applies, --exclude-type is applied in Go because ReadyQuery carries no type
// selector, and the limit is taken after that exclusion so the batched cap means
// the same thing on both arms.
func controlReadyBindingReady(dir string, binding beads.Store, includeEphemeral bool) ([]beads.Bead, error) {
	tier := beads.TierIssues
	if includeEphemeral {
		tier = beads.TierBoth
	}
	ready, err := binding.Ready(beads.ReadyQuery{TierMode: tier})
	if err != nil {
		return nil, fmt.Errorf("control-ready fallback: reading the graph binding for %s: %w", dir, err)
	}
	result := make([]beads.Bead, 0, len(ready))
	for _, bead := range ready {
		if bead.Type == controlReadyExcludeType {
			continue
		}
		result = append(result, bead)
		if len(result) == controlReadyFallbackLimit {
			log.Printf("control-ready fallback: the graph binding for %s returned at least the %d-item limit -- city-wide ready set may be truncated, some candidates/routes could see fewer beads than are actually ready", dir, controlReadyFallbackLimit)
			break
		}
	}
	beads.SortBeadsReadyOrder(result)
	return result, nil
}

// controlReadyScopedSummaryQueue mirrors the legacy shell query's individual
// assignee and route reads for shimmed bd. Each bounded summary is over one
// legacy slice (at most workflowServeScanLimit rows), rather than attempting
// to project a whole city's ready inventory through the fixed summary budget.
// It therefore preserves the summary completeness contract without silently
// dropping a controller's own work when unrelated city work is large.
func controlReadyScopedSummaryQueue(dir string, env map[string]string, parsed parsedControlReadyQuery) ([]hookBead, error) {
	rows, err := controlReadyScopedSummaryReady(dir, env, parsed)
	if err != nil {
		return nil, err
	}
	return beadsToHookBeads(rows), nil
}

// controlReadyScopedSummaryReady runs the scoped assignee and route summaries
// behind controlReadyScopedSummaryQueue and returns their merged rows.
func controlReadyScopedSummaryReady(dir string, env map[string]string, parsed parsedControlReadyQuery) ([]beads.Bead, error) {
	runtimeEnv := mergeRuntimeEnv(os.Environ(), env)
	includeEphemeral := parsed.includeEphemeral
	var groups [][]beads.Bead

	query := func(args []string) error {
		result, err := controlReadyFallbackQuery(append(args, "--summary-json"), dir, runtimeEnv, true, false)
		if err != nil {
			return err
		}
		groups = append(groups, result)
		return nil
	}
	for _, candidate := range controlReadyCandidates(parsed, runtimeEnv) {
		args := []string{"bd", "--readonly", "--sandbox", "ready"}
		if includeEphemeral {
			args = append(args, "--include-ephemeral")
		}
		args = append(args, "--assignee="+candidate, "--exclude-type="+controlReadyExcludeType, "--json", fmt.Sprintf("--limit=%d", workflowServeScanLimit))
		if err := query(args); err != nil {
			return nil, fmt.Errorf("control-ready assignee summary %q: %w", candidate, err)
		}
	}
	for _, route := range controlReadyRoutes(parsed) {
		for _, metadataKey := range []string{beadmeta.RunTargetMetadataKey, beadmeta.RoutedToMetadataKey} {
			args := []string{"bd", "--readonly", "--sandbox", "ready"}
			if includeEphemeral {
				args = append(args, "--include-ephemeral")
			}
			args = append(args,
				"--metadata-field", metadataKey+"="+route,
				"--unassigned",
				"--exclude-type="+controlReadyExcludeType,
			)
			for _, label := range beadmeta.DispatchHoldLabels {
				args = append(args, "--exclude-label", label)
			}
			args = append(args, "--json", "--sort", "oldest", fmt.Sprintf("--limit=%d", workflowServeScanLimit))
			if err := query(args); err != nil {
				return nil, fmt.Errorf("control-ready route summary %s=%q: %w", metadataKey, route, err)
			}
		}
	}
	return mergeControlReadyGroups(groups...), nil
}

// controlReadyIncompleteSummaryFallback answers a control-ready scan whose
// whole-set summary could not prove completeness. On a large rig the single
// unscoped ready summary overflows the bounded projection, and refusing it
// left the control dispatcher blind to its own work for as long as unrelated
// ready work stayed above the cap (gcw-f84jn). The scoped assignee and route
// summaries each cover one small slice, so they stay inside the bound; the
// relocated graph leg is merged exactly as controlReadyFallbackReady merges
// it, and the result goes through the same evaluateControlReady filter.
// handled is false when err is not a summary-integrity failure, or the
// environment does not use the summary contract.
func controlReadyIncompleteSummaryFallback(cause error, dir, cityPath string, env map[string]string, parsed parsedControlReadyQuery) (rows []beads.Bead, handled bool, err error) {
	var integrity *controlReadySummaryIntegrityError
	if !errors.As(cause, &integrity) || !controlReadyUsesSummary(env) {
		return nil, false, nil
	}
	rows, err = controlReadyScopedSummaryReady(dir, env, parsed)
	if err != nil {
		return nil, true, fmt.Errorf("%w; scoped fallback: %w", cause, err)
	}
	if binding, federated := controlGraphExtraLeg(cityPath, dir); federated {
		graphRows, err := controlReadyBindingReady(dir, binding, parsed.includeEphemeral)
		if err != nil {
			return nil, true, fmt.Errorf("%w; scoped fallback graph leg: %w", cause, err)
		}
		rows = mergeControlReadyLegs(rows, graphRows)
	}
	return rows, true, nil
}

// cachedControlReadyIncompleteSummaryFallback runs
// controlReadyIncompleteSummaryFallback for a cache entry whose whole-set
// summary prime failed integrity, reusing the entry's last fallback outcome
// for controlReadyFallbackTTL of that round's duration. The failed whole-set
// prime itself stays in its controlReadyCacheFailureBackoff, and a re-prime
// that fails again carries the memo forward, so a large rig pays one round of
// scoped summaries per fallback TTL rather than a subprocess fan-out on every
// drain-loop scan (gcw-dsi74, gcw-qg49y).
func cachedControlReadyIncompleteSummaryFallback(entry *controlReadyCacheEntry, dir, cityPath string, env map[string]string, parsed parsedControlReadyQuery) (rows []beads.Bead, handled bool, err error) {
	now := controlReadyNow()
	controlReadyCacheRegistry.mu.Lock()
	if !entry.fallbackAt.IsZero() && now.Sub(entry.fallbackAt) < controlReadyFallbackTTL(entry.fallbackDur) {
		rows, err = entry.fallbackRows, entry.fallbackErr
		controlReadyCacheRegistry.mu.Unlock()
		return rows, true, err
	}
	first := entry.fallbackAt.IsZero()
	controlReadyCacheRegistry.mu.Unlock()

	rows, handled, err = controlReadyIncompleteSummaryFallback(entry.err, dir, cityPath, env, parsed)
	if handled && first {
		log.Printf("control-ready: %v for %s; falling back to scoped assignee/route summaries", entry.err, dir)
	}
	if !handled {
		return nil, false, nil
	}
	end := controlReadyNow()
	controlReadyCacheRegistry.mu.Lock()
	entry.fallbackRows, entry.fallbackErr, entry.fallbackAt, entry.fallbackDur = rows, err, end, end.Sub(now)
	controlReadyCacheRegistry.mu.Unlock()
	return rows, true, err
}

var controlReadyExecutable = os.Executable

var controlReadyCommandRunner = runWorkQueryCommandWithEnv

func controlReadyExecutablePath() (string, error) {
	exe, err := controlReadyExecutable()
	if err != nil {
		return "", fmt.Errorf("resolve current gc executable: %w", err)
	}
	if strings.TrimSpace(exe) == "" {
		return "", fmt.Errorf("resolve current gc executable: empty path")
	}
	exe = filepath.Clean(exe)
	if !filepath.IsAbs(exe) {
		exe, err = filepath.Abs(exe)
		if err != nil {
			return "", fmt.Errorf("resolve absolute current gc executable: %w", err)
		}
	}
	return exe, nil
}

func controlReadyFallbackQuery(args []string, dir string, runtimeEnv []string, requireSummary, sortReady bool) ([]beads.Bead, error) {
	exe, err := controlReadyExecutablePath()
	if err != nil {
		return nil, err
	}
	display := shellquote.Join(append([]string{exe}, args...))
	output, err := controlReadyCommandRunner(exe, args, display, dir, runtimeEnv)
	if err != nil {
		return nil, err
	}
	trimmed := strings.TrimSpace(output)
	var result []beads.Bead
	if requireSummary {
		result, err = decodeControlReadySummary([]byte(trimmed))
	} else {
		if !workQueryHasReadyWork(trimmed) {
			return nil, nil
		}
		err = json.Unmarshal([]byte(trimmed), &result)
	}
	if err != nil {
		return nil, fmt.Errorf("control-ready fallback: decode bd ready output: %w", err)
	}
	if len(result) == controlReadyFallbackLimit {
		log.Printf("control-ready fallback: bd ready for %s returned exactly the %d-item limit -- city-wide ready set may be truncated, some candidates/routes could see fewer beads than are actually ready", dir, controlReadyFallbackLimit)
	}
	if sortReady {
		beads.SortBeadsReadyOrder(result)
	}
	return result, nil
}

// controlReadyShimmed reports whether the supplied worker environment routes
// bd through bdshim. Other control-plane paths use this to decide whether the
// shim-private unbounded flag is understood; control-ready discovery itself
// deliberately uses the bounded summary contract instead.
func controlReadyShimmed(env map[string]string) bool {
	runtimeEnv := mergeRuntimeEnv(os.Environ(), env)
	realBd := strings.TrimSpace(envListValue(runtimeEnv, citylayout.RealBdEnvVar))
	if realBd == "" {
		return false
	}
	realInfo, err := os.Stat(realBd)
	if err != nil || realInfo.IsDir() {
		return false
	}

	// Positively identify the expected city shim binary from GC_BIN if present.
	var expectedShimInfo os.FileInfo
	gcBin := strings.TrimSpace(envListValue(runtimeEnv, "GC_BIN"))
	shimDir := filepath.Dir(gcBin)
	if filepath.IsAbs(gcBin) && filepath.Base(shimDir) == "shimbin" {
		shimCandidate := filepath.Join(shimDir, "bd")
		if info, err := os.Stat(shimCandidate); err == nil && !info.IsDir() && info.Mode().Perm()&0o111 != 0 {
			expectedShimInfo = info
		}
	}

	pathValue := envListValue(runtimeEnv, "PATH")
	for _, dir := range filepath.SplitList(pathValue) {
		if dir == "" {
			// Match os/exec's Unix PATH semantics: an empty component means
			// the current directory, so "bd" resolves as "./bd" here.
			dir = "."
		}
		candidate := filepath.Join(dir, "bd")
		candidateInfo, err := os.Stat(candidate)
		if err != nil || candidateInfo.IsDir() || candidateInfo.Mode().Perm()&0o111 == 0 {
			continue
		}
		// Candidate must not be the raw bd binary pointed to by GC_BD_REAL.
		if os.SameFile(candidateInfo, realInfo) {
			return false
		}
		// Positively select the shim: candidate must match the expected shim
		// from GC_BIN or route to bdshim via symlink. A distinct raw bd on PATH
		// must not be treated as a shim.
		if expectedShimInfo != nil && os.SameFile(candidateInfo, expectedShimInfo) {
			return true
		}
		if shimRoutesToThinClient(candidate) {
			return true
		}
		return false
	}
	return false
}

// controlReadyUsesSummary reports whether the worker environment is fronted
// by bdshim. The compact flag is shim-provided and must not be sent to a raw
// bd binary, which preserves the configured bd_shim=off behavior.
func controlReadyUsesSummary(env map[string]string) bool {
	runtimeEnv := mergeRuntimeEnv(os.Environ(), env)
	if backend := strings.TrimSpace(envListValue(runtimeEnv, "GC_BEADS")); backend != "" && backend != "bd" {
		return false
	}
	return strings.TrimSpace(envListValue(runtimeEnv, citylayout.RealBdEnvVar)) != ""
}

// controlReadySummaryIntegrityError reports a summary that cannot prove it
// contains the complete ready set. Scheduling from such a projection would
// turn omitted rows into false absence.
type controlReadySummaryIntegrityError struct {
	Total   int
	Rows    int
	Omitted int
}

func (e *controlReadySummaryIntegrityError) Error() string {
	return fmt.Sprintf("incomplete control-ready summary: total=%d rows=%d omitted=%d", e.Total, e.Rows, e.Omitted)
}

// decodeControlReadySummary requires a complete bounded discovery projection
// and returns the scheduling fields used by the control-ready evaluator.
func decodeControlReadySummary(data []byte) ([]beads.Bead, error) {
	var envelope struct {
		bddispatch.BeadSummaryEnvelope
		Total   *int `json:"total"`
		Omitted *int `json:"omitted"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil {
		return nil, err
	}
	if envelope.SchemaVersion != "1" || envelope.Kind != bddispatch.BeadSummaryKind || envelope.Verb != "ready" {
		return nil, fmt.Errorf("unrecognized summary envelope (schema_version=%q kind=%q verb=%q)", envelope.SchemaVersion, envelope.Kind, envelope.Verb)
	}
	if envelope.Total == nil || envelope.Omitted == nil || envelope.Beads == nil {
		return nil, fmt.Errorf("incomplete control-ready summary: total, omitted and beads are required")
	}
	if *envelope.Omitted != 0 || *envelope.Total != len(envelope.Beads)+*envelope.Omitted {
		return nil, &controlReadySummaryIntegrityError{Total: *envelope.Total, Rows: len(envelope.Beads), Omitted: *envelope.Omitted}
	}
	result := make([]beads.Bead, 0, len(envelope.Beads))
	for _, summary := range envelope.Beads {
		for _, omitted := range summary.FieldsOmitted {
			if omitted == "routing_metadata."+beadmeta.InstantiatingMetadataKey {
				return nil, fmt.Errorf("incomplete control-ready summary: required %s omitted for %q", beadmeta.InstantiatingMetadataKey, summary.ID)
			}
		}
		result = append(result, beads.Bead{
			ID:        summary.ID,
			Title:     summary.Title,
			Status:    summary.Status,
			Type:      summary.Type,
			Priority:  summary.Priority,
			CreatedAt: summary.CreatedAt,
			Assignee:  summary.Assignee,
			ParentID:  summary.Parent,
			Labels:    append([]string(nil), summary.Labels...),
			Metadata:  beads.StringMap(summary.RoutingMetadata),
		})
	}
	return result, nil
}

var controlReadyCacheRegistry = struct {
	mu    sync.Mutex
	byDir map[string]*controlReadyCacheEntry
}{byDir: make(map[string]*controlReadyCacheEntry)}

// controlReadyCacheEntry holds a primed snapshot per leg for one scope dir.
// Its backing stores are closed when controlReadyCachesFor returns, once every
// leg has primed, so an entry is a set of CLOSED-backing snapshots: it
// may only be read through CachingStore.CachedReady, which answers entirely from
// the in-memory snapshot. Any read that would need to touch the backing must
// decline to controlReadyFallbackReady instead of consulting a closed handle.
//
// A bdshim-fronted worker environment primes ready (the bounded summary
// projection) instead of caches; err and retryAfter hold a failed summary
// prime so the readiness tick backs off instead of re-running it every tick.
type controlReadyCacheEntry struct {
	caches           []*beads.CachingStore
	ready            []beads.Bead
	queryKey         string
	primedAt         time.Time
	retryAfter       time.Time
	err              error
	includeEphemeral bool
	// fallbackRows/fallbackErr/fallbackAt memoize the scoped fallback taken
	// when the whole-set summary prime failed integrity (gcw-f84jn);
	// fallbackDur is how long that round took and sets its reuse TTL.
	fallbackRows []beads.Bead
	fallbackErr  error
	fallbackAt   time.Time
	fallbackDur  time.Duration
}

var controlReadyNow = time.Now

// controlReadyCachesFor returns a short-lived, best-effort in-process ready
// snapshot per leg for dir, reusing a set primed within controlReadyCacheTTL
// instead of re-priming on every drain-loop tick. Returns nil whenever the
// caches cannot be built or trusted; callers must treat nil as "fall back to a
// live bd query", not as an error -- an unopenable store here is possible in
// scopes this readiness scan does not normally run against (e.g. test fixtures
// with no rig configured) and the sibling control-bead-processing path
// (runControlDispatcherInStore) would already be failing loudly if it were a
// real production gap.
//
// The snapshots are taken over the SAME ledgers
// runControlDispatcherWithStoreAndConfig dispatches against —
// controlReadyCacheSources applies the routing rule controlBeadLedger resolves
// against — so the queue and the mutation are the same set. They must not
// diverge. A queue drawn from the work store while the dispatch closes the
// binding's copy re-offers the same id every tick -- ProcessControl no-ops on
// the already-closed copy, and drainWorkflowServeWork counts a no-op as progress
// -- so the drain loop never returns; and the beads the dispatch CREATES (fanout
// fragments, retry attempts, drain units) would land in a ledger the scan never
// reads, stalling the workflow at its first hop.
//
// A leg that fails to prime discards the whole set. A partial set would be a
// short queue that reads as a complete one, which is the same silent-underread
// shape as scanning the wrong ledger entirely.
//
// Known limitation (low-impact, not fixed here): concurrent callers racing a
// stale/missing entry for the same dir each independently open+prime their
// own stores rather than coalescing behind one in-flight prime -- last writer
// into controlReadyCacheRegistry wins. Same class of gap already accepted
// for CachingStore.List/Ready cache-miss reads; worth revisiting with a
// singleflight if overlapping invocations against the same city/dir become
// common (e.g. a restart handoff window), but the control-dispatcher serve
// loop's typical call pattern is sequential-per-tick per dir. Note the entries
// are keyed by scope dir, so on a split city every rig dispatcher primes the
// shared city binding independently (ga-n6gnr). That race stays safe under the
// per-prime close below: each caller closes only the scoped backing IT opened,
// and the registry loser's CachingStores are pure in-memory snapshots
// (CachedReady never touches a backing), so an overwritten entry is never a
// use-after-close.
func controlReadyCachesFor(dir, cityPath string, cfg *config.City) []*beads.CachingStore {
	entry := controlReadyCacheFor(dir, cityPath, cfg, nil, false)
	if entry == nil {
		return nil
	}
	return entry.caches
}

// controlReadyCacheFor is controlReadyCachesFor keyed additionally by the
// worker environment, the ephemeral tier, and the parsed query. A
// bdshim-fronted environment (controlReadyUsesSummary) primes the entry with
// the same ONE unscoped ready call controlReadyFallbackReady issues, through
// the bounded --summary-json contract, filtered in Go by evaluateControlReady
// (gcw-dsi74: a per-candidate/per-route fan-out re-introduced the exact
// N-subprocess-per-tick cost ga-ak6rt1 removed). A failed summary prime is
// cached with a retry-after so a malformed or unavailable shim does not turn
// the readiness tick into a subprocess storm; the failure stays loud to the
// caller throughout the pause.
func controlReadyCacheFor(dir, cityPath string, cfg *config.City, env map[string]string, includeEphemeral bool, parsed ...parsedControlReadyQuery) *controlReadyCacheEntry {
	now := controlReadyNow()
	queryKey := ""
	if len(parsed) > 0 {
		queryKey = controlReadyScopedQueryKey(parsed[0], mergeRuntimeEnv(os.Environ(), env))
	}
	controlReadyCacheRegistry.mu.Lock()
	entry, ok := controlReadyCacheRegistry.byDir[dir]
	fresh := ok && entry.includeEphemeral == includeEphemeral && entry.queryKey == queryKey && now.Sub(entry.primedAt) < controlReadyCacheTTL
	backingOff := ok && entry.includeEphemeral == includeEphemeral && entry.queryKey == queryKey && entry.err != nil && now.Before(entry.retryAfter)
	controlReadyCacheRegistry.mu.Unlock()
	if fresh || backingOff {
		return entry
	}

	if controlReadyUsesSummary(env) {
		ready, err := controlReadyFallbackReady(dir, cityPath, env, includeEphemeral)
		prev := entry
		entry = &controlReadyCacheEntry{ready: ready, queryKey: queryKey, primedAt: controlReadyNow(), err: err, includeEphemeral: includeEphemeral}
		if entry.err != nil {
			entry.retryAfter = now.Add(controlReadyCacheFailureBackoff)
			log.Printf("control-ready cache: bounded summary prime failed for %s: %v (retry after %s)", dir, entry.err, entry.retryAfter.Format(time.RFC3339))
		}
		controlReadyCacheRegistry.mu.Lock()
		if entry.err != nil && ok && prev.includeEphemeral == includeEphemeral && prev.queryKey == queryKey {
			// Still failing: keep the scoped fallback memo so its own TTL, not
			// the whole-set retry cadence, decides when the next round runs.
			entry.fallbackRows, entry.fallbackErr, entry.fallbackAt, entry.fallbackDur = prev.fallbackRows, prev.fallbackErr, prev.fallbackAt, prev.fallbackDur
		}
		controlReadyCacheRegistry.byDir[dir] = entry
		controlReadyCacheRegistry.mu.Unlock()
		return entry
	}

	caches := primeControlReadyCaches(dir, cityPath, cfg)
	if caches == nil {
		return nil
	}
	controlReadyCacheRegistry.mu.Lock()
	entry = &controlReadyCacheEntry{caches: caches, queryKey: queryKey, primedAt: now, includeEphemeral: includeEphemeral}
	controlReadyCacheRegistry.byDir[dir] = entry
	controlReadyCacheRegistry.mu.Unlock()
	return entry
}

// primeControlReadyCaches opens and primes one CachingStore snapshot per leg
// (see controlReadyCacheSources), closing the scoped backing it opened once
// every leg is in memory. It returns nil when any leg cannot be opened or
// primed.
func primeControlReadyCaches(dir, cityPath string, cfg *config.City) []*beads.CachingStore {
	sources, owned, err := controlReadyCacheSourcesFn(dir, cityPath, cfg)
	if err != nil {
		return nil
	}
	// Release the scoped backing handles the instant the snapshot is fully in
	// memory. NewCachingStore is passive here (nil onChange, StartReconciler is
	// never called) and CachedReady serves entirely from the primed in-memory
	// snapshot without touching the backing, so the backing only has to be open
	// for the PrimeActive scan itself. Closing it per prime keeps each prime's
	// read mark on the shared graph binding's WAL short-lived instead of held by
	// a leaked handle until *sql.DB GC-finalize -- the leak that let the WAL grow
	// unbounded because a passive checkpoint can never truncate past a live read
	// mark. This defer fires on BOTH the prime-failure early return and the
	// success path, so a prime that fails partway still closes whatever it
	// opened. Only owned legs are closed; the graph binding is process-shared
	// (see controlReadyCacheSources).
	defer func() {
		for _, s := range owned {
			if err := closeBeadStoreHandle(s); err != nil {
				log.Printf("control-ready cache: closing primed scope store for %s: %v", dir, err)
			}
		}
	}()
	caches := make([]*beads.CachingStore, 0, len(sources))
	for _, source := range sources {
		// Snapshot the engine, not the one-shot emitter around a binding leg:
		// this cache never writes, so the emitter adds nothing, and between the
		// cache and the engine it would hide the engine's ready projection.
		cs := beads.NewCachingStore(bindingEngine(source), nil)
		if err := cs.PrimeActive(); err != nil {
			log.Printf("control-ready cache: pre-prime failed for %s: %v (falling back to a live bd query)", dir, err)
			return nil
		}
		caches = append(caches, cs)
	}
	return caches
}

// controlReadyCacheSources returns the ordered ledgers to snapshot, applying the
// same routing rule as controlReadyFallbackReady so the cached answer and the
// fallback answer cannot disagree about which stores hold this scope's queue.
//
// owned is the subset of sources this call constructed and is therefore
// responsible for closing once the snapshot has been primed into memory. It is
// deliberately NOT every source. The graph-class binding legs
// (controlGraphBinding / controlGraphExtraLeg) resolve through the per-city
// cliStorageRoutes memo, so the binding store is process-shared and closed only
// by closeCLIStorageRoutes at process exit; CloseStore is a one-way latch, so
// closing a binding leg here would poison every later graph-class read and write
// in the process with ErrStoreClosed. Only the scoped leg
// (openControlStoreAtForCity) is freshly constructed per call, so only it is
// returned as owned.
//
// An error return must not hand back opened stores. controlReadyCachesFor
// registers its closing defer only after this call succeeds, so a store opened
// before an error return would have nothing to close it. Today the sole error
// return IS the failed open, so nothing is open on that path; an arm added
// later that can fail after a successful open must close what it opened before
// returning. The same obligation binds any controlReadyCacheSourcesFn seam.
func controlReadyCacheSources(dir, cityPath string, cfg *config.City) (sources, owned []beads.Store, err error) {
	// A relocated CITY scope does not open its scope store at all — that would
	// be a bd process this scan never reads. The binding is process-shared, so
	// nothing here owns it.
	if binding, relocated := controlGraphBinding(cityPath, dir); relocated {
		return []beads.Store{binding}, nil, nil
	}
	scoped, err := openControlStoreAtForCity(dir, cityPath, cfg)
	if err != nil {
		return nil, nil, err
	}
	if binding, federated := controlGraphExtraLeg(cityPath, dir); federated {
		return []beads.Store{scoped, binding}, []beads.Store{scoped}, nil
	}
	return []beads.Store{scoped}, []beads.Store{scoped}, nil
}

// controlReadyCacheSourcesFn is the test seam for controlReadyCacheSources,
// following the package's own var-seam idiom (cf. controlDispatcherServe,
// dispatch_runtime.go). Production always uses controlReadyCacheSources.
var controlReadyCacheSourcesFn = controlReadyCacheSources

// cachedControlReadyUnion merges the cached ready sets, requiring EVERY leg to
// answer from cache. A leg that is dirty or still priming sends the whole scan
// to the fallback rather than to a short answer assembled from the legs that
// happened to be warm.
func cachedControlReadyUnion(caches []*beads.CachingStore) ([]beads.Bead, bool) {
	if len(caches) == 0 {
		return nil, false
	}
	if len(caches) == 1 {
		return caches[0].CachedReady()
	}
	legs := make([][]beads.Bead, 0, len(caches))
	for _, cache := range caches {
		ready, ok := cache.CachedReady()
		if !ok {
			return nil, false
		}
		legs = append(legs, ready)
	}
	return mergeControlReadyLegs(legs...), true
}

// tryControlReadyFromCacheOrFallback answers a control-dispatcher readiness
// scan in-process instead of running workflowServeControlReadyQueryForBeads's
// shell script. handled reports whether workQuery was even recognized as a
// control-ready query; when handled is false the caller must run workQuery
// as a shell command exactly as before. This changes the DATA SOURCE for
// control-dispatcher readiness, not the decision logic (ga-ak6rt1): candidate
// precedence, legacy/bare route aliasing, and the instantiating-metadata
// dedup filter are reproduced exactly by evaluateControlReady.
func tryControlReadyFromCacheOrFallback(workQuery, dir string, env map[string]string) (queue []hookBead, handled bool, err error) {
	parsed, ok := parseControlReadyQuery(workQuery)
	if !ok {
		return nil, false, nil
	}

	cityPath := cityForStoreDir(dir)
	cfg, _ := loadCityConfig(cityPath, io.Discard)
	envList := mergeRuntimeEnv(os.Environ(), env)

	if !parsed.includeEphemeral || controlReadyUsesSummary(env) {
		if entry := controlReadyCacheFor(dir, cityPath, cfg, env, parsed.includeEphemeral, parsed); entry != nil {
			if entry.err != nil {
				if rows, ok, err := cachedControlReadyIncompleteSummaryFallback(entry, dir, cityPath, env, parsed); ok {
					if err != nil {
						return nil, true, err
					}
					return beadsToHookBeads(evaluateControlReady(rows, parsed, envList)), true, nil
				}
				return nil, true, entry.err
			}
			if entry.caches == nil {
				return beadsToHookBeads(evaluateControlReady(entry.ready, parsed, envList)), true, nil
			}
			if ready, ok := cachedControlReadyUnion(entry.caches); ok {
				return beadsToHookBeads(evaluateControlReady(ready, parsed, envList)), true, nil
			}
		}
	}

	ready, err := controlReadyFallbackReady(dir, cityPath, env, parsed.includeEphemeral)
	if err != nil {
		if rows, ok, fallbackErr := controlReadyIncompleteSummaryFallback(err, dir, cityPath, env, parsed); ok {
			if fallbackErr != nil {
				return nil, true, fallbackErr
			}
			return beadsToHookBeads(evaluateControlReady(rows, parsed, envList)), true, nil
		}
		return nil, true, err
	}
	return beadsToHookBeads(evaluateControlReady(ready, parsed, envList)), true, nil
}
