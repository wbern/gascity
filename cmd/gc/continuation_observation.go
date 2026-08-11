package main

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"strings"
	"unicode/utf8"

	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/mail"
)

const (
	continuationObservationSchemaVersion = "1"
	continuationHookSourceMaxBytes       = 64

	continuationBoundaryReset         = "reset"
	continuationBoundaryRuntimeStop   = "runtime_stop"
	continuationBoundaryRuntimeStart  = "runtime_start"
	continuationBoundaryProviderHook  = "provider_hook"
	continuationBoundaryMailInjection = "mail_injection"

	continuationSourceExplicitReset     = "explicit_reset"
	continuationSourceHandoff           = "handoff"
	continuationSourceSessionReconciler = "session_reconciler"
	continuationSourceConfigDrift       = "config_drift"
	continuationSourcePreCompact        = "pre_compact"
	continuationSourceSessionStart      = "session_start"
	continuationSourceUserPromptSubmit  = "user_prompt_submit"

	continuationOutcomeObserved  = "observed"
	continuationOutcomeRequested = "requested"
	continuationOutcomeCommitted = "committed"
	continuationOutcomeSucceeded = "succeeded"
	continuationOutcomeFailed    = "failed"
	continuationOutcomeInjected  = "injected"
	continuationOutcomeEmpty     = "empty"
	continuationOutcomeSkipped   = "skipped"

	continuationErrorRuntimeStop    = "runtime_stop_failed"
	continuationErrorCircuitReset   = "circuit_reset_failed"
	continuationErrorMetadataWrite  = "metadata_write_failed"
	continuationErrorRuntimeStart   = "runtime_start_failed"
	continuationErrorResetRequest   = "reset_request_failed"
	continuationErrorHookOutput     = "hook_output_failed"
	continuationErrorHookProcessing = "hook_processing_failed"
	continuationErrorHandoffMail    = "handoff_mail_failed"
	continuationErrorMailCheck      = "mail_check_failed"
	continuationErrorMailDegraded   = "mail_read_degraded"
	continuationErrorMailState      = "mail_state_failed"
)

type continuationObservation struct {
	Boundary          string
	Source            string
	Outcome           string
	SessionID         string
	SessionName       string
	Template          string
	Generation        string
	ContinuationEpoch string
	InstanceToken     string
	HookEvent         string
	HookSource        string
	OldWorkID         string
	NewWorkID         string
	MailIDs           []string
	MessageCount      *int
	BodyBytes         *int
	Route             string
	ErrorCode         string
}

// mailInjectionObservation accumulates the outcome of one `gc mail check
// --inject` pass so the mail-injection continuation boundary can be recorded
// once, on exit, from whichever path the check took. It lives here rather than
// in cmd_mail.go to keep this fork-owned diagnostic out of an upstream-owned
// file; cmd_mail.go carries only a nil-able parameter and the field writes.
//
// A nil *mailInjectionObservation is the non-injecting case and every method is
// a no-op on it, so non-hook callers stay on exactly the upstream code path.
type mailInjectionObservation struct {
	outcome      string
	route        string
	errorCode    string
	mailIDs      []string
	messageCount int
	bodyBytes    int
}

// fail marks the pass as failed with code, leaving any route already recorded
// by the caller intact.
func (o *mailInjectionObservation) fail(code string) {
	if o == nil {
		return
	}
	o.outcome = continuationOutcomeFailed
	o.errorCode = code
}

// injected records the message set actually written into the hook output and
// marks the pass as injected. The caller reports failure afterwards if the
// write itself fails, which overwrites this outcome.
func (o *mailInjectionObservation) injected(messages []mail.Message, text string) {
	if o == nil {
		return
	}
	o.mailIDs = make([]string, 0, len(messages))
	for _, message := range messages {
		o.mailIDs = append(o.mailIDs, message.ID)
	}
	o.messageCount = len(messages)
	o.bodyBytes = len(text)
	o.outcome = continuationOutcomeInjected
}

// skip marks the pass as deliberately not injecting (for example a suspended
// city), recording why as the route.
func (o *mailInjectionObservation) skip(route string) {
	if o == nil {
		return
	}
	o.outcome = continuationOutcomeSkipped
	o.route = route
}

// record emits the accumulated observation. Like recordContinuationObservation
// it has no return value: mail injection must not depend on whether the
// diagnostic was written.
func (o *mailInjectionObservation) record(rec events.Recorder) {
	if o == nil {
		return
	}
	recordContinuationObservation(rec, continuationObservation{
		Boundary:          continuationBoundaryMailInjection,
		Source:            continuationSourceUserPromptSubmit,
		Outcome:           o.outcome,
		SessionID:         os.Getenv("GC_SESSION_ID"),
		SessionName:       os.Getenv("GC_SESSION_NAME"),
		Template:          os.Getenv("GC_TEMPLATE"),
		Generation:        os.Getenv("GC_RUNTIME_EPOCH"),
		ContinuationEpoch: os.Getenv("GC_CONTINUATION_EPOCH"),
		InstanceToken:     os.Getenv("GC_INSTANCE_TOKEN"),
		HookEvent:         "UserPromptSubmit",
		HookSource:        os.Getenv("GC_HOOK_SOURCE"),
		MailIDs:           o.mailIDs,
		MessageCount:      continuationInt(o.messageCount),
		BodyBytes:         continuationInt(o.bodyBytes),
		Route:             o.route,
		ErrorCode:         o.errorCode,
	})
}

// recordContinuationObservation emits a best-effort diagnostic event. It has
// no return value by design: callers cannot make continuation behavior depend
// on whether observation succeeds.
func recordContinuationObservation(rec events.Recorder, observation continuationObservation) {
	if rec == nil {
		return
	}

	subject := strings.TrimSpace(observation.SessionName)
	if subject == "" {
		subject = strings.TrimSpace(observation.SessionID)
	}
	rec.Record(events.Event{
		Type:      events.SessionContinuationObserved,
		Actor:     "gc",
		Subject:   subject,
		SessionID: strings.TrimSpace(observation.SessionID),
		Payload: events.SessionContinuationObservedPayloadJSON(events.SessionContinuationObservedPayload{
			SchemaVersion:            continuationObservationSchemaVersion,
			Boundary:                 observation.Boundary,
			Source:                   observation.Source,
			Outcome:                  observation.Outcome,
			SessionName:              strings.TrimSpace(observation.SessionName),
			Template:                 strings.TrimSpace(observation.Template),
			Generation:               strings.TrimSpace(observation.Generation),
			ContinuationEpoch:        strings.TrimSpace(observation.ContinuationEpoch),
			InstanceTokenFingerprint: continuationTokenFingerprint(observation.InstanceToken),
			HookEvent:                strings.TrimSpace(observation.HookEvent),
			HookSource:               boundedContinuationHookSource(observation.HookSource),
			OldWorkID:                strings.TrimSpace(observation.OldWorkID),
			NewWorkID:                strings.TrimSpace(observation.NewWorkID),
			MailIDs:                  append([]string(nil), observation.MailIDs...),
			MessageCount:             observation.MessageCount,
			BodyBytes:                observation.BodyBytes,
			Route:                    strings.TrimSpace(observation.Route),
			ErrorCode:                strings.TrimSpace(observation.ErrorCode),
		}),
	})
}

func continuationTokenFingerprint(token string) string {
	token = strings.TrimSpace(token)
	if token == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(token))
	return "sha256:" + hex.EncodeToString(sum[:])
}

func continuationInt(value int) *int {
	return &value
}

func boundedContinuationHookSource(source string) string {
	source = strings.TrimSpace(strings.ToValidUTF8(source, "\uFFFD"))
	if len(source) <= continuationHookSourceMaxBytes {
		return source
	}
	source = source[:continuationHookSourceMaxBytes]
	for !utf8.ValidString(source) {
		source = source[:len(source)-1]
	}
	return source
}
