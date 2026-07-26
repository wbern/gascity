package events

import (
	"encoding/json"
	"testing"
)

type samplePayload struct {
	A string `json:"a"`
}

func (samplePayload) IsEventPayload() {}

func TestRegisterAndLookup(t *testing.T) {
	const evt = "test.register.lookup"
	// Clean up after the test to avoid polluting global registry.
	t.Cleanup(func() {
		payloadRegistryMu.Lock()
		delete(payloadRegistry, evt)
		payloadRegistryMu.Unlock()
	})

	RegisterPayload(evt, samplePayload{})
	got, ok := LookupPayload(evt)
	if !ok {
		t.Fatalf("expected registered event %q to be found", evt)
	}
	if _, ok := got.(samplePayload); !ok {
		t.Fatalf("expected samplePayload, got %T", got)
	}
}

func TestDecodePayloadRegistered(t *testing.T) {
	const evt = "test.decode.registered"
	t.Cleanup(func() {
		payloadRegistryMu.Lock()
		delete(payloadRegistry, evt)
		payloadRegistryMu.Unlock()
	})
	RegisterPayload(evt, samplePayload{})

	raw := json.RawMessage(`{"a":"hello"}`)
	got, registered, err := DecodePayload(evt, raw)
	if err != nil {
		t.Fatalf("decode err: %v", err)
	}
	if !registered {
		t.Fatalf("expected registered=true")
	}
	sp, ok := got.(samplePayload)
	if !ok {
		t.Fatalf("expected samplePayload, got %T", got)
	}
	if sp.A != "hello" {
		t.Fatalf("A = %q, want hello", sp.A)
	}
}

func TestDecodePayloadUnregistered(t *testing.T) {
	raw := json.RawMessage(`{"anything":true}`)
	got, registered, err := DecodePayload("test.never.registered", raw)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if registered {
		t.Fatalf("expected registered=false")
	}
	if got != nil {
		t.Fatalf("expected nil payload, got %v", got)
	}
}

func TestDecodePayloadEmptyBytesZeroValue(t *testing.T) {
	const evt = "test.decode.empty"
	t.Cleanup(func() {
		payloadRegistryMu.Lock()
		delete(payloadRegistry, evt)
		payloadRegistryMu.Unlock()
	})
	RegisterPayload(evt, NoPayload{})

	got, registered, err := DecodePayload(evt, nil)
	if err != nil {
		t.Fatalf("decode err: %v", err)
	}
	if !registered {
		t.Fatalf("expected registered=true")
	}
	if _, ok := got.(NoPayload); !ok {
		t.Fatalf("expected NoPayload, got %T", got)
	}
}

func TestRegisterConflictPanics(t *testing.T) {
	const evt = "test.conflict"
	t.Cleanup(func() {
		payloadRegistryMu.Lock()
		delete(payloadRegistry, evt)
		payloadRegistryMu.Unlock()
	})
	RegisterPayload(evt, samplePayload{})

	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic on conflicting re-registration")
		}
	}()
	RegisterPayload(evt, NoPayload{})
}

func TestRegisterSameTypeIdempotent(t *testing.T) {
	const evt = "test.idempotent"
	t.Cleanup(func() {
		payloadRegistryMu.Lock()
		delete(payloadRegistry, evt)
		payloadRegistryMu.Unlock()
	})
	RegisterPayload(evt, samplePayload{})
	// Second call with same type must not panic.
	RegisterPayload(evt, samplePayload{})
}

func TestSessionContinuationObservedPayloadContract(t *testing.T) {
	zero := 0
	sample, ok := LookupPayload(SessionContinuationObserved)
	if !ok {
		t.Fatalf("payload for %q is not registered", SessionContinuationObserved)
	}
	if _, ok := sample.(SessionContinuationObservedPayload); !ok {
		t.Fatalf("payload for %q has type %T, want SessionContinuationObservedPayload", SessionContinuationObserved, sample)
	}

	raw := SessionContinuationObservedPayloadJSON(SessionContinuationObservedPayload{
		SchemaVersion:            "1",
		Boundary:                 "reset",
		Source:                   "session_reconciler",
		Outcome:                  "committed",
		SessionName:              "rig/worker",
		Template:                 "worker",
		Generation:               "0",
		ContinuationEpoch:        "4",
		InstanceTokenFingerprint: "sha256:0123456789abcdef",
		HookEvent:                "PreCompact",
		HookSource:               "provider",
		OldWorkID:                "work-old",
		NewWorkID:                "work-new",
		MailIDs:                  []string{"mail-1", "mail-2"},
		MessageCount:             &zero,
		BodyBytes:                &zero,
		Route:                    "local",
		ErrorCode:                "runtime_stop_failed",
	})

	const want = `{"schema_version":"1","boundary":"reset","source":"session_reconciler","outcome":"committed","session_name":"rig/worker","template":"worker","generation":"0","continuation_epoch":"4","instance_token_fingerprint":"sha256:0123456789abcdef","hook_event":"PreCompact","hook_source":"provider","old_work_id":"work-old","new_work_id":"work-new","mail_ids":["mail-1","mail-2"],"message_count":0,"body_bytes":0,"route":"local","error_code":"runtime_stop_failed"}`
	if string(raw) != want {
		t.Fatalf("payload JSON = %s, want %s", raw, want)
	}
}
