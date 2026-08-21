package outputfirewall

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/testutil"
)

func TestMain(m *testing.M) {
	testutil.ClearManagedOutputFirewallEnv()
	os.Exit(m.Run())
}

func TestWriteCancellationAfterSpillRemovesUnpublishedArtifact(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv(managedEnv, "1")
	t.Setenv(budgetEnv, "512")
	t.Setenv(verbsEnv, "show")
	t.Setenv(spillModeEnv, "secure")
	t.Setenv(spillRootEnv, filepath.Dir(dir))
	t.Setenv(spillPathEnv, filepath.Base(dir))
	ctx, cancel := context.WithCancel(context.Background())
	afterSpillWrite = cancel
	t.Cleanup(func() { afterSpillWrite = nil })
	var stdout, stderr bytes.Buffer
	if code := Write(ctx, "managed_read", "show", []byte(`{"secret":"`+strings.Repeat("x", 1000)+`"}`), true, &stdout, &stderr); code != 1 {
		t.Fatalf("code=%d, want cancellation", code)
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout=%q, want no unpublished result", stdout.String())
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("unpublished spill remains: %v", entries)
	}
}

func TestWriteOversizedPayloadProducesValidManifest(t *testing.T) {
	t.Setenv(managedEnv, "1")
	t.Setenv(budgetEnv, "512")
	t.Setenv(verbsEnv, "show")
	t.Setenv(spillModeEnv, "disabled")
	var stdout, stderr bytes.Buffer
	if code := Write(context.Background(), "managed_read", "show", []byte(`{"secret":"`+strings.Repeat("x", 1000)+`"}`), true, &stdout, &stderr); code != 0 {
		t.Fatalf("code=%d stderr=%q", code, stderr.String())
	}
	if stdout.Len() > 512 || !json.Valid(stdout.Bytes()) || strings.Contains(stdout.String(), strings.Repeat("x", 32)) {
		t.Fatalf("stdout=%q", stdout.String())
	}
	var manifest struct {
		Remediation string `json:"remediation"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &manifest); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(manifest.Remediation, "--allow-unbounded") || !strings.Contains(manifest.Remediation, "spill.path") {
		t.Fatalf("remediation=%q", manifest.Remediation)
	}
}

func TestWriteDirectorySwapAfterSpillPublishesUnavailableManifest(t *testing.T) {
	parent := t.TempDir()
	dir := filepath.Join(parent, "output")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv(managedEnv, "1")
	t.Setenv(budgetEnv, "512")
	t.Setenv(verbsEnv, "show")
	t.Setenv(spillModeEnv, "secure")
	t.Setenv(spillRootEnv, parent)
	t.Setenv(spillPathEnv, "output")
	afterSpillWrite = func() {
		if err := os.Rename(dir, filepath.Join(parent, "replaced")); err != nil {
			t.Fatalf("rename: %v", err)
		}
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatalf("replace: %v", err)
		}
	}
	t.Cleanup(func() { afterSpillWrite = nil })
	var stdout, stderr bytes.Buffer
	if code := Write(context.Background(), "managed_read", "show", []byte(`{"secret":"`+strings.Repeat("x", 1000)+`"}`), true, &stdout, &stderr); code != 0 {
		t.Fatalf("code=%d stderr=%q", code, stderr.String())
	}
	var manifest struct {
		Spill struct {
			Mode string `json:"mode"`
			Path string `json:"path"`
		} `json:"spill"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.Spill.Mode != "unavailable" || manifest.Spill.Path != "" {
		t.Fatalf("spill=%#v", manifest.Spill)
	}
	entries, err := os.ReadDir(filepath.Join(parent, "replaced"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("artifact survived directory replacement: %v", entries)
	}
}

func TestWritePartialSpillFailureLeavesNoArtifact(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv(managedEnv, "1")
	t.Setenv(budgetEnv, "512")
	t.Setenv(verbsEnv, "show")
	t.Setenv(spillModeEnv, "secure")
	t.Setenv(spillRootEnv, filepath.Dir(dir))
	t.Setenv(spillPathEnv, filepath.Base(dir))
	writeSpillFile = func(f *os.File, payload []byte) error {
		if _, err := f.Write(payload[:16]); err != nil {
			return err
		}
		return syscall.ENOSPC
	}
	t.Cleanup(func() {
		writeSpillFile = func(f *os.File, payload []byte) error {
			n, err := f.Write(payload)
			if err == nil && n != len(payload) {
				return io.ErrShortWrite
			}
			return err
		}
	})
	var stdout, stderr bytes.Buffer
	if code := Write(context.Background(), "managed_read", "show", []byte(`{"secret":"`+strings.Repeat("x", 1000)+`"}`), true, &stdout, &stderr); code != 0 {
		t.Fatalf("code=%d stderr=%q", code, stderr.String())
	}
	if !json.Valid(stdout.Bytes()) || strings.Contains(stdout.String(), strings.Repeat("x", 32)) || !strings.Contains(stdout.String(), `"mode":"unavailable"`) {
		t.Fatalf("stdout=%q", stdout.String())
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("partial spill remains: %v", entries)
	}
}

func TestWriteShortSpillWriteLeavesNoArtifact(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv(managedEnv, "1")
	t.Setenv(budgetEnv, "512")
	t.Setenv(verbsEnv, "show")
	t.Setenv(spillModeEnv, "secure")
	t.Setenv(spillRootEnv, filepath.Dir(dir))
	t.Setenv(spillPathEnv, filepath.Base(dir))
	writeSpillFile = func(f *os.File, payload []byte) error { _, err := f.Write(payload[:16]); return err }
	t.Cleanup(func() {
		writeSpillFile = func(f *os.File, payload []byte) error {
			n, err := f.Write(payload)
			if err == nil && n != len(payload) {
				return io.ErrShortWrite
			}
			return err
		}
	})
	var stdout, stderr bytes.Buffer
	if code := Write(context.Background(), "managed_read", "show", []byte(`{"secret":"`+strings.Repeat("x", 1000)+`"}`), true, &stdout, &stderr); code != 0 {
		t.Fatalf("code=%d stderr=%q", code, stderr.String())
	}
	if !json.Valid(stdout.Bytes()) || !strings.Contains(stdout.String(), `"mode":"unavailable"`) {
		t.Fatalf("stdout=%q", stdout.String())
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("short spill remains: %v", entries)
	}
}

func TestCleanupRemovesOnlyExpiredOwnedArtifacts(t *testing.T) {
	dir := t.TempDir()
	expired := filepath.Join(dir, "output-0123456789abcdef0123456789abcdef")
	if err := os.WriteFile(expired, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-25 * time.Hour)
	if err := os.Chtimes(expired, old, old); err != nil {
		t.Fatal(err)
	}
	keep := filepath.Join(dir, "keep")
	if err := os.WriteFile(keep, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close() //nolint:errcheck
	cleanup(root, 24*time.Hour)
	if _, err := os.Stat(expired); !os.IsNotExist(err) {
		t.Fatalf("expired=%v", err)
	}
	if _, err := os.Stat(keep); err != nil {
		t.Fatalf("keep=%v", err)
	}
}

// CONTRACT CHANGE (2026-08-21). This test previously asserted that concurrent
// spills of the SAME payload land in DISTINCT files. That was the duplication
// bug, asserted as a requirement: names came from 16 random bytes, so a tree
// holding 2,088 distinct payloads had grown to 49,680 files / 6.3 GiB on gc2.
//
// The invariant that actually matters is unchanged and still checked here: each
// concurrent writer ends with a correct, complete, 0600 artifact. What changed
// is that identical payloads now CONVERGE on one file. Distinct payloads still
// get distinct files — asserted below so convergence cannot hide a collision.
func TestConcurrentSpillsShareOneArtifactPerPayload(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv(spillRootEnv, filepath.Dir(dir))
	t.Setenv(spillPathEnv, filepath.Base(dir))
	payload := []byte("payload")
	paths := make(chan string, 8)
	for range cap(paths) {
		go func() {
			root := prepareSpill()
			if root == nil {
				paths <- ""
				return
			}
			defer func() { _ = root.root.Close() }()
			name, err := artifactName(sha256.Sum256(payload))
			if err != nil || !writeSpill(context.Background(), root.root, name, payload) {
				paths <- ""
				return
			}
			paths <- filepath.Join(root.dir, name)
		}()
	}
	seen := map[string]bool{}
	for range cap(paths) {
		path := <-paths
		if path == "" {
			t.Fatal("concurrent writer failed")
		}
		seen[path] = true
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("artifact=%q err=%v mode=%v", path, err, info.Mode())
		}
		if got, err := os.ReadFile(path); err != nil || string(got) != string(payload) {
			t.Fatalf("content=%q err=%v", got, err)
		}
	}
	if len(seen) != 1 {
		t.Fatalf("identical payloads must share one artifact, got %d", len(seen))
	}
	// No temp files may survive a concurrent race.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected exactly one artifact, got %d entries", len(entries))
	}
}

func TestDistinctPayloadsGetDistinctArtifacts(t *testing.T) {
	a, err := artifactName(sha256.Sum256([]byte("alpha")))
	if err != nil {
		t.Fatal(err)
	}
	b, err := artifactName(sha256.Sum256([]byte("beta")))
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Fatalf("distinct payloads collided: %q", a)
	}
	// The reaper and cleanup() both validate this shape; content addressing must
	// not change it or the TTL sweep silently stops matching its own artifacts.
	if !artifact(a) || !artifact(b) {
		t.Fatalf("artifact name shape broken: %q %q", a, b)
	}
}

// Reuse MUST restart the TTL clock. Expiry is mtime + retention while the
// envelope advertises expires_at = now + retention; a reused file left with an
// old mtime could be swept while a live envelope still points at it.
func TestReuseRefreshesExpiryClock(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv(spillRootEnv, filepath.Dir(dir))
	t.Setenv(spillPathEnv, filepath.Base(dir))
	root := prepareSpill()
	if root == nil {
		t.Fatal("prepareSpill")
	}
	defer func() { _ = root.root.Close() }()

	payload := []byte("repeated-bead-list")
	name, err := artifactName(sha256.Sum256(payload))
	if err != nil {
		t.Fatal(err)
	}
	if !writeSpill(context.Background(), root.root, name, payload) {
		t.Fatal("first write failed")
	}
	path := filepath.Join(root.dir, name)
	stale := time.Now().Add(-23 * time.Hour)
	if err := os.Chtimes(path, stale, stale); err != nil {
		t.Fatal(err)
	}
	if !writeSpill(context.Background(), root.root, name, payload) {
		t.Fatal("reuse failed")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if time.Since(info.ModTime()) > time.Minute {
		t.Fatalf("reuse did not refresh mtime: %v", info.ModTime())
	}
	if got, err := os.ReadFile(path); err != nil || string(got) != string(payload) {
		t.Fatalf("content=%q err=%v", got, err)
	}
}

// A truncated or corrupt file at the content-addressed name must not be reused.
func TestReuseRejectsWrongSizedArtifact(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv(spillRootEnv, filepath.Dir(dir))
	t.Setenv(spillPathEnv, filepath.Base(dir))
	root := prepareSpill()
	if root == nil {
		t.Fatal("prepareSpill")
	}
	defer func() { _ = root.root.Close() }()

	payload := []byte("full-payload-bytes")
	name, err := artifactName(sha256.Sum256(payload))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root.dir, name)
	if err := os.WriteFile(path, []byte("trunc"), 0o600); err != nil {
		t.Fatal(err)
	}
	if reuseSpill(root.root, name, len(payload)) {
		t.Fatal("reused a wrong-sized artifact")
	}
	if !writeSpill(context.Background(), root.root, name, payload) {
		t.Fatal("write over corrupt artifact failed")
	}
	if got, err := os.ReadFile(path); err != nil || string(got) != string(payload) {
		t.Fatalf("corrupt artifact not repaired: %q err=%v", got, err)
	}
}

// A directory sitting at the artifact name is malformed content; reuse must
// refuse it rather than treat it as a payload.
func TestReuseRejectsNonRegularArtifact(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv(spillRootEnv, filepath.Dir(dir))
	t.Setenv(spillPathEnv, filepath.Base(dir))
	root := prepareSpill()
	if root == nil {
		t.Fatal("prepareSpill")
	}
	defer func() { _ = root.root.Close() }()

	payload := []byte("payload-for-dir-clash")
	name, err := artifactName(sha256.Sum256(payload))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root.dir, name), 0o700); err != nil {
		t.Fatal(err)
	}
	if reuseSpill(root.root, name, len(payload)) {
		t.Fatal("reused a directory as an artifact")
	}
}

// Deduplication must survive the TTL sweep: cleanup() removes by mtime, and a
// refreshed artifact is younger than the cutoff even though it was first
// written long ago.
func TestCleanupKeepsRefreshedArtifactRemovesStaleOne(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv(spillRootEnv, filepath.Dir(dir))
	t.Setenv(spillPathEnv, filepath.Base(dir))
	root := prepareSpill()
	if root == nil {
		t.Fatal("prepareSpill")
	}
	defer func() { _ = root.root.Close() }()

	live := []byte("still-referenced")
	liveName, err := artifactName(sha256.Sum256(live))
	if err != nil {
		t.Fatal(err)
	}
	dead := []byte("nobody-refers-to-this")
	deadName, err := artifactName(sha256.Sum256(dead))
	if err != nil {
		t.Fatal(err)
	}
	if !writeSpill(context.Background(), root.root, liveName, live) ||
		!writeSpill(context.Background(), root.root, deadName, dead) {
		t.Fatal("setup writes failed")
	}
	old := time.Now().Add(-48 * time.Hour)
	for _, n := range []string{liveName, deadName} {
		if err := os.Chtimes(filepath.Join(root.dir, n), old, old); err != nil {
			t.Fatal(err)
		}
	}
	// A fresh spill of the live payload refreshes it; the dead one stays stale.
	if !writeSpill(context.Background(), root.root, liveName, live) {
		t.Fatal("refresh failed")
	}
	cleanup(root.root, 24*time.Hour)
	if _, err := os.Stat(filepath.Join(root.dir, liveName)); err != nil {
		t.Fatalf("refreshed artifact was swept: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root.dir, deadName)); !os.IsNotExist(err) {
		t.Fatalf("stale artifact survived: %v", err)
	}
}

func TestPrepareSpillRejectsFinalAndIntermediateSymlinks(t *testing.T) {
	parent, target := t.TempDir(), t.TempDir()
	valid := filepath.Join(parent, "valid")
	if err := os.Mkdir(valid, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv(spillRootEnv, parent)
	t.Setenv(spillPathEnv, "valid")
	if root := prepareSpill(); root == nil {
		t.Fatal("rejected valid spill directory")
	} else {
		_ = root.root.Close()
	}
	final := filepath.Join(parent, "output")
	if err := os.Symlink(target, final); err != nil {
		t.Fatal(err)
	}
	t.Setenv(spillPathEnv, "output")
	if root := prepareSpill(); root != nil {
		_ = root.root.Close()
		t.Fatal("accepted final symlink")
	}
	linked := filepath.Join(parent, "linked")
	if err := os.Symlink(target, linked); err != nil {
		t.Fatal(err)
	}
	victimDir := filepath.Join(target, "output")
	if err := os.Mkdir(victimDir, 0o700); err != nil {
		t.Fatal(err)
	}
	victim := filepath.Join(victimDir, "output-0123456789abcdef0123456789abcdef")
	if err := os.WriteFile(victim, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(spillPathEnv, filepath.Join("linked", "output"))
	if root := prepareSpill(); root != nil {
		_ = root.root.Close()
		t.Fatal("accepted intermediate symlink")
	}
	if _, err := os.Stat(victim); err != nil {
		t.Fatalf("victim=%v", err)
	}
}

func TestRequiredSpillFailurePublishesUnavailableManifestAndFails(t *testing.T) {
	root := t.TempDir()
	blocked := filepath.Join(root, "blocked")
	if err := os.WriteFile(blocked, []byte("file"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(managedEnv, "1")
	t.Setenv(budgetEnv, "512")
	t.Setenv(verbsEnv, "show")
	t.Setenv(spillModeEnv, "required")
	t.Setenv(spillRootEnv, root)
	t.Setenv(spillPathEnv, filepath.Join("blocked", "output"))
	var stdout, stderr bytes.Buffer
	if code := Write(context.Background(), "managed_read", "show", []byte(`{"secret":"`+strings.Repeat("x", 1000)+`"}`), true, &stdout, &stderr); code != 1 {
		t.Fatalf("code=%d", code)
	}
	if !json.Valid(stdout.Bytes()) || !strings.Contains(stdout.String(), `"mode":"unavailable"`) || strings.Contains(stdout.String(), strings.Repeat("x", 32)) {
		t.Fatalf("stdout=%q", stdout.String())
	}
}
