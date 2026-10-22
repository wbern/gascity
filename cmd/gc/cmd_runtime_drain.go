package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/reconcilekey"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/spf13/cobra"
)

// drainOps abstracts drain signal operations for testability.
type drainOps interface {
	setDrain(sessionName string) error
	clearDrain(sessionName string) error
	isDraining(sessionName string) (bool, error)
	drainStartTime(sessionName string) (time.Time, error)
	setDrainAck(sessionName string) error
	isDrainAcked(sessionName string) (bool, error)
	setRestartRequested(sessionName string) error
	isRestartRequested(sessionName string) (bool, error)
	clearRestartRequested(sessionName string) error
	setDriftRestart(sessionName string) error
	isDriftRestart(sessionName string) (bool, error)
	clearDriftRestart(sessionName string) error
}

const drainAdmissionAttemptKey = "GC_DRAIN_ADMISSION_ATTEMPT"

// providerDrainOps implements drainOps using runtime.Provider metadata.
type providerDrainOps struct {
	sp        runtime.Provider
	admission *drainAckAdmission
}

type drainAckAdmission struct {
	front   *session.Store
	id      string
	fence   session.ExecutionFence
	attempt string
}

// bindDrainAckAdmission captures this operation's census before an asynchronous
// provider setter can outlive cancellation and a replacement census.
func bindDrainAckAdmission(dops drainOps, cityPath, sessionName string) (drainOps, error) {
	provider, ok := dops.(*providerDrainOps)
	if !ok {
		return dops, nil
	}
	// Resolve from the runtime name, just as the release census does. The
	// no-argument command deliberately does not trust GC_SESSION_ID.
	if _, err := os.Stat(filepath.Join(cityPath, "city.toml")); err != nil {
		if os.IsNotExist(err) {
			return dops, nil
		}
		return nil, err
	}
	cfg, err := loadCityConfigWithoutBuiltinPackRefresh(cityPath, io.Discard)
	if err != nil {
		return nil, err
	}
	store, err := openCityStoreAtWithConfig(cityPath, cfg)
	if err != nil {
		return nil, err
	}
	sessStore := cliSessionStore(store, cfg, cityPath)
	live := beads.HandlesFor(sessStore).Live
	if live == nil {
		return nil, beads.ErrConditionalWriteUnsupported
	}
	sessionID, err := resolveSessionID(sessStore, sessionName)
	if err != nil {
		return nil, err
	}
	front := sessionFrontDoor(sessStore)
	current, cell, enrolled, err := front.ExecutionEvidence(sessionID)
	if err != nil {
		return nil, err
	}
	if !enrolled {
		return dops, nil
	}
	if current.SessionNameMetadata != sessionName {
		return nil, session.ErrExecutionIncarnationChanged
	}
	if cell.Drain == nil {
		return nil, session.ErrDrainAdmissionChanged
	}
	fence := session.ExecutionFence{InstanceToken: os.Getenv("GC_INSTANCE_TOKEN"), Generation: os.Getenv("GC_RUNTIME_EPOCH")}
	gate, err := front.InspectDrainAdmission(sessionID, fence, cell.Drain.Attempt)
	if err != nil {
		return nil, err
	}
	if !gate.Committed {
		return nil, session.ErrDrainAdmissionChanged
	}
	// Freeze the authoritative generation even when the caller omitted its
	// optional epoch hint; the published frame must never have an empty epoch.
	fence.Generation = current.Generation
	bound := *provider
	bound.admission = &drainAckAdmission{front: front, id: sessionID, fence: fence, attempt: gate.Attempt}
	return &bound, nil
}

type runtimeDrainCheckJSON struct {
	SchemaVersion string `json:"schema_version"`
	OK            bool   `json:"ok"`
	Command       string `json:"command"`
	Session       string `json:"session"`
	Target        string `json:"target,omitempty"`
	Draining      bool   `json:"draining"`
}

type runtimeActionJSON struct {
	SchemaVersion string `json:"schema_version"`
	OK            bool   `json:"ok"`
	Command       string `json:"command"`
	Action        string `json:"action"`
	Session       string `json:"session"`
	Target        string `json:"target,omitempty"`
	Status        string `json:"status"`
}

func (o *providerDrainOps) setDrain(sessionName string) error {
	return o.sp.SetMeta(sessionName, "GC_DRAIN", strconv.FormatInt(time.Now().Unix(), 10))
}

func (o *providerDrainOps) clearDrain(sessionName string) error {
	return errors.Join(
		o.sp.RemoveMeta(sessionName, "GC_DRAIN_ACK"),
		o.sp.RemoveMeta(sessionName, drainAdmissionAttemptKey),
		o.sp.RemoveMeta(sessionName, reconcilerDrainAckSourceKey),
		// The incarnation stamp has exactly the acknowledgement's lifetime. Left
		// behind, it outlives every drain it described and waits on the pane to be
		// paired with some later ack's source.
		o.sp.RemoveMeta(sessionName, drainAckRequesterInstanceTokenKey),
		o.sp.RemoveMeta(sessionName, reconcilerDrainAckReasonKey),
		o.sp.RemoveMeta(sessionName, reconcilerDrainAckGenerationKey),
		o.sp.RemoveMeta(sessionName, "GC_DRAIN"),
	)
}

func (o *providerDrainOps) isDraining(sessionName string) (bool, error) {
	val, err := o.sp.GetMeta(sessionName, "GC_DRAIN")
	if err != nil {
		return false, fmt.Errorf("reading GC_DRAIN: %w", err)
	}
	return val != "", nil
}

func (o *providerDrainOps) drainStartTime(sessionName string) (time.Time, error) {
	val, err := o.sp.GetMeta(sessionName, "GC_DRAIN")
	if err != nil {
		return time.Time{}, fmt.Errorf("reading GC_DRAIN: %w", err)
	}
	if val == "" {
		return time.Time{}, fmt.Errorf("GC_DRAIN not set")
	}
	unix, err := strconv.ParseInt(val, 10, 64)
	if err != nil {
		return time.Time{}, fmt.Errorf("parsing GC_DRAIN timestamp %q: %w", val, err)
	}
	return time.Unix(unix, 0), nil
}

func (o *providerDrainOps) setDrainAck(sessionName string) error {
	if o.admission != nil {
		a := o.admission
		gate, err := a.front.InspectDrainAdmission(a.id, a.fence, a.attempt)
		if err != nil {
			return err
		}
		if !gate.Committed {
			return session.ErrDrainAdmissionChanged
		}
		if err := o.sp.SetMeta(sessionName, drainAdmissionAttemptKey, a.attempt); err != nil {
			return err
		}
	} else if err := o.sp.RemoveMeta(sessionName, drainAdmissionAttemptKey); err != nil {
		return err
	}
	if o.admission == nil {
		if err := o.sp.RemoveMeta(sessionName, drainAckFrameKey); err != nil {
			return err
		}
	}
	// The acknowledging agent records which incarnation it was. Readers bind
	// against this rather than trusting a bare source value that a recycled
	// chair carries over from whoever sat there last.
	//
	// The stamp lands BEFORE the source, and that order is load-bearing. The one
	// reader of this pairing (drainReminderAckPin) admits an acknowledgement on
	// the source alone, then reads the stamp in a SECOND provider round-trip.
	// Source-first, a reader landing between the two writes sees this ack's fresh
	// source beside the PREVIOUS occupant's stamp and mints agentAckBindingStale
	// — positive proof of residue about an acknowledgement that landed
	// microseconds ago, which is the one verdict that must never be minted by
	// accident. Stamp-first, the only stamp state a source-keyed reader can
	// observe is this ack's own: its digest, or empty and therefore unprovable.
	requesterInstanceToken := drainAckRequesterInstanceToken(sessionName)
	err := joinDrainAckMutationErrors(
		o.sp.RemoveMeta(sessionName, reconcilerDrainAckReasonKey),
		o.sp.RemoveMeta(sessionName, reconcilerDrainAckGenerationKey),
		o.sp.SetMeta(sessionName, drainAckRequesterInstanceTokenKey, requesterInstanceToken),
		o.sp.SetMeta(sessionName, reconcilerDrainAckSourceKey, drainAckSourceAgentValue),
		o.sp.SetMeta(sessionName, "GC_DRAIN_ACK", "1"),
	)
	if err != nil {
		return err
	}
	if o.admission != nil {
		a := o.admission
		info := session.Info{ID: a.id, InstanceToken: a.fence.InstanceToken, Generation: a.fence.Generation, SessionNameMetadata: sessionName}
		if err := publishDrainAckFrame(o.sp, sessionName, info, drainAckSourceAgentValue, "", a.attempt); err != nil {
			return err
		}
		_, err := a.front.InspectDrainAdmission(a.id, a.fence, a.attempt)
		return err
	}
	return nil
}

// drainAckRequesterInstanceToken returns the binding stamp for the acking pane's
// own incarnation, and ONLY when this pane is the session being acked. `gc
// runtime drain-ack <other>` is a cross-session ack: the caller's token is
// evidence about the CALLER, not about the target, and stamping it on the
// target's row reads back as agentAckBindingStale — positive proof of residue
// for an acknowledgement that landed seconds ago. An empty stamp degrades to
// agentAckBindingUnprovable instead, which is the direction this reader is
// meant to fail in.
//
// The identity comparison is deliberately made against the pane's own
// environment rather than through currentSessionRuntimeTarget: that resolver
// also demands a city path, so it errors for reasons that have nothing to do
// with WHO is acking, and a legitimate self-ack would silently degrade to
// unprovable whenever the city context was unresolvable.
func drainAckRequesterInstanceToken(sessionName string) string {
	target := strings.TrimSpace(sessionName)
	self := strings.TrimSpace(os.Getenv("GC_TMUX_SESSION"))
	if self == "" {
		self = strings.TrimSpace(os.Getenv("GC_SESSION_NAME"))
	}
	if target == "" || self == "" || self != target {
		return ""
	}
	return drainAckInstanceTokenDigest(os.Getenv("GC_INSTANCE_TOKEN"))
}

// drainAckInstanceTokenDigest maps an incarnation token onto the value that is
// safe to leave on a pane. GC_INSTANCE_TOKEN is a capability rather than an
// identifier — it fences drain and stop against stale incarnations — and this
// codebase keeps that value class off every command line for it: envArgvSafe
// excludes the key, and session start stages it through a private 0600 file
// instead of argv. A provider SetMeta is an argv channel on tmux
// (`set-environment -t <name> <key> <value>`), whose argument vector is
// world-readable in /proc/<pid>/cmdline, so stamping the token itself would
// reopen that exposure on every self-ack.
//
// The binding only ever needs an equality compare, which a digest answers
// without carrying the capability: equal tokens digest equal, and the digest
// grants nothing. Empty stays empty, so a degraded pane that has no token stamps
// nothing rather than the digest of "" — the unprovable arm is unchanged.
func drainAckInstanceTokenDigest(token string) string {
	token = strings.TrimSpace(token)
	if token == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func (o *providerDrainOps) isDrainAcked(sessionName string) (bool, error) {
	val, err := o.sp.GetMeta(sessionName, "GC_DRAIN_ACK")
	if err != nil {
		return false, fmt.Errorf("reading GC_DRAIN_ACK: %w", err)
	}
	return val == "1", nil
}

func (o *providerDrainOps) setRestartRequested(sessionName string) error {
	return o.sp.SetMeta(sessionName, "GC_RESTART_REQUESTED", strconv.FormatInt(time.Now().Unix(), 10))
}

func (o *providerDrainOps) isRestartRequested(sessionName string) (bool, error) {
	val, err := o.sp.GetMeta(sessionName, "GC_RESTART_REQUESTED")
	if err != nil {
		if runtime.IsSessionGone(err) {
			return false, nil
		}
		return false, fmt.Errorf("reading GC_RESTART_REQUESTED: %w", err)
	}
	return val != "", nil
}

func (o *providerDrainOps) clearRestartRequested(sessionName string) error {
	err := o.sp.RemoveMeta(sessionName, "GC_RESTART_REQUESTED")
	if runtime.IsSessionGone(err) {
		return nil
	}
	return err
}

func (o *providerDrainOps) setDriftRestart(sessionName string) error {
	return o.sp.SetMeta(sessionName, "GC_DRIFT_RESTART", "1")
}

func (o *providerDrainOps) isDriftRestart(sessionName string) (bool, error) {
	val, err := o.sp.GetMeta(sessionName, "GC_DRIFT_RESTART")
	if err != nil {
		return false, fmt.Errorf("reading GC_DRIFT_RESTART: %w", err)
	}
	return val == "1", nil
}

func (o *providerDrainOps) clearDriftRestart(sessionName string) error {
	return o.sp.RemoveMeta(sessionName, "GC_DRIFT_RESTART")
}

func joinDrainAckMutationErrors(errs ...error) error {
	var joined []error
	for _, err := range errs {
		if err == nil || drainAckMissingSessionBeadError(err) {
			continue
		}
		joined = append(joined, err)
	}
	return errors.Join(joined...)
}

func drainAckMissingSessionBeadError(err error) bool {
	return runtime.IsSessionGone(err) || errors.Is(err, beads.ErrNotFound)
}

// newDrainOps creates a drainOps from a runtime.Provider.
func newDrainOps(sp runtime.Provider) drainOps {
	return &providerDrainOps{sp: sp}
}

// ---------------------------------------------------------------------------
// gc runtime drain <name>
// ---------------------------------------------------------------------------

func newRuntimeDrainCmd(stdout, stderr io.Writer) *cobra.Command {
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   "drain <name>",
		Short: "Signal a session to drain (wind down gracefully)",
		Long: `Signal a session to drain — wind down its current work gracefully.

Sets a GC_DRAIN metadata flag on the session. The agent should check
for drain status periodically (via "gc runtime drain-check") and finish
its current task before exiting. Pass a session alias or ID. Use
"gc runtime undrain" to cancel.`,
		Args: cobra.ArbitraryArgs,
		RunE: func(_ *cobra.Command, args []string) error {
			if cmdRuntimeDrain(args, jsonOutput, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Output as JSON")
	return cmd
}

func cmdRuntimeDrain(args []string, jsonOutput bool, stdout, stderr io.Writer) int {
	if len(args) < 1 {
		fmt.Fprintln(stderr, "gc runtime drain: missing session alias or ID") //nolint:errcheck // best-effort stderr
		return 1
	}
	target, err := resolveSessionRuntimeTarget(args[0], stderr)
	if err != nil {
		fmt.Fprintf(stderr, "gc runtime drain: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	sp, err := newSessionProvider()
	if err != nil {
		fmt.Fprintf(stderr, "gc runtime drain: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	dops := newDrainOps(sp)
	rec := openCityRecorder(stderr)
	return doRuntimeDrain(dops, sp, rec, target.display, target.sessionName, jsonOutput, stdout, stderr)
}

// doRuntimeDrain sets the drain signal on a session.
func doRuntimeDrain(dops drainOps, sp runtime.Provider, rec events.Recorder,
	targetName, sn string, jsonOutput bool, stdout, stderr io.Writer,
) int {
	running, err := workerSessionTargetRunningWithConfig("", nil, sp, nil, sn)
	if err != nil {
		fmt.Fprintf(stderr, "gc runtime drain: observing %q: %v\n", targetName, err) //nolint:errcheck // best-effort stderr
		return 1
	}
	if !running {
		fmt.Fprintf(stderr, "gc runtime drain: session %q is not running\n", targetName) //nolint:errcheck // best-effort stderr
		return 1
	}
	if err := dops.setDrain(sn); err != nil {
		fmt.Fprintf(stderr, "gc runtime drain: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	rec.Record(events.Event{
		Type:    events.SessionDraining,
		Actor:   eventActor(),
		Subject: targetName,
	})
	if jsonOutput {
		if err := writeCLIJSONLine(stdout, runtimeActionJSON{
			SchemaVersion: "1",
			OK:            true,
			Command:       "runtime drain",
			Action:        "drain",
			Session:       sn,
			Target:        targetName,
			Status:        "draining",
		}); err != nil {
			fmt.Fprintf(stderr, "gc runtime drain: writing JSON: %v\n", err) //nolint:errcheck // best-effort stderr
			return 1
		}
		return 0
	}
	fmt.Fprintf(stdout, "Draining session '%s'\n", targetName) //nolint:errcheck // best-effort stdout
	return 0
}

// ---------------------------------------------------------------------------
// gc runtime undrain <name>
// ---------------------------------------------------------------------------

func newRuntimeUndrainCmd(stdout, stderr io.Writer) *cobra.Command {
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   "undrain <name>",
		Short: "Cancel drain on a session",
		Long: `Cancel a pending drain signal on a session.

Clears the GC_DRAIN and GC_DRAIN_ACK metadata flags, allowing the
session to continue normal operation. Pass a session alias or ID.`,
		Args: cobra.ArbitraryArgs,
		RunE: func(_ *cobra.Command, args []string) error {
			if cmdRuntimeUndrain(args, jsonOutput, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Output as JSON")
	return cmd
}

func cmdRuntimeUndrain(args []string, jsonOutput bool, stdout, stderr io.Writer) int {
	if len(args) < 1 {
		fmt.Fprintln(stderr, "gc runtime undrain: missing session alias or ID") //nolint:errcheck // best-effort stderr
		return 1
	}
	target, err := resolveSessionRuntimeTarget(args[0], stderr)
	if err != nil {
		fmt.Fprintf(stderr, "gc runtime undrain: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	sp, err := newSessionProvider()
	if err != nil {
		fmt.Fprintf(stderr, "gc runtime undrain: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	dops := newDrainOps(sp)
	rec := openCityRecorder(stderr)
	return doRuntimeUndrain(dops, sp, rec, target.display, target.sessionName, jsonOutput, stdout, stderr, target)
}

// cancelRuntimeUndrainAdmission retains the resolved session identity while
// capturing the provider incarnation and exact native attempt once.
func cancelRuntimeUndrainAdmission(sp runtime.Provider, target sessionRuntimeTarget) error {
	cfg, err := loadCityConfigWithoutBuiltinPackRefresh(target.cityPath, io.Discard)
	if err != nil {
		return err
	}
	store, err := openCityStoreAtWithConfig(target.cityPath, cfg)
	if err != nil {
		return err
	}
	front := sessionFrontDoor(cliSessionStore(store, cfg, target.cityPath))
	// OFF sessions without native evidence keep the legacy path, including
	// stores that do not expose conditional-write handles.
	row, err := front.Store().Get(target.sessionID)
	if err != nil {
		return err
	}
	if !executionProtocolEnabled(front, strings.TrimSpace(row.Metadata[session.WorkStartedKey]) != "") {
		return nil
	}
	current, cell, _, err := front.ExecutionEvidence(target.sessionID)
	if err != nil {
		return err
	}
	if current.SessionNameMetadata != target.sessionName {
		return session.ErrExecutionIncarnationChanged
	}
	runtimeID, err := sp.GetMeta(target.sessionName, "GC_SESSION_ID")
	if err != nil {
		return err
	}
	token, err := sp.GetMeta(target.sessionName, "GC_INSTANCE_TOKEN")
	if err != nil {
		return err
	}
	if runtimeID != target.sessionID || token == "" {
		return session.ErrExecutionIncarnationChanged
	}
	fence := session.ExecutionFence{InstanceToken: token, Generation: current.Generation}
	if cell.Drain == nil {
		// An absent census is a captured state too: CAS validates absence,
		// and refuses a replacement that arrives before cleanup.
		return front.CancelDrainAdmission(target.sessionID, fence, "")
	}
	gate, err := front.InspectDrainAdmission(target.sessionID, fence, cell.Drain.Attempt)
	if err != nil {
		return err
	}
	return front.CancelDrainAdmission(target.sessionID, fence, gate.Attempt)
}

// doRuntimeUndrain clears the drain signal on a session.
func doRuntimeUndrain(dops drainOps, sp runtime.Provider, rec events.Recorder,
	targetName, sn string, jsonOutput bool, stdout, stderr io.Writer, targets ...sessionRuntimeTarget,
) int {
	running, err := workerSessionTargetRunningWithConfig("", nil, sp, nil, sn)
	if err != nil {
		fmt.Fprintf(stderr, "gc runtime undrain: observing %q: %v\n", targetName, err) //nolint:errcheck // best-effort stderr
		return 1
	}
	if !running {
		fmt.Fprintf(stderr, "gc runtime undrain: session %q is not running\n", targetName) //nolint:errcheck // best-effort stderr
		return 1
	}
	if len(targets) > 0 {
		if err := cancelRuntimeUndrainAdmission(sp, targets[0]); err != nil {
			fmt.Fprintf(stderr, "gc runtime undrain: canceling native admission: %v\n", err) //nolint:errcheck // best-effort stderr
			return 1
		}
	}
	if err := dops.clearDrain(sn); err != nil {
		fmt.Fprintf(stderr, "gc runtime undrain: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	rec.Record(events.Event{
		Type:    events.SessionUndrained,
		Actor:   eventActor(),
		Subject: targetName,
	})
	if jsonOutput {
		if err := writeCLIJSONLine(stdout, runtimeActionJSON{
			SchemaVersion: "1",
			OK:            true,
			Command:       "runtime undrain",
			Action:        "undrain",
			Session:       sn,
			Target:        targetName,
			Status:        "undrained",
		}); err != nil {
			fmt.Fprintf(stderr, "gc runtime undrain: writing JSON: %v\n", err) //nolint:errcheck // best-effort stderr
			return 1
		}
		return 0
	}
	fmt.Fprintf(stdout, "Undrained session '%s'\n", targetName) //nolint:errcheck // best-effort stdout
	return 0
}

// ---------------------------------------------------------------------------
// gc runtime drain-check
// ---------------------------------------------------------------------------

func newRuntimeDrainCheckCmd(stdout, stderr io.Writer) *cobra.Command {
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   "drain-check [name]",
		Short: "Check if a session is draining (exit 0 = draining)",
		Long: `Check if a session is currently draining.

Returns exit code 0 if draining, 1 if not. Designed for use in
conditionals: "if gc runtime drain-check; then finish-up; fi". Without
arguments, uses the current session context.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if cmdRuntimeDrainCheck(args, jsonOutput, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Output as JSON")
	return cmd
}

func cmdRuntimeDrainCheck(args []string, jsonOutput bool, stdout, stderr io.Writer) int {
	if len(args) > 0 {
		target, err := resolveSessionRuntimeTarget(args[0], stderr)
		if err != nil {
			fmt.Fprintf(stderr, "gc runtime drain-check: %v\n", err) //nolint:errcheck // best-effort stderr
			return 1                                                 // silent — same as current "not draining" behavior
		}
		sp, err := newSessionProvider()
		if err != nil {
			fmt.Fprintf(stderr, "gc runtime drain-check: %v\n", err) //nolint:errcheck // best-effort stderr
			return 1
		}
		dops := newDrainOps(sp)
		return doRuntimeDrainCheck(dops, target.display, target.sessionName, jsonOutput, stdout, stderr)
	}

	current, err := currentSessionRuntimeTarget()
	if err != nil {
		return 1 // not in agent context → not draining
	}
	sp, err := newSessionProvider()
	if err != nil {
		fmt.Fprintf(stderr, "gc runtime drain-check: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	dops := newDrainOps(sp)
	return doRuntimeDrainCheck(dops, current.display, current.sessionName, jsonOutput, stdout, stderr)
}

// doRuntimeDrainCheck returns 0 if the session is draining, 1 otherwise.
// Silent on stdout — designed for `if gc runtime drain-check; then ...`.
func doRuntimeDrainCheck(dops drainOps, targetName, sn string, jsonOutput bool, stdout, stderr io.Writer) int {
	draining, err := dops.isDraining(sn)
	if err != nil {
		return 1
	}
	if !draining {
		if jsonOutput {
			if err := writeCLIJSONLine(stdout, runtimeDrainCheckJSON{
				SchemaVersion: "1",
				OK:            true,
				Command:       "runtime drain-check",
				Session:       sn,
				Target:        targetName,
				Draining:      false,
			}); err != nil {
				fmt.Fprintf(stderr, "gc runtime drain-check: writing JSON: %v\n", err) //nolint:errcheck // best-effort stderr
				return 1
			}
		}
		return 1
	}
	if jsonOutput {
		if err := writeCLIJSONLine(stdout, runtimeDrainCheckJSON{
			SchemaVersion: "1",
			OK:            true,
			Command:       "runtime drain-check",
			Session:       sn,
			Target:        targetName,
			Draining:      true,
		}); err != nil {
			fmt.Fprintf(stderr, "gc runtime drain-check: writing JSON: %v\n", err) //nolint:errcheck // best-effort stderr
			return 1
		}
	}
	return 0
}

// ---------------------------------------------------------------------------
// gc runtime drain-ack
// ---------------------------------------------------------------------------

func newRuntimeDrainAckCmd(stdout, stderr io.Writer) *cobra.Command {
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   "drain-ack [name]",
		Short: "Acknowledge drain — signal the controller to stop this session",
		Long: `Acknowledge a drain signal — tell the controller to stop this session.

Sets GC_DRAIN_ACK metadata on the session, then pokes the controller
socket so the reconciler stops the session immediately rather than on
its next patrol tick. Call this after the session has finished its
current work in response to a drain signal.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if cmdRuntimeDrainAck(args, jsonOutput, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Output as JSON")
	return cmd
}

func cmdRuntimeDrainAck(args []string, jsonOutput bool, stdout, stderr io.Writer) int {
	code, _ := cmdRuntimeDrainAckResult(args, jsonOutput, stdout, stderr)
	return code
}

func cmdRuntimeDrainAckResult(args []string, jsonOutput bool, stdout, stderr io.Writer) (int, error) {
	if len(args) > 0 {
		target, err := resolveSessionRuntimeTarget(args[0], stderr)
		if err != nil {
			fmt.Fprintf(stderr, "gc runtime drain-ack: %v\n", err) //nolint:errcheck // best-effort stderr
			return 1, err
		}
		sp, err := newSessionProvider()
		if err != nil {
			fmt.Fprintf(stderr, "gc runtime drain-ack: %v\n", err) //nolint:errcheck // best-effort stderr
			return 1, err
		}
		dops := newDrainOps(sp)
		return doRuntimeDrainAckResult(dops, target.cityPath, target.display, target.sessionName, target.sessionID, jsonOutput, stdout, stderr)
	}

	current, err := currentSessionRuntimeTarget()
	if err != nil {
		fmt.Fprintf(stderr, "gc runtime drain-ack: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1, err
	}
	sp, err := newSessionProvider()
	if err != nil {
		fmt.Fprintf(stderr, "gc runtime drain-ack: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1, err
	}
	dops := newDrainOps(sp)
	// Name-only key: the env-derived GC_SESSION_ID can be stale.
	return doRuntimeDrainAckResult(dops, current.cityPath, current.display, current.sessionName, "", jsonOutput, stdout, stderr)
}

// ---------------------------------------------------------------------------
// gc runtime request-restart
// ---------------------------------------------------------------------------

func newRuntimeRequestRestartCmd(stdout, stderr io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:   "request-restart",
		Short: "Request controller restart this session (returns immediately)",
		Long: `Signal the controller to stop and restart this session.

Sets GC_RESTART_REQUESTED metadata on the session, pokes the controller for
an immediate reconcile tick, and returns without waiting. Control-plane
authority over the actual stop/start stays with the controller's reconcile
loop; this command only signals it so the request need not wait for the next
periodic patrol tick.

The command exits 0 once the restart request is durably persisted and the
controller has been signaled, even if the controller has not yet acted. If
the controller cannot be signaled, the command exits 1 with a diagnostic —
the restart request itself remains durably set, so the controller still
picks it up on its next periodic reconcile tick regardless.

This command is designed to be called from within a session context.
It emits a session.draining event before signaling the controller.`,
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			if cmdRuntimeRequestRestart(stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
}

func cmdRuntimeRequestRestart(stdout, stderr io.Writer) int {
	current, err := currentSessionRuntimeTarget()
	if err != nil {
		fmt.Fprintf(stderr, "gc runtime request-restart: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	sp, err := newSessionProvider()
	if err != nil {
		fmt.Fprintf(stderr, "gc runtime request-restart: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	dops := newDrainOps(sp)
	store, storeErr := openCityStoreAt(current.cityPath)
	if storeErr != nil {
		fmt.Fprintf(stderr, "gc runtime request-restart: opening store: %v\n", storeErr) //nolint:errcheck // best-effort stderr
	}
	// Route the SESSION-class access (restart persist through the worker
	// boundary) to the session coordination-class store so a
	// [beads.classes.sessions] relocation reaches gc runtime request-restart.
	// The routing cfg is loaded refresh-free (the full cfg loads later, for
	// timeout/template resolution). Identity today, so byte-identical.
	var sessStore beads.Store
	if store != nil {
		routeCfg, _ := loadCityConfigWithoutBuiltinPackRefresh(current.cityPath, io.Discard)
		sessStore = cliSessionStore(store, routeCfg, current.cityPath)
	}
	rec := openCityRecorderAt(current.cityPath, stderr)
	cfg, _ := loadCityConfig(current.cityPath, stderr)
	var persistRestart func() error
	if store != nil {
		persistRestart = func() error {
			handle, err := workerHandleForSessionTargetWithConfig(current.cityPath, sessStore, sp, cfg, current.sessionName)
			if err != nil {
				return err
			}
			return handle.Reset(context.Background())
		}
	}
	_, pinned, err := sessionRestartableByController(sessStore, current.sessionName)
	if err != nil {
		fmt.Fprintf(stderr, "gc runtime request-restart: checking session type: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	return doRuntimeRequestRestart(dops, persistRestart, pinned, rec, current.display, current.sessionName,
		current.cityPath, stdout, stderr)
}

// controllerRestartTimeout computes the bounded timeout for waiting on the
// controller to act on a restart request: max(5*PatrolInterval, 5min), capped at 30min.
func controllerRestartTimeout(cfg *config.City) time.Duration {
	const floor = 5 * time.Minute
	const ceil = 30 * time.Minute
	patrol := 30 * time.Second
	if cfg != nil {
		patrol = cfg.Daemon.PatrolIntervalDuration()
	}
	d := 5 * patrol
	if d < floor {
		d = floor
	}
	if d > ceil {
		d = ceil
	}
	return d
}

// doRuntimeRequestRestart sets the restart-requested flag, pokes the
// controller for an immediate reconcile tick, and returns without waiting for
// the controller to act. Control-plane authority over the actual stop/start
// stays with the controller's reconcile loop; this call only signals it so
// the request need not wait for the next periodic tick.
//
// pinned marks a kill-protected named session (pin_awake == "true"): the
// reconciler refuses to collaterally kill such a session on a bare runtime
// restart-requested flag, so for pinned sessions persistRestart (which lands
// continuation_reset_pending, the explicit-reset escape hatch) is mandatory
// rather than best-effort. See sessionRestartableByController.
func doRuntimeRequestRestart(dops drainOps, persistRestart func() error, pinned bool, rec events.Recorder,
	targetName, sn, cityPath string, stdout, stderr io.Writer,
) int {
	if err := dops.setRestartRequested(sn); err != nil {
		fmt.Fprintf(stderr, "gc runtime request-restart: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	if pinned {
		if persistRestart == nil {
			fmt.Fprintf(stderr, "gc runtime request-restart: pinned session %q has no restart persistence available; not requesting restart\n", sn) //nolint:errcheck // best-effort stderr
			return 1
		}
		if err := persistRestart(); err != nil {
			fmt.Fprintf(stderr, "gc runtime request-restart: could not persist restart marker for pinned session %q; not requesting restart: %v\n", sn, err) //nolint:errcheck // best-effort stderr
			return 1
		}
	} else if persistRestart != nil {
		// Also persist the request through the worker boundary so it survives
		// tmux session death. Non-fatal here: the runtime flag above is primary.
		if err := persistRestart(); err != nil {
			fmt.Fprintf(stderr, "gc runtime request-restart: setting bead restart flag: %v\n", err) //nolint:errcheck // best-effort stderr
		}
	}
	rec.Record(events.Event{
		Type:    events.SessionDraining,
		Actor:   targetName,
		Subject: targetName,
		Message: "restart requested by session",
	})

	// Name-only key: sn is authoritative here, while GC_SESSION_ID can be stale.
	if err := pokeControllerForRestart(cityPath, reconcilekey.SessionNamed(sn)); err != nil {
		fmt.Fprintf(stderr, "gc runtime request-restart: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	fmt.Fprint(stdout, "Restart requested; controller notified for immediate reconcile.\n") //nolint:errcheck // best-effort stdout
	return 0
}

// waitForControllerRestart polls until the controller accepts the stop
// handoff (exit 0), the context is canceled by a signal (exit 0), or the
// bounded timeout expires (exit 1 with diagnostic).
//
// A cleared GC_RESTART_REQUESTED flag alone is not proof the controller acted:
// the reconciler's pinned-session collateral-skip clears the very same flag
// without killing the session (see pinnedConfiguredNamedSessionKillProtected
// in session_reconciler.go). sp confirms the session actually stopped before
// this reports success; while the flag is clear but the session is still
// running, polling continues until the deadline instead of returning early.
func waitForControllerRestart(ctx context.Context, dops drainOps, sp runtime.Provider, sn string, timeout time.Duration, stderr io.Writer) int {
	deadline := time.Now().Add(timeout)
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	var lastPollErr error

	for {
		select {
		case <-ctx.Done():
			// Signal received; leave the flag set so the controller still acts on its next tick.
			fmt.Fprint(stderr, "gc handoff: signal received; restart request remains set; controller will stop this session on its next reconcile tick\n") //nolint:errcheck // best-effort stderr
			return 0
		case <-ticker.C:
			requested, err := dops.isRestartRequested(sn)
			switch {
			case err != nil:
				lastPollErr = err
			case !requested && !sp.IsRunning(sn):
				// The controller accepted the stop handoff or the runtime is already gone.
				return 0
			default:
				lastPollErr = nil
			}
			if time.Now().After(deadline) {
				if lastPollErr != nil {
					fmt.Fprintf(stderr, "gc handoff: controller did not act within %s; last poll error: %v; check `gc dashboard` or `gc trace`\n", timeout, lastPollErr) //nolint:errcheck // best-effort stderr
				} else {
					fmt.Fprintf(stderr, "gc handoff: controller did not act within %s; check `gc dashboard` or `gc trace`\n", timeout) //nolint:errcheck // best-effort stderr
				}
				return 1
			}
		}
	}
}

// drainAckPokeController is a mutable global test seam over enqueueController.
// Tests that swap it MUST NOT call t.Parallel().
var drainAckPokeController = enqueueController

// drainAckReleaseHeldClaims is a mutable global test seam over
// releaseUnexecutedClaimsForSession, matching drainAckPokeController above.
// Tests that swap it MUST NOT call t.Parallel().
var drainAckReleaseHeldClaims = releaseUnexecutedClaimsForSession

// releaseUnexecutedClaimsForSession resolves this city's residency-correct work
// legs and gives back only assignments the session has not started executing.
//
// The leg set mirrors `gc session close`, which leads with the WORK store and
// hands in the relocated graph binding as a class leg: a claim that claim-time
// routing left in the binding is invisible to a work-led scan, and would be
// released by nothing. Store and census failures refuse acknowledgement so an
// unreadable claim cannot be mistaken for completed work.
func releaseUnexecutedClaimsForSession(cityPath, sessionName string, stderr io.Writer) error {
	if strings.TrimSpace(cityPath) == "" || strings.TrimSpace(sessionName) == "" {
		return nil
	}
	// Only read a real city. A city is a directory holding city.toml, and
	// opening a store somewhere that is not one does not find claims — it
	// PROVISIONS a store (a managed Dolt server included) in whatever directory
	// the caller happened to resolve. Drain-ack is reachable from contexts with
	// no city at all (a bare `gc hook --claim --drain-ack`, a test harness), and
	// before this release step it did no store I/O whatsoever, so the cost of
	// getting that wrong is a data directory and a server process where neither
	// belongs.
	if _, err := os.Stat(filepath.Join(cityPath, "city.toml")); err != nil {
		// A runtime root carrying .gc/ but no city.toml is the legacy shape, and
		// it is the one skip worth naming: it looks like a city to a human, a
		// session really can hold claims there, and staying silent would make a
		// release that never ran indistinguishable from one that found nothing.
		// It is NOT auto-upgraded here — provisioning a store against a layout
		// this function does not understand is the failure the city.toml gate
		// exists to prevent.
		if _, gcErr := os.Stat(filepath.Join(cityPath, ".gc")); gcErr == nil {
			fmt.Fprintf(stderr, "gc runtime drain-ack: %s has .gc/ but no city.toml; skipping the held-claim release for this legacy runtime root (run `gc doctor` to check the layout)\n", cityPath) //nolint:errcheck
		}
		return nil
	}
	// One current config load serves every open. A failed load refuses
	// acknowledgement rather than silently omitting rig legs.
	cfg, err := loadCityConfig(cityPath, io.Discard)
	if err != nil {
		return err
	}
	store, err := openCityStoreAtWithConfig(cityPath, cfg)
	if err != nil {
		return err
	}
	if store == nil {
		return errors.New("drain acknowledgement has no city store")
	}
	rigStores := func() map[string]beads.Store {
		if cfg == nil {
			return nil
		}
		return buildStandaloneRigStoresWithConfig(cfg, cityPath, io.Discard)
	}
	return releaseUnexecutedClaimsForSessionStore(cityPath, cfg, store, rigStores, sessionName, drainAckReleaseBudget, stderr)
}

// releaseUnexecutedClaimsForSessionStore resolves a runtime session name to the
// durable session bead behind it and releases the claims that bead's identities
// still hold. Resolution is the reason this step exists separately: a pool
// worker's runtime name lives in `session_name` metadata on a bead whose ID is
// something else entirely, so a direct Get on the runtime name would miss it.
//
// rigStores is a thunk, not a map, because opening the rig stores is real store
// I/O on the pre-ack path. It is called only once resolution and the session
// Get have both succeeded — a drain-ack that cannot find its session pays
// nothing for stores it would immediately discard.
func releaseUnexecutedClaimsForSessionStore(
	cityPath string,
	cfg *config.City,
	store beads.Store,
	rigStores func() map[string]beads.Store,
	sessionName string,
	budget time.Duration,
	stderr io.Writer,
) error {
	sessStore := cliSessionStore(store, cfg, cityPath)
	sessionID, err := resolveSessionID(sessStore, sessionName)
	if err != nil {
		return err
	}
	live := beads.HandlesFor(sessStore).Live
	if live == nil {
		return errors.New("drain acknowledgement has no live session reader")
	}
	sessionBead, err := live.Get(sessionID)
	if err != nil {
		return err
	}
	strict, err := hookExecutionEvidenceEnabled(cfg)
	if err != nil {
		return err
	}
	if strict || strings.TrimSpace(sessionBead.Metadata[session.WorkStartedKey]) != "" {
		fence := hookExecutionFenceOf(os.Environ())
		if fence.InstanceToken == "" || fence.InstanceToken != sessionBead.Metadata["instance_token"] ||
			fence.Generation != "" && fence.Generation != sessionBead.Metadata["generation"] {
			return session.ErrExecutionIncarnationChanged
		}
	}
	var rigs map[string]beads.Store
	if rigStores != nil {
		rigs = rigStores()
	}
	return releaseUnexecutedClaimsOnDrainAck(cityPath, cfg, store, rigs, sessionBead, budget, stderr)
}

// drainAckReleaseBudget bounds the whole held-claim release pass.
//
// The pass runs BEFORE the ack, which is the signal the controller waits on to
// stop this session, and it fans out over every work leg × every identity with
// only per-command ceilings underneath it. A slow or contended store would
// therefore make drain-ack hang for the product of those, turning a safety net
// into a stall on the exact path a draining worker needs to be fast. An
// incomplete census refuses acknowledgement and preserves its evidence for
// the next attempt.
const drainAckReleaseBudget = 15 * time.Second

// drainAckMutationTimeout bounds the drain-ack metadata write so a stalled
// remote store cannot wedge `gc runtime drain-ack` (and the `gc hook
// --drain-ack` no-work path that calls it) indefinitely. Its siblings on the
// hook hot path are already bounded (work query 60s, claim 10s); drain-ack was
// the one unbounded store write, so a slow store could hang the whole hook
// until an external timeout killed it. On timeout the ack is left unset and the
// controller re-drives the drain on its next tick (NDI), rather than blocking.
var drainAckMutationTimeout = 10 * time.Second

// errDrainAckTimeout marks a drain-ack write abandoned after
// drainAckMutationTimeout, distinguishing a slow store from a real failure.
var errDrainAckTimeout = errors.New("drain-ack write timed out")

// setDrainAckBounded runs dops.setDrainAck under drainAckMutationTimeout. A
// stalled store write is abandoned — its goroutine drains into the buffered
// channel and exits when the write finally returns — so the caller is never
// blocked past the budget.
func setDrainAckBounded(dops drainOps, sn string) error {
	done := make(chan error, 1)
	go func() { done <- dops.setDrainAck(sn) }()
	select {
	case err := <-done:
		return err
	case <-time.After(drainAckMutationTimeout):
		return errDrainAckTimeout
	}
}

// doRuntimeDrainAck releases any unexecuted claim the session still holds, sets
// the drain-ack flag, then pokes the controller so the reconciler observes the
// drained state immediately instead of waiting for its next patrol tick.
//
// The release runs BEFORE the ack, and the order is load-bearing: the ack is what
// tells the controller it may stop this session, so acknowledging first opens a
// window in which the session dies still holding exactly the claim this release
// exists to clear.
func doRuntimeDrainAck(dops drainOps, cityPath, targetName, sn, sessionID string, jsonOutput bool, stdout, stderr io.Writer) int {
	code, _ := doRuntimeDrainAckResult(dops, cityPath, targetName, sn, sessionID, jsonOutput, stdout, stderr)
	return code
}

func doRuntimeDrainAckResult(dops drainOps, cityPath, targetName, sn, sessionID string, jsonOutput bool, stdout, stderr io.Writer) (int, error) {
	if err := drainAckReleaseHeldClaims(cityPath, sn, stderr); err != nil {
		fmt.Fprintf(stderr, "gc runtime drain-ack: %v\n", err) //nolint:errcheck // best-effort diagnostic
		return 1, err
	}
	bound, err := bindDrainAckAdmission(dops, cityPath, sn)
	if err != nil {
		fmt.Fprintf(stderr, "gc runtime drain-ack: qualifying acknowledgement: %v\n", err) //nolint:errcheck // best-effort diagnostic
		return 1, err
	}
	dops = bound
	timedOut := false
	var ackOutcomeErr error
	if err := setDrainAckBounded(dops, sn); err != nil {
		if errors.Is(err, errDrainAckTimeout) {
			// The write can still land after this timeout. Preserve the CLI's
			// deferred result, but never report acknowledgement to the hook.
			timedOut = true
			ackOutcomeErr = err
			fmt.Fprintf(stderr, "gc runtime drain-ack: timed out after %s writing drain-ack for %s; controller will re-drive on its next tick\n", drainAckMutationTimeout, sn) //nolint:errcheck // best-effort stderr
		} else {
			fmt.Fprintf(stderr, "gc runtime drain-ack: %v\n", err) //nolint:errcheck // best-effort stderr
			return 1, err
		}
	}
	if err := drainAckPokeController(cityPath, reconcilekey.SessionRef(sessionID, sn)); err != nil {
		fmt.Fprintf(stderr, "gc runtime drain-ack: warning: poke failed: %v\n", err) //nolint:errcheck // best-effort stderr
	}
	status := "acknowledged"
	if timedOut {
		status = "deferred"
	}
	if jsonOutput {
		if err := writeCLIJSONLine(stdout, runtimeActionJSON{
			SchemaVersion: "1",
			OK:            true,
			Command:       "runtime drain-ack",
			Action:        "drain-ack",
			Session:       sn,
			Target:        targetName,
			Status:        status,
		}); err != nil {
			fmt.Fprintf(stderr, "gc runtime drain-ack: writing JSON: %v\n", err) //nolint:errcheck // best-effort stderr
			return 1, err
		}
		return 0, ackOutcomeErr
	}
	if timedOut {
		fmt.Fprintln(stdout, "Drain-ack deferred (store slow); controller poked to re-drive.") //nolint:errcheck // best-effort stdout
	} else {
		fmt.Fprintln(stdout, "Drain acknowledged. Controller poked for immediate stop.") //nolint:errcheck // best-effort stdout
	}
	return 0, ackOutcomeErr
}
