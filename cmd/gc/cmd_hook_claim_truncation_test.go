package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/internal/shellquote"
)

const hookFirewallManifest = `{"schema_version":"1","kind":"gc.output_firewall","reason":"byte_budget_exceeded",` +
	`"command_class":"managed_bd_passthrough","budget_bytes":32768,"serialized_bytes":90520,"sha256":"abc123",` +
	`"spill":{"mode":"secure","path":"/city/.gc/spill/output-deadbeef","expires_at":"2026-08-14T10:00:00Z"}}`

// A firewall manifest is a JSON object, and normalizeWorkQueryOutput wraps a
// lone object into a one-element array. Before gcw-qap3.16 that produced a
// phantom candidate with an empty ID instead of an error.
func TestDecodeHookClaimBeadsRejectsFirewallManifest(t *testing.T) {
	got, err := decodeHookClaimBeads(hookFirewallManifest + "\n")
	if err == nil {
		t.Fatalf("decodeHookClaimBeads() = %#v, want a truncation error", got)
	}
	if !errors.Is(err, beads.ErrOutputTruncated) {
		t.Fatalf("decodeHookClaimBeads() = %v, want ErrOutputTruncated", err)
	}
	if len(got) != 0 {
		t.Fatalf("decodeHookClaimBeads() returned %d phantom candidates", len(got))
	}
}

func TestDecodeHookClaimBeadsKeepsRealPayloads(t *testing.T) {
	got, err := decodeHookClaimBeads(`[{"id":"work-1","status":"open"}]`)
	if err != nil {
		t.Fatalf("decodeHookClaimBeads(): %v", err)
	}
	if len(got) != 1 || got[0].ID != "work-1" {
		t.Fatalf("decodeHookClaimBeads() = %#v", got)
	}
}

// The hook claim path bounds its own printed output (writeHookClaimJSON), so
// its internal store reads must not be bounded a second time: doing so is what
// made an existing assignment invisible and armed a wrong claim.
func TestHookClaimStoreReadsAreExemptFromTheOutputFirewall(t *testing.T) {
	t.Setenv(citylayout.RealBdEnvVar, "/usr/local/bin/bd")
	originalRunner := hookClaimCommandRunnerWithEnvContext
	t.Cleanup(func() { hookClaimCommandRunnerWithEnvContext = originalRunner })

	var readArgs []string
	hookClaimCommandRunnerWithEnvContext = func(_ context.Context, _ map[string]string) beads.CommandRunner {
		return func(_ string, _ string, args ...string) ([]byte, error) {
			readArgs = append([]string(nil), args...)
			return []byte(`[{"id":"work-1","status":"in_progress","assignee":"worker-1"}]`), nil
		}
	}

	bead, found, err := hookResolveBeadWithBdStore(context.Background(), "/rig", nil, "work-1")
	if err != nil || !found || bead.ID != "work-1" {
		t.Fatalf("hookResolveBeadWithBdStore() = (%#v, %v, %v)", bead, found, err)
	}
	if !reflect.DeepEqual(readArgs, []string{"show", "--json", "work-1", "--allow-unbounded"}) {
		t.Fatalf("read args = %#v, want the unbounded control-plane read", readArgs)
	}
}

// Truncation is not absence: a canonical re-read that was withheld must fall
// back to the bead the claim already returned rather than report failure and
// leave the claim stranded in_progress with nobody told they own it.
func TestHookClaimKeepsClaimWhenCanonicalReadIsTruncated(t *testing.T) {
	originalRunner := hookClaimCommandRunnerWithEnvContext
	t.Cleanup(func() { hookClaimCommandRunnerWithEnvContext = originalRunner })

	hookClaimCommandRunnerWithEnvContext = func(_ context.Context, _ map[string]string) beads.CommandRunner {
		return func(_ string, _ string, args ...string) ([]byte, error) {
			if len(args) > 0 && args[0] == "show" {
				return []byte(hookFirewallManifest + "\n"), nil
			}
			return []byte(`[{"id":"work-1","status":"in_progress","assignee":"worker-1"}]`), nil
		}
	}

	claimed, ok, err := hookClaimWithBdStore(context.Background(), "/rig", nil, "work-1", "worker-1")
	if err != nil {
		t.Fatalf("hookClaimWithBdStore() = %v, want the claim reported despite a withheld re-read", err)
	}
	if !ok || claimed.ID != "work-1" || claimed.Assignee != "worker-1" {
		t.Fatalf("hookClaimWithBdStore() = (%#v, %v), want the claimed bead", claimed, ok)
	}
}

// A genuinely missing bead after a committed claim is a real inconsistency and
// must stay fatal — only truncation is tolerated.
func TestHookClaimStillFailsWhenCanonicalReadIsMissing(t *testing.T) {
	originalRunner := hookClaimCommandRunnerWithEnvContext
	t.Cleanup(func() { hookClaimCommandRunnerWithEnvContext = originalRunner })

	hookClaimCommandRunnerWithEnvContext = func(_ context.Context, _ map[string]string) beads.CommandRunner {
		return func(_ string, _ string, args ...string) ([]byte, error) {
			if len(args) > 0 && args[0] == "show" {
				return []byte(`[]`), nil
			}
			return []byte(`[{"id":"work-1","status":"in_progress","assignee":"worker-1"}]`), nil
		}
	}

	_, ok, err := hookClaimWithBdStore(context.Background(), "/rig", nil, "work-1", "worker-1")
	if err == nil {
		t.Fatal("hookClaimWithBdStore() = nil error, want the missing canonical bead reported")
	}
	if !ok {
		t.Fatal("hookClaimWithBdStore() ok = false, want the committed claim still reported")
	}
	if !strings.Contains(err.Error(), "work-1") {
		t.Fatalf("error %q does not name the claimed bead", err)
	}
}

// The work query is a shell command whose stdout can itself be withheld inside
// a managed session. Every hook consumer (doHook, the federated store
// selection, the claim re-validation) funnels through this runner, so the
// manifest must become an error here rather than a phantom candidate
// downstream.
func TestShellWorkQueryRejectsFirewallManifest(t *testing.T) {
	out, err := shellWorkQueryWithEnv("printf '%s' "+shellquote.Quote(hookFirewallManifest), t.TempDir(), nil)
	if err == nil {
		t.Fatalf("shellWorkQueryWithEnv() = %q, want a truncation error", out)
	}
	if !errors.Is(err, beads.ErrOutputTruncated) {
		t.Fatalf("shellWorkQueryWithEnv() = %v, want ErrOutputTruncated", err)
	}
	if out != "" {
		t.Fatalf("shellWorkQueryWithEnv() returned the withheld manifest as output: %q", out)
	}
}

func TestShellWorkQueryKeepsRealPayloads(t *testing.T) {
	out, err := shellWorkQueryWithEnv(`printf '%s' '[{"id":"work-1","status":"open"}]'`, t.TempDir(), nil)
	if err != nil {
		t.Fatalf("shellWorkQueryWithEnv(): %v", err)
	}
	if !strings.Contains(out, "work-1") {
		t.Fatalf("shellWorkQueryWithEnv() = %q", out)
	}
}

// A hook read that reports truncation instead of work must not be printed as
// work, and must not be silent about why the session was told it has none.
func TestDoHookReportsWithheldWorkQueryLoudly(t *testing.T) {
	runner := func(string, string) (string, error) {
		return "", fmt.Errorf("running work query: %w", beads.ErrOutputTruncated)
	}
	var stdout, stderr bytes.Buffer
	if code := doHook("bd ready --json", ".", false, runner, &stdout, &stderr); code != 1 {
		t.Fatalf("doHook() = %d, want a reported failure", code)
	}
	if stdout.Len() != 0 {
		t.Fatalf("doHook() published %q as work", stdout.String())
	}
	if !strings.Contains(stderr.String(), "truncated by the managed output firewall") {
		t.Fatalf("stderr=%q does not name the truncation", stderr.String())
	}
}

// Without bdshim in front of bd there is nothing to honor --allow-unbounded,
// and raw bd would reject the unknown flag. The exemption is gated, not
// unconditional.
func TestHookClaimStoreOmitsUnboundedFlagWithoutTheShim(t *testing.T) {
	t.Setenv(citylayout.RealBdEnvVar, "")
	originalRunner := hookClaimCommandRunnerWithEnvContext
	t.Cleanup(func() { hookClaimCommandRunnerWithEnvContext = originalRunner })

	var readArgs []string
	hookClaimCommandRunnerWithEnvContext = func(_ context.Context, _ map[string]string) beads.CommandRunner {
		return func(_ string, _ string, args ...string) ([]byte, error) {
			readArgs = append([]string(nil), args...)
			return []byte(`[{"id":"work-1","status":"in_progress","assignee":"worker-1"}]`), nil
		}
	}

	if _, _, err := hookResolveBeadWithBdStore(context.Background(), "/rig", nil, "work-1"); err != nil {
		t.Fatalf("hookResolveBeadWithBdStore(): %v", err)
	}
	if !reflect.DeepEqual(readArgs, []string{"show", "--json", "work-1"}) {
		t.Fatalf("read args = %#v, want a plain bd read", readArgs)
	}
}
