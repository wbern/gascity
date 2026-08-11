package main

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestWriteHookClaimJSONReplacesOversizedManagedResponse(t *testing.T) {
	t.Setenv("GC_MANAGED_OUTPUT_FIREWALL", "1")
	t.Setenv("GC_MANAGED_OUTPUT_FIREWALL_BUDGET", "512")
	t.Setenv("GC_MANAGED_OUTPUT_FIREWALL_READ_VERBS", "hook")
	t.Setenv("GC_MANAGED_OUTPUT_FIREWALL_SPILL_MODE", "disabled")

	result := hookClaimJSONResult{
		SchemaVersion:        "1",
		OK:                   true,
		Command:              hookClaimCommandName,
		Action:               "work",
		ContinuationAssigned: []string{strings.Repeat("hook-secret", 200)},
	}
	var stdout, stderr bytes.Buffer
	if err := writeHookClaimJSON(context.Background(), &stdout, &stderr, result); err != nil {
		t.Fatalf("writeHookClaimJSON(): %v; stderr=%q", err, stderr.String())
	}
	if stdout.Len() > 512 || !json.Valid(stdout.Bytes()) {
		t.Fatalf("stdout is not bounded valid JSON: %d bytes %q", stdout.Len(), stdout.String())
	}
	if strings.Contains(stdout.String(), "hook-secret") || !strings.Contains(stdout.String(), "gc.output_firewall") {
		t.Fatalf("hook output was not safely replaced: %q", stdout.String())
	}
}
