package main

import "testing"

// The legacy drain-ack wraps its ops in persistedAckDrainOps. Binding the
// admission must look through the wrapper and keep it, or an enrolled session's
// late ack runs without the incarnation and admission fences (T8 arming-review
// D1). With no city.toml nothing can be enrolled, so the ops come back
// unchanged, but they must still be the wrapper around the same provider ops.
func TestBindDrainAckAdmissionKeepsThePersistedAckWrapper(t *testing.T) {
	provider := &providerDrainOps{}
	wrapped := persistedAckDrainOps{drainOps: provider}

	got, err := bindDrainAckAdmission(wrapped, t.TempDir(), "worker")
	if err != nil {
		t.Fatalf("bindDrainAckAdmission: %v", err)
	}
	again, ok := got.(persistedAckDrainOps)
	if !ok {
		t.Fatalf("bound ops = %T, want the persistedAckDrainOps wrapper kept", got)
	}
	if again.drainOps != drainOps(provider) {
		t.Fatalf("wrapper holds %T, want the same provider ops when nothing is enrolled", again.drainOps)
	}
}
