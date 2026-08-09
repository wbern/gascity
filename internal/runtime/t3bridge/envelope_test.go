package t3bridge

import (
	"encoding/json"
	"testing"
)

func TestBuildStartupEnvelope_ThreadReuseFollowsWakeMode(t *testing.T) {
	for _, tc := range []struct {
		name      string
		wakeMode  string
		wantReuse bool
	}{
		{name: "fresh", wakeMode: "fresh", wantReuse: false},
		{name: "resume", wakeMode: "resume", wantReuse: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := BuildStartupEnvelope(Intent{
				AgentKind: AgentKindNamed,
				WakeMode:  tc.wakeMode,
			})
			if err != nil {
				t.Fatalf("BuildStartupEnvelope: %v", err)
			}
			var envelope StartupEnvelope
			if err := json.Unmarshal(raw, &envelope); err != nil {
				t.Fatalf("unmarshal envelope: %v", err)
			}
			if envelope.Resume.AllowThreadReuse != tc.wantReuse {
				t.Fatalf("allowThreadReuse = %v, want %v", envelope.Resume.AllowThreadReuse, tc.wantReuse)
			}
		})
	}
}

func TestAllowThreadReuse_NamedFreshRecreatesThread(t *testing.T) {
	if AllowThreadReuse(AgentKindNamed, "fresh") {
		t.Fatal("named fresh sessions must recreate their T3 thread")
	}
}

func TestAllowThreadReuse_NamedResumeReusesThread(t *testing.T) {
	if !AllowThreadReuse(AgentKindNamed, "resume") {
		t.Fatal("named resume sessions should reuse their T3 thread")
	}
}

func TestAllowThreadReuse_PoolDoesNotReuseThread(t *testing.T) {
	if AllowThreadReuse(AgentKindPool, "sticky") {
		t.Fatal("pool sessions should not reuse one shared T3 thread")
	}
}

func TestLegacyNamedThreadReuse_FreshWakeNeverReuses(t *testing.T) {
	// A wake_mode=fresh worker must recreate a fresh thread even when it has a
	// stable name and no assigned bead (the legacy-named shape). Reusing would
	// resume the codex process and raise the working-directory disambiguation
	// modal on a drifted cwd — the regression this fix closes.
	if legacyNamedThreadReuse("", "crm/gastown.polecat-1", "fresh") {
		t.Fatal("wake_mode=fresh must never reuse the T3 thread")
	}
	// Even with a bead assigned, fresh stays fresh.
	if legacyNamedThreadReuse("crm-pyqss", "crm/gastown.polecat-1", "fresh") {
		t.Fatal("wake_mode=fresh must never reuse the T3 thread, even with a bead")
	}
}

func TestLegacyNamedThreadReuse_ResumeNamedReusesDurableThread(t *testing.T) {
	// A durable named session (no bead, has a name) with the default resume
	// wake mode keeps its one durable T3 thread — unchanged behavior.
	if !legacyNamedThreadReuse("", "mayor", "resume") {
		t.Fatal("legacy named + wake_mode=resume must reuse its durable thread")
	}
	// Empty wake mode defaults to resume semantics here.
	if !legacyNamedThreadReuse("", "mayor", "") {
		t.Fatal("legacy named + empty wake mode must reuse its durable thread")
	}
}

func TestLegacyNamedThreadReuse_SessionWithBeadDoesNotReuse(t *testing.T) {
	// A session actively assigned to a bead is not "legacy named" and must not
	// reuse regardless of wake mode.
	if legacyNamedThreadReuse("gcw-123", "worker-1", "resume") {
		t.Fatal("a session with an assigned bead must not reuse the durable thread")
	}
}

func TestLegacyNamedThreadReuse_NoSessionNameDoesNotReuse(t *testing.T) {
	if legacyNamedThreadReuse("", "", "resume") {
		t.Fatal("a session without a name must not reuse a durable thread")
	}
}
