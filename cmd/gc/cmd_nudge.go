package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/extmsg"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/mail"
	"github.com/gastownhall/gascity/internal/nudgepoller"
	"github.com/gastownhall/gascity/internal/nudgequeue"
	"github.com/gastownhall/gascity/internal/pidutil"
	"github.com/gastownhall/gascity/internal/reconcilekey"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/runtime/tmux"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/telemetry"
	"github.com/gastownhall/gascity/internal/worker"
	"github.com/spf13/cobra"
)

const (
	defaultQueuedNudgeTTL           = 24 * time.Hour
	defaultQueuedNudgeClaimTTL      = 2 * time.Minute
	defaultQueuedNudgeRetryDelay    = 15 * time.Second
	defaultQueuedNudgeMaxAttempts   = 5
	defaultQueuedNudgeDeadRetention = 1 * time.Hour

	// nudgeEnqueueMaintenanceBudget bounds the wall-clock time the foreground
	// `gc sling --nudge` enqueue path spends on best-effort nudge-queue
	// maintenance (expiring/pruning stale entries) while holding the
	// withNudgeQueueState flock. Without this, maintenance does O(backlog)
	// serial store writes with no cap, so a large backlog turns a sub-second
	// foreground call into a multi-minute hang. Items skipped once the budget
	// is exceeded are left untouched for the next enqueue, the per-session
	// poller, or the doctor reaper to handle — never dropped.
	nudgeEnqueueMaintenanceBudget = 2 * time.Second

	defaultNudgePollInterval   = 2 * time.Second
	defaultNudgePollQuiescence = 3 * time.Second
	// A controller wake can legitimately take a couple of minutes when the
	// session has to rematerialize a worktree and complete startup dialog.
	defaultNudgePollStartGrace = 5 * time.Minute

	// nudgePollIdleObserveEvery bounds how many consecutive ticks the poll
	// loop may skip nudgeObserveTarget while nothing is queued for this
	// target before forcing a fresh observation anyway. obs.Running is the
	// only signal that detects a dead session, so this cadence must stay
	// small enough that an idle target still exits promptly once its session
	// ends -- it only needs to be large enough to matter, not to eliminate
	// every idle-tick observation (ga-8x82f4).
	nudgePollIdleObserveEvery = 5

	// defaultNudgePollMemLimitMB is the soft Go runtime memory limit
	// (debug.SetMemoryLimit) installed for the long-lived `gc nudge poll`
	// sidecar. The sidecar re-parses the whole-file city bead store on every
	// poll tick that sees a changed file; on the file backend a large city
	// beads.json (100MB+) inflates the heap to several hundred MB per parse and
	// the runtime otherwise retains ~2x that as unreturned arena, so RSS
	// plateaus near 1GB per sidecar (gc-3ftcq). With one sidecar per active
	// session, the fleet-wide total dominates host memory and drives OOM. The
	// limit caps that arena growth without starving a single live parse (~400MB
	// for a 124MB file). Override with nudgePollMemLimitEnv; an operator-set
	// GOMEMLIMIT takes precedence and disables this default.
	defaultNudgePollMemLimitMB = 512

	// nudgePollMemLimitEnv overrides defaultNudgePollMemLimitMB (in MiB). Set to
	// "0" to disable the soft limit entirely.
	nudgePollMemLimitEnv = "GC_NUDGE_POLL_MEMLIMIT_MB"

	// nudgePollPprofAddrEnv sets the listen address for the sidecar's pprof
	// endpoint. The endpoint only starts when GC_PPROF=1 (see api.StartPprof);
	// pass a distinct address per sidecar to avoid colliding with the
	// supervisor's pprof server on the default 127.0.0.1:6060.
	nudgePollPprofAddrEnv = "GC_NUDGE_POLL_PPROF_ADDR"

	// nudgePollFreeOSInterval throttles debug.FreeOSMemory in the poll loop so
	// transient per-parse garbage is returned to the OS without paying a full
	// GC + scavenge on every short idle tick.
	nudgePollFreeOSInterval = 30 * time.Second
)

var errNudgeSessionFenceMismatch = errors.New("queued nudge session fence mismatch")

// nudgeManualDropCause is the failure cause `gc nudge drop` passes to
// recordQueuedNudgeFailureWithStore. Its Is method reports equivalence to
// errNudgeSessionFenceMismatch so failedQueuedNudge's unconditional
// dead-letter branch fires on the first call, the same branch drain already
// relies on for a nudge whose target fence no longer matches, rather than
// the attempts/TTL branch, which would just requeue a fresh item instead of
// dropping it.
type nudgeManualDropCause struct{}

func (nudgeManualDropCause) Error() string { return "dropped by operator (gc nudge drop)" }

func (nudgeManualDropCause) Is(target error) bool { return target == errNudgeSessionFenceMismatch }

var errNudgeManualDrop error = nudgeManualDropCause{}

var (
	// Test seams for cmd_nudge_test.go. Tests that replace these package
	// variables must stay serial; do not use t.Parallel in those tests.
	nudgeCityUsesManagedReconciler           = cityUsesManagedReconciler
	nudgePokeController                      = enqueueController
	nudgeObserveTarget                       = workerObserveNudgeTarget
	nudgePollDeliverQueued                   = tryDeliverQueuedNudgesByPoller
	nudgeWarningWriter             io.Writer = os.Stderr
	// nudgeTargetAtInteractivePrompt reports whether the target's live pane is
	// showing an interactive selection/confirmation prompt that a stray Enter
	// would answer (e.g. an AskUserQuestion option menu). A wait-idle nudge to
	// such a session must be queued as a deferred reminder rather than injected
	// as keystrokes, or its Enter submits the focused option — answers the
	// operator never chose. Fails open (false) on any peek error so delivery
	// falls back to existing behavior. Overridable in tests. (gascity#2892)
	nudgeTargetAtInteractivePrompt = workerNudgeTargetAtInteractivePrompt
)

// nudgeInteractivePromptPeekLines bounds the pane capture used to detect an
// open interactive prompt; a selection menu and its confirm footer fit well
// within this window.
const nudgeInteractivePromptPeekLines = 40

type nudgeDeliveryMode string

const (
	nudgeDeliveryImmediate nudgeDeliveryMode = "immediate"
	nudgeDeliveryWaitIdle  nudgeDeliveryMode = "wait-idle"
	nudgeDeliveryQueue     nudgeDeliveryMode = "queue"
)

type queuedNudge = nudgequeue.Item

type nudgeQueueState = nudgequeue.State

type nudgeTarget struct {
	cityPath          string
	cityName          string
	cfg               *config.City
	alias             string
	aliasHistory      []string
	identity          string
	transport         string
	agent             config.Agent
	resolved          *config.ResolvedProvider
	sessionID         string
	continuationEpoch string
	sessionName       string
}

type nudgeStatusJSON struct {
	SchemaVersion string            `json:"schema_version"`
	Command       string            `json:"command"`
	CityPath      string            `json:"city_path"`
	Agent         string            `json:"agent"`
	Session       string            `json:"session"`
	SessionID     string            `json:"session_id,omitempty"`
	Counts        nudgeStatusCounts `json:"counts"`
	Pending       []queuedNudge     `json:"pending"`
	InFlight      []queuedNudge     `json:"in_flight"`
	Dead          []queuedNudge     `json:"dead"`

	// DispatchSkips is the dispatch tick's running, city-wide (not
	// agent-scoped) count of silent skips by reason, since the queue state
	// file was created. Omitted when empty (legacy-mode cities never
	// populate it; a fresh queue has no ticks recorded yet).
	DispatchSkips map[string]int64 `json:"dispatch_skips,omitempty"`
}

type nudgeStatusCounts struct {
	Pending  int `json:"pending"`
	InFlight int `json:"in_flight"`
	Dead     int `json:"dead"`
}

func (t nudgeTarget) agentKey() string {
	if t.alias != "" {
		return t.alias
	}
	if t.sessionID != "" {
		return t.sessionID
	}
	if qn := t.agent.QualifiedName(); qn != "" {
		return qn
	}
	if t.identity != "" {
		return t.identity
	}
	return t.sessionName
}

func (t nudgeTarget) pollerKey() string {
	// Queue items keep alias-oriented Agent values for matching/readability;
	// poller ownership prefers the concrete session ID so live generations that
	// share one runtime session name do not reuse each other's sidecar. Keep
	// the concrete-ID precedence aligned with session.PollerKeyFromBead for
	// session-bead-derived poller launches.
	if t.sessionID != "" {
		return t.sessionID
	}
	return t.agentKey()
}

func (t nudgeTarget) queueKeys() []string {
	var keys []string
	seen := map[string]bool{}
	for _, key := range []string{t.alias, t.sessionID, t.agent.QualifiedName(), t.identity, t.sessionName} {
		key = strings.TrimSpace(key)
		if key == "" || seen[key] {
			continue
		}
		seen[key] = true
		keys = append(keys, key)
	}
	for _, key := range t.aliasHistory {
		key = strings.TrimSpace(key)
		if key == "" || seen[key] {
			continue
		}
		seen[key] = true
		keys = append(keys, key)
	}
	return keys
}

func (t nudgeTarget) matchesQueueAgent(agent string) bool {
	agent = strings.TrimSpace(agent)
	if agent == "" {
		return false
	}
	for _, key := range t.queueKeys() {
		if key == agent {
			return true
		}
	}
	return false
}

func (t nudgeTarget) sessionTransport() string {
	if t.transport != "" {
		return t.transport
	}
	return t.agent.Session
}

func (t nudgeTarget) providerName() string {
	if t.resolved != nil && strings.TrimSpace(t.resolved.Name) != "" {
		return strings.TrimSpace(t.resolved.Name)
	}
	if strings.TrimSpace(t.agent.Provider) != "" {
		return strings.TrimSpace(t.agent.Provider)
	}
	if t.cfg != nil {
		return strings.TrimSpace(t.cfg.Workspace.Provider)
	}
	return ""
}

type queuedNudgeOptions struct {
	ID                string
	SessionID         string
	ContinuationEpoch string
	Reference         *nudgeReference
}

func newNudgeCmd(stdout, stderr io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "nudge",
		Short: "Inspect and deliver deferred nudges",
		Long: `Inspect and deliver deferred nudges.

Deferred nudges are reminders that were queued because the target agent
was asleep or was not at a safe interactive boundary yet.`,
	}
	cmd.AddCommand(
		newNudgeStatusCmd(stdout, stderr),
		newNudgeDrainCmd(stdout, stderr),
		newNudgePollCmd(stdout, stderr),
		newNudgeDropCmd(stdout, stderr),
	)
	return cmd
}

func newNudgeStatusCmd(stdout, stderr io.Writer) *cobra.Command {
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   "status [session]",
		Short: "Show queued and dead-letter nudges for a session",
		Long: `Show queued and dead-letter nudges for a session.

Defaults to $GC_ALIAS or $GC_SESSION_ID when run inside a session.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if cmdNudgeStatus(args, jsonOutput, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Output as JSON")
	return cmd
}

func newNudgeDrainCmd(stdout, stderr io.Writer) *cobra.Command {
	var inject bool
	var hookFormat string
	cmd := &cobra.Command{
		Use:    "drain [session]",
		Short:  "Deliver queued nudges for a session",
		Long:   "Deliver queued nudges for a session. Used by runtime hooks.",
		Args:   cobra.MaximumNArgs(1),
		Hidden: true,
		RunE: func(_ *cobra.Command, args []string) error {
			if cmdNudgeDrainWithFormat(args, inject, hookFormat, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&inject, "inject", false, "emit <system-reminder> output for hook injection")
	cmd.Flags().StringVar(&hookFormat, "hook-format", "", "format hook output for a provider")
	return cmd
}

func newNudgePollCmd(stdout, stderr io.Writer) *cobra.Command {
	var sessionName string
	var interval time.Duration
	var quiescence time.Duration
	cmd := &cobra.Command{
		Use:    "poll [session]",
		Short:  "Poll and deliver queued nudges for sessions that need out-of-band delivery",
		Long:   "Poll and deliver queued nudges for sessions that need an out-of-band delivery fallback. Used internally.",
		Args:   cobra.MaximumNArgs(1),
		Hidden: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if cmdNudgePoll(args, sessionName, interval, quiescence, cmd.Flags().Changed("interval"), stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&sessionName, "session", "", "runtime session name (defaults to $GC_SESSION_NAME)")
	cmd.Flags().DurationVar(&interval, "interval", defaultNudgePollInterval, "poll interval (overrides [session] nudge_poll_interval)")
	cmd.Flags().DurationVar(&quiescence, "quiescence", defaultNudgePollQuiescence, "minimum inactivity before injecting")
	return cmd
}

func newNudgeDropCmd(stdout, stderr io.Writer) *cobra.Command {
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   "drop <id>...",
		Short: "Dead-letter one or more pending or in-flight nudges",
		Long: `Dead-letter one or more pending or in-flight nudges by ID.

Each dropped nudge is terminalized through the same dead-letter path a
failed delivery attempt uses, so it lands in "gc nudge status" as dead
rather than disappearing silently. Find IDs with "gc nudge status".

Dropping an in-flight nudge dead-letters it even if it was already
injected into the session but not yet acked.

This only accepts explicit nudge IDs; it does not do bulk or age-based
selection.`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if cmdNudgeDrop(args, jsonOutput, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Output as JSON")
	return cmd
}

func cmdNudgeStatus(args []string, jsonOutput bool, stdout, stderr io.Writer) int {
	targetID := os.Getenv("GC_ALIAS")
	if targetID == "" {
		targetID = os.Getenv("GC_SESSION_ID")
	}
	if len(args) > 0 {
		targetID = args[0]
	}
	if targetID == "" {
		fmt.Fprintln(stderr, "gc nudge status: session not specified (set $GC_ALIAS/$GC_SESSION_ID or pass an alias/id)") //nolint:errcheck
		return 1
	}

	target, err := resolveNudgeTarget(targetID, stderr)
	if err != nil {
		fmt.Fprintf(stderr, "gc nudge status: %v\n", err) //nolint:errcheck
		return 1
	}

	pending, inFlight, dead, err := listQueuedNudgesForTarget(target.cityPath, target, time.Now())
	if err != nil {
		fmt.Fprintf(stderr, "gc nudge status: %v\n", err) //nolint:errcheck
		return 1
	}

	// City-wide (not agent-scoped) skip-reason totals from the supervisor
	// dispatch tick, read directly off the persisted queue state — a raw
	// read so this doesn't re-run the maintenance sweep listQueuedNudgesForTarget
	// above already ran. Best-effort: a load failure here must not fail the
	// whole status command over a diagnostics-only field.
	var dispatchSkips map[string]int64
	if qs, qsErr := nudgequeue.LoadState(target.cityPath); qsErr == nil {
		dispatchSkips = qs.DispatchSkips
	}

	if jsonOutput {
		if err := writeCLIJSONLine(stdout, nudgeStatusJSON{
			SchemaVersion: "1",
			Command:       "nudge status",
			CityPath:      target.cityPath,
			Agent:         target.agentKey(),
			Session:       target.sessionName,
			SessionID:     target.sessionID,
			Counts: nudgeStatusCounts{
				Pending:  len(pending),
				InFlight: len(inFlight),
				Dead:     len(dead),
			},
			Pending:       nudgeStatusItems(pending),
			InFlight:      nudgeStatusItems(inFlight),
			Dead:          nudgeStatusItems(dead),
			DispatchSkips: dispatchSkips,
		}); err != nil {
			fmt.Fprintf(stderr, "gc nudge status: writing JSON: %v\n", err) //nolint:errcheck
			return 1
		}
		return 0
	}

	tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintf(tw, "AGENT\tPENDING\tIN_FLIGHT\tDEAD\tSESSION\n") //nolint:errcheck
	_, _ = fmt.Fprintf(tw, "%s\t%d\t%d\t%d\t%s\n",
		target.agentKey(), len(pending), len(inFlight), len(dead), target.sessionName)
	_ = tw.Flush()

	if len(pending) > 0 {
		fmt.Fprintln(stdout, "") //nolint:errcheck
		for _, item := range pending {
			_, _ = fmt.Fprintf(stdout, "pending  %s  due=%s  source=%s  %s\n",
				item.ID, formatDueTime(item.DeliverAfter), item.Source, item.Message)
		}
	}
	if len(inFlight) > 0 {
		fmt.Fprintln(stdout, "") //nolint:errcheck
		for _, item := range inFlight {
			_, _ = fmt.Fprintf(stdout, "in-flight  %s  lease=%s  source=%s  %s\n",
				item.ID, formatDueTime(item.LeaseUntil), item.Source, item.Message)
		}
	}
	if len(dead) > 0 {
		fmt.Fprintln(stdout, "") //nolint:errcheck
		for _, item := range dead {
			_, _ = fmt.Fprintf(stdout, "dead     %s  reason=%s  source=%s  %s\n",
				item.ID, deadReason(item), item.Source, item.Message)
		}
	}
	if len(dispatchSkips) > 0 {
		fmt.Fprintln(stdout, "")                                                         //nolint:errcheck
		fmt.Fprintln(stdout, "dispatch-tick skips (city-wide, all agents, cumulative):") //nolint:errcheck
		for _, reason := range sortedNudgeDispatchSkipReasons(dispatchSkips) {
			_, _ = fmt.Fprintf(stdout, "  %s=%d\n", reason, dispatchSkips[reason])
		}
	}
	return 0
}

// sortedNudgeDispatchSkipReasons returns counts's keys in deterministic
// (alphabetical) order for stable CLI output.
func sortedNudgeDispatchSkipReasons(counts map[string]int64) []string {
	reasons := make([]string, 0, len(counts))
	for reason := range counts {
		reasons = append(reasons, reason)
	}
	sort.Strings(reasons)
	return reasons
}

func nudgeStatusItems(items []queuedNudge) []queuedNudge {
	out := make([]queuedNudge, len(items))
	for i, item := range items {
		item.Shadow = nil
		out[i] = item
	}
	return out
}

// nudgeDropStatus classifies an ID passed to `gc nudge drop` against the
// current queue state, before the drop's write pass runs.
type nudgeDropStatus int

const (
	nudgeDropStatusNotFound nudgeDropStatus = iota
	nudgeDropStatusDroppable
	nudgeDropStatusAlreadyDead
)

type nudgeDropResult struct {
	ID      string `json:"id"`
	OK      bool   `json:"ok"`
	Outcome string `json:"outcome,omitempty"`
	Error   string `json:"error,omitempty"`
}

type nudgeDropJSON struct {
	SchemaVersion string            `json:"schema_version"`
	OK            bool              `json:"ok"`
	Command       string            `json:"command"`
	CityPath      string            `json:"city_path"`
	Results       []nudgeDropResult `json:"results"`
}

func cmdNudgeDrop(args []string, jsonOutput bool, stdout, stderr io.Writer) int {
	var ids []string
	for _, arg := range args {
		id := strings.TrimSpace(arg)
		if id == "" {
			continue
		}
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		fmt.Fprintln(stderr, "gc nudge drop: no nudge ID given") //nolint:errcheck
		return 1
	}

	cityPath, err := resolveCity()
	if err != nil {
		fmt.Fprintf(stderr, "gc nudge drop: %v\n", err) //nolint:errcheck
		return 1
	}
	return doNudgeDrop(cityPath, ids, jsonOutput, stdout, stderr)
}

func doNudgeDrop(cityPath string, ids []string, jsonOutput bool, stdout, stderr io.Writer) int {
	now := time.Now()
	statuses, err := classifyQueuedNudgeIDs(cityPath, ids, now)
	if err != nil {
		fmt.Fprintf(stderr, "gc nudge drop: %v\n", err) //nolint:errcheck
		return 1
	}

	var droppable []string
	for _, id := range ids {
		if statuses[id] == nudgeDropStatusDroppable {
			droppable = append(droppable, id)
		}
	}
	// Classification and the write pass take the queue lock separately -- the
	// flock is per-process and non-reentrant -- so an ID can be delivered and
	// acked in between. Report from the write pass's own dead-lettered slice
	// rather than the classification, or a nudge nobody dropped is still
	// reported as dropped.
	dropped := make(map[string]bool, len(droppable))
	if len(droppable) > 0 {
		store, opened, err := openNudgeBeadStoreOwned(cityPath)
		if err != nil {
			fmt.Fprintf(stderr, "gc nudge drop: %v\n", err) //nolint:errcheck
			return 1
		}
		defer closeBeadStoreHandle(opened) //nolint:errcheck // best-effort
		deadLettered, err := recordQueuedNudgeFailureDetailed(cityPath, store, droppable, errNudgeManualDrop, now)
		if err != nil {
			fmt.Fprintf(stderr, "gc nudge drop: %v\n", err) //nolint:errcheck
			return 1
		}
		for _, item := range deadLettered {
			dropped[item.ID] = true
		}
	}

	exit := 0
	results := make([]nudgeDropResult, 0, len(ids))
	for _, id := range ids {
		switch statuses[id] {
		case nudgeDropStatusDroppable:
			if !dropped[id] {
				exit = 1
				results = append(results, nudgeDropResult{ID: id, OK: false, Error: "no longer queued (delivered or acked concurrently)"})
				fmt.Fprintf(stderr, "gc nudge drop %s: no longer queued (delivered or acked concurrently)\n", id) //nolint:errcheck
				continue
			}
			results = append(results, nudgeDropResult{ID: id, OK: true, Outcome: "dropped"})
			if !jsonOutput {
				fmt.Fprintf(stdout, "Dropped nudge %s\n", id) //nolint:errcheck
			}
		case nudgeDropStatusAlreadyDead:
			exit = 1
			results = append(results, nudgeDropResult{ID: id, OK: false, Error: "already dead-lettered"})
			fmt.Fprintf(stderr, "gc nudge drop %s: already dead-lettered\n", id) //nolint:errcheck
		default:
			exit = 1
			results = append(results, nudgeDropResult{ID: id, OK: false, Error: "no such queued nudge"})
			fmt.Fprintf(stderr, "gc nudge drop %s: no such queued nudge\n", id) //nolint:errcheck
		}
	}

	if jsonOutput {
		if err := writeCLIJSONLine(stdout, nudgeDropJSON{
			SchemaVersion: "1",
			OK:            exit == 0,
			Command:       "nudge drop",
			CityPath:      cityPath,
			Results:       results,
		}); err != nil {
			fmt.Fprintf(stderr, "gc nudge drop: writing JSON: %v\n", err) //nolint:errcheck
			return 1
		}
	}
	return exit
}

// classifyQueuedNudgeIDs reports, for each of the given IDs, whether it is
// currently droppable (pending or in-flight), already dead-lettered, or not
// found in the queue at all. It runs the same maintenance sweep every other
// queue read does, so a since-expired or since-recovered item is classified
// against current state rather than a stale snapshot.
func classifyQueuedNudgeIDs(cityPath string, ids []string, now time.Time) (map[string]nudgeDropStatus, error) {
	want := make(map[string]bool, len(ids))
	for _, id := range ids {
		want[id] = true
	}
	statuses := make(map[string]nudgeDropStatus, len(ids))
	err := runNudgeQueueMutationAt(cityPath, "classify", beads.NudgesStore{}, true, now, func(state *nudgeQueueState) error {
		clear(statuses)
		var front *nudgequeue.Store
		deadline := noMaintenanceDeadline()
		if err := recoverExpiredInFlightNudges(state, front, now, deadline); err != nil {
			return err
		}
		if err := pruneExpiredQueuedNudges(state, front, now, deadline); err != nil {
			return err
		}
		if err := pruneDeadQueuedNudges(state, front, now, deadline); err != nil {
			return err
		}
		for _, item := range state.Pending {
			if want[item.ID] {
				statuses[item.ID] = nudgeDropStatusDroppable
			}
		}
		for _, item := range state.InFlight {
			if want[item.ID] {
				statuses[item.ID] = nudgeDropStatusDroppable
			}
		}
		for _, item := range state.Dead {
			if want[item.ID] {
				statuses[item.ID] = nudgeDropStatusAlreadyDead
			}
		}
		return nil
	})
	return statuses, err
}

func cmdNudgeDrainWithFormat(args []string, inject bool, hookFormat string, stdout, stderr io.Writer) int {
	// --inject writes a <system-reminder> straight into a provider's system
	// prompt, and gc stages the overlays that call it into the session work
	// directory — commonly a city or rig root — so a human who opens the same
	// provider there gets them too. With no gc identity this did not come from
	// a session gc started, so there is no session to drain for. Naming a
	// session explicitly is a deliberate request and is still served; the plain
	// non-inject form is the human-facing one and is untouched (#5304).
	if inject && len(args) == 0 && !hookHasManagedIdentity() {
		return 0
	}
	// On every prompt, emit a live clock (operator-local + UTC + epoch) and
	// the agent's active formula step (if any) as UserPromptSubmit hook context.
	// When a nudge also fires we fold everything into that nudge's single
	// provider-formatted payload (see the combined write below); otherwise this
	// deferred fallback emits clock+step on their own. Either way exactly one
	// provider hook context is written per invocation, so JSON formats
	// (codex/gemini) stay one valid document rather than two concatenated objects.
	// See clock_inject.go and wisp_step_inject.go.
	var wispExtra string // step reminder; captured by defer closure
	emittedHookContext := false
	var injectPrefix string
	var contextUsage contextUsageSample
	if inject {
		// Read the provider hook input once (UserPromptSubmit JSON on stdin,
		// pipe-only — see readHookStdin) and build the shared inject prefix:
		// the clock line plus, when context pressure crosses its threshold,
		// the context-usage guidance (see context_inject.go).
		contextUsage = readContextUsageSample(readHookStdin(), hookFormat)
		injectPrefix = clockInjectLine()
		defer func() {
			if !emittedHookContext {
				line := injectPrefix + wispExtra
				if line != "" {
					_ = writeProviderHookContextForEvent(stdout, hookFormat, "UserPromptSubmit", line)
				}
			}
		}()
	}
	targetID := os.Getenv("GC_ALIAS")
	if targetID == "" {
		targetID = os.Getenv("GC_SESSION_ID")
	}
	if len(args) > 0 {
		targetID = args[0]
	}
	if targetID == "" {
		if inject {
			injectPrefix += contextInjectLineForSample(contextUsage, nil, nil)
			return 0
		}
		fmt.Fprintln(stderr, "gc nudge drain: session not specified (set $GC_ALIAS/$GC_SESSION_ID or pass an alias/id)") //nolint:errcheck
		return 1
	}

	// Empty-queue fast path. Resolving the target loads the city config and
	// opens the session store, and on an empty queue the only things it would
	// feed are the context advisory (which renders nothing for a payload with
	// no usage sample, whatever the agent's policy) and the step reminder
	// (which depends only on the city and the hook's identity env). The hook
	// context written is therefore the same as the full path's, so skip the
	// resolution; a queued item or a usage sample takes the full path.
	wispPrefetched := false
	if inject && !contextUsage.ok {
		if cityPath, ok := nudgeDrainExplicitCityPath(); ok && nudgeQueueIsEmpty(cityPath) {
			wispExtra = wispStepInjectionContent(cityPath, false)
			wispPrefetched = true
			// The full path writes the step reminder only once the target
			// resolves, so a non-empty reminder still needs that proof. Recheck
			// the queue too: a nudge enqueued during the lookup must be claimed.
			if wispExtra == "" && nudgeQueueIsEmpty(cityPath) {
				return 0
			}
		}
	}

	// With no explicit target, the drain is the hook's own: GC_SESSION_ID
	// names its session bead exactly, so it is preferred over GC_ALIAS.
	target, err := resolveNudgeTargetPreferringHookIdentity(targetID, len(args) == 0, stderr)
	if err != nil {
		if inject {
			if errors.Is(err, errHookSessionClosed) {
				fmt.Fprintf(stderr, "gc nudge drain: %v; not draining its nudges\n", err) //nolint:errcheck
			}
			wispExtra = ""
			injectPrefix += contextInjectLineForSample(contextUsage, nil, nil)
			return 0
		}
		fmt.Fprintf(stderr, "gc nudge drain: %v\n", err) //nolint:errcheck
		return 1
	}
	if inject {
		injectPrefix += contextInjectLineForSample(contextUsage, target.cfg.AgentDefaults.ContextAdvisory, target.agent.ContextAdvisory)
		if !wispPrefetched {
			wispExtra = wispStepInjectionContent(target.cityPath, false)
		}
	}

	now := time.Now()
	items, err := claimDueQueuedNudgesForTarget(target.cityPath, target, now)
	if err != nil {
		if inject {
			return 0
		}
		fmt.Fprintf(stderr, "gc nudge drain: %v\n", err) //nolint:errcheck
		return 1
	}
	if len(items) == 0 {
		if inject {
			return 0
		}
		return 1
	}
	deliveryStore := openNudgeBeadStore(target.cityPath)
	// Two-store split: the nudge-queue delivery store stays on the nudges class
	// (openNudgeBeadStore), while the session-class ops — wait-bead reads in
	// splitQueuedNudgesForDelivery and the last-nudge-delivered stamp — route
	// through the session store. Identity today (single backend).
	deliverySessStore := cliSessionStore(deliveryStore.Store, target.cfg, target.cityPath)
	var deliverySessFront *session.Store
	var deliveryMailProvider mail.Provider
	if deliveryStore.Store != nil {
		deliverySessFront = sessionFrontDoor(deliverySessStore)
		// Same split for the mail gate: the message re-read in
		// splitQueuedNudgesForDelivery is messaging-class (cliMailStore), while the
		// provider's session-addressing half reuses the session store resolved above.
		deliveryMailProvider = newMailProviderWithSessionStore(cliMailStore(deliveryStore.Store, target.cfg, target.cityPath).Store, deliverySessStore)
	}
	items, rejected := splitQueuedNudgesForTarget(target, items)
	if len(rejected) > 0 {
		_ = recordQueuedNudgeFailureWithStore(target.cityPath, deliveryStore, queuedNudgeIDs(rejected), errNudgeSessionFenceMismatch, time.Now())
	}
	candidates := items
	items, blocked, err := splitQueuedNudgesForDelivery(sessionFrontDoor(deliverySessStore), deliveryMailProvider, candidates)
	if err != nil {
		// Release the claims so the next drain or poller pass retries
		// promptly instead of waiting out the in-flight lease.
		_ = releaseQueuedNudgeClaims(target.cityPath, queuedNudgeIDs(candidates))
		if inject {
			fmt.Fprintf(stderr, "gc nudge drain: validating claimed nudges: %v\n", err) //nolint:errcheck
			return 0
		}
		fmt.Fprintf(stderr, "gc nudge drain: validating claimed nudges: %v\n", err) //nolint:errcheck
		return 1
	}
	if len(blocked) > 0 {
		if err := terminalizeBlockedQueuedNudges(target.cityPath, blocked); err != nil {
			// Best-effort: blocked-item bookkeeping must not abort delivery
			// of the remaining items. The blocked items stay in-flight and
			// lease expiry returns them to pending for a later pass.
			fmt.Fprintf(stderr, "gc nudge drain: withdrawing blocked nudges: %v\n", err) //nolint:errcheck
		}
	}
	if len(items) == 0 {
		if inject {
			return 0
		}
		return 1
	}

	var out string
	if inject {
		out = formatNudgeInjectOutput(items)
	} else {
		out = formatNudgeRuntimeMessage(items)
	}
	var writeErr error
	if inject {
		// Fold the clock and active formula step into the nudge so a single
		// provider-formatted payload carries all; this is the one place the
		// combined context is written.
		emittedHookContext = true
		writeErr = writeProviderHookContextForEvent(stdout, hookFormat, "UserPromptSubmit", injectPrefix+out+wispExtra)
	} else {
		_, writeErr = io.WriteString(stdout, out)
	}
	if writeErr != nil {
		_ = recordQueuedNudgeFailureWithStore(target.cityPath, deliveryStore, queuedNudgeIDs(items), writeErr, time.Now())
		if inject {
			return 0
		}
		fmt.Fprintf(stderr, "gc nudge drain: writing output: %v\n", writeErr) //nolint:errcheck
		return 1
	}
	if inject {
		// gc hook run discards this output if the hook overruns its deadline,
		// so ack only while there is time to finish; otherwise release the
		// claims so the next drain delivers the nudges again.
		if !hookDeadlineAllowsNudgeAck(fmt.Sprintf("%d injected nudge(s)", len(items)), stderr) {
			if err := releaseQueuedNudgeClaims(target.cityPath, queuedNudgeIDs(items)); err != nil {
				fmt.Fprintf(stderr, "gc nudge drain: releasing injected nudges: %v; their lease expiry returns them\n", err) //nolint:errcheck
			}
			return 0
		}
		if err := ackQueuedNudgesWithOutcome(target.cityPath, queuedNudgeIDs(items), "accepted_for_injection", "", "hook-transport-accepted"); err != nil {
			fmt.Fprintf(stderr, "gc nudge drain: recording injection ack: %v\n", err) //nolint:errcheck
			return 0
		}
		// The delivery stamp is best-effort bookkeeping after the destructive
		// ack; it is skipped rather than run into the deadline.
		if hookDeadlineStillLeavesTime() {
			stampLastNudgeDeliveredAt(deliverySessFront, target.sessionID, time.Now())
		}
		return 0
	}
	if err := ackQueuedNudges(target.cityPath, queuedNudgeIDs(items)); err != nil {
		fmt.Fprintf(stderr, "gc nudge drain: %v\n", err) //nolint:errcheck
		return 1
	}
	stampLastNudgeDeliveredAt(deliverySessFront, target.sessionID, time.Now())
	return 0
}

func queuedNudgeOptionsFromTarget(target nudgeTarget) queuedNudgeOptions {
	return queuedNudgeOptions{
		SessionID:         target.sessionID,
		ContinuationEpoch: target.continuationEpoch,
	}
}

// configureNudgePollRuntime bounds the long-lived nudge-poll sidecar's memory
// footprint and, when GC_PPROF=1, exposes a pprof endpoint for diagnosing it.
//
// Root cause (gc-3ftcq): each poll tick that observes a changed city beads.json
// re-parses the entire whole-file store. On the file backend a 100MB+ beads.json
// parses to ~400MB of live heap, and across reparses the Go runtime retains the
// arena (default GOGC headroom), so RSS plateaus near 1GB per sidecar. One
// sidecar runs per active session, so the fleet total is the dominant host-OOM
// driver. A soft memory limit makes the GC return freed arena to the OS instead
// of holding ~2x headroom; the per-parse working set itself is irreducible
// without a query-capable read path (tracked as follow-up).
//
// It returns a cleanup func the caller must defer.
func configureNudgePollRuntime(stderr io.Writer) func() {
	// Respect an operator-provided GOMEMLIMIT; debug.SetMemoryLimit would
	// otherwise silently override it. Only install our default when none is set.
	if strings.TrimSpace(os.Getenv("GOMEMLIMIT")) == "" {
		limitMB := defaultNudgePollMemLimitMB
		if raw := strings.TrimSpace(os.Getenv(nudgePollMemLimitEnv)); raw != "" {
			switch v, err := strconv.Atoi(raw); {
			case err != nil || v < 0:
				// An unparseable or negative override is almost certainly an
				// operator typo (e.g. "512mb"). Warn instead of silently
				// falling back so the misconfiguration is visible; "0" remains
				// a valid, intentional disable handled by the v>0 check below.
				fmt.Fprintf(stderr, "gc nudge poll: ignoring invalid %s=%q; using default %dMiB\n", //nolint:errcheck
					nudgePollMemLimitEnv, raw, defaultNudgePollMemLimitMB)
			default:
				limitMB = v
			}
		}
		if limitMB > 0 {
			debug.SetMemoryLimit(int64(limitMB) * 1024 * 1024)
		}
	}

	srv, err := api.StartPprof(strings.TrimSpace(os.Getenv(nudgePollPprofAddrEnv)))
	if err != nil {
		// pprof is a diagnostic nicety; a bind failure (e.g. address already in
		// use by another sidecar) must not stop the poller from delivering.
		fmt.Fprintf(stderr, "gc nudge poll: pprof: %v\n", err) //nolint:errcheck
	}
	if srv == nil {
		return func() {}
	}
	return func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		srv.Shutdown(ctx) //nolint:errcheck // best-effort shutdown on exit
	}
}

// resolveNudgePollInterval picks the poller cycle interval: an explicitly
// passed --interval flag wins; otherwise a positive [session]
// nudge_poll_interval from the city config; otherwise the passed default.
func resolveNudgePollInterval(cityPath string, flagValue time.Duration, flagExplicit bool) time.Duration {
	if flagExplicit {
		return flagValue
	}
	cfg, err := loadCityConfigWithoutBuiltinPackRefresh(cityPath, io.Discard)
	if err != nil || cfg == nil {
		return flagValue
	}
	if d := cfg.Session.NudgePollIntervalDuration(); d > 0 {
		return d
	}
	return flagValue
}

func cmdNudgePoll(args []string, sessionName string, interval, quiescence time.Duration, intervalExplicit bool, _ io.Writer, stderr io.Writer) int {
	targetID := os.Getenv("GC_ALIAS")
	if targetID == "" {
		targetID = os.Getenv("GC_SESSION_ID")
	}
	if len(args) > 0 {
		targetID = args[0]
	}
	if targetID == "" {
		fmt.Fprintln(stderr, "gc nudge poll: session not specified (set $GC_ALIAS/$GC_SESSION_ID or pass an alias/id)") //nolint:errcheck
		return 1
	}
	target, err := resolveNudgeTarget(targetID, stderr)
	if err != nil {
		fmt.Fprintf(stderr, "gc nudge poll: %v\n", err) //nolint:errcheck
		return 1
	}
	if sessionName != "" {
		target.sessionName = sessionName
	}
	if target.sessionName == "" {
		fmt.Fprintln(stderr, "gc nudge poll: session name unavailable") //nolint:errcheck
		return 1
	}
	interval = resolveNudgePollInterval(target.cityPath, interval, intervalExplicit)

	release, err := acquireNudgePollerLease(target.cityPath, target.sessionName, target.pollerKey())
	if err != nil {
		if errors.Is(err, errNudgePollerRunning) {
			return 0
		}
		fmt.Fprintf(stderr, "gc nudge poll: %v\n", err) //nolint:errcheck
		return 1
	}
	defer release()

	stopRuntime := configureNudgePollRuntime(stderr)
	defer stopRuntime()

	sp, err := newSessionProvider()
	if err != nil {
		fmt.Fprintf(stderr, "gc nudge poll: %v\n", err) //nolint:errcheck
		return 1
	}
	store, err := openNudgeBeadStoreErr(target.cityPath)
	if err != nil || store.Store == nil {
		fmt.Fprintf(stderr, "gc nudge poll: opening the nudge store for %q: %v\n", target.agentKey(), err) //nolint:errcheck
		return 1
	}
	// Session-class store for the observe read (nudgeObserveTarget); the raw
	// nudges store keeps flowing to the queue-delivery path. Identity today.
	sessStore := cliSessionStore(store.Store, target.cfg, target.cityPath)
	var missingSince time.Time
	var lastFreeOS time.Time
	var idleTicksSinceObserve int
	gate := newNudgePollGate()
	skips := &nudgePollSkips{cityPath: target.cityPath}
	defer func() { skips.flush(time.Now(), stderr) }()
	for {
		// Each tick that observes a changed beads.json re-parses the whole-file
		// store, leaving several hundred MB of transient garbage. The soft
		// memory limit caps live arena, but proactively returning freed pages to
		// the OS on a throttled cadence keeps steady-state RSS near the live
		// working set rather than the GC's high-water mark.
		if now := time.Now(); now.Sub(lastFreeOS) >= nudgePollFreeOSInterval {
			debug.FreeOSMemory()
			lastFreeOS = now
		}

		// Cheap gate, read before paying for the expensive observe/deliver
		// pair: this target's queue state alone (no tmux/session probe) tells
		// us whether there is anything to deliver this tick. When nothing is
		// due, most ticks skip nudgeObserveTarget entirely -- but never for
		// more than nudgePollIdleObserveEvery consecutive ticks, so a poller
		// whose session died while its queue sat empty still notices and
		// exits instead of being immortalized (ga-8x82f4).
		hasDue := gate.hasDue(target, time.Now())
		observeThisTick := hasDue
		if !observeThisTick {
			idleTicksSinceObserve++
			if idleTicksSinceObserve >= nudgePollIdleObserveEvery {
				observeThisTick = true
			}
		}
		if !observeThisTick {
			time.Sleep(interval)
			continue
		}
		idleTicksSinceObserve = 0

		// Observing reads the bead store, and on a stopped city that read
		// restarts the bd and Dolt servers `gc stop` just retired (#6857).
		if nudgePollCityStopped(target, sp) {
			fmt.Fprintf(stderr, "gc nudge poll: city controller and session %q are not running; exiting and leaving queued nudges for the next start\n", target.sessionName) //nolint:errcheck
			return 0
		}
		obs, err := nudgeObserveTarget(target, sessStore, sp)
		if err != nil {
			fmt.Fprintf(stderr, "gc nudge poll: %v\n", err) //nolint:errcheck
			// Transient observation failures (store hiccup, runtime probe
			// race) must not kill the poller while queued work is pending:
			// for an idle session this sidecar is the only delivery path, so
			// exiting here strands the queue until something re-launches a
			// poller. Reuse the missing-session grace window; persistent
			// failures still exit once it elapses or the queue drains.
			now := time.Now()
			if shouldKeepNudgePollerAlive(target, missingSince, now) {
				if missingSince.IsZero() {
					missingSince = now
				}
				time.Sleep(interval)
				continue
			}
			return 1
		}
		if !obs.Running {
			now := time.Now()
			if shouldKeepNudgePollerAlive(target, missingSince, now) {
				if missingSince.IsZero() {
					missingSince = now
				}
				time.Sleep(interval)
				continue
			}
			return 0
		}
		missingSince = time.Time{}
		if !hasDue {
			// Observed only to satisfy the idle-cadence liveness check above;
			// the cheap gate already established there is nothing to deliver.
			time.Sleep(interval)
			continue
		}
		delivered, pollErr := nudgePollDeliverQueued(target, store.Store, cliSessionStore(store.Store, target.cfg, target.cityPath), sp, quiescence, obs)
		if pollErr != nil {
			fmt.Fprintf(stderr, "gc nudge poll: %v\n", pollErr) //nolint:errcheck
		}
		if delivered {
			continue
		}
		// Mirror dispatchAllQueuedNudges' skip accounting (nudge_dispatcher.go),
		// gate included: "not-delivered" means this target HAD due queued work
		// and still got nothing this tick -- most commonly
		// tryDeliverQueuedNudgesByPoller's quiescence gate rejecting a
		// continuously busy session. A running session with nothing queued is
		// the dispatcher's separate "not-matched" class and is deliberately not
		// counted here: the poll loop never exits while the session runs, so
		// counting it would turn this into a tick counter and take the queue
		// flock every interval. See #5317. Re-checked fresh (not reusing the
		// pre-observe hasDue above) because the delivery attempt just run can
		// itself have consumed the only due item.
		countNudgePollSkip(gate, skips, target, pollErr, time.Now(), stderr)
		time.Sleep(interval)
	}
}

const (
	nudgePollCensusTTL         = 30 * time.Second
	nudgePollSkipFlushInterval = 30 * time.Second
)

type nudgePollGate struct {
	census   map[string]struct{}
	censusAt time.Time
}

func newNudgePollGate() *nudgePollGate { return &nudgePollGate{} }

func (g *nudgePollGate) hasDue(target nudgeTarget, now time.Time) bool {
	readCensus := func() map[string]struct{} {
		if g.census != nil && now.Sub(g.censusAt) >= 0 && now.Sub(g.censusAt) < nudgePollCensusTTL {
			return g.census
		}
		live := liveNudgeFenceSessionIDsForCity(target.cityPath)
		if live != nil {
			g.census, g.censusAt = live, now
		}
		return live
	}
	eligible, err := claimableNudgeSnapshotWithCensus(target.cityPath, target, now, readCensus)
	return err == nil && len(eligible) > 0
}

type nudgePollSkips struct {
	cityPath  string
	counts    map[string]int64
	lastFlush time.Time
}

func (s *nudgePollSkips) add(reason string, now time.Time, stderr io.Writer) {
	if s.counts == nil {
		s.counts = make(map[string]int64)
		s.lastFlush = now
	}
	s.counts[reason]++
	if now.Sub(s.lastFlush) >= nudgePollSkipFlushInterval {
		s.flush(now, stderr)
	}
}

func (s *nudgePollSkips) flush(now time.Time, stderr io.Writer) {
	if len(s.counts) == 0 {
		return
	}
	if err := recordNudgeDispatchSkips(s.cityPath, s.counts); err != nil {
		if stderr != nil {
			fmt.Fprintf(stderr, "gc nudge poll: recording dispatch skip counter: %v\n", err) //nolint:errcheck
		}
		return
	}
	s.counts = nil
	s.lastFlush = now
}

func countNudgePollSkip(gate *nudgePollGate, skips *nudgePollSkips, target nudgeTarget, pollErr error, now time.Time, stderr io.Writer) {
	if !gate.hasDue(target, now) {
		return
	}
	reason := "not-delivered"
	if pollErr != nil {
		reason = "not-delivered-error"
	}
	skips.add(reason, now, stderr)
}

func shouldKeepNudgePollerAlive(target nudgeTarget, missingSince, now time.Time) bool {
	// Lock-free read (ga-2kzci3 FR5): this is a liveness check, not a
	// maintenance operation, and must not wait on the queue's writer lock.
	pending, inFlight, _, err := listQueuedNudgesForTargetSnapshot(target.cityPath, target, now)
	if err != nil || (len(pending) == 0 && len(inFlight) == 0) {
		return false
	}
	if missingSince.IsZero() {
		return true
	}
	return now.Sub(missingSince) < defaultNudgePollStartGrace
}

func deliverSessionNudge(target nudgeTarget, message string, mode nudgeDeliveryMode, jsonOutput bool, stdout, stderr io.Writer) int {
	store, err := openNudgeBeadStoreErr(target.cityPath)
	if err != nil || store.Store == nil {
		fmt.Fprintf(stderr, "gc session nudge: opening the nudge store for %q: %v\n", target.agentKey(), err) //nolint:errcheck
		return 1
	}
	sp, err := newSessionProvider()
	if err != nil {
		fmt.Fprintf(stderr, "gc session nudge: %v\n", err) //nolint:errcheck
		return 1
	}
	return deliverSessionNudgeWithWorker(target, store.Store, sp, message, mode, jsonOutput, stdout, stderr)
}

func deliverSessionNudgeWithWorker(target nudgeTarget, store beads.Store, sp runtime.Provider, message string, mode nudgeDeliveryMode, jsonOutput bool, stdout, stderr io.Writer) int {
	if mode == nudgeDeliveryQueue {
		return queueSessionNudgeWithWorker(target, store, sp, message, mode, jsonOutput, "", stdout, stderr)
	}
	// Two-store split: the raw store keeps threading the nudge-enqueue currency,
	// while the session-class observe/handle reads below route through sessStore.
	// nil store -> cliSessionStore returns nil (identity), preserving the
	// store-less WithProvider path.
	sessStore := cliSessionStore(store, target.cfg, target.cityPath)
	queueManagedWake, err := shouldQueueManagedNudgeWake(target, store, sp)
	if err != nil {
		fmt.Fprintf(stderr, "gc session nudge: %v\n", err) //nolint:errcheck
		return 1
	}
	if queueManagedWake {
		return queueManagedSessionNudgeWake(target, store, message, mode, jsonOutput, stdout, stderr)
	}
	// Never type into a pending permission prompt (#2892): its highlighted
	// "❯ 1. Yes" reads as an idle composer, and the nudge's Enter would
	// approve the tool call. wait-idle already means "deliver at a safe
	// boundary", so queue it behind the prompt; immediate cannot wait, so
	// refuse with an explanation.
	pending, err := nudgeTargetPendingInteraction(target, sessStore, sp, mode, stderr)
	if err != nil {
		fmt.Fprintf(stderr, "gc session nudge: %v\n", err) //nolint:errcheck
		return 1
	}
	if pending != nil {
		return refuseOrQueueNudgeBehindPendingInteraction(target, store, sp, message, mode, jsonOutput, describeWorkerPendingInteraction(pending), stdout, stderr)
	}
	// A wait-idle nudge to a RUNNING-but-busy target must not block the caller in
	// the worker's synchronous WaitForIdle (runtimeHandleWaitIdleTimeout, 30s):
	// the session never reports idle for the whole window, so the caller stalls
	// ~30s while the city store's Dolt connection sits idle in scope and gets
	// reaped server-side ("[mysql] unexpected EOF"). Detect "busy" without
	// blocking (LastActivity, the same quiescence signal the nudge poller uses)
	// and queue immediately instead — the supervisor dispatcher (or the
	// per-session poller in legacy mode) delivers at the next idle boundary, off
	// the caller's critical path. An idle target still delivers live below (its
	// WaitForIdle returns promptly); an unknown LastActivity is treated as
	// not-busy so untracked sessions keep the existing behavior.
	//
	// Restrict this to the claude, non-ACP transport — the ONLY case
	// RuntimeHandle.nudgeWaitIdle actually blocks on WaitForIdle. ACP delivers
	// live in-process and non-claude returns immediately (see
	// internal/worker/runtime_handle.go nudgeWaitIdle), so short-circuiting
	// those would needlessly downgrade live delivery to queued. (gco-90ui)
	if mode == nudgeDeliveryWaitIdle && target.sessionTransport() != "acp" && target.providerName() == "claude" {
		if obs, obsErr := nudgeObserveTarget(target, sessStore, sp); obsErr == nil && obs.Running &&
			(nudgeObservationBusy(obs) || nudgeTargetAtInteractivePrompt(target, sp)) {
			// The session is running but not safe for live delivery: either
			// actively busy, or parked at an interactive prompt whose default
			// action is bound to Enter. Queue as a deferred reminder so the
			// supervisor dispatcher delivers it at the next between-turns
			// boundary instead of injecting keystrokes into the prompt.
			return queueSessionNudgeWithWorker(target, store, sp, message, mode, jsonOutput, worker.NudgeUndeliveredNoIdleBoundary, stdout, stderr)
		}
	}
	delivery, ok := workerNudgeDeliveryForMode(mode)
	if !ok {
		fmt.Fprintf(stderr, "gc session nudge: unknown delivery mode %q\n", mode) //nolint:errcheck
		return 1
	}
	handle, err := workerHandleForNudgeTarget(target, sessStore, sp)
	if err != nil {
		fmt.Fprintf(stderr, "gc session nudge: %v\n", err) //nolint:errcheck
		return 1
	}
	result, err := handle.Nudge(context.Background(), sessionActor(session.ActorAgent, target.cityPath), worker.NudgeRequest{
		Text:     message,
		Delivery: delivery,
		Source:   "session",
	})
	if err != nil && errors.Is(err, tmux.ErrNudgeSubmitDeliveredUnobserved) {
		// The submit Enter was delivered and the composer drained; only the
		// busy-indicator OBSERVATION timed out. Delivery is proven, so this
		// must report success like any other delivered nudge, not a CLI
		// failure — fall through to the normal success path below.
		result.Delivered = true
		err = nil
	}
	if err != nil {
		if errors.Is(err, runtime.ErrSessionNotFound) && target.sessionTransport() == "acp" {
			if mode == nudgeDeliveryWaitIdle {
				return queueSessionNudgeWithWorker(target, store, sp, message, mode, jsonOutput, "", stdout, stderr)
			}
			if mode == nudgeDeliveryImmediate {
				fmt.Fprintf(stderr, "gc session nudge: live ACP delivery failed for %s because this process does not own the ACP connection; retry with --delivery=wait-idle or --delivery=queue so the queued dispatcher can deliver it\n", target.agentKey()) //nolint:errcheck
				return 1
			}
		}
		if errors.Is(err, runtime.ErrPendingInteraction) {
			// The prompt appeared between the probe above and delivery; the
			// runtime refused to type into it.
			return refuseOrQueueNudgeBehindPendingInteraction(target, store, sp, message, mode, jsonOutput, err.Error(), stdout, stderr)
		}
		fmt.Fprintf(stderr, "gc session nudge: %v\n", err) //nolint:errcheck
		return 1
	}
	if result.Undelivered == worker.NudgeQueuedHeld {
		// The session is held and queued the nudge itself (CONTRACT v5.9 D8);
		// a held wait-idle nudge (NudgeUndeliveredHeld) is queued below.
		return writeQueuedSessionNudgeResult(target, mode, jsonOutput, result.Undelivered, stdout, stderr)
	}
	if mode == nudgeDeliveryWaitIdle && !result.Delivered {
		return queueSessionNudgeWithWorker(target, store, sp, message, mode, jsonOutput, result.Undelivered, stdout, stderr)
	}
	// Stamp every delivered nudge: a resumed session's manager already
	// reports a delivered-but-unobserved submit as plain delivery.
	if result.Delivered && store != nil {
		stampLastNudgeDeliveredAt(sessionFrontDoor(sessStore), target.sessionID, time.Now())
	}
	if jsonOutput {
		return writeCLIJSONLineOrExit(stdout, stderr, "gc session nudge", sessionNudgeJSON{
			SchemaVersion: "1",
			OK:            true,
			Target:        target.agentKey(),
			SessionID:     target.sessionID,
			SessionName:   target.sessionName,
			Delivery:      string(mode),
			Queued:        false,
			Outcome:       "delivered",
		})
	}
	fmt.Fprintf(stdout, "Nudged %s\n", target.agentKey()) //nolint:errcheck
	return 0
}

// nudgeTargetPendingInteraction reports the interaction the target session is
// waiting on, if any. Providers that cannot report interactions report none.
// A look that fails is an error for an immediate nudge, which cannot wait for
// an answer. A wait-idle nudge reports the failure on stderr and goes on: its
// delivery looks for a prompt again before it types, and the nudge is queued
// behind one it finds.
func nudgeTargetPendingInteraction(target nudgeTarget, sessStore beads.Store, sp runtime.Provider, mode nudgeDeliveryMode, stderr io.Writer) (*worker.PendingInteraction, error) {
	handle, err := workerHandleForNudgeTarget(target, sessStore, sp)
	if err != nil {
		return nil, err
	}
	pending, err := handle.Pending(context.Background())
	if err == nil {
		return pending, nil
	}
	err = fmt.Errorf("checking %s for a pending permission prompt: %w", target.agentKey(), err)
	if mode != nudgeDeliveryWaitIdle {
		return nil, err
	}
	fmt.Fprintf(stderr, "gc session nudge: %v; delivering when idle, which looks again before it types\n", err) //nolint:errcheck
	return nil, nil
}

func describeWorkerPendingInteraction(pending *worker.PendingInteraction) string {
	what := strings.TrimSpace(pending.Kind)
	if what == "" {
		what = "interaction"
	}
	if prompt := strings.TrimSpace(pending.Prompt); prompt != "" {
		what += " " + strconv.Quote(prompt)
	}
	if pending.RequestID != "" {
		what += ", request " + pending.RequestID
	}
	return what
}

// refuseOrQueueNudgeBehindPendingInteraction handles a nudge to a session that
// is waiting on a permission prompt: wait-idle queues it for delivery after
// the prompt is answered; immediate fails with an explanation.
func refuseOrQueueNudgeBehindPendingInteraction(target nudgeTarget, store beads.Store, sp runtime.Provider, message string, mode nudgeDeliveryMode, jsonOutput bool, what string, stdout, stderr io.Writer) int {
	if mode == nudgeDeliveryWaitIdle {
		return queueSessionNudgeWithWorker(target, store, sp, message, mode, jsonOutput, worker.NudgeUndeliveredPendingInteraction, stdout, stderr)
	}
	fmt.Fprintf(stderr, "gc session nudge: %s is waiting on a permission prompt (%s); typing now would answer it. Approve or deny it first (POST .../session/{id}/respond), or retry with --delivery=wait-idle or --delivery=queue to deliver after it is answered\n", target.agentKey(), what) //nolint:errcheck
	return 1
}

func shouldQueueManagedNudgeWake(target nudgeTarget, store beads.Store, sp runtime.Provider) (bool, error) {
	if !canRequestManagedNudgeWake(target, store) {
		return false, nil
	}
	// The observe is a session-class read; route it through the session store
	// (identity today). The nil-check above keeps the raw store deliberately.
	obs, err := nudgeObserveTarget(target, cliSessionStore(store, target.cfg, target.cityPath), sp)
	if err != nil {
		return false, fmt.Errorf("observing managed session before wake routing: %w", err)
	}
	return !obs.Running, nil
}

// nudgeObservationBusy reports, WITHOUT blocking, whether the observed session
// is actively busy right now. It uses the LastActivity timestamp — the same
// quiescence signal the nudge poller relies on (defaultNudgePollQuiescence). An
// unknown (nil/zero) LastActivity returns false (treat as not-busy) so callers
// fall back to the existing wait-idle path rather than queue a session whose
// activity cannot be observed.
//
// This is the near-inverse of pollerSessionIdleEnough's LastActivity branch
// (idle when time-since >= quiescence); they intentionally differ on the
// unknown-LastActivity case — the poller falls back to a blocking WaitForIdle,
// whereas this non-blocking caller-side check must not, so it defaults to
// not-busy.
func nudgeObservationBusy(obs worker.LiveObservation) bool {
	if obs.LastActivity == nil || obs.LastActivity.IsZero() {
		return false
	}
	return time.Since(*obs.LastActivity) < defaultNudgePollQuiescence
}

func canRequestManagedNudgeWake(target nudgeTarget, store beads.Store) bool {
	return store != nil &&
		strings.TrimSpace(target.cityPath) != "" &&
		target.sessionID != "" &&
		nudgeCityUsesManagedReconciler(target.cityPath)
}

func queueManagedSessionNudgeWake(target nudgeTarget, store beads.Store, message string, mode nudgeDeliveryMode, jsonOutput bool, stdout, stderr io.Writer) int {
	item := newQueuedNudgeWithOptions(target.agentKey(), message, "session", time.Now(), queuedNudgeOptionsFromTarget(target))
	outcome, err := enqueueManagedNudgeThenWake(target, store, item)
	if err != nil {
		fmt.Fprintf(stderr, "gc session nudge: %v\n", err) //nolint:errcheck
		return 1
	}
	var undelivered worker.NudgeUndeliveredReason
	if outcome == session.WakeHeld {
		undelivered = worker.NudgeQueuedHeld
	} else if err := nudgePokeController(target.cityPath, reconcilekey.Session(target.sessionID)); err != nil {
		fmt.Fprintf(stderr, "gc session nudge: warning: poke failed: %v\n", err) //nolint:errcheck
	}
	return writeQueuedSessionNudgeResult(target, mode, jsonOutput, undelivered, stdout, stderr)
}

func enqueueManagedNudgeThenWake(target nudgeTarget, store beads.Store, item queuedNudge) (session.WakeRequestOutcome, error) {
	// store is class-mixed here: the enqueue/rollback arms are nudge-class (wrap
	// into the typed NudgesStore), while the wake arm reads the session bead and
	// wakes it (sessions class), so it routes through the session coordination-class
	// store via cliSessionStore (identity today). enqueue (NudgesStore wrap) and
	// rollback (nudgeFrontDoor) stay nudges.
	nudges := beads.NudgesStore{Store: store}
	if err := enqueueQueuedNudgeWithStore(target.cityPath, nudges, item); err != nil {
		return 0, err
	}
	outcome, err := requestManagedNudgeWake(target, cliSessionFrontDoor(store, target.cfg, target.cityPath))
	if err != nil {
		if rollbackErr := rollbackQueuedNudge(target.cityPath, nudges, item, "managed wake failed: "+err.Error()); rollbackErr != nil {
			return 0, errors.Join(err, fmt.Errorf("rolling back queued nudge %q after managed wake failure: %w", item.ID, rollbackErr))
		}
		return 0, err
	}
	return outcome, nil
}

// requestManagedNudgeWake asks the controller to wake a managed session that
// owns queued nudges, never over an operator's hold (CONTRACT v5.9 D8): a
// held row keeps its nudge queued until an operator resumes it. sessFront is
// the session-class front door (cliSessionFrontDoor).
func requestManagedNudgeWake(target nudgeTarget, sessFront *session.Store) (session.WakeRequestOutcome, error) {
	if !sessFront.Backed() || target.sessionID == "" {
		// The item IS enqueued; what did not happen is the wake that gets a
		// stopped session to drain it. Returning nil silently made those two
		// facts indistinguishable, so a nudge to a stopped session with no
		// session-store backing looked delivered and simply sat in the queue.
		// Report the downgrade rather than failing the enqueue: the queued item
		// is still valid and the controller's own patrol tick can deliver it.
		if nudgeWarningWriter != nil {
			fmt.Fprintf(nudgeWarningWriter, //nolint:errcheck // best-effort warning
				"gc session nudge: queued for %s but no managed wake was requested (%s); it is delivered when the session next runs\n",
				target.agentKey(), managedNudgeWakeSkipReason(target, sessFront))
		}
		return 0, nil
	}
	return sessFront.RequestWakeUnlessHeld(target.sessionID, false, time.Now().UTC())
}

// managedNudgeWakeSkipReason names which precondition of the managed wake was
// missing, so the downgrade line is actionable rather than merely present.
func managedNudgeWakeSkipReason(target nudgeTarget, sessFront *session.Store) string {
	switch {
	case !sessFront.Backed():
		return "no session store is backing this command"
	case target.sessionID == "":
		return "the target resolved to no session bead id"
	default:
		return "unknown"
	}
}

func workerHandleForNudgeTarget(target nudgeTarget, store beads.Store, sp runtime.Provider) (worker.Handle, error) {
	if target.sessionName != "" {
		if target.sessionID != "" || target.continuationEpoch != "" {
			obs, err := workerObserveSessionTargetWithConfig(target.cityPath, store, sp, target.cfg, target.sessionName)
			if err == nil {
				matches, matchErr := nudgeTargetLiveGenerationMatches(target, obs, sp)
				if matchErr != nil {
					return nil, matchErr
				}
				if !matches {
					return nil, fmt.Errorf("%w: live runtime %q no longer matches session generation %q", runtime.ErrSessionNotFound, target.sessionName, target.pollerKey())
				}
				if !obs.Running && target.sessionID != "" {
					return workerHandleForSessionWithConfig(target.cityPath, store, sp, target.cfg, target.sessionID)
				}
			}
			if err != nil && !errors.Is(err, runtime.ErrSessionNotFound) && !errors.Is(err, session.ErrSessionNotFound) {
				return nil, err
			}
		}
		handle, err := runtimeWorkerHandleWithConfig(
			target.cityPath,
			store,
			sp,
			target.cfg,
			target.sessionName,
			strings.TrimSpace(target.providerName()),
			strings.TrimSpace(target.sessionTransport()),
			nil,
		)
		if err == nil {
			return handle, nil
		}
		if target.sessionID == "" || !errors.Is(err, runtime.ErrSessionNotFound) {
			return nil, err
		}
	}
	if target.sessionID != "" {
		return workerHandleForSessionWithConfig(target.cityPath, store, sp, target.cfg, target.sessionID)
	}
	return runtimeWorkerHandleWithConfig(
		target.cityPath,
		store,
		sp,
		target.cfg,
		target.sessionName,
		strings.TrimSpace(target.providerName()),
		strings.TrimSpace(target.sessionTransport()),
		nil,
	)
}

const (
	// nudgeObserveTransientAttempts/nudgeObserveTransientBackoff mirror
	// beads.BdStore.runBDTransientRead: the notify observe/read is
	// idempotent, so a bounded retry on a transient Dolt connection drop
	// (see beads.IsTransientConnError) is safe and prevents the nudge from
	// being silently lost to a blip.
	nudgeObserveTransientAttempts = 3
	nudgeObserveTransientBackoff  = 50 * time.Millisecond
)

// workerObserveNudgeTargetOnce is a test seam for workerObserveNudgeTarget's
// single-attempt observation logic.
var workerObserveNudgeTargetOnce = workerObserveNudgeTargetImpl

// workerObserveNudgeTarget observes a nudge target, retrying a bounded
// number of times on a transient Dolt/bd connection error. On persistent
// failure the error is returned to the caller rather than swallowed, so it
// can be logged instead of silently dropping the notice.
// workerNudgeTargetAtInteractivePrompt peeks the target session's live pane and
// reports whether it is showing an interactive selection/confirmation prompt.
// It fails open (false) on a missing name or any peek error so nudge delivery
// falls back to the existing wait-idle behavior rather than erroring. (gascity#2892)
func workerNudgeTargetAtInteractivePrompt(target nudgeTarget, sp runtime.Provider) bool {
	if sp == nil {
		return false
	}
	name := target.sessionName
	if name == "" {
		name = target.sessionID
	}
	if name == "" {
		return false
	}
	pane, err := sp.Peek(name, nudgeInteractivePromptPeekLines)
	if err != nil {
		return false
	}
	return runtime.IsAtInteractivePrompt(pane)
}

func workerObserveNudgeTarget(target nudgeTarget, store beads.Store, sp runtime.Provider) (worker.LiveObservation, error) {
	var (
		obs worker.LiveObservation
		err error
	)
	for attempt := 1; attempt <= nudgeObserveTransientAttempts; attempt++ {
		obs, err = workerObserveNudgeTargetOnce(target, store, sp)
		if err == nil || !beads.IsTransientConnError(err) || attempt == nudgeObserveTransientAttempts {
			return obs, err
		}
		time.Sleep(time.Duration(attempt) * nudgeObserveTransientBackoff)
	}
	return obs, err
}

func workerObserveNudgeTargetImpl(target nudgeTarget, store beads.Store, sp runtime.Provider) (worker.LiveObservation, error) {
	if target.sessionName != "" {
		obs, err := workerObserveSessionTargetWithConfig(target.cityPath, store, sp, target.cfg, target.sessionName)
		if err != nil {
			return worker.LiveObservation{}, err
		}
		matches, err := nudgeTargetLiveGenerationMatches(target, obs, sp)
		if err != nil {
			return worker.LiveObservation{}, err
		}
		if !matches {
			obs.Running = false
			obs.Alive = false
			obs.Attached = false
			obs.AttachedErr = nil
			obs.LastActivity = nil
		}
		return obs, nil
	}
	if target.sessionID != "" {
		return workerObserveSessionTargetWithConfig(target.cityPath, store, sp, target.cfg, target.sessionID)
	}
	return workerObserveSessionTargetWithConfig(target.cityPath, store, sp, target.cfg, target.sessionName)
}

func nudgeTargetLiveGenerationMatches(target nudgeTarget, obs worker.LiveObservation, sp runtime.Provider) (bool, error) {
	if !obs.Running || (target.sessionID == "" && target.continuationEpoch == "") {
		return true, nil
	}
	if target.sessionID != "" {
		for _, liveID := range []string{obs.SessionID, obs.RuntimeSessionID} {
			liveID = strings.TrimSpace(liveID)
			if liveID != "" && liveID != target.sessionID {
				return false, nil
			}
		}
	}
	if sp == nil || target.sessionName == "" {
		return true, nil
	}
	if target.sessionID != "" {
		liveID, err := sp.GetMeta(target.sessionName, "GC_SESSION_ID")
		if err != nil && !runtime.IsSessionGone(err) {
			return false, err
		}
		if strings.TrimSpace(liveID) != "" && strings.TrimSpace(liveID) != target.sessionID {
			return false, nil
		}
	}
	if target.continuationEpoch != "" {
		liveEpoch, err := sp.GetMeta(target.sessionName, "GC_CONTINUATION_EPOCH")
		if err != nil && !runtime.IsSessionGone(err) {
			return false, err
		}
		if strings.TrimSpace(liveEpoch) != "" && strings.TrimSpace(liveEpoch) != target.continuationEpoch {
			return false, nil
		}
	}
	return true, nil
}

func deliverSessionNudgeWithProvider(target nudgeTarget, sp runtime.Provider, mode nudgeDeliveryMode, stdout, stderr io.Writer) int {
	return deliverSessionNudgeWithWorker(target, nil, sp, "check deploy status", mode, false, stdout, stderr)
}

// queueSessionNudgeWithWorker enqueues a nudge the live leg could not (or must
// not) deliver. undelivered names why live delivery was skipped so the operator
// line can say it; it is empty for a caller that queued by request rather than
// by downgrade.
func queueSessionNudgeWithWorker(target nudgeTarget, store beads.Store, sp runtime.Provider, message string, mode nudgeDeliveryMode, jsonOutput bool, undelivered worker.NudgeUndeliveredReason, stdout, stderr io.Writer) int {
	if err := enqueueQueuedNudge(target.cityPath, newQueuedNudgeWithOptions(target.agentKey(), message, "session", time.Now(), queuedNudgeOptionsFromTarget(target))); err != nil {
		fmt.Fprintf(stderr, "gc session nudge: %v\n", err) //nolint:errcheck
		return 1
	}
	// The observe is a session-class read; route through the session store
	// (identity today). The enqueue above stays on its own nudge store.
	if obs, err := workerObserveNudgeTarget(target, cliSessionStore(store, target.cfg, target.cityPath), sp); err == nil && obs.Running {
		maybeStartNudgePoller(target)
	}
	return writeQueuedSessionNudgeResult(target, mode, jsonOutput, undelivered, stdout, stderr)
}

// writeQueuedSessionNudgeResult reports a queued nudge truthfully: WHERE it was
// queued (the flock'd state.json that is the queue's authority — the shadow bead
// is a projection of it, so an operator looking for the item needs this path)
// and, when the live leg was skipped rather than tried, WHY.
func writeQueuedSessionNudgeResult(target nudgeTarget, mode nudgeDeliveryMode, jsonOutput bool, undelivered worker.NudgeUndeliveredReason, stdout, stderr io.Writer) int {
	if jsonOutput {
		return writeCLIJSONLineOrExit(stdout, stderr, "gc session nudge", sessionNudgeJSON{
			SchemaVersion: "1",
			OK:            true,
			Target:        target.agentKey(),
			SessionID:     target.sessionID,
			SessionName:   target.sessionName,
			Delivery:      string(mode),
			Queued:        true,
			Outcome:       "queued",
		})
	}
	fmt.Fprintf(stdout, //nolint:errcheck // best-effort stdout
		"Queued nudge for %s in %s%s\n",
		target.agentKey(), nudgequeue.StatePath(target.cityPath), queuedNudgeDowngradeNote(target, undelivered))
	return 0
}

// queuedNudgeDowngradeNote renders the reason the live leg was skipped, or "" for
// a nudge that was queued by request. It names the provider for the unsupported
// case because that is the actionable half: the operator's next question is
// always "unsupported by what".
func queuedNudgeDowngradeNote(target nudgeTarget, undelivered worker.NudgeUndeliveredReason) string {
	switch undelivered {
	case worker.NudgeUndeliveredProviderUnsupported:
		provider := strings.TrimSpace(target.providerName())
		if provider == "" {
			provider = "this provider"
		}
		return fmt.Sprintf(" (live delivery is unsupported for %s; the queued dispatcher delivers it)", provider)
	case worker.NudgeUndeliveredNoIdleBoundary:
		return " (the session never reached an idle boundary; the queued dispatcher delivers it)"
	case worker.NudgeUndeliveredPendingInteraction:
		return " (the session is waiting on a permission prompt; the queued dispatcher delivers it after the prompt is answered)"
	case worker.NudgeUndeliveredHeld, worker.NudgeQueuedHeld:
		return " (the session is held by an operator or a wait; the nudge is delivered the next time the session runs, and expires after 24h)"
	default:
		return ""
	}
}

func sendMailNotify(target nudgeTarget, sender, messageID string) error {
	store, err := openNudgeBeadStoreErr(target.cityPath)
	if err != nil {
		return err
	}
	if store.Store == nil {
		return fmt.Errorf("opening the nudge store for %q returned no store", target.agentKey())
	}
	sp, err := newSessionProvider()
	if err != nil {
		return err
	}
	return sendMailNotifyWithWorker(target, store.Store, sp, sender, messageID)
}

// sendMailNotifyWithProvider is the store-less notify path (human sender,
// nil bead store). There is never a backing message store to address here,
// so unlike sendMailNotify it has no messageID to thread through — the
// resulting nudge carries no reference and is delivered unconditionally,
// same as before the #5321 fix.
func sendMailNotifyWithProvider(target nudgeTarget, sp runtime.Provider) error {
	return sendMailNotifyWithWorker(target, nil, sp, "human", "")
}

func sendMailNotifyWithWorker(target nudgeTarget, store beads.Store, sp runtime.Provider, sender, messageID string) error {
	msg := fmt.Sprintf("You have mail from %s", sender)
	now := time.Now()
	// Carry the mail message ID as the nudge's re-validation reference so
	// blockedQueuedNudgeReason can re-read the message at delivery time and
	// withdraw the nudge if it was already read or has since been archived
	// (see gastownhall/gascity#5321). messageID is empty for callers that
	// have no addressable message (e.g. sendMailNotifyWithProvider's
	// store-less human-sender path), in which case the nudge carries no
	// reference and is delivered unconditionally, same as before this fix.
	opts := queuedNudgeOptionsFromTarget(target)
	if messageID != "" {
		opts.Reference = &nudgeReference{Kind: "mail", ID: messageID}
	}
	// Session-class store for the observe/handle reads and the last-nudge stamp
	// below; the raw store keeps flowing to canRequestManagedNudgeWake,
	// enqueueManagedNudgeThenWake, and enqueueQueuedNudge (nudge class). nil store
	// -> nil (identity), preserving the store-less WithProvider caller.
	sessStore := cliSessionStore(store, target.cfg, target.cityPath)
	obs, err := workerObserveNudgeTarget(target, sessStore, sp)
	if err != nil {
		return err
	}
	if obs.Running {
		handle, err := workerHandleForNudgeTarget(target, sessStore, sp)
		if err == nil {
			result, nudgeErr := handle.Nudge(context.Background(), sessionActor(session.ActorAgent, target.cityPath), worker.NudgeRequest{
				Text:     msg,
				Delivery: worker.NudgeDeliveryWaitIdle,
				Source:   "mail",
				Wake:     worker.NudgeWakeLiveOnly,
			})
			delivered := nudgeErr == nil && result.Delivered
			// The submit Enter can be delivered and the composer drained with
			// only the busy-indicator OBSERVATION timing out. Delivery is
			// proven either way, so this must not fall through to the queue
			// path below and duplicate the mail notification.
			unobservedButDelivered := errors.Is(nudgeErr, tmux.ErrNudgeSubmitDeliveredUnobserved)
			if delivered || unobservedButDelivered {
				telemetry.RecordNudge(context.Background(), target.agentKey(), nil)
				var sessFront *session.Store
				if store != nil {
					sessFront = sessionFrontDoor(sessStore)
				}
				stampLastNudgeDeliveredAt(sessFront, target.sessionID, time.Now())
				return nil
			}
		}
	}
	if !obs.Running && canRequestManagedNudgeWake(target, store) {
		item := newQueuedNudgeWithOptions(target.agentKey(), msg, "mail", now, opts)
		outcome, err := enqueueManagedNudgeThenWake(target, store, item)
		if err != nil || outcome == session.WakeHeld {
			return err // a held session keeps the nudge queued; no poke
		}
		if err := nudgePokeController(target.cityPath, reconcilekey.Session(target.sessionID)); err != nil {
			if nudgeWarningWriter != nil {
				fmt.Fprintf(nudgeWarningWriter, "gc mail notify: warning: poke failed after managed wake: %v\n", err) //nolint:errcheck
			}
		}
		return nil
	}
	if err := enqueueQueuedNudge(target.cityPath, newQueuedNudgeWithOptions(target.agentKey(), msg, "mail", now, opts)); err != nil {
		return err
	}
	if obs.Running {
		maybeStartNudgePoller(target)
	}
	return nil
}

func resolveNudgeTarget(identifier string, warningWriter ...io.Writer) (nudgeTarget, error) {
	return resolveNudgeTargetPreferringHookIdentity(identifier, false, warningWriter...)
}

// resolveNudgeTargetPreferringHookIdentity is resolveNudgeTarget for a caller
// that may target the hook's own session. With preferHookIdentity, an open
// session bead named by GC_SESSION_ID is the target, resolved with one Get and
// without entering the named-session materialize path, and a closed one is
// refused (errHookSessionClosed) rather than replaced by whatever identifier
// resolves to now — a successor holding the alias. When GC_SESSION_ID is unset
// or stale, identifier resolves exactly as resolveNudgeTarget resolves it.
func resolveNudgeTargetPreferringHookIdentity(identifier string, preferHookIdentity bool, warningWriter ...io.Writer) (nudgeTarget, error) {
	cityPath, err := resolveCity()
	if err != nil {
		return nudgeTarget{}, err
	}
	cfg, err := loadCityConfig(cityPath, warningWriter...)
	if err != nil {
		return nudgeTarget{}, err
	}
	store := openNudgeBeadStore(cityPath)
	if store.Store != nil {
		// Named-session materialization is a session WRITE, and the follow-up Get
		// reads the session bead; both route through the session-class store
		// (identity today) while the raw store remains the nudge-queue currency.
		sessStore := cliSessionStore(store.Store, cfg, cityPath)
		if preferHookIdentity {
			identity, ok, err := resolveHookIdentity(sessStore)
			if err != nil {
				return nudgeTarget{}, err
			}
			if ok {
				if identity.info.Closed {
					return nudgeTarget{}, closedHookSessionError(identity.info.ID)
				}
				return resolveNudgeTargetFromSessionInfo(cityPath, cfg, identity.info), nil
			}
		}
		sessionID, err := resolveSessionIDMaterializingNamed(cityPath, cfg, sessStore, identifier)
		if err == nil {
			info, getErr := sessionFrontDoor(sessStore).Get(sessionID)
			if getErr != nil {
				return nudgeTarget{}, getErr
			}
			return resolveNudgeTargetFromSessionInfo(cityPath, cfg, info), nil
		}
		if !errors.Is(err, session.ErrSessionNotFound) {
			return nudgeTarget{}, err
		}
	}
	return nudgeTarget{}, fmt.Errorf("%w: %q", session.ErrSessionNotFound, identifier)
}

// nudgeTargetFields carries the pre-extracted session attributes buildNudgeTarget
// needs. resolveNudgeTargetFromSessionInfo populates it from a session.Info
// projection, so the identity-resolution tail lives in exactly one place.
type nudgeTargetFields struct {
	sessionID         string
	sessionName       string
	alias             string
	agentName         string
	template          string
	commonName        string
	aliasHistory      []string
	transport         string
	provider          string
	continuationEpoch string
}

// resolveNudgeTargetFromSessionInfo reads the session attributes buildNudgeTarget
// needs from a session.Info projection (the typed front door) rather than cracking
// the raw session bead. Note the transport source is i.TransportMetadata (the RAW
// value), not i.Transport (which normalizeTransport would make non-empty), so the
// found.Session fallback below fires identically.
func resolveNudgeTargetFromSessionInfo(cityPath string, cfg *config.City, i session.Info) nudgeTarget {
	sessionName := strings.TrimSpace(i.SessionNameMetadata)
	if sessionName == "" {
		sessionName = sessionNameFromBeadID(i.ID)
	}
	return buildNudgeTarget(cityPath, cfg, nudgeTargetFields{
		sessionID:         i.ID,
		sessionName:       sessionName,
		alias:             strings.TrimSpace(i.Alias),
		agentName:         strings.TrimSpace(i.AgentName),
		template:          strings.TrimSpace(i.Template),
		commonName:        strings.TrimSpace(i.CommonName),
		aliasHistory:      i.AliasHistory,
		transport:         strings.TrimSpace(i.TransportMetadata),
		provider:          strings.TrimSpace(i.Provider),
		continuationEpoch: strings.TrimSpace(i.ContinuationEpoch),
	})
}

func buildNudgeTarget(cityPath string, cfg *config.City, f nudgeTargetFields) nudgeTarget {
	cityName := loadedCityName(cfg, cityPath)
	identity := firstNonEmpty(f.agentName, f.template, f.commonName)
	target := nudgeTarget{
		cityPath:          cityPath,
		cityName:          cityName,
		cfg:               cfg,
		identity:          identity,
		alias:             f.alias,
		aliasHistory:      f.aliasHistory,
		transport:         f.transport,
		resolved:          &config.ResolvedProvider{Name: f.provider},
		sessionID:         f.sessionID,
		continuationEpoch: f.continuationEpoch,
		sessionName:       f.sessionName,
	}
	target.agent = parseNudgeAgentIdentity(identity)
	for _, candidate := range []string{f.agentName, f.template, f.commonName} {
		if candidate == "" {
			continue
		}
		found, ok := resolveAgentIdentity(cfg, candidate, "")
		if !ok {
			continue
		}
		target.agent = found
		target.identity = found.QualifiedName()
		if target.transport == "" {
			target.transport = found.Session
		}
		if resolved, err := config.ResolveProvider(&found, &cfg.Workspace, cfg.Providers, exec.LookPath); err == nil {
			if resolved.Name == "" {
				resolved.Name = fallbackProviderName(found.Provider, cfg)
				if resolved.Name == "" && target.resolved != nil {
					resolved.Name = target.resolved.Name
				}
			}
			target.resolved = resolved
		}
		break
	}
	if target.identity == "" {
		target.identity = target.agent.QualifiedName()
	}
	if target.identity == "" {
		target.identity = f.sessionName
	}
	return target
}

func parseNudgeAgentIdentity(identity string) config.Agent {
	dir, name := config.ParseQualifiedName(identity)
	return config.Agent{Dir: dir, Name: name}
}

func fallbackProviderName(agentProvider string, cfg *config.City) string {
	if agentProvider != "" {
		return agentProvider
	}
	if cfg != nil {
		return cfg.Workspace.Provider
	}
	return ""
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func parseNudgeDeliveryMode(raw string) (nudgeDeliveryMode, error) {
	switch nudgeDeliveryMode(raw) {
	case nudgeDeliveryImmediate, nudgeDeliveryWaitIdle, nudgeDeliveryQueue:
		return nudgeDeliveryMode(raw), nil
	default:
		return "", fmt.Errorf("unknown delivery mode %q (want immediate, wait-idle, or queue)", raw)
	}
}

func tryDeliverQueuedNudgesByPoller(target nudgeTarget, store, sessStore beads.Store, sp runtime.Provider, quiescence time.Duration, obs worker.LiveObservation) (bool, error) {
	matches, err := nudgeTargetLiveGenerationMatches(target, obs, sp)
	if err != nil || !matches {
		return false, err
	}
	// An operator's hold keeps queued delivery off the session too (CONTRACT
	// v5.9 D8 7(a)), as a kill fence does in the dispatcher.
	if sessStore != nil && target.sessionID != "" {
		if info, err := sessionFrontDoor(sessStore).Get(target.sessionID); err == nil && session.HoldVerdictInfo(info, obs.Running, time.Now()) {
			return false, nil
		}
	}
	if !pollerSessionIdleEnough(target, sp, quiescence, obs) {
		return false, nil
	}
	items, err := claimDueQueuedNudgesForTarget(target.cityPath, target, time.Now())
	if err != nil || len(items) == 0 {
		return false, err
	}
	deliveryStore := store
	if deliveryStore == nil {
		deliveryStore = openNudgeBeadStore(target.cityPath).Store
	}
	// sessStore is the SESSION-class store the caller resolved from the WORK store
	// (the dispatcher threads cr.sessionsBeadStore().Store; the CLI poll derives it
	// from its work store), so the worker handle, the front-door stamp, and the
	// gc:wait-bead split do NOT fall back to the nudges store when nudges relocates
	// independently of sessions. deliveryStore stays the NUDGES store for the queue
	// record/dead-letter path. When a caller passes nil sessStore (the store-less
	// test corner), fall back to the pre-split derivation so the handle
	// (nil -> runtime-only) and the stamp/split (from the fallback-opened store) stay
	// byte-identical. Identity today.
	handleSessStore := sessStore
	deliverySessStore := sessStore
	if sessStore == nil {
		handleSessStore = cliSessionStore(store, target.cfg, target.cityPath)
		deliverySessStore = cliSessionStore(deliveryStore, target.cfg, target.cityPath)
	}
	var deliverySessFront *session.Store
	var deliveryMailProvider mail.Provider
	if deliveryStore != nil {
		deliverySessFront = sessionFrontDoor(deliverySessStore)
		// Same split for the mail gate: the message re-read in
		// splitQueuedNudgesForDelivery is messaging-class (cliMailStore), while the
		// provider's session-addressing half reuses the session store resolved above.
		deliveryMailProvider = newMailProviderWithSessionStore(cliMailStore(deliveryStore, target.cfg, target.cityPath).Store, deliverySessStore)
	}
	// Bookkeeping for fence-mismatched and blocked items is best-effort: a
	// failure there must not abort delivery of the remaining claimable items.
	// Items whose bookkeeping failed stay in-flight; lease expiry returns
	// them to pending and a later pass retries.
	var bookkeepErr error
	items, rejected := splitQueuedNudgesForTarget(target, items)
	if len(rejected) > 0 {
		if recErr := recordQueuedNudgeFailureWithStore(target.cityPath, beads.NudgesStore{Store: deliveryStore}, queuedNudgeIDs(rejected), errNudgeSessionFenceMismatch, time.Now()); recErr != nil {
			bookkeepErr = fmt.Errorf("dead-lettering fence-mismatched nudges: %w", recErr)
		}
	}
	candidates := items
	items, blocked, err := splitQueuedNudgesForDelivery(sessionFrontDoor(deliverySessStore), deliveryMailProvider, candidates)
	if err != nil {
		relErr := releaseQueuedNudgeClaims(target.cityPath, queuedNudgeIDs(candidates))
		return false, errors.Join(bookkeepErr, err, relErr)
	}
	if len(blocked) > 0 {
		if termErr := terminalizeBlockedQueuedNudges(target.cityPath, blocked); termErr != nil {
			bookkeepErr = errors.Join(bookkeepErr, fmt.Errorf("withdrawing blocked nudges: %w", termErr))
		}
	}
	var staleMail []queuedNudge
	items, staleMail, err = splitStaleMailQueuedNudges(target, deliveryMailProvider, deliverySessStore, items)
	if err != nil && nudgeWarningWriter != nil {
		fmt.Fprintf(nudgeWarningWriter, "gc nudge: warning: checking mail staleness for %s: %v\n", target.agentKey(), err) //nolint:errcheck
	}
	if len(staleMail) > 0 {
		if termErr := terminalizeBlockedQueuedNudges(target.cityPath, map[string][]queuedNudge{"mail-already-read": staleMail}); termErr != nil {
			bookkeepErr = errors.Join(bookkeepErr, fmt.Errorf("withdrawing stale mail nudges: %w", termErr))
		}
	}
	if len(items) == 0 {
		return false, bookkeepErr
	}
	var msg string
	if target.sessionTransport() == "acp" {
		msg = formatNudgeRuntimeMessage(items)
	} else {
		msg = formatNudgeInjectOutput(items)
	}
	handle, err := workerHandleForNudgeTarget(target, handleSessStore, sp)
	if err != nil {
		relErr := releaseQueuedNudgeClaims(target.cityPath, queuedNudgeIDs(items))
		return false, errors.Join(bookkeepErr, err, relErr)
	}
	result, err := handle.Nudge(context.Background(), sessionActor(session.ActorAgent, target.cityPath), worker.NudgeRequest{
		Text:     msg,
		Delivery: worker.NudgeDeliveryDefault,
		Source:   "queue",
		Wake:     worker.NudgeWakeLiveOnly,
	})
	if err != nil {
		telemetry.RecordNudge(context.Background(), target.agentKey(), err)
		if retryAt, released := queuedNudgeReleaseAfter(err, time.Now()); released {
			if recErr := releaseQueuedNudgeClaimsUntil(target.cityPath, queuedNudgeIDs(items), retryAt); recErr != nil {
				return false, errors.Join(bookkeepErr, recErr)
			}
			return false, bookkeepErr
		}
		if errors.Is(err, tmux.ErrNudgeSubmitDeliveredUnobserved) {
			// The submit Enter was delivered and the composer drained; only the
			// busy-indicator OBSERVATION timed out. Delivery is proven, so this
			// must ack like a success, not run through failedQueuedNudge's
			// attempt-counting/dead-letter path — that would re-inject the same
			// reminder on the next pass.
			stampLastNudgeDeliveredAt(deliverySessFront, target.sessionID, time.Now())
			ackErr := ackQueuedNudgesWithOutcome(target.cityPath, queuedNudgeIDs(items), "injected_unobserved", "", "provider-nudge-return")
			return true, errors.Join(bookkeepErr, ackErr)
		}
		if recErr := recordQueuedNudgeFailureWithStore(target.cityPath, beads.NudgesStore{Store: deliveryStore}, queuedNudgeIDs(items), err, time.Now()); recErr != nil {
			return false, errors.Join(bookkeepErr, recErr)
		}
		return false, bookkeepErr
	}
	if !result.Delivered {
		// The runtime declined without an error (e.g. the session stopped
		// between observation and delivery). Release the claims so the next
		// pass retries promptly instead of waiting out the in-flight lease.
		relErr := releaseQueuedNudgeClaims(target.cityPath, queuedNudgeIDs(items))
		return false, errors.Join(bookkeepErr, relErr)
	}
	telemetry.RecordNudge(context.Background(), target.agentKey(), nil)
	stampLastNudgeDeliveredAt(deliverySessFront, target.sessionID, time.Now())
	return true, errors.Join(bookkeepErr, ackQueuedNudges(target.cityPath, queuedNudgeIDs(items)))
}

func stampLastNudgeDeliveredAt(sessFront *session.Store, sessionID string, t time.Time) {
	if sessFront == nil || sessionID == "" {
		return
	}
	// Best-effort stamp. Delivery already succeeded, so a metadata write
	// failure here must not bubble back to the caller and force a redelivery.
	_ = sessFront.SetMarker(sessionID, session.MetadataLastNudgeDeliveredAt, t.UTC().Format(time.RFC3339))
}

func pollerSessionIdleEnough(target nudgeTarget, sp runtime.Provider, quiescence time.Duration, obs worker.LiveObservation) bool {
	if quiescence <= 0 {
		return true
	}
	if obs.LastActivity != nil && !obs.LastActivity.IsZero() {
		return time.Since(*obs.LastActivity) >= quiescence
	}
	if pollerCanDeliverWithoutActivitySignal(target, sp) {
		return true
	}
	if target.sessionName == "" {
		return false
	}
	waiter, ok := sp.(runtime.IdleWaitProvider)
	if !ok {
		return false
	}
	// The poller may take up to the quiescence window to exit while this
	// runtime idle check is in progress.
	ctx, cancel := context.WithTimeout(context.Background(), quiescence)
	defer cancel()
	return waiter.WaitForIdle(ctx, target.sessionName, quiescence) == nil
}

func pollerCanDeliverWithoutActivitySignal(target nudgeTarget, sp runtime.Provider) bool {
	if sp == nil || target.sessionName == "" {
		return false
	}
	if sp.Capabilities().CanReportActivity {
		return false
	}
	sleeper, ok := sp.(runtime.SleepCapabilityProvider)
	if !ok {
		return false
	}
	return sleeper.SleepCapability(target.sessionName) == runtime.SessionSleepCapabilityTimedOnly
}

func maybeStartNudgePoller(target nudgeTarget) {
	if target.sessionName == "" {
		return
	}
	// Reap stale poller PID files before deciding whether to spawn. Owning
	// processes only remove their PID file via the release closure, so any
	// poller that is killed/crashes/os.Exit's leaves the .pid behind forever.
	// This low-frequency hook keeps the pollers directory from growing without
	// bound. The sibling .pid.lock is intentionally left in place (removing it
	// races concurrent acquirers — see reapStaleNudgePoller). Best-effort:
	// never block a spawn.
	_ = reapStaleNudgePollers(target.cityPath)
	// Supervisor-hosted dispatcher owns delivery in supervisor mode; the
	// per-session poller would race with it and reintroduce the bd-shellout
	// load it was designed to eliminate.
	if nudgeDispatcherIsSupervisor(target.cfg) {
		return
	}
	// ACP session/prompt delivery requires the process that owns the
	// in-memory ACP connection. A sidecar `gc nudge poll` process can
	// observe the control socket but cannot safely deliver prompts. In
	// legacy mode this leaves warm-idle ACP delivery to hook-drain only;
	// configure daemon.nudge_dispatcher = "supervisor" for dispatcher-owned
	// queued delivery.
	if target.sessionTransport() == "acp" {
		return
	}
	if err := startNudgePoller(target.cityPath, target.pollerKey(), target.sessionName); err != nil {
		return
	}
}

func withNudgeTargetFence(store beads.Store, target nudgeTarget) nudgeTarget {
	if target.sessionName == "" {
		return target
	}
	if target.sessionID != "" && target.continuationEpoch != "" {
		return target
	}
	if store == nil {
		return target
	}
	open, err := loadOpenSessionInfos(store)
	if err != nil {
		return target
	}
	for _, info := range open {
		if info.SessionNameMetadata != target.sessionName {
			continue
		}
		if target.sessionID == "" {
			target.sessionID = info.ID
		}
		if target.continuationEpoch == "" {
			target.continuationEpoch = info.ContinuationEpoch
		}
		return target
	}
	return target
}

// withNudgeTargetFenceFromLiveSession is withNudgeTargetFence for a caller
// that has already read the target's live session: when that session carries
// the target's session name it is the open session the fence would find, so
// the fence is taken from it without listing the city's sessions.
func withNudgeTargetFenceFromLiveSession(store beads.Store, live *session.Info, target nudgeTarget) nudgeTarget {
	if live == nil || live.Closed || target.sessionName == "" || live.SessionNameMetadata != target.sessionName {
		return withNudgeTargetFence(store, target)
	}
	if target.sessionID == "" {
		target.sessionID = live.ID
	}
	if target.continuationEpoch == "" {
		target.continuationEpoch = live.ContinuationEpoch
	}
	return target
}

var startNudgePoller = ensureNudgePoller

// nudgeDispatcherIsSupervisor reports whether the city is configured to use
// the supervisor-hosted nudge dispatcher rather than per-session pollers.
// A nil cfg defaults to legacy mode, matching DaemonConfig.NudgeDispatcherMode.
func nudgeDispatcherIsSupervisor(cfg *config.City) bool {
	if cfg == nil {
		return false
	}
	return cfg.Daemon.NudgeDispatcherMode() == "supervisor"
}

// splitQueuedNudgesForTarget partitions claimed nudges into deliverable items
// and rejects. Items whose fence (SessionID / ContinuationEpoch) does not match
// the current target's fence are re-fenced in memory to the target's fence and
// returned as deliverable when the target itself is resolved to a live session
// bead. Claim already admitted the item: either the SessionID still matches
// (stale-epoch Leg A) or the fenced session is no longer a live occupant
// (session-replacement Leg B; see queuedNudgeClaimableForTarget). A stale
// fence then just marks a prior incarnation of the same seat. Delivering it
// there is what the seat's nudges were queued for; dead-lettering the whole
// queue every time a session respawns (fresh wake after a failed spawn bumps
// ContinuationEpoch; a re-materialization can mint a new SessionID) is the
// bug this coalesces around (ga-bow, thunderfartcity:gp-u63s, #5816).
//
// A target with no resolved session identity (sessionID == "") cannot be
// re-fenced against and still rejects, matching the pre-existing invariant
// that fenced items are not delivered to an unresolved target.
func splitQueuedNudgesForTarget(target nudgeTarget, items []queuedNudge) ([]queuedNudge, []queuedNudge) {
	if len(items) == 0 {
		return nil, nil
	}
	var deliverable []queuedNudge
	var rejected []queuedNudge
	for _, item := range items {
		if queuedNudgeMatchesTargetFence(target, item) {
			deliverable = append(deliverable, item)
			continue
		}
		if target.sessionID == "" {
			rejected = append(rejected, item)
			continue
		}
		item.SessionID = target.sessionID
		item.ContinuationEpoch = target.continuationEpoch
		deliverable = append(deliverable, item)
	}
	return deliverable, rejected
}

// splitQueuedNudgesForDelivery partitions claimed nudges into deliverable items
// and reason-tagged blocked items. The sessFront param is the session
// coordination-class write front door: blockedQueuedNudgeReason reads the
// referenced gc:wait bead (coordclass.ClassSessions) to gate wait-sourced
// nudges. Callers construct it at the root over the session-class store (via
// cliSessionStore) so a [beads.classes.sessions] relocation reaches it. mp is
// the mail provider used the same way to gate mail-sourced nudges; it may be
// nil, which behaves as if no mail nudge ever carried a reference (delivered
// unconditionally, matching pre-#5321 behavior).
func splitQueuedNudgesForDelivery(sessFront *session.Store, mp mail.Provider, items []queuedNudge) ([]queuedNudge, map[string][]queuedNudge, error) {
	if len(items) == 0 {
		return nil, nil, nil
	}
	deliverable := make([]queuedNudge, 0, len(items))
	blocked := make(map[string][]queuedNudge)
	for _, item := range items {
		reason, shouldBlock, err := blockedQueuedNudgeReason(sessFront, mp, item)
		if err != nil {
			return nil, nil, err
		}
		if shouldBlock {
			blocked[reason] = append(blocked[reason], item)
			continue
		}
		deliverable = append(deliverable, item)
	}
	return deliverable, blocked, nil
}

// blockedQueuedNudgeReason re-validates a claimed nudge against the current
// state of the thing it announces, at delivery time. It's the per-source
// dispatch table: each queued-nudge source that carries a re-checkable
// reference gets its own gate below, so an item whose referent has since been
// resolved (a wait that fired, a mail message already read) is withdrawn
// instead of waking the target for stale news. See gastownhall/gascity#5321.
func blockedQueuedNudgeReason(sessFront *session.Store, mp mail.Provider, item queuedNudge) (string, bool, error) {
	switch item.Source {
	case "wait":
		return blockedQueuedWaitNudgeReason(sessFront, item)
	case "mail":
		return blockedQueuedMailNudgeReason(mp, item)
	default:
		return "", false, nil
	}
}

func blockedQueuedWaitNudgeReason(sessFront *session.Store, item queuedNudge) (string, bool, error) {
	if !sessFront.Backed() || item.Reference == nil || item.Reference.Kind != "bead" || item.Reference.ID == "" {
		return "", false, nil
	}
	wait, err := sessFront.GetWait(item.Reference.ID)
	if err != nil {
		if errors.Is(err, beads.ErrNotFound) {
			return "wait-missing", true, nil
		}
		if errors.Is(err, session.ErrNotAWait) {
			return "wait-reference-invalid", true, nil
		}
		return "", false, err
	}
	switch wait.State {
	case waitStateReady:
		return "", false, nil
	case waitStateCanceled:
		return "wait-canceled", true, nil
	case waitStateClosed:
		return "wait-closed", true, nil
	case waitStateExpired:
		return "wait-expired", true, nil
	case waitStateFailed:
		return "wait-failed", true, nil
	default:
		return "wait-not-ready", true, nil
	}
}

// blockedQueuedMailNudgeReason re-reads the mail message a queued mail nudge
// announces. A message that has vanished (e.g. archived — gastownhall/gascity#4422
// deletes the underlying bead) is withdrawn as "mail-missing" rather than
// treated as an error, mirroring the wait path's not-found handling: the
// obvious predicate "is this still unread" would otherwise never fire for an
// archived message, since it's neither read nor unread. A message that has
// been read since the nudge was queued is withdrawn as "mail-already-read",
// which is #5321's primary target case.
func blockedQueuedMailNudgeReason(mp mail.Provider, item queuedNudge) (string, bool, error) {
	if mp == nil || item.Reference == nil || item.Reference.Kind != "mail" || item.Reference.ID == "" {
		return "", false, nil
	}
	msg, err := mp.Get(item.Reference.ID)
	if err != nil {
		if errors.Is(err, mail.ErrNotFound) {
			return "mail-missing", true, nil
		}
		return "", false, err
	}
	if msg.Read {
		return "mail-already-read", true, nil
	}
	return "", false, nil
}

func terminalizeBlockedQueuedNudges(cityPath string, blocked map[string][]queuedNudge) error {
	for reason, items := range blocked {
		if err := ackQueuedNudgesWithOutcome(cityPath, queuedNudgeIDs(items), "failed", reason, "delivery-withdrawn"); err != nil {
			return err
		}
	}
	return nil
}

// targetHasUnreadMail reports whether target's own mailbox still has at
// least one unread message. It re-verifies unread state at nudge-delivery
// time for a deferred "you have mail" reminder: ga-vtxkj5 found the queue
// replaying that reminder long after the mail had already been read, because
// nothing re-checked unread state between enqueue and delivery.
//
// The check is deliberately coarse: it looks at aggregate unread count for
// the target, not the specific message that produced the reminder, because
// queued mail nudges carry no message ID to check individually. Treating
// "still has unread mail" as the safe default (on ambiguity or error, report
// unread) preserves the TestSendMailNotifyQueuesIndependentRemindersForEachMail
// guarantee from PR #2968 that independent mail arrivals never get silently
// dropped -- this only withdraws a reminder when unread count is truly zero.
//
// mp must read the messaging-class store (cliMailStore), not the nudges
// store, so unread mail is visible when mail is routed separately. A nil mp
// counts as unread, so the reminder is kept.
func targetHasUnreadMail(target nudgeTarget, mp mail.Provider, sessStore beads.Store) (bool, error) {
	if mp == nil || target.sessionID == "" {
		return true, nil
	}
	resolved, err := resolveMailTargetsWithConfig(target.cityPath, target.cfg, sessStore, target.sessionID)
	if err != nil {
		return true, err
	}
	messages, err := collectMailMessages(mp.Check, resolved.recipients)
	if err != nil {
		return true, err
	}
	return len(messages) > 0, nil
}

// splitStaleMailQueuedNudges partitions items into ones that should still be
// delivered (keep) and mail-sourced ones whose target has since read all
// mail and so must be withdrawn instead of replayed as a stale reminder
// (stale). Non-mail items always pass through into keep untouched.
func splitStaleMailQueuedNudges(target nudgeTarget, mp mail.Provider, sessStore beads.Store, items []queuedNudge) (keep, stale []queuedNudge, err error) {
	hasMailItem := false
	for _, item := range items {
		if item.Source == "mail" {
			hasMailItem = true
			break
		}
	}
	if !hasMailItem {
		return items, nil, nil
	}
	unread, err := targetHasUnreadMail(target, mp, sessStore)
	if err != nil {
		return items, nil, err
	}
	if unread {
		return items, nil, nil
	}
	keep = make([]queuedNudge, 0, len(items))
	for _, item := range items {
		if item.Source == "mail" {
			stale = append(stale, item)
			continue
		}
		keep = append(keep, item)
	}
	return keep, stale, nil
}

func ensureNudgePoller(cityPath, agentName, sessionName string) error {
	pidPath := nudgePollerPIDPath(cityPath, sessionName, agentName)
	return withNudgePollerPIDLock(pidPath, func() error {
		if running, _ := existingPollerPID(pidPath, cityPath, sessionName, agentName); running {
			return nil
		}
		exe, err := os.Executable()
		if err != nil {
			return err
		}
		cmd := newNudgePollerCommand(exe, os.Environ(), cityPath, sessionName, agentName)
		if err := cmd.Start(); err != nil {
			return err
		}
		if err := writeNudgePollerPID(pidPath, cmd.Process.Pid); err != nil {
			_ = cmd.Process.Kill()
			_ = cmd.Process.Release()
			return err
		}
		return cmd.Process.Release()
	})
}

// newNudgePollerCommand builds the detached poller child over environ, without
// the spawning session's identity (nudgepoller.ChildEnv), so the poller
// never reads as that session's leaked process.
func newNudgePollerCommand(exe string, environ []string, cityPath, sessionName, agentName string) *exec.Cmd {
	cmd := exec.Command(exe, nudgepoller.CommandArgs(cityPath, sessionName, agentName)...)
	cmd.Env = nudgepoller.ChildEnv(environ)
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	disableProductMetricsForChild(cmd)
	return cmd
}

func formatNudgeInjectOutput(items []queuedNudge) string {
	var sb strings.Builder
	sb.WriteString("<system-reminder>\n")
	if len(items) == 1 {
		sb.WriteString("You have a deferred reminder that was queued until a safe boundary:\n\n")
	} else {
		fmt.Fprintf(&sb, "You have %d deferred reminders that were queued until a safe boundary:\n\n", len(items))
	}
	for _, item := range items {
		// Sanitize attacker-controllable fields before interpolating into
		// the <system-reminder> block — without this, a sender can inject
		// </system-reminder> sequences and break out of the reminder.
		// See gastownhall/gascity#2195.
		source := extmsg.SanitizeForSystemReminder(item.Source)
		message := extmsg.SanitizeForSystemReminder(item.Message)
		fmt.Fprintf(&sb, "- [%s] %s\n", source, message)
	}
	sb.WriteString("\nHandle them after this turn.\n")
	sb.WriteString("</system-reminder>\n")
	return sb.String()
}

func formatNudgeRuntimeMessage(items []queuedNudge) string {
	var sb strings.Builder
	sb.WriteString("Deferred reminders:\n")
	for _, item := range items {
		fmt.Fprintf(&sb, "- [%s] %s\n", item.Source, item.Message)
	}
	sb.WriteString("\nThese were queued until the session went idle.\n")
	return sb.String()
}

func formatDueTime(ts time.Time) string {
	if ts.IsZero() {
		return "now"
	}
	d := time.Until(ts).Round(time.Second)
	switch {
	case d <= 0:
		return "now"
	case d < time.Minute:
		return d.String()
	default:
		return ts.Format(time.RFC3339)
	}
}

func deadReason(item queuedNudge) string {
	if item.LastError != "" {
		return item.LastError
	}
	if !item.ExpiresAt.IsZero() && item.ExpiresAt.Before(time.Now()) {
		return "expired"
	}
	return "dead-letter"
}

func newQueuedNudge(agentName, message string, now time.Time) queuedNudge {
	return newQueuedNudgeWithOptions(agentName, message, "session", now, queuedNudgeOptions{})
}

func newQueuedNudgeWithOptions(agentName, message, source string, now time.Time, opts queuedNudgeOptions) queuedNudge {
	id := opts.ID
	if id == "" {
		id = newQueuedNudgeID()
	}
	return queuedNudge{
		ID:                id,
		Agent:             agentName,
		SessionID:         opts.SessionID,
		ContinuationEpoch: opts.ContinuationEpoch,
		Source:            source,
		Message:           message,
		Reference:         opts.Reference,
		CreatedAt:         now.UTC(),
		DeliverAfter:      now.UTC(),
		ExpiresAt:         now.Add(defaultQueuedNudgeTTL).UTC(),
	}
}

func newQueuedNudgeID() string {
	var buf [6]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return fmt.Sprintf("nudge-%d", time.Now().UnixNano())
	}
	return "nudge-" + hex.EncodeToString(buf[:])
}

func queuedNudgeIDs(items []queuedNudge) []string {
	ids := make([]string, 0, len(items))
	for _, item := range items {
		ids = append(ids, item.ID)
	}
	return ids
}

func queuedNudgeMatchesTargetFence(target nudgeTarget, item queuedNudge) bool {
	if item.SessionID != "" && item.SessionID != target.sessionID {
		return false
	}
	if item.ContinuationEpoch != "" && item.ContinuationEpoch != target.continuationEpoch {
		return false
	}
	return true
}

// liveNudgeFenceSessionIDs is a test seam for the session-liveness census
// used by queuedNudgeClaimableForTarget. When non-nil, claim uses this
// instead of listing session beads. Tests that replace it must not use
// t.Parallel.
var liveNudgeFenceSessionIDs func(cityPath string) map[string]struct{}

func liveNudgeFenceSessionIDsForCity(cityPath string) map[string]struct{} {
	if liveNudgeFenceSessionIDs != nil {
		return liveNudgeFenceSessionIDs(cityPath)
	}
	return loadLiveNudgeFenceSessionIDsFromCity(cityPath)
}

// loadLiveNudgeFenceSessionIDsFromCity returns session IDs that still occupy
// a seat and therefore still own their fenced nudges. A nil result means the
// census was unavailable; callers must fail closed and leave mismatched
// SessionID items pending.
func loadLiveNudgeFenceSessionIDsFromCity(cityPath string) map[string]struct{} {
	store, opened := openOwnedNudgeBeadStore(cityPath)
	defer closeBeadStoreHandle(opened) //nolint:errcheck // best-effort
	if store.Store == nil {
		return nil
	}
	open, err := loadOpenSessionInfos(cliSessionStore(store.Store, nil, cityPath))
	if err != nil {
		return nil
	}
	live := make(map[string]struct{})
	for _, info := range open {
		if sessionHoldsNudgeFence(info) {
			live[info.ID] = struct{}{}
		}
	}
	return live
}

// sessionHoldsNudgeFence reports whether a session still occupies its seat
// for nudge-fence purposes. Drained, archived, failed-create, and closed
// sessions are superseded: their queued nudges may rebind to a successor.
// Asleep, draining, and quarantined sessions still own their fence so a
// concurrent sibling (or an in-flight drain) is not stolen.
func sessionHoldsNudgeFence(info session.Info) bool {
	if info.Closed || strings.TrimSpace(info.ID) == "" {
		return false
	}
	switch info.State {
	case session.StateDrained, session.StateArchived, session.StateFailedCreate:
		return false
	default:
		return true
	}
}

// queuedNudgeClaimableForTarget reports whether this target may claim item.
// liveSessions is a lazy census of session IDs that still hold a nudge fence;
// it is invoked only on the session-replacement path so the idle/exact-match
// claim tick does not list session beads.
//
// The two fence predicates agree this way:
//   - matching fence (same SessionID, including stale epoch) → claimable (Leg A)
//   - mismatched SessionID whose fenced session is no longer live, and whose
//     successor target is in the census → claimable (Leg B);
//     splitQueuedNudgesForTarget re-fences onto the successor
//   - mismatched SessionID that is still live → not claimable (sibling fence)
//   - census unavailable, empty, or missing the target → fail closed
func queuedNudgeClaimableForTarget(target nudgeTarget, item queuedNudge, liveSessions func() map[string]struct{}) bool {
	if !target.matchesQueueAgent(item.Agent) {
		return false
	}
	if item.SessionID != "" {
		if target.sessionID == "" {
			return false
		}
		if item.SessionID == target.sessionID {
			return true
		}
		if liveSessions == nil {
			return false
		}
		live := liveSessions()
		if live == nil {
			return false
		}
		if _, targetLive := live[target.sessionID]; !targetLive {
			// Census did not observe this target (empty city, store
			// unavailable, or the successor bead is not listed). Fail
			// closed rather than treating "no live sessions" as a
			// license to steal sibling fences.
			return false
		}
		_, stillLive := live[item.SessionID]
		return !stillLive
	}
	if item.ContinuationEpoch != "" && target.continuationEpoch == "" {
		return false
	}
	return true
}

// nudgeMaintenanceStore opens the nudge front door only after a queue commit
// has leased shadow debt for payment. It closes exactly the handle it opened;
// a caller-supplied store remains borrowed and is never closed here.
type nudgeMaintenanceStore struct {
	cityPath string
	opened   bool
	store    beads.NudgesStore
	// handle is what ensureOpen actually opened, which is not always
	// store.Store: see openOwnedNudgeBeadStore. close releases this and
	// nothing else.
	handle beads.Store
}

// ensureOpen opens the underlying store exactly once and returns it.
func (m *nudgeMaintenanceStore) ensureOpen() beads.NudgesStore {
	if !m.opened {
		m.opened = true
		m.store, m.handle = openOwnedNudgeBeadStore(m.cityPath)
	}
	return m.store
}

// frontDoor opens the shadow store lazily for a payer running after the queue
// transaction. The returned wrapper is used only outside the flock callback.
func (m *nudgeMaintenanceStore) frontDoor() *nudgequeue.Store {
	store := m.ensureOpen()
	if store.Store == nil {
		return nil
	}
	return nudgeFrontDoor(store)
}

// close releases the handle this frame opened (if any). It never touches a
// caller-passed store because this type only ever opens its own, and never the
// storage routes' shared engine, which it may hold but does not own.
func (m *nudgeMaintenanceStore) close() error {
	if !m.opened {
		return nil
	}
	return closeBeadStoreHandle(m.handle)
}

// nudgeQueueHasWork reports whether the queue holds deliverable or dead-letter
// work. Retired items are never served; their due shadow debts are paid by the
// maintenance sweep independently of this poller liveness predicate.
func nudgeQueueHasWork(state *nudgeQueueState) bool {
	return len(state.Pending) > 0 || len(state.InFlight) > 0 || len(state.Dead) > 0
}

// nudgeQueueIsEmpty reports whether the city's persisted nudge queue holds no
// pending, in-flight, or dead item. It reads the atomically replaced state file
// without taking the queue lock; an unreadable queue is not empty.
func nudgeQueueIsEmpty(cityPath string) bool {
	state, err := nudgequeue.LoadState(cityPath)
	return err == nil && !nudgeQueueHasWork(&state)
}

// nudgeDrainExplicitCityPath returns the city resolveCity would pick when the
// explicit city environment alone decides it: no --city/--rig flag and no
// remote selector outranks GC_CITY/GC_CITY_PATH/GC_CITY_ROOT. Unlike
// resolveCity it does not load the city config to name a rig.
func nudgeDrainExplicitCityPath() (string, bool) {
	if cityFlag != "" || rigFlag != "" || readRemoteSelection().hasExplicitRemote() {
		return "", false
	}
	return resolveExplicitCityPathEnv()
}

func claimDueQueuedNudgesForTarget(cityPath string, target nudgeTarget, now time.Time) ([]queuedNudge, error) {
	eligible, err := claimableNudgeSnapshot(cityPath, target, now)
	if err != nil || len(eligible) == 0 {
		return nil, err
	}
	if nudgeClaimAfterSnapshot != nil {
		nudgeClaimAfterSnapshot()
	}
	return claimDueQueuedNudgesMatching(cityPath, now, func(item queuedNudge) bool {
		return eligible[nudgeClaimIdentity(item)]
	})
}

// nudgeClaimAfterSnapshot lets the regression test insert work between the
// census-bearing snapshot and the locked claim. Production leaves it nil.
// Tests that set it must stay serial.
var nudgeClaimAfterSnapshot func()

func claimableNudgeSnapshot(cityPath string, target nudgeTarget, now time.Time) (map[queuedNudgeClaimIdentity]bool, error) {
	return claimableNudgeSnapshotWithCensus(cityPath, target, now, func() map[string]struct{} {
		return liveNudgeFenceSessionIDsForCity(cityPath)
	})
}

func claimableNudgeSnapshotWithCensus(cityPath string, target nudgeTarget, now time.Time, readCensus func() map[string]struct{}) (map[queuedNudgeClaimIdentity]bool, error) {
	// Census and candidate selection happen against one lock-free queue
	// snapshot. The locked claim may take only identities judged here; an item
	// that appears after this snapshot has no census decision and stays queued.
	snapshot, err := nudgequeue.LoadState(cityPath)
	if err != nil {
		return nil, err
	}
	liveLoaded := false
	var live map[string]struct{}
	eligible := make(map[queuedNudgeClaimIdentity]bool)
	consider := func(item queuedNudge) {
		if !item.ExpiresAt.IsZero() && !item.ExpiresAt.After(now) {
			return
		}
		if !target.matchesQueueAgent(item.Agent) {
			return
		}
		if item.DeliverAfter.After(now) {
			return
		}
		if item.SessionID != "" && item.SessionID != target.sessionID {
			if target.sessionID == "" {
				return
			}
			if !liveLoaded {
				if readCensus != nil {
					live = readCensus()
				}
				liveLoaded = true
			}
			if live == nil {
				return
			}
			if _, targetLive := live[target.sessionID]; !targetLive {
				return
			}
			if _, oldLive := live[item.SessionID]; oldLive {
				return
			}
		}
		if item.ContinuationEpoch != "" && target.continuationEpoch == "" {
			return
		}
		eligible[nudgeClaimIdentity(item)] = true
	}
	for _, item := range snapshot.Pending {
		consider(item)
	}
	for _, item := range snapshot.InFlight {
		if item.LeaseUntil.IsZero() || !item.LeaseUntil.After(now) {
			consider(item)
		}
	}
	if len(eligible) == 0 {
		return eligible, nil
	}
	return eligible, nil
}

type queuedNudgeClaimIdentity struct {
	id                string
	createdAt         int64
	agent             string
	sessionID         string
	continuationEpoch string
}

func nudgeClaimIdentity(item queuedNudge) queuedNudgeClaimIdentity {
	return queuedNudgeClaimIdentity{
		id:                item.ID,
		createdAt:         item.CreatedAt.UnixNano(),
		agent:             item.Agent,
		sessionID:         item.SessionID,
		continuationEpoch: item.ContinuationEpoch,
	}
}

func claimDueQueuedNudgesMatching(cityPath string, now time.Time, match func(queuedNudge) bool) ([]queuedNudge, error) {
	var claimed []queuedNudge
	err := runNudgeQueueMutationAt(cityPath, "claim", beads.NudgesStore{}, true, now, func(state *nudgeQueueState) error {
		claimed = claimed[:0]
		var front *nudgequeue.Store
		deadline := now.Add(nudgeEnqueueMaintenanceBudget)
		if err := recoverExpiredInFlightNudges(state, front, now, deadline); err != nil {
			return err
		}
		if err := pruneExpiredQueuedNudges(state, front, now, deadline); err != nil {
			return err
		}
		if err := pruneDeadQueuedNudges(state, front, now, deadline); err != nil {
			return err
		}
		pending := state.Pending[:0]
		for _, item := range state.Pending {
			if !match(item) {
				pending = append(pending, item)
				continue
			}
			if !item.DeliverAfter.IsZero() && item.DeliverAfter.After(now) {
				pending = append(pending, item)
				continue
			}
			item.ClaimedAt = now.UTC()
			item.LeaseUntil = now.Add(defaultQueuedNudgeClaimTTL).UTC()
			state.InFlight = append(state.InFlight, item)
			claimed = append(claimed, item)
		}
		state.Pending = pending
		sortQueuedNudges(state)
		return nil
	})
	return claimed, err
}

func listQueuedNudges(cityPath, agentName string, now time.Time) ([]queuedNudge, []queuedNudge, []queuedNudge, error) {
	var pending []queuedNudge
	var inFlight []queuedNudge
	var dead []queuedNudge
	err := runNudgeQueueMutationAt(cityPath, "list", beads.NudgesStore{}, true, now, func(state *nudgeQueueState) error {
		pending = nil
		inFlight = nil
		dead = nil
		var front *nudgequeue.Store
		deadline := now.Add(nudgeEnqueueMaintenanceBudget)
		if err := recoverExpiredInFlightNudges(state, front, now, deadline); err != nil {
			return err
		}
		if err := pruneExpiredQueuedNudges(state, front, now, deadline); err != nil {
			return err
		}
		if err := pruneDeadQueuedNudges(state, front, now, deadline); err != nil {
			return err
		}
		for _, item := range state.Pending {
			if item.Agent == agentName {
				pending = append(pending, item)
			}
		}
		for _, item := range state.InFlight {
			if item.Agent == agentName {
				inFlight = append(inFlight, item)
			}
		}
		for _, item := range state.Dead {
			if item.Agent == agentName {
				dead = append(dead, item)
			}
		}
		return nil
	})
	return pending, inFlight, dead, err
}

// listQueuedNudgesForTargetSnapshot returns target's queue buckets from a
// lock-free read of the persisted state (ga-2kzci3 FR5, ported from the
// unmerged ga-cn8dkj fix for the sibling cmdNudgeStatus regression). It is
// the read-only peer of listQueuedNudgesForTarget: it runs no maintenance
// pass, opens no bead store, takes no flock, and never writes.
//
// A liveness check like shouldKeepNudgePollerAlive must not wait on the
// queue's writer lock. Reading without the lock is safe because WithState
// rewrites state.json atomically (temp file + rename), so a reader always
// observes one whole snapshot, never a torn one.
//
// The bucketing below mirrors the in-memory effect of the recover/prune
// passes so the result matches what a maintaining caller would report,
// while mutating nothing. Dead-letter retention pruning is deliberately NOT
// mirrored: it depends on the bead store, and treating a not-yet-swept dead
// letter as still dead is harmless for a liveness check.
func listQueuedNudgesForTargetSnapshot(cityPath string, target nudgeTarget, now time.Time) ([]queuedNudge, []queuedNudge, []queuedNudge, error) {
	state, err := nudgequeue.LoadState(cityPath)
	if err != nil {
		return nil, nil, nil, err
	}
	var pending, inFlight, dead []queuedNudge
	add := func(bucket *[]queuedNudge, item queuedNudge) {
		if target.matchesQueueAgent(item.Agent) {
			*bucket = append(*bucket, item)
		}
	}
	expired := func(item queuedNudge) bool {
		return !item.ExpiresAt.IsZero() && !item.ExpiresAt.After(now)
	}
	// Mirrors recoverExpiredInFlightNudges: expired in-flight items read as
	// dead, and lease-expired ones read as pending again.
	for _, item := range state.InFlight {
		switch {
		case expired(item):
			if item.LastError == "" {
				item.LastError = "expired"
			}
			add(&dead, item)
		case item.LeaseUntil.IsZero() || !item.LeaseUntil.After(now):
			add(&pending, item)
		default:
			add(&inFlight, item)
		}
	}
	// Mirrors pruneExpiredQueuedNudges: expired pending items read as dead.
	for _, item := range state.Pending {
		if expired(item) {
			if item.LastError == "" {
				item.LastError = "expired"
			}
			add(&dead, item)
			continue
		}
		add(&pending, item)
	}
	for _, item := range state.Dead {
		add(&dead, item)
	}
	return pending, inFlight, dead, nil
}

func listQueuedNudgesForTarget(cityPath string, target nudgeTarget, now time.Time) ([]queuedNudge, []queuedNudge, []queuedNudge, error) {
	return listQueuedNudgesForTargetWithClock(cityPath, target, now, clock.Real{})
}

// listQueuedNudgesForTargetWithClock is the clock-injectable core of
// listQueuedNudgesForTarget (mirrors enqueueQueuedNudgeWithStoreAndClock's
// seam). Tests can pass a clock.Fake so the maintenance pass's
// now.Add(nudgeEnqueueMaintenanceBudget) deadline check is evaluated against
// a controlled clock instead of real wall-clock time, which otherwise races
// against this pass's own file I/O under -race.
func listQueuedNudgesForTargetWithClock(cityPath string, target nudgeTarget, now time.Time, clk clock.Clock) ([]queuedNudge, []queuedNudge, []queuedNudge, error) {
	var pending []queuedNudge
	var inFlight []queuedNudge
	var dead []queuedNudge
	err := runNudgeQueueMutationAt(cityPath, "list_target", beads.NudgesStore{}, true, now, func(state *nudgeQueueState) error {
		pending = nil
		inFlight = nil
		dead = nil
		var front *nudgequeue.Store
		deadline := now.Add(nudgeEnqueueMaintenanceBudget)
		if err := recoverExpiredInFlightNudgesWithClock(state, front, now, deadline, clk); err != nil {
			return err
		}
		if err := pruneExpiredQueuedNudgesWithClock(state, front, now, deadline, clk); err != nil {
			return err
		}
		if err := pruneDeadQueuedNudgesWithClock(state, front, now, deadline, clk); err != nil {
			return err
		}
		for _, item := range state.Pending {
			if target.matchesQueueAgent(item.Agent) {
				pending = append(pending, item)
			}
		}
		for _, item := range state.InFlight {
			if target.matchesQueueAgent(item.Agent) {
				inFlight = append(inFlight, item)
			}
		}
		for _, item := range state.Dead {
			if target.matchesQueueAgent(item.Agent) {
				dead = append(dead, item)
			}
		}
		return nil
	})
	return pending, inFlight, dead, err
}

func enqueueQueuedNudge(cityPath string, item queuedNudge) error {
	return enqueueQueuedNudgeWithStore(cityPath, beads.NudgesStore{}, item)
}

func rollbackQueuedNudge(cityPath string, store beads.NudgesStore, item queuedNudge, reason string) error {
	if cityPath == "" || item.ID == "" {
		return nil
	}
	now := time.Now()
	found := false
	err := retryNudgeBookkeeping(func() error {
		return withNudgeQueueStateUsingStore(cityPath, "rollback", store, func(state *nudgeQueueState) error {
			removed := []queuedNudge(nil)
			state.Pending, removed = takeQueuedNudgesByID(state.Pending, item.ID, removed)
			state.InFlight, removed = takeQueuedNudgesByID(state.InFlight, item.ID, removed)
			for _, queued := range removed {
				found = true
				queued.LastError = reason
				queued.DeadAt = now.UTC()
				setQueuedNudgeShadowDebt(&queued, "failed", reason, now)
				state.Dead = append(state.Dead, queued)
			}
			sortQueuedNudges(state)
			return nil
		})
	})
	if err != nil {
		return err
	}
	if found {
		return nil
	}
	item.LastError = reason
	return markQueuedNudgeTerminal(store, item, "failed", reason, "", now)
}

func takeQueuedNudgesByID(items []queuedNudge, id string, removed []queuedNudge) ([]queuedNudge, []queuedNudge) {
	if len(items) == 0 {
		return items, removed
	}
	filtered := make([]queuedNudge, 0, len(items))
	for _, item := range items {
		if item.ID == id {
			removed = append(removed, item)
			continue
		}
		filtered = append(filtered, item)
	}
	return filtered, removed
}

func enqueueQueuedNudgeWithStore(cityPath string, store beads.NudgesStore, item queuedNudge) error {
	return enqueueQueuedNudgeWithStoreAndClock(cityPath, store, item, clock.Real{})
}

func enqueueQueuedNudgeWithStoreAndClock(cityPath string, store beads.NudgesStore, item queuedNudge, clk clock.Clock) error {
	ownStore := false
	var opened beads.Store
	if store.Store == nil {
		store, opened = openOwnedNudgeBeadStore(cityPath)
		ownStore = true
	}
	if ownStore {
		defer closeBeadStoreHandle(opened) //nolint:errcheck // best-effort
	}
	var front *nudgequeue.Store
	if store.Store != nil {
		front = nudgeFrontDoor(store)
	}
	// Maintain and dedupe before allocating a private shadow. The final
	// admission repeats this under flock because another producer can win
	// while the new shadow is being created outside the lock.
	prepare := func(state *nudgeQueueState, now, deadline time.Time) error {
		if err := recoverExpiredInFlightNudgesWithClock(state, front, now, deadline, clk); err != nil {
			return err
		}
		if err := pruneExpiredQueuedNudgesWithClock(state, front, now, deadline, clk); err != nil {
			return err
		}
		if err := pruneDeadQueuedNudgesWithClock(state, front, now, deadline, clk); err != nil {
			return err
		}
		sortQueuedNudges(state)
		return nil
	}
	duplicate := false
	// Preflight commits only pure maintenance. Leave its debts unleased so
	// the final mutation pays one bounded batch for the entire enqueue.
	if err := nudgequeue.WithStateWithin(cityPath, "enqueue_prepare", nudgeQueueLockWait(), func(state *nudgeQueueState) error {
		now := clk.Now()
		deadline := clk.Now().Add(nudgeEnqueueMaintenanceBudget)
		if err := prepare(state, now, deadline); err != nil {
			return err
		}
		duplicate = queuedNudgeExists(state, item.ID)
		return nil
	}); err != nil {
		return err
	}
	if duplicate {
		if err := withNudgeQueueStateUsingStore(cityPath, "enqueue_dedupe", store, func(*nudgeQueueState) error { return nil }); err != nil {
			return err
		}
		pingNudgeWakeSocket(cityPath)
		return nil
	}
	beadID, created, err := nudgeFrontDoor(store).CreateShadow(item)
	if err != nil {
		return err
	}
	// Incoming shadow identities cannot be adopted by a fresh generation.
	item.BeadID = beadID
	admitted := false
	err = withNudgeQueueStateUsingStore(cityPath, "enqueue", store, func(state *nudgeQueueState) error {
		// Shadow creation and lock acquisition may have been slow. Admission
		// needs its own current time and bounded pure-maintenance window so
		// supersession is not skipped solely because preflight's window elapsed.
		now := clk.Now()
		deadline := clk.Now().Add(nudgeEnqueueMaintenanceBudget)
		if err := prepare(state, now, deadline); err != nil {
			return err
		}
		if queuedNudgeExists(state, item.ID) {
			return nil
		}
		if err := dropRetiredDebtsOnShadow(state, item, clk.Now()); err != nil {
			return err
		}
		// Supersede pending and in-flight nudges for the same (agent, source, reference).
		if item.Reference != nil && item.Reference.ID != "" {
			matchesSupersession := func(existing queuedNudge) bool {
				return existing.Agent == item.Agent && existing.Source == item.Source &&
					existing.Reference != nil && existing.Reference.Kind == item.Reference.Kind &&
					existing.Reference.ID == item.Reference.ID
			}
			filtered := state.Pending[:0]
			for i, existing := range state.Pending {
				if clk.Now().After(deadline) {
					filtered = append(filtered, state.Pending[i:]...)
					break
				}
				if matchesSupersession(existing) {
					existing.DeadAt = now.UTC()
					existing.LastError = "superseded"
					setQueuedNudgeShadowDebt(&existing, "superseded", "superseded", now)
					state.Dead = append(state.Dead, existing)
					continue
				}
				filtered = append(filtered, existing)
			}
			state.Pending = filtered
			// Also supersede in-flight nudges. Note: an active delivery may
			// already be running for a superseded item. When it completes, its
			// ack/failure won't find the item in InFlight and will no-op.
			// This causes at most one redundant delivery, not data corruption.
			inFlight := state.InFlight[:0]
			for i, existing := range state.InFlight {
				if clk.Now().After(deadline) {
					inFlight = append(inFlight, state.InFlight[i:]...)
					break
				}
				if matchesSupersession(existing) {
					existing.DeadAt = now.UTC()
					existing.LastError = "superseded"
					setQueuedNudgeShadowDebt(&existing, "superseded", "superseded", now)
					state.Dead = append(state.Dead, existing)
					continue
				}
				inFlight = append(inFlight, existing)
			}
			state.InFlight = inFlight
		}
		admitted = true
		state.Pending = append(state.Pending, item)
		sortQueuedNudges(state)
		return nil
	})
	// Each attempt owns a fresh bead identity. A failed or deduplicated
	// admission cannot close a shadow used by the winning generation.
	if (err != nil || !admitted) && created && store.Store != nil && beadID != "" {
		// Roll back the leaked shadow bead through the nudge front door, which
		// stamps the canonical close_reason before Close so BdStore.Close can
		// forward it as `bd close --reason` and satisfy validation.on-close=error.
		// Preserve the original enqueue error, but return rollback failures too
		// so leaked open nudge beads are diagnosable.
		if rbErr := nudgeFrontDoor(store).RollbackEnqueue(beadID); rbErr != nil {
			err = errors.Join(err, fmt.Errorf("rollback nudge bead %q: %w", beadID, rbErr))
		}
	}
	if err == nil {
		// Best-effort wake of the supervisor's nudge dispatcher. Legacy-mode
		// cities and ad-hoc invocations (no listener) get a fast dial
		// failure and fall through to the per-session poller / patrol tick.
		pingNudgeWakeSocket(cityPath)
	}
	return err
}

func ackQueuedNudges(cityPath string, ids []string) error {
	return ackQueuedNudgesWithOutcome(cityPath, ids, "injected", "", "provider-nudge-return")
}

func ackQueuedNudgesWithOutcome(cityPath string, ids []string, outcome, reason, commitBoundary string) error {
	if len(ids) == 0 {
		return nil
	}
	want := make(map[string]bool, len(ids))
	for _, id := range ids {
		want[id] = true
	}
	var ledger *nudgeShadowLedger
	commit := func(state *nudgeQueueState) error {
		now := time.Now()
		ledger = newNudgeShadowLedger(now)
		ledger.reset()
		deadline := now.Add(nudgeEnqueueMaintenanceBudget)
		if err := recoverExpiredInFlightNudges(state, nil, now, deadline); err != nil {
			return err
		}
		if err := pruneExpiredQueuedNudges(state, nil, now, deadline); err != nil {
			return err
		}
		if err := pruneDeadQueuedNudges(state, nil, now, deadline); err != nil {
			return err
		}
		var terminal []queuedNudge
		filtered := state.Pending[:0]
		for _, item := range state.Pending {
			if want[item.ID] {
				terminal = append(terminal, item)
				continue
			}
			filtered = append(filtered, item)
		}
		state.Pending = filtered
		inFlight := state.InFlight[:0]
		for _, item := range state.InFlight {
			if want[item.ID] {
				terminal = append(terminal, item)
				continue
			}
			inFlight = append(inFlight, item)
		}
		state.InFlight = inFlight
		for _, item := range terminal {
			item.DeadAt = now.UTC()
			ledger.owe(&item, outcome, reason, commitBoundary, now)
			if item.Shadow != nil {
				state.Retired = append(state.Retired, item)
			}
		}
		sortQueuedNudges(state)
		return nil
	}
	if err := retryNudgeBookkeeping(func() error { return updateNudgeQueueStateUsingStore(cityPath, "ack", beads.NudgesStore{}, commit) }); err != nil {
		return err
	}
	if ledger == nil || len(ledger.taken) == 0 {
		return nil
	}
	settleNudgeShadowDebts(cityPath, beads.NudgesStore{}, ledger.taken, nudgeEnqueueMaintenanceBudget, time.Now())
	return nil
}

// queuedNudgeReleaseAfter reports whether a delivery error leaves the nudges
// undelivered rather than failed, so their claims are released without
// spending one of their bounded attempts, and when a later pass may claim them
// again. A vanished session and a seat holding a pending interaction are
// refusals, not delivery failures: nothing was typed, and charging them would
// dead-letter a good nudge for arriving at the wrong moment. A session that
// went away may be back at once. The ACP provider refuses a nudge while it
// holds a permission request, and the next pass tries again. A permission
// prompt the runtime refused to type into (#2892) stays up until someone
// answers it, so claiming the nudges again on the very next pass would only
// be refused again.
func queuedNudgeReleaseAfter(err error, now time.Time) (time.Time, bool) {
	switch {
	// Before ErrPendingInteraction: the session layer wraps this refusal in
	// session.ErrPendingInteraction, which is that sentinel.
	case errors.Is(err, runtime.ErrNudgeRefusedPendingInteraction):
		return time.Time{}, true
	case errors.Is(err, runtime.ErrPendingInteraction):
		return now.Add(defaultQueuedNudgeRetryDelay), true
	case errors.Is(err, runtime.ErrSessionNotFound):
		return time.Time{}, true
	// The runtime refused before typing anything (the composer was not ready):
	// not a failed delivery attempt, so the same durable items stay eligible for
	// the next safe boundary.
	case errors.Is(err, tmux.ErrNudgeInputNotReady):
		return time.Time{}, true
	default:
		return time.Time{}, false
	}
}

func releaseQueuedNudgeClaims(cityPath string, ids []string) error {
	return releaseQueuedNudgeClaimsUntil(cityPath, ids, time.Time{})
}

// releaseQueuedNudgeClaimsUntil returns claimed nudges to pending. A non-zero
// deliverAfter keeps them from being claimed again before then.
func releaseQueuedNudgeClaimsUntil(cityPath string, ids []string, deliverAfter time.Time) error {
	if len(ids) == 0 {
		return nil
	}
	want := make(map[string]bool, len(ids))
	for _, id := range ids {
		want[id] = true
	}
	return retryNudgeBookkeeping(func() error {
		return updateNudgeQueueState(cityPath, "release", func(state *nudgeQueueState) error {
			now := time.Now()
			var front *nudgequeue.Store
			deadline := now.Add(nudgeEnqueueMaintenanceBudget)
			if err := recoverExpiredInFlightNudges(state, front, now, deadline); err != nil {
				return err
			}
			if err := pruneExpiredQueuedNudges(state, front, now, deadline); err != nil {
				return err
			}
			if err := pruneDeadQueuedNudges(state, front, now, deadline); err != nil {
				return err
			}
			var released []queuedNudge
			inFlight := state.InFlight[:0]
			for _, item := range state.InFlight {
				if !want[item.ID] {
					inFlight = append(inFlight, item)
					continue
				}
				item.ClaimedAt = time.Time{}
				item.LeaseUntil = time.Time{}
				if !deliverAfter.IsZero() {
					item.DeliverAfter = deliverAfter.UTC()
				}
				released = append(released, item)
			}
			state.InFlight = inFlight
			state.Pending = append(state.Pending, released...)
			sortQueuedNudges(state)
			return nil
		})
	})
}

func recordQueuedNudgeFailure(cityPath string, ids []string, cause error, now time.Time) error {
	return recordQueuedNudgeFailureWithStore(cityPath, beads.NudgesStore{}, ids, cause, now)
}

func recordQueuedNudgeFailureWithStore(cityPath string, store beads.NudgesStore, ids []string, cause error, now time.Time) error {
	_, err := recordQueuedNudgeFailureDetailed(cityPath, store, ids, cause, now)
	return err
}

// The dead-lettered slice reports which of the given IDs actually reached the
// dead letter queue in this pass, which is not the same set the caller asked
// for: an item delivered and acked since the caller last read the queue is
// simply gone by the time the write pass takes the lock. doNudgeDrop reports
// per-ID outcomes from this slice for exactly that reason;
// recordQueuedNudgeFailureWithStore discards it.
func recordQueuedNudgeFailureDetailed(cityPath string, store beads.NudgesStore, ids []string, cause error, now time.Time) ([]queuedNudge, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	ownStore := false
	var opened beads.Store
	if store.Store == nil {
		store, opened = openOwnedNudgeBeadStore(cityPath)
		ownStore = true
	}
	if ownStore {
		defer closeBeadStoreHandle(opened) //nolint:errcheck // best-effort
	}
	var front *nudgequeue.Store
	if store.Store != nil {
		front = nudgeFrontDoor(store)
	}
	want := make(map[string]bool, len(ids))
	for _, id := range ids {
		want[id] = true
	}
	var deadLettered []queuedNudge
	err := retryNudgeBookkeeping(func() error {
		return runNudgeQueueMutationAt(cityPath, "record_failure", store, true, now, func(state *nudgeQueueState) error {
			deadLettered = deadLettered[:0]
			deadline := now.Add(nudgeEnqueueMaintenanceBudget)
			if err := recoverExpiredInFlightNudges(state, front, now, deadline); err != nil {
				return err
			}
			if err := pruneExpiredQueuedNudges(state, front, now, deadline); err != nil {
				return err
			}
			if err := pruneDeadQueuedNudges(state, front, now, deadline); err != nil {
				return err
			}
			var requeued []queuedNudge
			var dead []queuedNudge
			pending := state.Pending[:0]
			for _, item := range state.Pending {
				if !want[item.ID] {
					pending = append(pending, item)
					continue
				}
				updated, deadLetter := failedQueuedNudge(item, cause, now)
				if deadLetter {
					setQueuedNudgeShadowDebt(&updated, terminalStateForDeadQueuedNudge(updated), updated.LastError, now)
					dead = append(dead, updated)
					deadLettered = append(deadLettered, updated)
					continue
				}
				requeued = append(requeued, updated)
			}
			state.Pending = pending
			inFlight := state.InFlight[:0]
			for _, item := range state.InFlight {
				if !want[item.ID] {
					inFlight = append(inFlight, item)
					continue
				}
				updated, deadLetter := failedQueuedNudge(item, cause, now)
				if deadLetter {
					setQueuedNudgeShadowDebt(&updated, terminalStateForDeadQueuedNudge(updated), updated.LastError, now)
					dead = append(dead, updated)
					deadLettered = append(deadLettered, updated)
					continue
				}
				requeued = append(requeued, updated)
			}
			state.InFlight = inFlight
			state.Pending = append(state.Pending, requeued...)
			state.Dead = append(state.Dead, dead...)
			sortQueuedNudges(state)
			return nil
		})
	})
	if err != nil {
		return nil, err
	}
	for _, item := range deadLettered {
		if warning := unconfirmedDropWarning(item); warning != "" && nudgeWarningWriter != nil {
			fmt.Fprintln(nudgeWarningWriter, warning) //nolint:errcheck // best-effort warning emission
		}
	}
	return deadLettered, nil
}

func failedQueuedNudge(item queuedNudge, cause error, now time.Time) (queuedNudge, bool) {
	item.Attempts++
	item.LastAttemptAt = now.UTC()
	item.LastError = cause.Error()
	item.ClaimedAt = time.Time{}
	item.LeaseUntil = time.Time{}
	if errors.Is(cause, errNudgeSessionFenceMismatch) {
		item.DeadAt = now.UTC()
		return item, true
	}
	// An unconfirmed submit is terminal, never retried. The keystrokes DID
	// reach tmux — the error says so — and only the busy-state confirmation
	// went unobserved, so re-sending can merely type the same text at the pane
	// a second time. It cannot recover a message the agent never got, because
	// the agent did get it. The two errors that genuinely warrant another pass
	// are handled before this point: runtime.ErrSessionNotFound releases the
	// claim, and a runtime that declines without an error leaves
	// result.Delivered false and releases too.
	//
	// Without this, deploying ErrNudgeSubmitUnconfirmed (upstream #5012) turned
	// every unconfirmable pane into defaultQueuedNudgeMaxAttempts copies of the
	// same reminder, delivered 15s apart to an agent that already had it —
	// including agents that were not idle, since a queued idle-mail reminder
	// carries a frozen snapshot of its own text. Measured live on gc2
	// 2026-08-06: four items at attempts 3-5, all bearing this error.
	// tmux/adapter.go:1324 already applies the same reasoning to the startup
	// nudge; the queued path was simply never given it.
	if errors.Is(cause, tmux.ErrNudgeSubmitUnconfirmed) ||
		errors.Is(cause, tmux.ErrNudgeSubmitDeliveredUnobserved) {
		item.DeadAt = now.UTC()
		return item, true
	}
	if item.Attempts >= defaultQueuedNudgeMaxAttempts || (!item.ExpiresAt.IsZero() && !item.ExpiresAt.After(now)) {
		item.DeadAt = now.UTC()
		return item, true
	}
	item.DeliverAfter = now.Add(defaultQueuedNudgeRetryDelay).UTC()
	return item, false
}

func terminalStateForDeadQueuedNudge(item queuedNudge) string {
	switch strings.TrimSpace(item.LastError) {
	case "expired":
		return "expired"
	case "superseded":
		return "superseded"
	default:
		return "failed"
	}
}

// noMaintenanceDeadline returns a deadline far enough in the future that a
// nudge-queue maintenance pass never stops early. Used by tests that
// exercise the recover/prune primitives' own full-drain behavior directly.
// Every production call site is bounded instead -- nudgeEnqueueMaintenanceBudget
// for per-call maintenance (claim, list, ack, release, record-failure),
// nudgeMaintenanceSweepBudget for the supervisor's whole-queue sweep -- per
// ga-2kzci3: an unconditional full-backlog drain on every call does
// unbounded O(backlog) work under the queue's exclusive flock, and the
// recover/prune passes already bail out cleanly on deadline, leaving the
// remainder for the next bounded pass to pick up.
func noMaintenanceDeadline() time.Time {
	return time.Now().Add(24 * time.Hour)
}

// recordNudgeDispatchSkips merges a dispatch tick's per-reason skip counts
// into the queue state's running totals. Called at most once per tick, after
// the per-session loop finishes, so it never nests inside the loop's own
// withNudgeQueueState-backed calls (claimDueQueuedNudgesForTarget et al) —
// the queue lock is a per-process flock and is not reentrant.
func recordNudgeDispatchSkips(cityPath string, counts map[string]int64) error {
	if len(counts) == 0 {
		return nil
	}
	return updateNudgeQueueState(cityPath, "dispatch_skips", func(state *nudgeQueueState) error {
		if state.DispatchSkips == nil {
			state.DispatchSkips = make(map[string]int64, len(counts))
		}
		for reason, n := range counts {
			state.DispatchSkips[reason] += n
		}
		return nil
	})
}

// nudgeMaintenanceSweepBudget bounds how long a single
// runNudgeQueueMaintenanceSweep pass is allowed to consider itself current
// for, relative to the caller's own now (ga-2kzci3 FR3). Unlike
// noMaintenanceDeadline (real-wall-clock + 24h, used by the latency-
// insensitive callers that must always drain the full backlog), this
// deadline is derived from the passed-in now so a stale now -- one whose
// budget has already elapsed relative to the real clock -- causes the
// per-item bail check in recoverExpiredInFlightNudgesWithClock /
// pruneExpiredQueuedNudgesWithClock / pruneDeadQueuedNudgesWithClock to stop
// before mutating anything, rather than silently draining the whole backlog
// against a wall-clock deadline that ignores how stale now actually is.
// Comfortably above a realistic single-pass sweep duration and comfortably
// below the staleness a caller is expected to catch.
const nudgeMaintenanceSweepBudget = 5 * time.Minute

// nudgeMaintenanceDebounceWindow bounds how close together two
// runNudgeQueueMaintenanceSweep calls for the same city can be, in terms of
// their own now, before the second is treated as a redundant same-tick
// sweep and skipped (ga-2kzci3 FR4). The sweep has one production caller,
// dispatchAllQueuedNudges (nudge_dispatcher.go), which runs it once per
// dispatch pass, before the per-session loop -- so the bursts come from how
// often a pass fires, not from the session count. nudgeDispatchTick fires
// on every wake-socket signal plus once at the end of each patrol tick, so
// a busy city can invoke this many times in quick succession for the same
// queue.
const nudgeMaintenanceDebounceWindow = time.Second

var (
	nudgeMaintenanceDebounceMu  sync.Mutex
	nudgeMaintenanceLastSweepAt = map[string]time.Time{}
)

// nudgeMaintenanceSweepDebounced reports whether a sweep for cityPath at now
// falls within nudgeMaintenanceDebounceWindow of the now of the last sweep
// that actually ran for that city, and if not, records now as that marker.
// A debounced call leaves the marker untouched.
func nudgeMaintenanceSweepDebounced(cityPath string, now time.Time) bool {
	nudgeMaintenanceDebounceMu.Lock()
	defer nudgeMaintenanceDebounceMu.Unlock()
	if last, ok := nudgeMaintenanceLastSweepAt[cityPath]; ok {
		elapsed := now.Sub(last)
		if elapsed < 0 {
			elapsed = -elapsed
		}
		if elapsed < nudgeMaintenanceDebounceWindow {
			return true
		}
	}
	nudgeMaintenanceLastSweepAt[cityPath] = now
	return false
}

// runNudgeQueueMaintenanceSweep runs the queue's recover/TTL-expiry/dead-
// letter-retention passes over the whole queue, independent of whether any
// pending item currently matches an open session. Every other maintenance
// call site (claimDueQueuedNudgesMatching, listQueuedNudges,
// listQueuedNudgesForTarget, ...) only runs these passes as a side effect of
// an operation scoped to a specific agent/target, so an item whose target
// never matches never gets swept by any of them. The supervisor dispatch
// tick is the one path that iterates the whole queue every cycle regardless
// of match outcome, so it owns running this sweep unconditionally.
func runNudgeQueueMaintenanceSweep(cityPath string, now time.Time) error {
	return runNudgeQueueMaintenanceSweepWithStore(cityPath, beads.NudgesStore{}, now)
}

// runNudgeQueueMaintenanceSweepWithStore borrows the resolved nudges store.
// A nil store retains the lazy opener path; only a handle opened here is closed.
func runNudgeQueueMaintenanceSweepWithStore(cityPath string, store beads.NudgesStore, now time.Time) error {
	if nudgeMaintenanceSweepDebounced(cityPath, now) {
		return nil
	}
	return runNudgeQueueMutationAt(cityPath, "maintenance", store, true, now, func(state *nudgeQueueState) error {
		var front *nudgequeue.Store
		deadline := now.Add(nudgeMaintenanceSweepBudget)
		if err := recoverExpiredInFlightNudges(state, front, now, deadline); err != nil {
			return err
		}
		if err := pruneExpiredQueuedNudges(state, front, now, deadline); err != nil {
			return err
		}
		return pruneDeadQueuedNudges(state, front, now, deadline)
	})
}

func pruneExpiredQueuedNudges(state *nudgeQueueState, front *nudgequeue.Store, now, deadline time.Time) error {
	return pruneExpiredQueuedNudgesWithClock(state, front, now, deadline, clock.Real{})
}

func pruneExpiredQueuedNudgesWithClock(state *nudgeQueueState, _ *nudgequeue.Store, now, deadline time.Time, clk clock.Clock) error {
	filtered := state.Pending[:0]
	for i, item := range state.Pending {
		if clk.Now().After(deadline) {
			filtered = append(filtered, state.Pending[i:]...)
			break
		}
		if !item.ExpiresAt.IsZero() && !item.ExpiresAt.After(now) {
			item.DeadAt = now.UTC()
			if item.LastError == "" {
				item.LastError = "expired"
			}
			setQueuedNudgeShadowDebt(&item, "expired", item.LastError, now)
			state.Dead = append(state.Dead, item)
			continue
		}
		filtered = append(filtered, item)
	}
	state.Pending = filtered
	sortQueuedNudges(state)
	return nil
}

func recoverExpiredInFlightNudges(state *nudgeQueueState, front *nudgequeue.Store, now, deadline time.Time) error {
	return recoverExpiredInFlightNudgesWithClock(state, front, now, deadline, clock.Real{})
}

func recoverExpiredInFlightNudgesWithClock(state *nudgeQueueState, _ *nudgequeue.Store, now, deadline time.Time, clk clock.Clock) error {
	filtered := state.InFlight[:0]
	for i, item := range state.InFlight {
		if clk.Now().After(deadline) {
			filtered = append(filtered, state.InFlight[i:]...)
			break
		}
		if !item.ExpiresAt.IsZero() && !item.ExpiresAt.After(now) {
			item.DeadAt = now.UTC()
			if item.LastError == "" {
				item.LastError = "expired"
			}
			setQueuedNudgeShadowDebt(&item, "expired", item.LastError, now)
			state.Dead = append(state.Dead, item)
			continue
		}
		if item.LeaseUntil.IsZero() || !item.LeaseUntil.After(now) {
			item.ClaimedAt = time.Time{}
			item.LeaseUntil = time.Time{}
			item.DeliverAfter = now.UTC()
			state.Pending = append(state.Pending, item)
			continue
		}
		filtered = append(filtered, item)
	}
	state.InFlight = filtered
	sortQueuedNudges(state)
	return nil
}

// pruneDeadQueuedNudges removes dead-letter items older than defaultQueuedNudgeDeadRetention.
// A shadow-backed item (non-empty BeadID) is pruned only once its terminal bead record is
// confirmed in the store; shadow-backed items without a confirmed terminal bead are retained
// so terminal history is not lost if the bead store write failed. A shadowless item has no
// record to confirm and is pruned on DeadAt alone. Items with a zero DeadAt are retained.
func pruneDeadQueuedNudges(state *nudgeQueueState, front *nudgequeue.Store, now, deadline time.Time) error {
	return pruneDeadQueuedNudgesWithClock(state, front, now, deadline, clock.Real{})
}

func pruneDeadQueuedNudgesWithClock(state *nudgeQueueState, _ *nudgequeue.Store, now, deadline time.Time, clk clock.Clock) error {
	cutoff := now.Add(-defaultQueuedNudgeDeadRetention)
	filtered := state.Dead[:0]
	for i, item := range state.Dead {
		if clk.Now().After(deadline) {
			filtered = append(filtered, state.Dead[i:]...)
			break
		}
		if queuedNudgeHasShadow(item) {
			// Old binaries persisted dead letters without a payment record. Read
			// the shadow only after releasing this flock; Verify makes that
			// payment conditional on the shadow not already being terminal.
			if item.Shadow == nil {
				reason := strings.TrimSpace(item.LastError)
				if reason == "" {
					reason = "failed"
				}
				terminalAt := item.DeadAt
				if terminalAt.IsZero() {
					terminalAt = now
				}
				item.Shadow = &nudgequeue.ShadowDebt{State: terminalStateForDeadQueuedNudge(item), Reason: reason, At: terminalAt.UTC(), Verify: true}
			}
			if !item.DeadAt.IsZero() && item.DeadAt.Before(cutoff) && item.Shadow.Paid {
				continue
			}
		} else if !item.DeadAt.IsZero() && item.DeadAt.Before(cutoff) {
			// No shadow to confirm: the queue entry is the only record, so
			// it ages out on DeadAt alone.
			continue
		}
		filtered = append(filtered, item)
	}
	state.Dead = filtered
	return nil
}

func setQueuedNudgeShadowDebt(item *queuedNudge, state, reason string, at time.Time) {
	if !queuedNudgeHasShadow(*item) {
		item.Shadow = nil
		return
	}
	item.Shadow = &nudgequeue.ShadowDebt{State: state, Reason: reason, At: at.UTC()}
}

func queuedNudgeExists(state *nudgeQueueState, id string) bool {
	for _, item := range state.Pending {
		if item.ID == id {
			return true
		}
	}
	for _, item := range state.InFlight {
		if item.ID == id {
			return true
		}
	}
	for _, item := range state.Dead {
		if item.ID == id {
			return true
		}
	}
	return false
}

func sortQueuedNudges(state *nudgeQueueState) {
	nudgequeue.SortState(state)
}

func withNudgeQueueState(cityPath string, fn func(*nudgeQueueState) error) error {
	return withNudgeQueueStateVerb(cityPath, "nudge", fn)
}

func withNudgeQueueStateVerb(cityPath, verb string, fn func(*nudgeQueueState) error) error {
	return withNudgeQueueStateUsingStore(cityPath, verb, beads.NudgesStore{}, fn)
}

// withNudgeQueueStateUsingStore keeps the queue callback pure. It leases any
// due shadow debts in the same state commit, pays them after unlocking, then
// confirms the payment with a second short hold. A caller-owned store is
// borrowed and never closed here.
func withNudgeQueueStateUsingStore(cityPath, verb string, store beads.NudgesStore, fn func(*nudgeQueueState) error) error {
	return runNudgeQueueMutation(cityPath, verb, store, false, fn)
}

func updateNudgeQueueStateUsingStore(cityPath, verb string, store beads.NudgesStore, fn func(*nudgeQueueState) error) error {
	return runNudgeQueueMutation(cityPath, verb, store, true, fn)
}

func runNudgeQueueMutation(cityPath, verb string, store beads.NudgesStore, updateIfChanged bool, fn func(*nudgeQueueState) error) error {
	return runNudgeQueueMutationAt(cityPath, verb, store, updateIfChanged, time.Now(), fn)
}

func runNudgeQueueMutationAt(cityPath, verb string, store beads.NudgesStore, updateIfChanged bool, now time.Time, fn func(*nudgeQueueState) error) error {
	ledger := newNudgeShadowLedger(now)
	callback := func(state *nudgeQueueState) error {
		ledger.reset()
		if realNow := time.Now(); realNow.After(ledger.now) {
			ledger.now = realNow
		}
		if err := fn(state); err != nil {
			return err
		}
		if verb == "maintenance" && time.Now().After(now.Add(nudgeMaintenanceSweepBudget)) {
			return nil
		}
		takeDueNudgeShadowDebts(state, ledger, nudgeShadowPayBatch)
		return nil
	}
	var err error
	if updateIfChanged {
		err = nudgequeue.UpdateIfChangedWithin(cityPath, verb, nudgeQueueLockWait(), callback)
	} else {
		err = nudgequeue.WithStateWithin(cityPath, verb, nudgeQueueLockWait(), callback)
	}
	if err != nil || len(ledger.taken) == 0 {
		return err
	}
	payBudget := nudgeEnqueueMaintenanceBudget
	if verb == "maintenance" {
		payBudget = nudgeControllerPayBudget
	}
	settleNudgeShadowDebts(cityPath, store, ledger.taken, payBudget, now)
	return nil // the queue commit is authoritative; maintenance retries unpaid debts
}

func updateNudgeQueueState(cityPath, verb string, fn func(*nudgeQueueState) error) error {
	return updateNudgeQueueStateUsingStore(cityPath, verb, beads.NudgesStore{}, fn)
}

func nudgePollerPIDPath(cityPath, sessionName, agentName string) string {
	return citylayout.RuntimePath(cityPath, "nudges", "pollers", nudgepoller.PollerFileStem(sessionName, agentName)+".pid")
}

// reapStaleNudgePollers removes orphaned nudge poller PID files left behind by
// pollers that were killed, crashed, or os.Exit'd without running their release
// closure. A *.pid file is stale when its contents are unparseable or its PID
// is no longer alive. The sibling .pid.lock file is deliberately NOT removed —
// it is the stable per-key flock mutex inode and removing it under the lock
// would race concurrent acquirers (see reapStaleNudgePoller). The work is done
// under the same per-file lock the lease path uses so a concurrently starting
// poller is never raced. It is best-effort: a missing pollers directory is a
// no-op and per-file errors are accumulated and returned without aborting the
// sweep.
func reapStaleNudgePollers(cityPath string) error {
	pollersDir := citylayout.RuntimePath(cityPath, "nudges", "pollers")
	entries, err := os.ReadDir(pollersDir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("reading nudge pollers dir: %w", err)
	}
	var errs []error
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".pid") {
			continue
		}
		pidPath := filepath.Join(pollersDir, entry.Name())
		if err := reapStaleNudgePoller(pidPath); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// reapStaleNudgePoller removes pidPath when the PID it names is unparseable or
// no longer alive. It takes the per-file lock so the liveness check and removal
// cannot race a poller acquiring the same lease, mirroring exactly what the
// lease release closure does (which is safe).
func reapStaleNudgePoller(pidPath string) error {
	return withNudgePollerPIDLock(pidPath, func() error {
		data, err := os.ReadFile(pidPath)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read nudge poller pid %q: %w", pidPath, err)
		}
		pidText := strings.TrimSpace(string(data))
		var pid int
		if n, parseErr := fmt.Sscanf(pidText, "%d", &pid); parseErr == nil && n == 1 && pidutil.Alive(pid) {
			return nil
		}
		// Remove ONLY the stale .pid. The sibling .pid.lock is intentionally
		// left in place: it is the stable per-key flock mutex inode. Calling
		// os.Remove on it while we hold LOCK_EX would let a third concurrent
		// acquirer create a brand-new lock inode and run its critical section
		// alongside a blocked acquirer that inherited the now-orphaned inode,
		// breaking mutual exclusion and double-spawning pollers — exactly what
		// the lease exists to prevent. Leave the lock inode permanently stable.
		if err := os.Remove(pidPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove stale nudge poller pid %q: %w", pidPath, err)
		}
		return nil
	})
}

var errNudgePollerRunning = errors.New("nudge poller already running")

func acquireNudgePollerLease(cityPath, sessionName, agentName string) (func(), error) {
	pidPath := nudgePollerPIDPath(cityPath, sessionName, agentName)
	if err := os.MkdirAll(filepath.Dir(pidPath), 0o755); err != nil {
		return nil, fmt.Errorf("creating nudge poller dir: %w", err)
	}
	pid := []byte(fmt.Sprintf("%d\n", os.Getpid()))
	release := func() {
		current, err := os.ReadFile(pidPath)
		if err != nil {
			return
		}
		if strings.TrimSpace(string(current)) == strings.TrimSpace(string(pid)) {
			_ = os.Remove(pidPath)
		}
	}
	err := withNudgePollerPIDLock(pidPath, func() error {
		current, err := os.ReadFile(pidPath)
		switch {
		case err == nil && strings.TrimSpace(string(current)) == strings.TrimSpace(string(pid)):
			return nil
		case err == nil:
			if running, _ := existingPollerPID(pidPath, cityPath, sessionName, agentName); running {
				return errNudgePollerRunning
			}
		case !errors.Is(err, os.ErrNotExist):
			return fmt.Errorf("read nudge poller pid: %w", err)
		}
		return writeNudgePollerPID(pidPath, os.Getpid())
	})
	if err != nil {
		return nil, err
	}
	return release, nil
}

func existingPollerPID(pidPath, cityPath, sessionName, agentName string) (bool, error) {
	data, err := os.ReadFile(pidPath)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	pidText := strings.TrimSpace(string(data))
	if pidText == "" {
		return false, nil
	}
	var pid int
	if _, err := fmt.Sscanf(pidText, "%d", &pid); err != nil || pid <= 0 {
		return false, nil
	}
	if cityPath == "" || sessionName == "" || agentName == "" {
		return false, nil
	}
	if pidutil.AliveWithCmdline(pid, nudgepoller.CmdlineMatcher(cityPath, sessionName, agentName)) {
		return true, nil
	}
	return false, nil
}

func writeNudgePollerPID(pidPath string, pid int) error {
	data := []byte(fmt.Sprintf("%d\n", pid))
	if err := fsys.WriteFileAtomic(fsys.OSFS{}, pidPath, data, 0o644); err != nil {
		return fmt.Errorf("write nudge poller pid: %w", err)
	}
	return nil
}

func withNudgePollerPIDLock(pidPath string, fn func() error) error {
	lockPath := pidPath + ".lock"
	if err := os.MkdirAll(filepath.Dir(pidPath), 0o755); err != nil {
		return fmt.Errorf("creating nudge poller dir: %w", err)
	}
	lockFile, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("opening nudge poller lock: %w", err)
	}
	defer lockFile.Close() //nolint:errcheck
	if err := syscall.Flock(int(lockFile.Fd()), syscall.LOCK_EX); err != nil {
		return fmt.Errorf("locking nudge poller: %w", err)
	}
	defer syscall.Flock(int(lockFile.Fd()), syscall.LOCK_UN) //nolint:errcheck
	return fn()
}

// unconfirmedDropWarning returns an operator-facing line when a queued nudge is
// dead-lettered because its submit could not be confirmed, and "" otherwise.
//
// e22aed815 made ErrNudgeSubmitUnconfirmed terminal so the queue stops re-typing
// a message the agent already received (the keystrokes reached tmux; only the
// busy-state confirmation went unobserved). That is right, but it made the
// outcome invisible: if "unconfirmed" ever means the Enter genuinely did not
// submit, the reminder is dropped and nothing says so. This is the counter an
// operator can watch — a rising number here is the signal that the terminal
// decision has started costing real deliveries, rather than an agent that
// mysteriously never answered.
//
// Deliberately silent for every other dead-letter cause. A fence mismatch or an
// exhausted retry budget is ordinary and already visible in `gc nudge status`;
// warning about those too would train operators to ignore the line that matters.
func unconfirmedDropWarning(item queuedNudge) string {
	if !strings.Contains(item.LastError, tmux.ErrNudgeSubmitUnconfirmed.Error()) {
		return ""
	}
	return fmt.Sprintf(
		"gc nudge: WARNING: dropped queued nudge %q for %q — submit was delivered to tmux but not confirmed, so it was not retried. "+
			"If the agent did not act on it, the message was lost (see gcw-os58).",
		item.ID, item.Agent)
}
