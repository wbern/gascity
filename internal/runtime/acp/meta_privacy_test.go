package acp

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
)

// The ACP sidecar carries session identity and drain state. Unlike the herdr
// and subprocess sidecars it is never seeded from the session environment, so
// no API key lands here — but a reader can still impersonate the session and a
// writer can forge a drain acknowledgement, and the default directory sat on a
// path every user on the host shares.
func TestSetMetaWritesOwnerOnlyFiles(t *testing.T) {
	p := NewProviderWithDir(t.TempDir(), Config{})
	if err := p.SetMeta("s1", "GC_ALIAS", "kernel"); err != nil {
		t.Fatalf("SetMeta: %v", err)
	}

	info, err := os.Lstat(p.metaPath("s1", "GC_ALIAS"))
	if err != nil {
		t.Fatalf("Lstat: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Errorf("sidecar mode = %04o, want 0600", got)
	}
	dir, err := os.Lstat(p.dir)
	if err != nil {
		t.Fatalf("Lstat dir: %v", err)
	}
	if got := dir.Mode().Perm(); got != 0o700 {
		t.Errorf("provider dir mode = %04o, want 0700", got)
	}
	// Control: the tightening must not cost the write.
	if got, _ := p.GetMeta("s1", "GC_ALIAS"); got != "kernel" {
		t.Errorf("GetMeta = %q, want %q", got, "kernel")
	}
}

// os.WriteFile's perm argument is consulted only when the file is created, and
// MkdirAll returns nil on an existing directory whatever its mode. A host that
// already ran an older binary therefore keeps its 0755 directory and 0644
// sidecars unless each write narrows them explicitly.
func TestSetMetaNarrowsPreExistingWideModes(t *testing.T) {
	dir := t.TempDir()
	p := NewProviderWithDir(dir, Config{})
	path := p.metaPath("s1", "GC_ALIAS")
	if err := os.WriteFile(path, []byte("old"), 0o644); err != nil {
		t.Fatalf("seed sidecar: %v", err)
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatalf("seed dir: %v", err)
	}

	if err := p.SetMeta("s1", "GC_ALIAS", "kernel"); err != nil {
		t.Fatalf("SetMeta: %v", err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("Lstat: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Errorf("pre-existing sidecar mode = %04o, want 0600 after narrowing", got)
	}
	dirInfo, err := os.Lstat(dir)
	if err != nil {
		t.Fatalf("Lstat dir: %v", err)
	}
	if got := dirInfo.Mode().Perm(); got != 0o700 {
		t.Errorf("pre-existing dir mode = %04o, want 0700 after narrowing", got)
	}
	// Control: narrowing must not cost the write.
	if got, _ := p.GetMeta("s1", "GC_ALIAS"); got != "kernel" {
		t.Errorf("GetMeta = %q, want %q", got, "kernel")
	}
}

// The city-less default path is identical for every user on the host, and
// MkdirAll returns nil on a directory someone else owns. Namespacing by euid
// keeps two legitimate users off one path so the ownership check in
// EnsurePrivateDir is a real check rather than a permanent failure for whoever
// logs in second.
func TestDefaultProviderDirIsPerEUID(t *testing.T) {
	// Equality, not a substring: under euid 0 or 1 — root CI, most containers —
	// any stray digit anywhere in the temp path would satisfy a Contains check
	// and the assertion would pass on an un-namespaced directory.
	want := filepath.Join(os.TempDir(), fmt.Sprintf("gc-acp-%d", os.Geteuid()))
	if got := defaultProviderDir(); got != want {
		t.Errorf("default provider dir = %q, want %q", got, want)
	}
}

// A provider is intentionally constructible with a path that later proves
// unsafe: constructors cannot return an error. Start must nevertheless
// validate again before it reserves a session, stages a worktree, or spawns a
// child that could write control or metadata sidecars through that path.
func TestStartRejectsUnsafeProviderDirectoryBeforeSpawn(t *testing.T) {
	target := t.TempDir()
	dir := filepath.Join(t.TempDir(), "acp")
	if err := os.Symlink(target, dir); err != nil {
		t.Fatalf("create unsafe provider-dir symlink: %v", err)
	}
	marker := filepath.Join(t.TempDir(), "spawned")
	p := NewProviderWithDir(dir, Config{})

	err := p.Start(context.Background(), "unsafe-dir", runtime.Config{
		Command: "touch " + marker,
	})
	if err == nil || !strings.Contains(err.Error(), "private provider directory") {
		t.Fatalf("Start error = %v, want unsafe private directory error", err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("child spawned despite unsafe provider directory: marker stat err = %v", err)
	}
	if entries, err := os.ReadDir(target); err != nil {
		t.Fatalf("read target directory: %v", err)
	} else if len(entries) != 0 {
		t.Fatalf("unsafe directory target has sidecars: %v", entries)
	}
}
