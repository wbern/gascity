package usage

import (
	"context"
	"path/filepath"
	"testing"
)

func TestInvocationLedgerCompletesPersistedBindingWithoutCurrentAssignment(t *testing.T) {
	ctx := context.Background()
	binding, err := NewBoundInvocationAttribution("gc2", "gas-city", "work-original", "1", "invoke-1", Head{State: HeadClean, SHA: "start"})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "invocations.jsonl")
	ledger := NewInvocationLedger(path)
	if err := ledger.Bind(ctx, InvocationRecord{SessionID: "session-1", Attribution: binding}); err != nil {
		t.Fatalf("Bind: %v", err)
	}

	// A session can be reassigned before delayed transcript recovery runs. The
	// caller only supplies the stable session+invocation pair; there is no API
	// that accepts a current work bead, name, workdir, or timestamp to resolve.
	completed, err := NewInvocationLedger(path).Complete(ctx, "session-1", "invoke-1", "provider-response-1", Head{State: HeadDirty, SHA: "finish"})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if completed.Attribution.WorkBeadID != "work-original" {
		t.Fatalf("WorkBeadID = %q, want original binding", completed.Attribution.WorkBeadID)
	}
	if completed.Attribution.StartHead != (Head{State: HeadClean, SHA: "start"}) {
		t.Fatalf("StartHead = %+v", completed.Attribution.StartHead)
	}
	if completed.Attribution.CompletionHead != (Head{State: HeadDirty, SHA: "finish"}) {
		t.Fatalf("CompletionHead = %+v", completed.Attribution.CompletionHead)
	}
	if completed.UpstreamRequestID != "provider-response-1" {
		t.Fatalf("UpstreamRequestID = %q", completed.UpstreamRequestID)
	}

	// Completion replay is the same immutable record, not a new attribution or
	// a fresh record whose later values overwrite the original binding.
	replayed, err := NewInvocationLedger(path).Complete(ctx, "session-1", "invoke-1", "provider-response-1", Head{State: HeadDirty, SHA: "finish"})
	if err != nil {
		t.Fatalf("replay Complete: %v", err)
	}
	if replayed != completed {
		t.Fatalf("replayed record = %+v, want %+v", replayed, completed)
	}
}

func TestFactForCompletedInvocationRequiresExactProviderIdentity(t *testing.T) {
	record := InvocationRecord{
		SessionID: "session-1",
		Attribution: InvocationAttribution{
			State:          AttributionBound,
			City:           "gc2",
			Rig:            "gas-city",
			WorkBeadID:     "work-1",
			Attempt:        "1",
			InvocationID:   "turn-1",
			StartHead:      Head{State: HeadClean, SHA: "start"},
			CompletionHead: Head{State: HeadDirty, SHA: "finish"},
		},
		UpstreamRequestID: "provider-response-1",
	}

	fact, err := FactForCompletedInvocation(Fact{Kind: KindModel, UpstreamReqID: "provider-response-1"}, record)
	if err != nil {
		t.Fatalf("FactForCompletedInvocation: %v", err)
	}
	if fact.AttributionV2 == nil || *fact.AttributionV2 != record.Attribution {
		t.Fatalf("Fact attribution = %+v, want %+v", fact.AttributionV2, record.Attribution)
	}
	if _, err := FactForCompletedInvocation(Fact{Kind: KindModel, UpstreamReqID: "lookalike"}, record); err == nil {
		t.Fatal("lookalike upstream identity was attributed")
	}
}
