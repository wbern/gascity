package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

func TestWorktreeScanTextReportUsesManagedRootsAndSkipsLiveSessions(t *testing.T) {
	clearGCEnv(t)

	cityDir := t.TempDir()
	rigRoot := filepath.Join(cityDir, "rigs", "demo")
	liveDir := filepath.Join(cityDir, ".gc", "worktrees", "live")
	orphanDir := filepath.Join(cityDir, ".gc", "worktrees", "orphan")
	nestedStray := filepath.Join(rigRoot, "nested-stray")

	for _, dir := range []string{liveDir, orphanDir, nestedStray} {
		if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	restore := stubWorktreeScanDeps(t)
	worktreeResolveCity = func() (string, error) { return cityDir, nil }
	worktreeLoadCityConfig = func(string, ...io.Writer) (*config.City, error) {
		return &config.City{Rigs: []config.Rig{{Name: "demo", Path: filepath.Join("rigs", "demo")}}}, nil
	}
	worktreeOpenCityStoreAt = func(string) (beads.Store, error) { return nil, nil }
	worktreeListAllSessionBeads = func(beads.Store, beads.ListQuery) ([]beads.Bead, error) {
		return []beads.Bead{{ID: "gc-session-1", Metadata: map[string]string{"worker_dir": liveDir}}}, nil
	}
	newGitProbe = func(workDir string) gitProbe {
		switch filepath.Clean(workDir) {
		case filepath.Clean(orphanDir):
			return fakeStrayProbe{isRepo: true}
		case filepath.Clean(nestedStray):
			return fakeStrayProbe{isRepo: true, uncommitted: true}
		default:
			t.Fatalf("unexpected git probe for %q", workDir)
			return nil
		}
	}
	t.Cleanup(restore)

	var stdout, stderr bytes.Buffer
	cmd := newWorktreeCmd(&stdout, &stderr)
	cmd.SetArgs([]string{"scan"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute() = %v; stderr=%s", err, stderr.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}

	out := stdout.String()
	for _, want := range []string{
		"RECLAIMABLE",
		orphanDir,
		nestedStray,
		// gcw-kb6r (#41) widened this reason: the gate now also holds a
		// worktree whose only content is ignored files, and the text says so.
		"uncommitted or ignored work",
		"2 stray checkout(s): 1 reclaimable, 1 kept",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("stdout missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, liveDir) {
		t.Fatalf("stdout unexpectedly includes live-bound worktree %q:\n%s", liveDir, out)
	}
}

func TestWorktreeScanJSONOutput(t *testing.T) {
	clearGCEnv(t)

	restore := stubWorktreeScanDeps(t)
	worktreeResolveCity = func() (string, error) { return t.TempDir(), nil }
	worktreeLoadCityConfig = func(string, ...io.Writer) (*config.City, error) { return &config.City{}, nil }
	worktreeOpenCityStoreAt = func(string) (beads.Store, error) { return nil, nil }
	worktreeListAllSessionBeads = func(beads.Store, beads.ListQuery) ([]beads.Bead, error) { return nil, nil }
	worktreeScanStrayWorktrees = func([]string, map[string]bool, func(string) gitProbe) ([]strayWorktree, error) {
		return []strayWorktree{{Path: "/tmp/orphan", Reclaimable: true}}, nil
	}
	t.Cleanup(restore)

	var stdout, stderr bytes.Buffer
	cmd := newWorktreeCmd(&stdout, &stderr)
	cmd.SetArgs([]string{"scan", "--json"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute() = %v; stderr=%s", err, stderr.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}

	// The payload is an {schema_version, ok, ...} envelope, not a bare array:
	// the CLI's JSON contract rejects anything else, which is why this flag
	// was inert for its entire life despite this test passing.
	var got worktreeScanJSON
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("stdout is not valid JSON: %v\n%s", err, stdout.String())
	}
	if got.SchemaVersion != worktreeJSONSchemaVersion || !got.OK {
		t.Fatalf("envelope = %#v, want schema_version %q and ok true", got, worktreeJSONSchemaVersion)
	}
	if len(got.Strays) != 1 || got.Strays[0].Path != "/tmp/orphan" || !got.Strays[0].Reclaimable {
		t.Fatalf("decoded JSON = %#v, want one reclaimable orphan", got.Strays)
	}
}

func TestWorktreeScanEmptyCase(t *testing.T) {
	clearGCEnv(t)

	restore := stubWorktreeScanDeps(t)
	worktreeResolveCity = func() (string, error) { return t.TempDir(), nil }
	worktreeLoadCityConfig = func(string, ...io.Writer) (*config.City, error) { return &config.City{}, nil }
	worktreeOpenCityStoreAt = func(string) (beads.Store, error) { return nil, nil }
	worktreeListAllSessionBeads = func(beads.Store, beads.ListQuery) ([]beads.Bead, error) { return nil, nil }
	worktreeScanStrayWorktrees = func([]string, map[string]bool, func(string) gitProbe) ([]strayWorktree, error) {
		return nil, nil
	}
	t.Cleanup(restore)

	var stdout, stderr bytes.Buffer
	cmd := newWorktreeCmd(&stdout, &stderr)
	cmd.SetArgs([]string{"scan"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute() = %v; stderr=%s", err, stderr.String())
	}
	if got := strings.TrimSpace(stdout.String()); got != "No stray worktrees found under managed roots." {
		t.Fatalf("stdout = %q, want empty-case message", got)
	}
}

func TestWorktreeScanFailsClosedWhenStoreUnavailable(t *testing.T) {
	clearGCEnv(t)

	restore := stubWorktreeScanDeps(t)
	worktreeResolveCity = func() (string, error) { return t.TempDir(), nil }
	worktreeLoadCityConfig = func(string, ...io.Writer) (*config.City, error) { return &config.City{}, nil }
	worktreeOpenCityStoreAt = func(string) (beads.Store, error) { return nil, errors.New("store offline") }
	t.Cleanup(restore)

	var stdout, stderr bytes.Buffer
	cmd := newWorktreeCmd(&stdout, &stderr)
	cmd.SetArgs([]string{"scan"})
	err := cmd.Execute()
	if !errors.Is(err, errExit) {
		t.Fatalf("Execute() = %v, want errExit", err)
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q, want empty on failure", stdout.String())
	}
	if !strings.Contains(stderr.String(), "loading live session set: store offline") {
		t.Fatalf("stderr = %q, want live-set failure", stderr.String())
	}
}

func stubWorktreeScanDeps(t *testing.T) func() {
	t.Helper()
	origResolveCity := worktreeResolveCity
	origLoadCityConfig := worktreeLoadCityConfig
	origOpenCityStoreAt := worktreeOpenCityStoreAt
	origListAllSessionBeads := worktreeListAllSessionBeads
	origScanStrayWorktrees := worktreeScanStrayWorktrees
	origNewGitProbe := newGitProbe
	return func() {
		worktreeResolveCity = origResolveCity
		worktreeLoadCityConfig = origLoadCityConfig
		worktreeOpenCityStoreAt = origOpenCityStoreAt
		worktreeListAllSessionBeads = origListAllSessionBeads
		worktreeScanStrayWorktrees = origScanStrayWorktrees
		newGitProbe = origNewGitProbe
	}
}
