package main

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"unicode/utf8"

	"github.com/gastownhall/gascity/internal/events"
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
