//go:build integration

package main

import (
	"context"
	"database/sql"
	"fmt"
	"math/rand"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads/contract"
)

// The idle gate is only sound if DOLT_HASHOF_DB(), read over the pooled
// connection exactly as controlReadyReadDoltVersion reads it, changes for
// every write that can change a `bd ready --include-ephemeral` answer --
// including writes to the dolt_ignore'd tables bd keeps wisps in -- and for
// nothing else. These tests pin that against a throwaway dolt sql-server.
//
// Observed with dolt 2.6.0:
//   (a) an autocommit INSERT into a tracked table changes the hash;
//   (b) an INSERT inside another session's open transaction leaves the hash
//       unchanged until that session COMMITs, then changes it (the gate's
//       probe sees only committed transactions, the same snapshot a fresh bd
//       ready process would read);
//   (c) an autocommit INSERT/UPDATE/DELETE on dolt_ignore'd wisp tables
//       changes the hash;
//   (d) a read-only SELECT and a no-op UPDATE leave the hash unchanged.

// idleGateDoltServer is one throwaway dolt sql-server serving database
// idleGateDoltDatabase, with bd's wisp tables dolt_ignore'd as bd's schema
// migration 0019 does.
type idleGateDoltServer struct {
	target contract.DoltConnectionTarget
	db     *sql.DB
}

const idleGateDoltDatabase = "idlegate"

// idleGateReservedPorts are live services on shared hosts the test must never
// bind or dial.
var idleGateReservedPorts = map[int]bool{19549: true, 19550: true, 28207: true, 52453: true}

func idleGateDoltPort(t *testing.T) int {
	t.Helper()
	for i := 0; i < 50; i++ {
		port := 29000 + rand.Intn(1000)
		if idleGateReservedPorts[port] {
			continue
		}
		ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
		if err != nil {
			continue
		}
		_ = ln.Close()
		return port
	}
	t.Fatal("no free port in 29000-29999")
	return 0
}

func idleGateScratchDir(t *testing.T) string {
	t.Helper()
	parent := os.TempDir()
	if info, err := os.Stat("/var/tmp"); err == nil && info.IsDir() {
		parent = "/var/tmp"
	}
	dir, err := os.MkdirTemp(parent, "idlegate-dolt-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func startIdleGateDoltServer(t *testing.T) *idleGateDoltServer {
	t.Helper()
	doltBin, err := exec.LookPath("dolt")
	if err != nil {
		t.Skip("dolt not on PATH")
	}
	scratch := idleGateScratchDir(t)
	dbDir := filepath.Join(scratch, idleGateDoltDatabase)
	if err := os.MkdirAll(dbDir, 0o755); err != nil {
		t.Fatal(err)
	}
	env := append(os.Environ(), "DOLT_ROOT_PATH="+filepath.Join(scratch, "root"), "DOLT_DISABLE_EVENT_FLUSH=1")
	initCmd := exec.Command(doltBin, "init", "--name", "idlegate", "--email", "idlegate@example.invalid")
	initCmd.Dir, initCmd.Env = dbDir, env
	if out, err := initCmd.CombinedOutput(); err != nil {
		t.Fatalf("dolt init: %v\n%s", err, out)
	}
	port := idleGateDoltPort(t)
	server := exec.Command(doltBin, "sql-server", "--host", "127.0.0.1", "--port", strconv.Itoa(port))
	server.Dir, server.Env = dbDir, env
	logFile, err := os.Create(filepath.Join(scratch, "server.log"))
	if err != nil {
		t.Fatal(err)
	}
	server.Stdout, server.Stderr = logFile, logFile
	if err := server.Start(); err != nil {
		t.Fatalf("starting dolt sql-server: %v", err)
	}
	t.Cleanup(func() {
		_ = server.Process.Kill()
		_ = server.Wait()
		_ = logFile.Close()
	})

	db, err := sql.Open("mysql", fmt.Sprintf("root@tcp(127.0.0.1:%d)/%s?parseTime=true", port, idleGateDoltDatabase))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	deadline := time.Now().Add(30 * time.Second)
	for {
		if err = db.Ping(); err == nil {
			break
		}
		if time.Now().After(deadline) {
			log, _ := os.ReadFile(filepath.Join(scratch, "server.log"))
			t.Fatalf("dolt sql-server on %d never answered: %v\n%s", port, err, log)
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Setenv("GC_DOLT_PASSWORD", "")
	s := &idleGateDoltServer{
		target: contract.DoltConnectionTarget{Host: "127.0.0.1", Port: strconv.Itoa(port), User: "root", Database: idleGateDoltDatabase},
		db:     db,
	}
	for _, stmt := range []string{
		"CREATE TABLE issues (id VARCHAR(64) PRIMARY KEY, status VARCHAR(32) NOT NULL)",
		"REPLACE INTO dolt_ignore VALUES ('wisps', true)",
		"REPLACE INTO dolt_ignore VALUES ('wisp_%', true)",
		"CREATE TABLE wisps (id VARCHAR(64) PRIMARY KEY, status VARCHAR(32) NOT NULL)",
		"CREATE TABLE wisp_dependencies (issue_id VARCHAR(64), depends_on_id VARCHAR(64), PRIMARY KEY (issue_id, depends_on_id))",
		"INSERT INTO issues VALUES ('gc-1', 'open')",
		"CALL DOLT_COMMIT('-Am', 'schema')",
	} {
		s.exec(t, stmt)
	}
	return s
}

func (s *idleGateDoltServer) exec(t *testing.T, stmt string) {
	t.Helper()
	if _, err := s.db.Exec(stmt); err != nil {
		t.Fatalf("%s: %v", stmt, err)
	}
}

// hash reads the version exactly as the gate does.
func (s *idleGateDoltServer) hash(t *testing.T) string {
	t.Helper()
	v, err := controlReadyReadDoltVersion(s.target)
	if err != nil {
		t.Fatalf("controlReadyReadDoltVersion: %v", err)
	}
	if v == "" {
		t.Fatal("empty DOLT_HASHOF_DB()")
	}
	return v
}

func TestControlReadyIdleGateDoltHashTracksAutocommitWrite(t *testing.T) {
	s := startIdleGateDoltServer(t)
	before := s.hash(t)
	s.exec(t, "INSERT INTO issues VALUES ('gc-2', 'open')")
	after := s.hash(t)
	t.Logf("(a) autocommit INSERT: before=%s after=%s changed=%t", before, after, before != after)
	if before == after {
		t.Fatal("(a) an autocommit INSERT left DOLT_HASHOF_DB() unchanged")
	}
}

func TestControlReadyIdleGateDoltHashTracksOtherSessionCommit(t *testing.T) {
	s := startIdleGateDoltServer(t)
	ctx := context.Background()
	conn, err := s.db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close() //nolint:errcheck // test connection
	before := s.hash(t)
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec("INSERT INTO issues VALUES ('gc-3', 'open')"); err != nil {
		t.Fatal(err)
	}
	during := s.hash(t)
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	after := s.hash(t)
	t.Logf("(b) open tx in another session: before=%s during=%s (changed=%t) after-commit=%s (changed=%t)",
		before, during, before != during, after, during != after)
	if before != during {
		t.Errorf("(b) an uncommitted transaction in another session changed the hash; expected snapshot isolation")
	}
	if during == after {
		t.Fatal("(b) the other session's COMMIT left DOLT_HASHOF_DB() unchanged")
	}
}

func TestControlReadyIdleGateDoltHashTracksIgnoredWispTables(t *testing.T) {
	s := startIdleGateDoltServer(t)
	var staged int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM dolt_status WHERE table_name IN ('wisps', 'wisp_dependencies')").Scan(&staged); err != nil {
		t.Fatal(err)
	}
	if staged != 0 {
		t.Fatalf("wisp tables appear in dolt_status (%d rows); they are not dolt_ignore'd", staged)
	}
	steps := []string{
		"INSERT INTO wisps VALUES ('gc-wisp-1', 'open')",
		"UPDATE wisps SET status = 'closed' WHERE id = 'gc-wisp-1'",
		"INSERT INTO wisp_dependencies VALUES ('gc-wisp-1', 'gc-1')",
		"DELETE FROM wisps WHERE id = 'gc-wisp-1'",
	}
	prev := s.hash(t)
	for _, stmt := range steps {
		s.exec(t, stmt)
		next := s.hash(t)
		t.Logf("(c) dolt_ignore'd write %q: before=%s after=%s changed=%t", stmt, prev, next, prev != next)
		if prev == next {
			t.Fatalf("(c) %q on a dolt_ignore'd table left DOLT_HASHOF_DB() unchanged: the idle gate would hide wisp work", stmt)
		}
		prev = next
	}
}

func TestControlReadyIdleGateDoltHashStableWithoutWrites(t *testing.T) {
	s := startIdleGateDoltServer(t)
	before := s.hash(t)
	var n int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM issues WHERE status = 'open'").Scan(&n); err != nil {
		t.Fatal(err)
	}
	afterRead := s.hash(t)
	s.exec(t, "UPDATE issues SET status = 'open' WHERE id = 'gc-1'")
	s.exec(t, "UPDATE wisps SET status = 'open' WHERE id = 'absent'")
	afterNoop := s.hash(t)
	t.Logf("(d) read-only SELECT: before=%s after=%s changed=%t; no-op UPDATEs: after=%s changed=%t",
		before, afterRead, before != afterRead, afterNoop, afterRead != afterNoop)
	if before != afterRead {
		t.Errorf("(d) a read-only SELECT changed DOLT_HASHOF_DB()")
	}
	if afterRead != afterNoop {
		t.Errorf("(d) a no-op UPDATE changed DOLT_HASHOF_DB()")
	}
}
