package exec //nolint:revive // internal package, always imported with alias

import (
	"encoding/json"
	"testing"
)

// TestBeadWireToBeadCarriesPlainColumns pins bd's four plain columns through
// the exec provider's wire type.
//
// This is one of four hand-written Bead mappings, and adding a field to
// beads.Bead does not make it flow through any of them: beadWire declares its
// own field set, so a column it omits is dropped before toBead runs. An
// exec:<script> store that faithfully emits bd's JSON would still have had
// these four discarded here.
func TestBeadWireToBeadCarriesPlainColumns(t *testing.T) {
	const raw = `{
		"id": "exec-1",
		"title": "Gate: human",
		"status": "open",
		"type": "gate",
		"await_type": "human",
		"created_by": "seeder",
		"owner": "owner@example.com",
		"notes": "waiting on a human"
	}`
	var w beadWire
	if err := json.Unmarshal([]byte(raw), &w); err != nil {
		t.Fatalf("unmarshal bead wire: %v", err)
	}
	got := w.toBead()
	for _, tc := range []struct{ field, got, want string }{
		{"AwaitType", got.AwaitType, "human"},
		{"CreatedBy", got.CreatedBy, "seeder"},
		{"Owner", got.Owner, "owner@example.com"},
		{"Notes", got.Notes, "waiting on a human"},
	} {
		if tc.got != tc.want {
			t.Errorf("beadWire.toBead dropped %s: got %q, want %q", tc.field, tc.got, tc.want)
		}
	}
}
