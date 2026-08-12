package outputfirewall

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

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

func TestConcurrentSpillsUseDistinctPrivateArtifacts(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv(spillRootEnv, filepath.Dir(dir))
	t.Setenv(spillPathEnv, filepath.Base(dir))
	paths := make(chan string, 8)
	for range cap(paths) {
		go func() {
			root := prepareSpill()
			if root == nil {
				paths <- ""
				return
			}
			defer func() { _ = root.root.Close() }()
			name, err := artifactName()
			if err != nil || !writeSpill(context.Background(), root.root, name, []byte("payload")) {
				paths <- ""
				return
			}
			paths <- filepath.Join(root.dir, name)
		}()
	}
	seen := map[string]bool{}
	for range cap(paths) {
		path := <-paths
		if path == "" || seen[path] {
			t.Fatalf("path=%q", path)
		}
		seen[path] = true
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("artifact=%q err=%v mode=%v", path, err, info.Mode())
		}
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
