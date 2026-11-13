package main

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/mail"
	"github.com/gastownhall/gascity/internal/mail/beadmail"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/telemetry"
	"github.com/spf13/cobra"
)

func newHandoffCmd(stdout, stderr io.Writer) *cobra.Command {
	var target string
	var auto bool
	var hookFormat string
	var jsonOut bool
	var force bool
	var recycle bool
	var key string
	cmd := &cobra.Command{
		Use:   "handoff [subject] [message]",
		Short: "Send handoff mail and restart controller-managed sessions",
		Long: `Convenience command for context handoff.

Self-handoff (default): sends mail to self. If the current session is
controller-restartable, requests a restart, pokes the controller for an
immediate reconcile tick, and returns without waiting for the controller to
act. For on-demand configured named sessions, sends mail and returns without
requesting restart: handoff intentionally leaves the user-attended session
running instead of restarting it out from under the user.

Self-handoff with --recycle deliberately requests a fresh controller-managed
conversation after writing the continuation mail, including for an on-demand
configured named session. Use it only when the current agent is explicitly
ready to leave and resume from its handoff; it never lets the controller
unilaterally recycle an attended session.

For controller-restartable sessions, equivalent to:

  gc mail send $GC_ALIAS <subject> [message]
  gc runtime request-restart

The restart request is recorded on this session's row, bound to this
session's incarnation: it is recorded only while the session row, or the
live runtime under the session's name, still carries the GC_INSTANCE_TOKEN
this process runs under. A check before the mail refuses the restart, and
sends no mail, when this process has no GC_INSTANCE_TOKEN (an agent CLI that
scrubs *TOKEN* variables from the shells it spawns removes it), when the
session row and the live runtime name another incarnation, or when the
session store cannot compare-and-swap where the request needs it (below).
The write after the mail decides again: a row that moved after the check can
still refuse it there, with the mail already sent. Outside a worker site the
runtime's GC_RESTART_REQUESTED hint follows, then the controller poke.
gc handoff exits 0 once the request is recorded, even when the controller
could not be signaled: a warning says so, and the controller acts at its next
pass. It exits 1 with no restart requested and no runtime hint set when the
request cannot be recorded: no GC_INSTANCE_TOKEN, an unreachable session
store, a session whose row and live runtime name another incarnation, or a
session store that cannot compare-and-swap where the request needs it: a v2
city records the request only by compare-and-swap, and
[beads] conditional_writes = "require" refuses any write the store cannot
fence.

Auto handoff (--auto): sends mail to self and returns without requesting a
restart. This is for PreCompact hooks, where the provider is already managing
the context compaction lifecycle.

Remote handoff (--target): sends mail to a target session. If the target is
controller-restartable, kills it so the reconciler restarts it with the handoff
mail waiting. For on-demand configured named targets, sends mail and returns
without killing the session.

For controller-restartable targets, equivalent to:

  gc mail send <target> <subject> [message]
  gc session kill <target>

While the op journal records ops, a restartable self-handoff is recorded as a
restart{fresh} op and a notice op to self; a rerun with the same --key reports
the first run's outcome without sending the mail again. The key covers neither
the subject nor the message: a rerun under it sends no mail whatever they say,
and its --json result leaves the subject out. The key is the handoff's own: gc
runtime request-restart or gc session reset under the same --key records
another op. The restart stops this session, and so can stop the run before it
closes its restart op: a rerun with the same --key is then refused as in
progress until the op's deadline passes, and after that replays the first
run's success when its notice op succeeded and the session row shows that the
restart is still pending or has run, else reports the op's failure. The
notice op rides the restart op: when the restart op is not recorded
(ops.lifecycle off), no notice op is recorded either, even under ops.content =
"require". --auto and --target record no ops and refuse --key. A self handoff
of a session the controller does not restart records no ops and warns that
--key is not recorded.

Self-handoff requires session context (GC_ALIAS or GC_SESSION_ID, plus
GC_SESSION_NAME and city context env). Remote handoff accepts a session alias
or ID. Subject is required unless --auto is set.`,
		Args: func(cmd *cobra.Command, args []string) error {
			if auto {
				return cobra.MaximumNArgs(2)(cmd, args)
			}
			return cobra.RangeArgs(1, 2)(cmd, args)
		},
		RunE: func(_ *cobra.Command, args []string) error {
			out := stdout
			if jsonOut {
				out = io.Discard
			}
			if cmdHandoff(handoffRun{
				args: args, target: target, auto: auto, hookFormat: hookFormat, force: force,
				key: key, recycle: recycle, asJSON: jsonOut, stdout: out, jsonOut: stdout, stderr: stderr,
			}) != 0 {
				return errExit
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&target, "target", "", "Remote session alias or ID to handoff (kills only controller-restartable sessions)")
	cmd.Flags().BoolVar(&auto, "auto", false, "Send handoff mail without requesting restart (for PreCompact hooks)")
	cmd.Flags().StringVar(&hookFormat, "hook-format", "", "format hook output for a provider")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "emit JSON summary")
	cmd.Flags().BoolVar(&force, "force", false, "destroy a target even when it has live background subagents")
	cmd.Flags().BoolVar(&recycle, "recycle", false, "restart this session after sending the handoff mail, including an attended named session")
	addOpKeyFlag(cmd, &key, handoffOpKeyNote)
	return cmd
}

type handoffJSONResult struct {
	SchemaVersion string `json:"schema_version"`
	OK            bool   `json:"ok"`
	Mode          string `json:"mode"`
	Target        string `json:"target,omitempty"`
	Auto          bool   `json:"auto"`
	// Subject is the sent mail's subject; a replay sends none and leaves it
	// out.
	Subject string `json:"subject,omitempty"`
	// OpID, NoticeOpID, Key and Replayed name the restart op and notice op
	// that back a recorded self-handoff (OJ-6).
	OpID       string `json:"op_id,omitempty"`
	NoticeOpID string `json:"notice_op_id,omitempty"`
	Key        string `json:"key,omitempty"`
	Replayed   bool   `json:"replayed,omitempty"`
}

func handoffJSONMode(target string, auto bool) string {
	if target != "" {
		return "remote"
	}
	if auto {
		return "auto"
	}
	return "self"
}

func handoffJSONSubject(args []string, auto bool) string {
	if len(args) > 0 {
		return args[0]
	}
	if auto {
		return "context cycle"
	}
	return "HANDOFF: context cycle"
}

// cmdHandoff runs gc handoff and, under --json, writes its result to
// run.jsonOut once it succeeds.
func cmdHandoff(run handoffRun) int {
	code, ops := runHandoff(run)
	if code != 0 || !run.asJSON {
		return code
	}
	subject := handoffJSONSubject(run.args, run.auto)
	if ops.replayed {
		// A replay sends no mail, and its op does not hold the subject the
		// first run sent.
		subject = ""
	}
	return writeCLIJSONLineOrExit(run.jsonOut, run.stderr, "gc handoff", handoffJSONResult{
		SchemaVersion: "1",
		OK:            true,
		Mode:          handoffJSONMode(run.target, run.auto),
		Target:        run.target,
		Auto:          run.auto,
		Subject:       subject,
		OpID:          ops.opID,
		NoticeOpID:    ops.noticeOpID,
		Key:           ops.key,
		Replayed:      ops.replayed,
	})
}

// handoffRun is one gc handoff invocation.
type handoffRun struct {
	args       []string
	target     string
	auto       bool
	hookFormat string
	force      bool
	// recycle is --recycle: restart this session even when it is an attended
	// named session the controller would not restart.
	recycle bool
	// key is --key; asJSON is --json, under which an op refusal's failure
	// envelope goes to jsonOut while the text output (stdout) is discarded.
	key     string
	asJSON  bool
	stdout  io.Writer
	jsonOut io.Writer
	stderr  io.Writer
}

// errHandoffKeyWithoutOps refuses --key on a handoff that records no ops.
var errHandoffKeyWithoutOps = errors.New("--key applies only to a self handoff: --auto and --target record no ops")

// runHandoff runs gc handoff and returns its exit code and the op fields of
// its JSON result.
func runHandoff(run handoffRun) (int, handoffOpFields) {
	args, target, auto, hookFormat, force, recycle, stdout, stderr := run.args, run.target, run.auto, run.hookFormat, run.force, run.recycle, run.stdout, run.stderr
	if run.key != "" && (auto || target != "") {
		fmt.Fprintf(stderr, "gc handoff: %v\n", errHandoffKeyWithoutOps) //nolint:errcheck // best-effort stderr
		return 1, handoffOpFields{}
	}
	if target != "" {
		if auto {
			fmt.Fprintln(stderr, "gc handoff: --auto cannot be used with --target") //nolint:errcheck // best-effort stderr
			return 1, handoffOpFields{}
		}
		if recycle {
			fmt.Fprintln(stderr, "gc handoff: --recycle cannot be used with --target") //nolint:errcheck // best-effort stderr
			return 1, handoffOpFields{}
		}
		return cmdHandoffRemoteWithForce(args, target, force, stdout, stderr), handoffOpFields{}
	}
	if auto && recycle {
		fmt.Fprintln(stderr, "gc handoff: --recycle cannot be used with --auto") //nolint:errcheck // best-effort stderr
		return 1, handoffOpFields{}
	}

	current, err := currentSessionRuntimeTarget()
	if err != nil {
		fmt.Fprintf(stderr, "gc handoff: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1, handoffOpFields{}
	}

	store, err := openCityStoreAt(current.cityPath)
	if err != nil {
		fmt.Fprintf(stderr, "gc handoff: %v\n", err)                    //nolint:errcheck // best-effort stderr
		fmt.Fprintln(stderr, "hint: run \"gc doctor\" for diagnostics") //nolint:errcheck // best-effort stderr
		return 1, handoffOpFields{}
	}
	// Route the handoff's SESSION-class access (restartability, restart-request
	// clear/persist, and beadmail's session-addressing arm) to the session
	// coordination-class store so a [beads.classes.sessions] relocation reaches gc
	// handoff the same way it reaches the controller. The routing cfg is loaded
	// refresh-free — the auto branch runs before the main cfg load; identity today,
	// so byte-identical.
	routeCfg, _ := loadCityConfigWithoutBuiltinPackRefresh(current.cityPath, io.Discard)
	sessStore := cliSessionStore(store, routeCfg, current.cityPath)
	// The handoff bead is ClassMessaging; left on the work store, a relocated
	// city writes the handoff into the ledger nothing delivers from.
	msgStore := cliMailStore(store, routeCfg, current.cityPath).Store
	rec := openCityRecorderAt(current.cityPath, stderr)
	if auto {
		return doHandoffAuto(msgStore, sessStore, rec, current.display, args, hookFormat, stdout, stderr), handoffOpFields{}
	}

	sp, err := newSessionProvider()
	if err != nil {
		fmt.Fprintf(stderr, "gc handoff: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1, handoffOpFields{}
	}
	cfg, cfgErr := loadCityConfig(current.cityPath, stderr)
	r := rowFirstRestart{
		verb:        "gc handoff",
		cityPath:    current.cityPath,
		sessionName: current.sessionName,
		display:     current.display,
		token:       os.Getenv(runtime.InstanceTokenEnv),
		record:      sessionRestartRecorder(current.cityPath, sessStore, sp, cfg, current.sessionName),
		check:       sessionRestartChecker(current.cityPath, sessStore, sp, cfg, current.sessionName),
		hint:        restartHintOps(current.cityPath, sp),
		poke:        pokeControllerForRestart,
		stderr:      stderr,
		ops:         newRestartOps(cfg, cfgErr, sessStore, run.key, current.sessionName, current.display, run.asJSON, run.jsonOut, stderr),
	}

	outcome := doHandoffWithRecycleOutcome(msgStore, sessStore, rec, newDrainOps(sp), r, current.display, current.sessionName, args, recycle, stdout, stderr)
	if outcome.code != 0 || !outcome.restartRequested {
		return outcome.code, r.ops.jsonFields()
	}
	// The request is recorded: the poke is only a hint and never fails the
	// command (notifyController prints why the controller was not notified).
	r.notifyController()
	return 0, r.ops.jsonFields()
}

// cmdHandoffRemote sends handoff mail to a remote session and kills its runtime.
// Returns immediately (non-blocking). The reconciler restarts the target.
func cmdHandoffRemote(args []string, target string, stdout, stderr io.Writer) int {
	return cmdHandoffRemoteWithForce(args, target, false, stdout, stderr)
}

func cmdHandoffRemoteWithForce(args []string, target string, force bool, stdout, stderr io.Writer) int {
	cityPath, err := resolveCity()
	if err != nil {
		fmt.Fprintf(stderr, "gc handoff: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	if onWorkerSite(cityPath) {
		fmt.Fprintf(stderr, "gc handoff: %v\n", errWorkerSiteHandoffTargetNeedsController) //nolint:errcheck // best-effort stderr
		return 1
	}
	targetInfo, err := resolveSessionRuntimeTarget(target, stderr)
	if err != nil {
		fmt.Fprintf(stderr, "gc handoff: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	store, code := openCityStore(stderr, "gc handoff")
	if store == nil {
		return code
	}
	cfg, _ := loadCityConfig(cityPath, stderr)
	// Route SESSION-class access (sender identity resolution, restartability, remote
	// kill/observe/identity, resolveSessionID, beadmail's session addressing) to the
	// session coordination-class store; identity today, so byte-identical.
	sessStore := cliSessionStore(store, cfg, cityPath)
	// See cmdHandoff: the message bead is ClassMessaging and routes on its own.
	msgStore := cliMailStore(store, cfg, cityPath).Store
	sender, ok := resolveDefaultMailSenderForCommand(cityPath, cfg, sessStore, stderr, "gc handoff")
	if !ok {
		return 1
	}

	sp, err := newSessionProvider()
	if err != nil {
		fmt.Fprintf(stderr, "gc handoff: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	rec := openCityRecorder(stderr)
	return doHandoffRemoteWithForce(cityPath, msgStore, sessStore, rec, sp, targetInfo.sessionName, targetInfo.display, sender, args, force, stdout, stderr)
}

type handoffOutcome struct {
	code             int
	restartRequested bool
}

// doHandoff sends a handoff mail to self and records a restart request when
// the controller can restart the current session. Testable: does not block.
func doHandoff(msgStore, sessStore beads.Store, rec events.Recorder, dops drainOps, r rowFirstRestart,
	sessionAddress, sessionName string, args []string, stdout, stderr io.Writer,
) int {
	return doHandoffWithOutcome(msgStore, sessStore, rec, dops, r, sessionAddress, sessionName, args, stdout, stderr).code
}

// doHandoffWithOutcome checks restartability, sends the handoff mail and, for
// a restartable session, records the restart request row first (SG-3). A
// restartable handoff whose caller names no incarnation, or whose incarnation
// a check of the row refuses (rowFirstRestart.refusedBeforeMail), is refused
// before the mail: the mail is addressed to the session, so a left-over
// incarnation's mail would reach its successor, and a retry would send it
// again. The fenced write after the mail stays the authority, since the row
// can move between the check and the write. dops clears a stale runtime
// restart flag of a session the controller does not restart.
func doHandoffWithOutcome(msgStore, sessStore beads.Store, rec events.Recorder, dops drainOps, r rowFirstRestart,
	sessionAddress, sessionName string, args []string, stdout, stderr io.Writer,
) handoffOutcome {
	return doHandoffWithRecycleOutcome(msgStore, sessStore, rec, dops, r, sessionAddress, sessionName, args, false, stdout, stderr)
}

// doHandoffWithRecycleOutcome is doHandoffWithOutcome plus the explicit
// self-handoff --recycle opt-in, which requests a restart even for an attended
// named session the controller would not restart.
func doHandoffWithRecycleOutcome(msgStore, sessStore beads.Store, rec events.Recorder, dops drainOps, r rowFirstRestart,
	sessionAddress, sessionName string, args []string, recycle bool, stdout, stderr io.Writer,
) handoffOutcome {
	restartable, err := sessionRestartableByController(sessStore, sessionName)
	if err != nil {
		fmt.Fprintf(stderr, "gc handoff: checking session type: %v\n", err) //nolint:errcheck // best-effort stderr
		return handoffOutcome{code: 1}
	}
	// restart: the session restarts after the mail, either because the controller
	// restarts it or because the caller deliberately recycles it (--recycle).
	restart := restartable || recycle
	if restart {
		if err := r.refusedBeforeMail(); err != nil {
			fmt.Fprintf(stderr, "gc handoff: %s\n", r.notRecorded(err)) //nolint:errcheck // best-effort stderr
			return handoffOutcome{code: 1}
		}
	}

	// A restartable handoff proposes its restart op, then its notice op,
	// before the mail (OJ-6). A replay of a successful run sends nothing.
	if restart {
		switch r.ops.startHandoff(r.verb) {
		case cliOpRefused:
			return handoffOutcome{code: 1}
		case cliOpReplayed:
			handoffReplayNote(r.verb, stderr)
			fmt.Fprint(stdout, "Handoff: restart requested.\n") //nolint:errcheck // best-effort stdout
			return handoffOutcome{code: 0}
		}
		defer r.ops.end()
	}

	if !restart {
		r.ops.unrecordedKey(r.verb)
	}
	extraLabels := []string{"priority:1"}
	if restart {
		extraLabels = append(extraLabels, mail.AutoHandoffLabel, mail.ArchiveAfterInjectLabel)
	}
	b, ok := createHandoffMail(msgStore, sessStore, rec, sessionAddress, sessionAddress, args, "HANDOFF: context cycle", extraLabels, stderr)
	if restart {
		var mailErr error
		if !ok {
			mailErr = errHandoffMailFailed
		}
		r.ops.mailSent(mailErr)
	}
	if !ok {
		return handoffOutcome{code: 1}
	}

	if !restart {
		if err := clearRestartRequest(sessStore, dops, sessionName); err != nil {
			fmt.Fprintf(stderr, "gc handoff: clearing stale restart request: %v\n", err) //nolint:errcheck // best-effort stderr
			return handoffOutcome{code: 1}
		}
		fmt.Fprintf(stdout, "Handoff: sent mail %s (named session; restart skipped).\nHint: run 'gc handoff --recycle' to request deliberate restart for an attended session.\n", b.ID) //nolint:errcheck // best-effort stdout
		return handoffOutcome{code: 0}
	}

	if err := r.recordRestart(); err != nil {
		fmt.Fprintf(stderr, "gc handoff: sent mail %s; %s\n", b.ID, r.notRecorded(err)) //nolint:errcheck // best-effort stderr
		return handoffOutcome{code: 1}
	}
	rec.Record(events.Event{
		Type:    events.SessionDraining,
		Actor:   sessionAddress,
		Subject: sessionAddress,
		Message: "handoff",
	})
	observation := handoffContinuationObservation(sessStore, sessionName)
	observation.Outcome = continuationOutcomeRequested
	recordContinuationObservation(rec, observation)

	fmt.Fprintf(stdout, "Handoff: sent mail %s, requesting restart...\n", b.ID) //nolint:errcheck // best-effort stdout
	return handoffOutcome{code: 0, restartRequested: true}
}

func handoffContinuationObservation(sessStore beads.Store, sessionName string) continuationObservation {
	observation := continuationObservation{
		Boundary:    continuationBoundaryReset,
		Source:      continuationSourceHandoff,
		SessionName: sessionName,
	}
	if sessStore == nil {
		return observation
	}
	sessionID, err := resolveSessionID(sessStore, sessionName)
	if err != nil {
		return observation
	}
	observation.SessionID = sessionID
	bead, err := sessStore.Get(sessionID)
	if err != nil {
		return observation
	}
	observation.SessionName = firstNonEmptyGCString(bead.Metadata["session_name"], sessionName)
	observation.Template = bead.Metadata["template"]
	observation.Generation = bead.Metadata["generation"]
	observation.ContinuationEpoch = bead.Metadata["continuation_epoch"]
	observation.InstanceToken = bead.Metadata["instance_token"]
	observation.OldWorkID = bead.Metadata[session.CurrentBeadIDKey]
	return observation
}

// handoffObservationFromInfo is handoffContinuationObservation for a session
// that is already resolved: the same fields, read from the record in hand
// instead of the store.
func handoffObservationFromInfo(info session.Info, sessionName string) continuationObservation {
	return continuationObservation{
		Boundary:          continuationBoundaryReset,
		Source:            continuationSourceHandoff,
		SessionName:       firstNonEmptyGCString(info.SessionName, sessionName),
		SessionID:         info.ID,
		Template:          info.Template,
		Generation:        info.Generation,
		ContinuationEpoch: info.ContinuationEpoch,
		InstanceToken:     info.InstanceToken,
		OldWorkID:         info.CurrentlyProcessingBeadID,
	}
}

// doHandoffAuto sends handoff mail to self without requesting restart. The
// calling session is resolved once, from its own bead id (GC_SESSION_ID), and
// serves both the continuation observation and the mail's sender route; only a
// hook without that id falls back to resolving sessionAddress by name.
func doHandoffAuto(msgStore, sessStore beads.Store, rec events.Recorder, sessionAddress string, args []string, hookFormat string, stdout, stderr io.Writer) int {
	var pinned *hookIdentity
	var observation continuationObservation
	if identity, ok, err := resolveHookIdentity(sessStore); err == nil && ok {
		pinned = &identity
		observation = handoffObservationFromInfo(identity.info, sessionAddress)
	} else {
		observation = handoffContinuationObservation(sessStore, sessionAddress)
	}
	observation.Boundary = continuationBoundaryProviderHook
	observation.Source = continuationSourcePreCompact
	observation.HookEvent = "PreCompact"
	observation.HookSource = os.Getenv("GC_HOOK_SOURCE")
	observation.Route = hookFormat
	b, suppressed, ok := createHandoffMailWithDedupAs(pinned, msgStore, sessStore, rec, sessionAddress, sessionAddress, args, "context cycle", []string{
		mail.AutoHandoffLabel,
		mail.ArchiveAfterInjectLabel,
		"priority:1",
	}, "auto-handoff:"+sessionAddress, stderr)
	if !ok {
		observation.Outcome = continuationOutcomeFailed
		observation.ErrorCode = continuationErrorHandoffMail
		recordContinuationObservation(rec, observation)
		return 1
	}
	observation.MailIDs = []string{b.ID}
	observation.MessageCount = continuationInt(1)
	observation.BodyBytes = continuationInt(len([]byte(b.Body)))
	message := fmt.Sprintf("Handoff: sent auto mail %s (restart skipped).\n", b.ID)
	if suppressed {
		message = fmt.Sprintf("Handoff: auto mail %s already pending.\n", b.ID)
	}
	if err := writeProviderHookContextForEvent(stdout, hookFormat, "PreCompact", message); err != nil {
		observation.Outcome = continuationOutcomeFailed
		observation.ErrorCode = continuationErrorHookOutput
		recordContinuationObservation(rec, observation)
		fmt.Fprintf(stderr, "gc handoff: writing hook output: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	observation.Outcome = continuationOutcomeObserved
	recordContinuationObservation(rec, observation)
	return 0
}

// createHandoffMail produces a handoff message through the mail.Provider domain
// seam. The handoff command speaks mail.Message; the message-bead serialization
// (Type="message", thread label, extra labels, sender-route metadata) is
// confined inside beadmail.Provider.SendHandoff. The returned mail.Message
// carries the assigned ID for the caller's confirmation output.
func createHandoffMail(msgStore, sessStore beads.Store, rec events.Recorder, senderAddress, recipientAddress string, args []string, defaultSubject string, extraLabels []string, stderr io.Writer) (mail.Message, bool) {
	var pinned *hookIdentity
	if identity, ok, err := resolveHookIdentity(sessStore); err == nil && ok {
		pinned = &identity
	}
	msg, _, ok := createHandoffMailWithDedupAs(pinned, msgStore, sessStore, rec, senderAddress, recipientAddress, args, defaultSubject, extraLabels, "", stderr)
	return msg, ok
}

// createHandoffMailWithDedupAs is createHandoffMail for a caller that has
// already resolved the calling session (pinned, nil when it has none), so the
// sender is not resolved a second time. A non-empty dedupKey suppresses a
// second mail while one under the key is pending.
func createHandoffMailWithDedupAs(pinned *hookIdentity, msgStore, sessStore beads.Store, rec events.Recorder, senderAddress, recipientAddress string, args []string, defaultSubject string, extraLabels []string, dedupKey string, stderr io.Writer) (mail.Message, bool, bool) {
	subject := defaultSubject
	if len(args) > 0 {
		subject = args[0]
	}
	var message string
	if len(args) > 1 {
		message = args[1]
	}

	// Handoff intentionally constructs the concrete bead-backed provider rather
	// than resolving the configured mail provider (GC_MAIL / city.toml): handoff
	// needs the thread label and handoff-specific extra-labels that SendHandoff
	// expresses, which aren't part of the generic provider surface. Built as a
	// two-store provider (mirroring newCityMailProvider): the message bead is
	// ClassMessaging, beadmail's addressing reads are ClassSessions.
	provider := beadmail.NewWithStores(beads.MailStore{Store: msgStore}, beads.SessionStore{Store: sessStore})
	// Handoff mail is sent from the calling session. When its own bead id
	// resolves, the provider takes the sender route from that bead instead of
	// resolving senderAddress again. A failed lookup pins nothing: the sender
	// resolution below asks the store itself and reports what it finds.
	if pinned != nil {
		pinned.pinSender(provider)
	}
	msg, suppressed, err := provider.SendHandoff(mail.HandoffIntent{
		From:        senderAddress,
		To:          recipientAddress,
		Subject:     subject,
		Body:        message,
		ThreadID:    handoffThreadID(),
		ExtraLabels: extraLabels,
		DedupKey:    dedupKey,
	})
	if err != nil {
		fmt.Fprintf(stderr, "gc handoff: creating mail: %v\n", err) //nolint:errcheck // best-effort stderr
		return mail.Message{}, false, false
	}
	if suppressed {
		return msg, true, true
	}
	rec.Record(events.Event{
		Type:    events.MailSent,
		Actor:   msg.From,
		Subject: msg.ID,
		Message: recipientAddress,
		Payload: mailEventPayload(nil),
	})
	return msg, false, true
}

// sessionRestartableByController reports whether the controller is willing to
// restart the named session: every session but a named one outside "always"
// mode. A session with no row is restartable.
func sessionRestartableByController(sessStore beads.Store, sessionName string) (bool, error) {
	if sessStore == nil || sessionName == "" {
		return true, nil
	}
	id, err := resolveSessionID(sessStore, sessionName)
	if err != nil {
		if errors.Is(err, session.ErrSessionNotFound) {
			return true, nil
		}
		return false, fmt.Errorf("resolving session %q: %w", sessionName, err)
	}
	b, err := sessStore.Get(id)
	if err != nil {
		return false, fmt.Errorf("loading session %q: %w", id, err)
	}
	if !isNamedSessionBead(b) {
		return true, nil
	}
	return namedSessionMode(b) == "always", nil
}

func clearRestartRequest(sessStore beads.Store, dops drainOps, sessionName string) error {
	if sessionName == "" {
		return nil
	}
	var errs []error
	if dops != nil {
		// A worker site's guard holds no runtime flag to clear; the bead
		// marker below is the only one there.
		if err := dops.clearRestartRequested(sessionName); err != nil && !restartFlagNotApplicable(err) {
			errs = append(errs, fmt.Errorf("clearing runtime restart flag: %w", err))
		}
	}
	if sessStore == nil {
		return errors.Join(errs...)
	}
	id, err := resolveSessionID(sessStore, sessionName)
	if err != nil {
		if errors.Is(err, session.ErrSessionNotFound) {
			return errors.Join(errs...)
		}
		errs = append(errs, fmt.Errorf("resolving session %q: %w", sessionName, err))
		return errors.Join(errs...)
	}
	if err := sessionFrontDoor(sessStore).ApplyPatch(id, map[string]string{
		"restart_requested":          "",
		"continuation_reset_pending": "",
	}); err != nil {
		errs = append(errs, fmt.Errorf("clearing bead restart flag: %w", err))
	}
	return errors.Join(errs...)
}

func doHandoffRemoteWithForce(cityPath string, msgStore, sessStore beads.Store, rec events.Recorder, sp runtime.Provider,
	sessionName, targetAddress, sender string, args []string, force bool, stdout, stderr io.Writer,
) int {
	restartable, err := sessionRestartableByController(sessStore, sessionName)
	if err != nil {
		fmt.Fprintf(stderr, "gc handoff: checking session type: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	// Decide whether the kill may proceed before any mail is created: a
	// refusal that has already sent the handoff would be delivered twice
	// once the operator retries with --force.
	running := false
	if restartable {
		running, err = workerSessionTargetRunningWithConfig("", sessStore, sp, nil, sessionName)
		if err != nil {
			fmt.Fprintf(stderr, "gc handoff: observing %s: %v\n", targetAddress, err) //nolint:errcheck // best-effort stderr
			return 1
		}
		if running && !force && refuseKillForLiveSubagents("gc handoff", workerHandleForSessionTargetWithConfig, "", sessStore, sp, nil, sessionName, stderr) {
			return 1
		}
	}

	b, ok := createHandoffMail(msgStore, sessStore, rec, sender, targetAddress, args, "HANDOFF: context cycle", []string{"priority:1"}, stderr)
	if !ok {
		return 1
	}

	if !restartable {
		if err := clearRestartRequest(sessStore, newDrainOps(sp), sessionName); err != nil {
			fmt.Fprintf(stderr, "gc handoff: clearing stale restart request: %v\n", err) //nolint:errcheck // best-effort stderr
			return 1
		}
		fmt.Fprintf(stdout, "Handoff: sent mail %s to %s (named session; kill skipped because the controller cannot restart it)\n", b.ID, targetAddress) //nolint:errcheck // best-effort stdout
		return 0
	}
	if !running {
		fmt.Fprintf(stdout, "Handoff: sent mail %s to %s (session not running; will be delivered on next start)\n", b.ID, targetAddress) //nolint:errcheck // best-effort stdout
		return 0
	}
	if !force && refuseKillForLiveSubagents("gc handoff", workerHandleForSessionTargetWithConfig, "", sessStore, sp, nil, sessionName, stderr) {
		return 1
	}
	// Resolve the agent identity before the kill, while the session bead is
	// still live. The metric label uses the agent identity (not the sanitized
	// runtime session name) so handoff stops join the start/crash/kill counters.
	agentIdentity := sessionAgentMetricIdentityByName(sessStore, sessionName)
	continuation := handoffContinuationObservation(sessStore, sessionName)
	continuation.Boundary = continuationBoundaryRuntimeStop
	if err := workerKillSessionTargetWithConfig(cityPath, sessStore, sp, nil, sessionName); err != nil {
		continuation.Outcome = continuationOutcomeFailed
		continuation.ErrorCode = continuationErrorRuntimeStop
		recordContinuationObservation(rec, continuation)
		fmt.Fprintf(stderr, "gc handoff: killing %s: %v\n", targetAddress, err) //nolint:errcheck // best-effort stderr
		return 1
	}
	sessionID, resolveErr := resolveSessionID(sessStore, sessionName)
	if resolveErr != nil {
		// The session was just killed; resolution can fail if its bead
		// has been closed mid-flight. Fall back to the runtime name so
		// subscribers still get a usable correlation key.
		sessionID = sessionName
	}
	rec.Record(events.Event{
		Type:    events.SessionStopped,
		Actor:   sender,
		Subject: targetAddress,
		Message: "handoff",
		Payload: api.SessionLifecyclePayloadJSON(sessionID, "", "handoff"),
	})
	continuation.Outcome = continuationOutcomeSucceeded
	recordContinuationObservation(rec, continuation)
	telemetry.RecordAgentStop(context.Background(), sessionName, agentIdentity, "handoff", nil)

	fmt.Fprintf(stdout, "Handoff: sent mail %s to %s, killed session (reconciler will restart)\n", b.ID, targetAddress) //nolint:errcheck // best-effort stdout
	return 0
}

// handoffThreadID generates a unique thread ID for handoff messages.
func handoffThreadID() string {
	b := make([]byte, 6)
	rand.Read(b) //nolint:errcheck
	return fmt.Sprintf("thread-%x", b)
}
