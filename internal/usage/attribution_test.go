package usage

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestBoundInvocationAttributionFreezesStartAndCompletionHeads(t *testing.T) {
	start := Head{State: HeadClean, SHA: "start-sha"}
	binding, err := NewBoundInvocationAttribution("gc2", "gas-city", "work-1", "1", "invoke-1", start)
	if err != nil {
		t.Fatalf("NewBoundInvocationAttribution: %v", err)
	}

	completed, err := binding.WithCompletionHead(Head{State: HeadDirty, SHA: "completion-sha"})
	if err != nil {
		t.Fatalf("WithCompletionHead: %v", err)
	}
	if completed.State != AttributionBound {
		t.Fatalf("State = %q, want %q", completed.State, AttributionBound)
	}
	if completed.WorkBeadID != "work-1" || completed.Attempt != "1" || completed.InvocationID != "invoke-1" {
		t.Fatalf("binding identity changed: %+v", completed)
	}
	if completed.StartHead != start {
		t.Fatalf("StartHead = %+v, want original %+v", completed.StartHead, start)
	}
	if completed.CompletionHead != (Head{State: HeadDirty, SHA: "completion-sha"}) {
		t.Fatalf("CompletionHead = %+v", completed.CompletionHead)
	}
}

func TestBoundAttributionRejectsUnboundedIdentity(t *testing.T) {
	_, err := NewBoundInvocationAttribution(strings.Repeat("c", 257), "gas-city", "work-1", "1", "invoke-1", Head{State: HeadClean, SHA: "start"})
	if err == nil {
		t.Fatal("unbounded city identity was accepted")
	}
}

func TestFactCarriesAdditiveV2Attribution(t *testing.T) {
	binding, err := NewBoundInvocationAttribution("gc2", "gas-city", "work-1", "1", "invoke-1", Head{State: HeadClean, SHA: "start"})
	if err != nil {
		t.Fatal(err)
	}
	completed, err := binding.WithCompletionHead(Head{State: HeadClean, SHA: "finish"})
	if err != nil {
		t.Fatal(err)
	}

	encoded, err := json.Marshal(Fact{Kind: KindModel, AttributionV2: &completed})
	if err != nil {
		t.Fatal(err)
	}
	var wire map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &wire); err != nil {
		t.Fatal(err)
	}
	if _, ok := wire["attribution_v2"]; !ok {
		t.Fatalf("encoded Fact missing additive attribution_v2: %s", encoded)
	}
	if _, ok := wire["run_id"]; ok {
		t.Fatalf("v2 attribution must not manufacture a v1 run id: %s", encoded)
	}
}

func TestUnboundAttributionDoesNotSerializeEmptyHeadObjects(t *testing.T) {
	unbound := UnboundInvocationAttribution()
	encoded, err := json.Marshal(unbound)
	if err != nil {
		t.Fatal(err)
	}
	var wire map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &wire); err != nil {
		t.Fatal(err)
	}
	if _, ok := wire["start_head"]; ok {
		t.Fatalf("unbound attribution serialized empty start head: %s", encoded)
	}
	if _, ok := wire["completion_head"]; ok {
		t.Fatalf("unbound attribution serialized empty completion head: %s", encoded)
	}
}
