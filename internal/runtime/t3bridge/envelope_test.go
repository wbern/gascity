package t3bridge

import "testing"

func TestAllowThreadReuse_NamedFreshStillReusesThread(t *testing.T) {
	if !allowThreadReuse(AgentKindNamed, "fresh") {
		t.Fatal("named fresh sessions should still reuse their T3 thread")
	}
}

func TestAllowThreadReuse_PoolDoesNotReuseThread(t *testing.T) {
	if allowThreadReuse(AgentKindPool, "sticky") {
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
