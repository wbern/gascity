package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

type hookFirewallFailingWriter struct{}

func (hookFirewallFailingWriter) Write([]byte) (int, error) {
	return 0, errors.New("output unavailable")
}

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

func TestDoHookReplacesOversizedManagedReadOutput(t *testing.T) {
	t.Setenv("GC_MANAGED_OUTPUT_FIREWALL", "1")
	t.Setenv("GC_MANAGED_OUTPUT_FIREWALL_BUDGET", "512")
	t.Setenv("GC_MANAGED_OUTPUT_FIREWALL_READ_VERBS", "hook")
	t.Setenv("GC_MANAGED_OUTPUT_FIREWALL_SPILL_MODE", "disabled")
	runner := func(string, string) (string, error) {
		return `[{"id":"gcw-1","status":"open","description":"` + strings.Repeat("hook-secret", 200) + `"}]`, nil
	}
	var stdout, stderr bytes.Buffer
	if code := doHook("bd ready --json", ".", false, runner, &stdout, &stderr); code != 0 {
		t.Fatalf("doHook() = %d, stderr=%q", code, stderr.String())
	}
	if stdout.Len() > 512 || !json.Valid(stdout.Bytes()) || strings.Contains(stdout.String(), "hook-secret") || !strings.Contains(stdout.String(), "gc.output_firewall") {
		t.Fatalf("stdout=%d %q", stdout.Len(), stdout.String())
	}
}

func TestDoHookReportsManagedOutputPublishFailure(t *testing.T) {
	t.Setenv("GC_MANAGED_OUTPUT_FIREWALL", "1")
	t.Setenv("GC_MANAGED_OUTPUT_FIREWALL_READ_VERBS", "hook")
	runner := func(string, string) (string, error) { return `[{"id":"gcw-1","status":"open"}]`, nil }
	var stderr bytes.Buffer
	if code := doHook("bd ready --json", ".", false, runner, hookFirewallFailingWriter{}, &stderr); code != 1 {
		t.Fatalf("doHook() = %d, want failed publish; stderr=%q", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "output firewall could not publish") {
		t.Fatalf("stderr=%q", stderr.String())
	}
}
