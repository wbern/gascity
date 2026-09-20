package events

import (
	"encoding/json"
	"testing"
)

func TestAdmissionVetoRegistered(t *testing.T) {
	// Must be in KnownEventTypes
	found := false
	for _, et := range KnownEventTypes {
		if et == AdmissionVeto {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("%q not found in KnownEventTypes", AdmissionVeto)
	}

	sample, ok := LookupPayload(AdmissionVeto)
	if !ok {
		t.Fatalf("LookupPayload(%q) returned false", AdmissionVeto)
	}
	if _, ok := sample.(AdmissionVetoPayload); !ok {
		t.Fatalf("expected AdmissionVetoPayload, got %T", sample)
	}
}

func TestAdmissionVetoPayloadDecode(t *testing.T) {
	raw := json.RawMessage(`{
		"signal": "memory_psi_avg10",
		"value": "1.50",
		"threshold": "1.00",
		"pool": "crm/gastown.polecat",
		"reason": "memory_psi_avg10=1.50_exceeds_1.00"
	}`)

	got, registered, err := DecodePayload(AdmissionVeto, raw)
	if err != nil {
		t.Fatalf("DecodePayload error: %v", err)
	}
	if !registered {
		t.Fatal("expected registered=true")
	}
	payload, ok := got.(AdmissionVetoPayload)
	if !ok {
		t.Fatalf("expected AdmissionVetoPayload, got %T", got)
	}
	if payload.Signal != "memory_psi_avg10" {
		t.Errorf("Signal = %q, want %q", payload.Signal, "memory_psi_avg10")
	}
	if payload.Value != "1.50" {
		t.Errorf("Value = %q, want %q", payload.Value, "1.50")
	}
	if payload.Threshold != "1.00" {
		t.Errorf("Threshold = %q, want %q", payload.Threshold, "1.00")
	}
	if payload.Pool != "crm/gastown.polecat" {
		t.Errorf("Pool = %q, want %q", payload.Pool, "crm/gastown.polecat")
	}
	if payload.Reason != "memory_psi_avg10=1.50_exceeds_1.00" {
		t.Errorf("Reason = %q, want %q", payload.Reason, "memory_psi_avg10=1.50_exceeds_1.00")
	}
}

func TestParseAdmissionVetoReason(t *testing.T) {
	tests := []struct {
		reason    string
		wantSig   string
		wantVal   string
		wantThres string
	}{
		{
			reason:    "memory_psi_avg10=1.50_exceeds_1.00",
			wantSig:   "memory_psi_avg10",
			wantVal:   "1.50",
			wantThres: "1.00",
		},
		{
			reason:    "swap_free_kb=1000000_below_2000000",
			wantSig:   "swap_free_kb",
			wantVal:   "1000000",
			wantThres: "2000000",
		},
		{
			reason:    "load5=15.0_exceeds_12.0",
			wantSig:   "load5",
			wantVal:   "15.0",
			wantThres: "12.0",
		},
		{
			reason:    "quota_exceeded",
			wantSig:   "quota_exceeded",
			wantVal:   "",
			wantThres: "",
		},
		{
			reason:    "provider_refusal: rate_limit_reached",
			wantSig:   "provider_refusal: rate_limit_reached",
			wantVal:   "",
			wantThres: "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.reason, func(t *testing.T) {
			sig, val, thres := ParseAdmissionVetoReason(tc.reason)
			if sig != tc.wantSig {
				t.Errorf("signal = %q, want %q", sig, tc.wantSig)
			}
			if val != tc.wantVal {
				t.Errorf("value = %q, want %q", val, tc.wantVal)
			}
			if thres != tc.wantThres {
				t.Errorf("threshold = %q, want %q", thres, tc.wantThres)
			}
		})
	}
}

func TestNewAdmissionVetoPayload(t *testing.T) {
	payload := NewAdmissionVetoPayload("worker", "memory_psi_avg10=1.50_exceeds_1.00")
	if payload.Pool != "worker" {
		t.Errorf("Pool = %q, want worker", payload.Pool)
	}
	if payload.Signal != "memory_psi_avg10" {
		t.Errorf("Signal = %q, want memory_psi_avg10", payload.Signal)
	}
	if payload.Value != "1.50" {
		t.Errorf("Value = %q, want 1.50", payload.Value)
	}
	if payload.Threshold != "1.00" {
		t.Errorf("Threshold = %q, want 1.00", payload.Threshold)
	}
	if payload.Reason != "memory_psi_avg10=1.50_exceeds_1.00" {
		t.Errorf("Reason = %q, want memory_psi_avg10=1.50_exceeds_1.00", payload.Reason)
	}
}
