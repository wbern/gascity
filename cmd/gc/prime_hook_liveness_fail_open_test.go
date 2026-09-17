package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The SessionStart hook is the ONLY delivery point for an agent's rendered
// startup prompt (managedSessionHookPromptAlreadyDelivered suppresses it only
// when GC_STARTUP_PROMPT_DELIVERED=1, which managed sessions do not set). The
// gate in doPrimeWithHookFormatOpts consults the session bead to decide whether
// the pane is a live managed session and emits NOTHING when it is not.
//
// gcw-kasmq: that consult opens the bead store, and a store failure was folded
// into the same "not live" answer — so a transient store outage silently
// delivered an empty hook payload and the agent came up with no role, no
// identity and no handoff, with nothing on stdout or stderr to say why.
//
// These tests pin the three-way distinction the gate must make:
//
//	store consulted, session live       -> deliver the prompt (no degraded marker)
//	store consulted, definite negative  -> deliver nothing (unchanged)
//	store NOT consultable               -> deliver the prompt, marked degraded
//
// "Store not consultable" is simulated with GC_BEADS=sqlite, a provider that
// openStoreResultAtForCityWithConfig rejects with an error — the same shape as
// the real failure (an error out of the store open) without needing a Dolt
// outage in a unit test.
const (
	kasmqPromptContent  = "you are the test worker; resume your work\n"
	kasmqUnsupportedEnv = "sqlite"
)

func writeKasmqPrimeCity(t *testing.T) string {
	t.Helper()
	cityDir := t.TempDir()
	promptDir := filepath.Join(cityDir, "prompts")
	if err := os.MkdirAll(promptDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(promptDir): %v", err)
	}
	if err := os.WriteFile(filepath.Join(promptDir, "worker.md"), []byte(kasmqPromptContent), 0o644); err != nil {
		t.Fatalf("WriteFile(prompt): %v", err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(`
[workspace]
name = "gastown"

[[agent]]
name = "worker"
prompt_template = "prompts/worker.md"
`), 0o644); err != nil {
		t.Fatalf("WriteFile(city.toml): %v", err)
	}
	return cityDir
}

// runKasmqSessionStartHook runs the SessionStart prime hook for a managed
// "worker" session and returns the decoded additionalContext plus stderr.
func runKasmqSessionStartHook(t *testing.T, cityDir, sessionID, beadsProvider string) (string, string) {
	t.Helper()
	t.Setenv("GC_CITY", cityDir)
	t.Setenv("GC_AGENT", "worker")
	t.Setenv("GC_ALIAS", "worker")
	t.Setenv("GC_TEMPLATE", "worker")
	t.Setenv("GC_SESSION_NAME", "gastown--worker")
	t.Setenv("GC_SESSION_ID", sessionID)
	t.Setenv(managedSessionHookEnv, "1")
	t.Setenv("GC_HOOK_SOURCE", "startup")
	t.Setenv("GC_HOOK_EVENT_NAME", "SessionStart")
	// Deliberately NOT startupPromptDeliveredEnv: managed gc sessions do not set
	// it, so the hook owns startup-prompt delivery. That is what makes an empty
	// payload a total loss rather than a missing extra.
	withPrimeHookStdin(t)
	t.Setenv("GC_BEADS", beadsProvider)

	var stdout, stderr bytes.Buffer
	if code := doPrimeWithHookFormat(nil, &stdout, &stderr, true, hookOutputFormatCodex, false); code != 0 {
		t.Fatalf("doPrimeWithHookFormat() = %d, want 0; stderr=%q", code, stderr.String())
	}
	if strings.TrimSpace(stdout.String()) == "" {
		return "", stderr.String()
	}
	var out struct {
		HookSpecificOutput struct {
			AdditionalContext string `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
		t.Fatalf("hook output is not JSON: %v; stdout=%q", err, stdout.String())
	}
	return out.HookSpecificOutput.AdditionalContext, stderr.String()
}

// Given a managed SessionStart whose bead store cannot be opened,
// When the prime hook runs,
// Then it still delivers the agent's role prompt and beacon, marks the context
// degraded, and says so on stderr — because "we could not ask" is not "this
// session is not live".
func TestSessionStartHook_StoreUnavailable_StillDeliversPrompt(t *testing.T) {
	clearGCEnv(t)
	disableManagedDoltRecoveryForTest(t)

	cityDir := writeKasmqPrimeCity(t)
	sessionID := createPrimeHookSession(t, cityDir, "gastown--worker", "worker")

	context, stderr := runKasmqSessionStartHook(t, cityDir, sessionID, kasmqUnsupportedEnv)

	if strings.TrimSpace(context) == "" {
		t.Fatalf("additionalContext is empty: a store outage blanked the agent's entire role prompt (stderr=%q)", stderr)
	}
	if !strings.Contains(context, strings.TrimSpace(kasmqPromptContent)) {
		t.Errorf("additionalContext = %q, want the rendered startup prompt", context)
	}
	if !strings.Contains(context, "[gastown] worker") {
		t.Errorf("additionalContext = %q, want the hook beacon", context)
	}
	if !strings.Contains(context, primeHookLivenessDegradedMarker) {
		t.Errorf("additionalContext = %q, want the degraded marker %q so the agent knows its context is incomplete",
			context, primeHookLivenessDegradedMarker)
	}
	if !strings.Contains(stderr, "session liveness unverified") {
		t.Errorf("stderr = %q, want a warning: this path is silent today, which is why it went unnoticed", stderr)
	}
}

// Given a healthy store that reports no such session bead,
// When the prime hook runs,
// Then it still delivers nothing — the suppression gate must keep working for a
// pane that genuinely is not a live managed session. This is the over-fix guard.
func TestSessionStartHook_HealthyStoreDefiniteNegative_StaysSuppressed(t *testing.T) {
	clearGCEnv(t)
	disableManagedDoltRecoveryForTest(t)

	cityDir := writeKasmqPrimeCity(t)
	// Seed a real session bead so the store is genuinely readable, then prime
	// with a session id that does not exist: a definite negative, not an outage.
	createPrimeHookSession(t, cityDir, "gastown--worker", "worker")

	context, stderr := runKasmqSessionStartHook(t, cityDir, "gc-session-does-not-exist", "file")

	if strings.TrimSpace(context) != "" {
		t.Fatalf("additionalContext = %q, want empty: a non-live session must stay suppressed (stderr=%q)", context, stderr)
	}
}

// Given a healthy store and a live session bead,
// When the prime hook runs,
// Then it delivers the role prompt WITHOUT the degraded marker — the happy path
// must not start advertising degradation.
func TestSessionStartHook_HealthyStoreLiveSession_DeliversCleanPrompt(t *testing.T) {
	clearGCEnv(t)
	disableManagedDoltRecoveryForTest(t)

	cityDir := writeKasmqPrimeCity(t)
	sessionID := createPrimeHookSession(t, cityDir, "gastown--worker", "worker")

	context, stderr := runKasmqSessionStartHook(t, cityDir, sessionID, "file")

	if !strings.Contains(context, strings.TrimSpace(kasmqPromptContent)) {
		t.Fatalf("additionalContext = %q, want the rendered startup prompt (stderr=%q)", context, stderr)
	}
	if strings.Contains(context, primeHookLivenessDegradedMarker) {
		t.Errorf("additionalContext = %q, want NO degraded marker on the healthy path", context)
	}
}

// primeHookSessionLiveness is the three-way answer the gate needs. These cases
// pin it directly, so a future refactor cannot quietly collapse "unknown" back
// into "not live" without a failing test.
func TestPrimeHookSessionLiveness_DistinguishesUnknownFromNotLive(t *testing.T) {
	clearGCEnv(t)
	disableManagedDoltRecoveryForTest(t)

	cityDir := writeKasmqPrimeCity(t)
	sessionID := createPrimeHookSession(t, cityDir, "gastown--worker", "worker")

	t.Setenv("GC_SESSION_NAME", "gastown--worker")

	t.Run("live session is live and consulted", func(t *testing.T) {
		t.Setenv("GC_BEADS", "file")
		t.Setenv("GC_SESSION_ID", sessionID)
		live, consulted := primeHookSessionLiveness(cityDir)
		if !live || !consulted {
			t.Fatalf("primeHookSessionLiveness() = (%t, %t), want (true, true)", live, consulted)
		}
	})

	t.Run("absent session is not live but consulted", func(t *testing.T) {
		t.Setenv("GC_BEADS", "file")
		t.Setenv("GC_SESSION_ID", "gc-session-does-not-exist")
		live, consulted := primeHookSessionLiveness(cityDir)
		if live || !consulted {
			t.Fatalf("primeHookSessionLiveness() = (%t, %t), want (false, true)", live, consulted)
		}
	})

	t.Run("unopenable store is not consulted", func(t *testing.T) {
		t.Setenv("GC_BEADS", kasmqUnsupportedEnv)
		t.Setenv("GC_SESSION_ID", sessionID)
		live, consulted := primeHookSessionLiveness(cityDir)
		if consulted {
			t.Fatalf("primeHookSessionLiveness() = (%t, %t), want consulted=false when the store cannot be opened", live, consulted)
		}
	})

	t.Run("missing session env is a definite negative", func(t *testing.T) {
		// No GC_SESSION_ID: an ordinary shell, not a managed pane. This must stay
		// a definite negative so fail-open never primes a plain human terminal.
		t.Setenv("GC_BEADS", "file")
		t.Setenv("GC_SESSION_ID", "")
		live, consulted := primeHookSessionLiveness(cityDir)
		if live || !consulted {
			t.Fatalf("primeHookSessionLiveness() = (%t, %t), want (false, true)", live, consulted)
		}
	})
}

// The liveness consult must stay lazy. An earlier iteration of this fix
// evaluated it unconditionally, which added a store open plus a city-config load
// to every plain (non-hook) `gc prime` — measurable cost on a path that has no
// session to verify. Non-hook prime must not touch the store at all, which this
// pins by making the store unopenable and requiring the prompt anyway.
func TestPrimeNonHookMode_DoesNotConsultSessionStore(t *testing.T) {
	clearGCEnv(t)
	disableManagedDoltRecoveryForTest(t)

	cityDir := writeKasmqPrimeCity(t)

	t.Setenv("GC_CITY", cityDir)
	t.Setenv("GC_AGENT", "worker")
	t.Setenv("GC_ALIAS", "worker")
	t.Setenv("GC_BEADS", kasmqUnsupportedEnv)

	var stdout, stderr bytes.Buffer
	if code := doPrime(nil, &stdout, &stderr); code != 0 {
		t.Fatalf("doPrime() = %d, want 0; stderr=%q", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), strings.TrimSpace(kasmqPromptContent)) {
		t.Fatalf("stdout = %q, want the rendered prompt with no store involvement", stdout.String())
	}
	if strings.Contains(stdout.String(), primeHookLivenessDegradedMarker) {
		t.Errorf("stdout = %q, want no degraded marker outside hook mode", stdout.String())
	}
}
