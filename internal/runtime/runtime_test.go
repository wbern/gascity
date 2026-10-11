package runtime

import (
	"errors"
	"fmt"
	"testing"
)

func TestSyncWorkDirEnvSetsGCDir(t *testing.T) {
	cfg := SyncWorkDirEnv(Config{WorkDir: "/tmp/work"})
	if got := cfg.Env["GC_DIR"]; got != "/tmp/work" {
		t.Fatalf("GC_DIR = %q, want %q", got, "/tmp/work")
	}
}

func TestSyncWorkDirEnvCopiesEnvBeforeMutation(t *testing.T) {
	original := map[string]string{"GC_DIR": "/stale", "GC_AGENT": "worker"}
	cfg := SyncWorkDirEnv(Config{
		WorkDir: "/tmp/work",
		Env:     original,
	})
	if got := cfg.Env["GC_DIR"]; got != "/tmp/work" {
		t.Fatalf("GC_DIR = %q, want %q", got, "/tmp/work")
	}
	if got := original["GC_DIR"]; got != "/stale" {
		t.Fatalf("original GC_DIR mutated to %q", got)
	}
	if got := cfg.Env["GC_AGENT"]; got != "worker" {
		t.Fatalf("GC_AGENT = %q, want %q", got, "worker")
	}
}

func TestHasManagedStartupHints(t *testing.T) {
	tests := []struct {
		name string
		cfg  Config
		want bool
	}{
		{name: "none", cfg: Config{}, want: false},
		{name: "ready prompt", cfg: Config{ReadyPromptPrefix: "> "}, want: true},
		{name: "ready delay", cfg: Config{ReadyDelayMs: 100}, want: true},
		{name: "process names", cfg: Config{ProcessNames: []string{"claude"}}, want: true},
		{name: "permission warning", cfg: Config{EmitsPermissionWarning: true}, want: true},
		{name: "startup dialog override", cfg: Config{AcceptStartupDialogs: boolPtr(false)}, want: true},
		{name: "nudge", cfg: Config{Nudge: "Check your hook."}, want: true},
		{name: "pre start", cfg: Config{PreStart: []string{"echo pre"}}, want: true},
		{name: "session setup", cfg: Config{SessionSetup: []string{"echo setup"}}, want: true},
		{name: "session setup script", cfg: Config{SessionSetupScript: "setup.sh"}, want: true},
		{name: "session live", cfg: Config{SessionLive: []string{"echo live"}}, want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := HasManagedStartupHints(tt.cfg); got != tt.want {
				t.Fatalf("HasManagedStartupHints() = %v, want %v", got, tt.want)
			}
		})
	}
}

func boolPtr(v bool) *bool {
	return &v
}

// A typed refusal that reads as "session gone" is taken by
// MergeBackendStopErrors and every !IsSessionGone(err) Stop caller as an
// idempotent success, so the sentinels and their wraps must never match.
func TestTypedRuntimeSentinelsNeverMatchIsSessionGone(t *testing.T) {
	for _, sentinel := range []error{ErrStopRefused, ErrStopUnsupported, ErrMetaUnsupported, ErrListUnsupported} {
		wrapped := fmt.Errorf("exec provider script %q op %q: %w", "/packs/box/runtime.sh", "stop", sentinel)
		for _, err := range []error{sentinel, wrapped} {
			if IsSessionGone(err) {
				t.Errorf("IsSessionGone(%q) = true, want false", err)
			}
		}
	}
	if !errors.Is(ErrStopUnsupported, ErrStopRefused) {
		t.Error("ErrStopUnsupported does not wrap ErrStopRefused")
	}
}

// emptyJoin is a multi-error with no children: only its own text can decide.
type emptyJoin struct{ msg string }

func (e emptyJoin) Error() string   { return e.msg }
func (e emptyJoin) Unwrap() []error { return nil }

// A joined error is gone only when every leaf is. The auto provider's Stop
// joins a failed default backend with the ACP backend's "session not found";
// read as gone, that failed kill was taken for a stopped runtime (C8.8).
func TestIsSessionGoneRequiresEveryJoinedLeafGone(t *testing.T) {
	failure := errors.New("tmux -u: gate-shim: simulated tmux failure (tmux.c1)")
	serverGone := errors.New("no tmux server running")
	notFound := fmt.Errorf("stopping %q: %w", "sky", ErrSessionNotFound)
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{name: "nil", err: nil, want: false},
		{name: "single: sentinel", err: ErrSessionNotFound, want: true},
		{name: "single: wrapped sentinel", err: notFound, want: true},
		{name: "single: legacy text", err: errors.New("session sky not running"), want: true},
		{name: "single: missing server", err: fmt.Errorf("killing session sky: %w", serverGone), want: true},
		{name: "single: failure", err: failure, want: false},
		{name: "joined: all gone", err: errors.Join(notFound, serverGone), want: true},
		{name: "joined: failure first", err: errors.Join(failure, ErrSessionNotFound), want: false},
		{name: "joined: failure last", err: errors.Join(ErrSessionNotFound, failure), want: false},
		{name: "joined: auto stop, default kill failed", err: MergeBackendStopErrors(
			BackendError{Label: "default", Err: failure},
			BackendError{Label: "acp", Err: ErrSessionNotFound},
		), want: false},
		{name: "joined: multi-%w mixed", err: fmt.Errorf("%w; %w", failure, ErrSessionNotFound), want: false},
		{name: "nested: all gone", err: errors.Join(errors.Join(ErrSessionNotFound, serverGone), notFound), want: true},
		{name: "nested: inner failure", err: errors.Join(ErrSessionNotFound, errors.Join(serverGone, failure)), want: false},
		{name: "wrapped join: all gone", err: fmt.Errorf("k8s stop %q: %w", "sky", errors.Join(notFound, serverGone)), want: true},
		{name: "wrapped join: mixed, wrap text says gone", err: fmt.Errorf("session not found: %w", errors.Join(failure, ErrSessionNotFound)), want: false},
		{name: "doubly wrapped join: mixed", err: fmt.Errorf("a: %w", fmt.Errorf("b: %w", errors.Join(ErrSessionNotFound, failure))), want: false},
		{name: "empty join: own text gone", err: emptyJoin{msg: "session not found"}, want: true},
		{name: "empty join: own text failure", err: emptyJoin{msg: "permission denied"}, want: false},
		{name: "wrapped empty join: wrap text decides", err: fmt.Errorf("session not found: %w", emptyJoin{msg: "permission denied"}), want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsSessionGone(tc.err); got != tc.want {
				t.Fatalf("IsSessionGone(%q) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

func TestMetaValueFoldsOnlyMetaUnsupported(t *testing.T) {
	transport := fmt.Errorf("reading GC_K: %w", ErrRuntimeUnavailable)
	cases := []struct {
		name    string
		v       string
		err     error
		want    string
		wantErr error
	}{
		{"value", "tok", nil, "tok", nil},
		{"unset", "", nil, "", nil},
		{"meta unsupported", "", fmt.Errorf("exec get-meta: %w", ErrMetaUnsupported), "", nil},
		{"transport", "", transport, "", transport},
		{"not found", "", ErrSessionNotFound, "", ErrSessionNotFound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := MetaValue(tc.v, tc.err)
			if got != tc.want || !errors.Is(err, tc.wantErr) {
				t.Errorf("MetaValue(%q, %v) = (%q, %v), want (%q, %v)", tc.v, tc.err, got, err, tc.want, tc.wantErr)
			}
		})
	}
}
