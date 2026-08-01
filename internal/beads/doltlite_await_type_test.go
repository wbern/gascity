//go:build gascity_native_beads

package beads

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"
)

// TestDoltliteReadStoreProjectsAwaitType pins that a gate bead's await_type
// reaches the domain Bead, and therefore the controller's typed wire.
//
// await_type is the one field of bd's four-field gate contract
// (id/status/created_at/await_type) the controller did not carry. Measured
// against the first gate bead this city has ever held:
//
//	bd gate list --json   -> await_type present, metadata null
//	GET /beads?type=gate  -> id,title,status,issue_type,created_at,description
//
// So routing `bd gate list` would have dropped it silently. It is a plain
// VARCHAR(32) column on bd's issues table (migration 0001) — not metadata and
// not computed — so the gate track was blocked on a field that only needed
// projecting.
func TestDoltliteReadStoreProjectsAwaitType(t *testing.T) {
	store, closeStore := newAwaitTypeDoltliteStore(t, true)
	defer closeStore()

	rows, err := store.List(ListQuery{Type: "gate"})
	if err != nil {
		t.Fatalf("List gate beads: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("gate rows = %d, want 1", len(rows))
	}
	if got := rows[0].AwaitType; got != "human" {
		t.Fatalf("AwaitType = %q, want %q", got, "human")
	}
}

// TestDoltliteReadStoreToleratesMissingAwaitTypeColumn pins the schema-presence
// probe. Snapshots predating bd's gate columns have no await_type, and the main
// list projection must not start erroring on them: a hard failure there would
// take down every list read, 21% of shim-facing traffic. The field is simply
// empty, exactly as the ephemeral/no_history flags already degrade.
func TestDoltliteReadStoreToleratesMissingAwaitTypeColumn(t *testing.T) {
	store, closeStore := newAwaitTypeDoltliteStore(t, false)
	defer closeStore()

	rows, err := store.List(ListQuery{Type: "gate"})
	if err != nil {
		t.Fatalf("List on a schema without await_type: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("gate rows = %d, want 1", len(rows))
	}
	if got := rows[0].AwaitType; got != "" {
		t.Fatalf("AwaitType = %q, want empty when the column is absent", got)
	}
}

// newAwaitTypeDoltliteStore builds a doltlite fixture holding one gate bead,
// with or without bd's await_type column.
func newAwaitTypeDoltliteStore(t *testing.T, withAwaitType bool) (*DoltliteReadStore, func()) {
	t.Helper()
	dir := t.TempDir()
	beadsDir := filepath.Join(dir, ".beads")
	if err := os.MkdirAll(filepath.Join(beadsDir, "doltlite"), 0o755); err != nil {
		t.Fatalf("mkdir doltlite dir: %v", err)
	}
	meta := []byte(`{"backend":"doltlite","database":"doltlite","dolt_database":"hq"}`)
	if err := os.WriteFile(filepath.Join(beadsDir, "metadata.json"), meta, 0o600); err != nil {
		t.Fatalf("write metadata: %v", err)
	}

	dbPath := filepath.Join(beadsDir, "doltlite", "hq.db")
	db, err := sql.Open("sqlite", dbPath+"?_busy_timeout=10000")
	if err != nil {
		t.Fatalf("open doltlite fixture db: %v", err)
	}
	cols := `,
			ephemeral INTEGER DEFAULT 0,
			no_history INTEGER DEFAULT 0`
	insertCols, insertVals := "", ""
	if withAwaitType {
		cols += `,
			await_type TEXT DEFAULT ''`
		insertCols, insertVals = ", await_type", ", 'human'"
	}
	createTestDoltliteSchemaWithRowColumns(t, db, cols)
	if _, err := db.Exec(`INSERT INTO issues (id, title, status, issue_type, priority, created_at, updated_at, metadata` +
		insertCols + `) VALUES ('gate-1', 'Gate: human', 'open', 'gate', 2, '2026-08-01T12:00:00Z', '2026-08-01T12:00:00Z', '{}'` +
		insertVals + `)`); err != nil {
		t.Fatalf("insert gate fixture: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close fixture db: %v", err)
	}

	backing := NewBdStore(dir, func(string, string, ...string) ([]byte, error) {
		t.Fatal("backing bd runner should not be called by doltlite read tests")
		return nil, nil
	})
	store, err := NewDoltliteReadStore(dir, backing)
	if err != nil {
		t.Fatalf("NewDoltliteReadStore: %v", err)
	}
	return store, func() { _ = store.CloseStore() }
}
