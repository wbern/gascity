package main

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/gastownhall/gascity/internal/events"
)

type continuationObservationRecorder struct {
	recorded []events.Event
}

func (r *continuationObservationRecorder) Record(event events.Event) {
	r.recorded = append(r.recorded, event)
}

func TestRecordContinuationObservationRedactsAndBoundsProviderData(t *testing.T) {
	rec := &continuationObservationRecorder{}
	rawToken := "secret-instance-token"
	rawHookSource := strings.Repeat("å", 80)

	recordContinuationObservation(rec, continuationObservation{
		Boundary:          continuationBoundaryReset,
		Source:            continuationSourceSessionReconciler,
		Outcome:           continuationOutcomeCommitted,
		SessionID:         "session-1",
		SessionName:       "rig/worker",
		Template:          "worker",
		Generation:        "0",
		ContinuationEpoch: "4",
		InstanceToken:     rawToken,
		HookSource:        rawHookSource,
	})

	if len(rec.recorded) != 1 {
		t.Fatalf("recorded %d events, want 1", len(rec.recorded))
	}
	gotEvent := rec.recorded[0]
	if gotEvent.Type != events.SessionContinuationObserved {
		t.Fatalf("event type = %q, want %q", gotEvent.Type, events.SessionContinuationObserved)
	}
	if gotEvent.Actor != "gc" || gotEvent.Subject != "rig/worker" || gotEvent.SessionID != "session-1" {
		t.Fatalf("event envelope = %#v", gotEvent)
	}
	if strings.Contains(string(gotEvent.Payload), rawToken) {
		t.Fatal("payload contains the raw instance token")
	}

	decoded, registered, err := events.DecodePayload(gotEvent.Type, gotEvent.Payload)
	if err != nil {
		t.Fatalf("DecodePayload: %v", err)
	}
	if !registered {
		t.Fatal("continuation payload is not registered")
	}
	payload, ok := decoded.(events.SessionContinuationObservedPayload)
	if !ok {
		t.Fatalf("decoded payload type = %T", decoded)
	}
	if payload.Generation != "0" || payload.ContinuationEpoch != "4" {
		t.Fatalf("identity fields = generation %q epoch %q", payload.Generation, payload.ContinuationEpoch)
	}
	if payload.MessageCount != nil || payload.BodyBytes != nil {
		t.Fatalf("reset-only count fields = message_count %v body_bytes %v, want absent", payload.MessageCount, payload.BodyBytes)
	}
	if payload.InstanceTokenFingerprint == "" || payload.InstanceTokenFingerprint == rawToken {
		t.Fatalf("token fingerprint = %q", payload.InstanceTokenFingerprint)
	}
	if len(payload.HookSource) > continuationHookSourceMaxBytes {
		t.Fatalf("hook source length = %d, want <= %d", len(payload.HookSource), continuationHookSourceMaxBytes)
	}
	if !utf8.ValidString(payload.HookSource) {
		t.Fatalf("hook source is not valid UTF-8: %q", payload.HookSource)
	}
}

func TestRecordContinuationObservationNilRecorderIsNoop(_ *testing.T) {
	recordContinuationObservation(nil, continuationObservation{
		Boundary: continuationBoundaryReset,
		Source:   continuationSourceExplicitReset,
		Outcome:  continuationOutcomeFailed,
	})
}

func BenchmarkRecordContinuationObservation(b *testing.B) {
	observation := continuationObservation{
		Boundary:          continuationBoundaryMailInjection,
		Source:            continuationSourceUserPromptSubmit,
		Outcome:           continuationOutcomeInjected,
		SessionID:         "session-id-123",
		SessionName:       "demo/planner",
		Template:          "architect",
		Generation:        "63",
		ContinuationEpoch: "12",
		InstanceToken:     "opaque-instance-token",
		HookEvent:         "UserPromptSubmit",
		HookSource:        "codex-provider",
		MailIDs:           []string{"mail-1"},
		MessageCount:      continuationInt(1),
		BodyBytes:         continuationInt(512),
		Route:             "fallback",
	}
	sample := &continuationObservationRecorder{}
	recordContinuationObservation(sample, observation)
	payloadBytes := len(sample.recorded[0].Payload)
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		recordContinuationObservation(events.Discard, observation)
	}
	b.ReportMetric(float64(payloadBytes), "payload_bytes")
}
