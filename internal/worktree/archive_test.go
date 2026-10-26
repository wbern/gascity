package worktree

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
)

func TestArchivePrivateRoundTrip(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(base, "source")
	dest := filepath.Join(base, "archives")
	if err := os.MkdirAll(filepath.Join(source, ".codex"), 0o700); err != nil {
		t.Fatal(err)
	}
	payload := []byte("synthetic configuration\n")
	if err := os.WriteFile(filepath.Join(source, ".codex/config.toml"), payload, 0o640); err != nil {
		t.Fatal(err)
	}
	plan, err := PrepareArchive(source, []string{".codex/config.toml"}, ArchiveBounds{MaxFiles: 8, MaxFileBytes: 1024, MaxTotalBytes: 4096, MinFreeBytes: 1024})
	if err != nil {
		t.Fatal(err)
	}
	defer archiveClose(plan)
	entries := plan.Entries()
	entries[0].Path = "mutated"
	if plan.Entries()[0].Path != ".codex/config.toml" {
		t.Fatal("entries expose mutable plan")
	}
	result, err := WritePrivateArchive(plan, dest, []byte(`{"kind":"test"}`), []byte(`{"request":"test"}`), func(string) (int64, error) { return 1 << 30, nil })
	if err != nil {
		t.Fatal(err)
	}
	if result.Files != 1 || result.Bytes != int64(len(payload)) {
		t.Fatalf("result = %+v", result)
	}
	copied, err := os.ReadFile(filepath.Join(result.Path, "data/.codex/config.toml"))
	if err != nil || string(copied) != string(payload) {
		t.Fatalf("copy mismatch: %v", err)
	}
	info, err := os.Stat(filepath.Join(result.Path, "data/.codex/config.toml"))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("private file: %v %v", info, err)
	}
	manifest, err := os.ReadFile(filepath.Join(result.Path, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(manifest)
	if hex.EncodeToString(sum[:]) != result.ManifestSHA256 {
		t.Fatal("manifest digest mismatch")
	}
	if err := VerifyArchiveSource(plan); err != nil {
		t.Fatal(err)
	}
}

func TestArchiveRefusesChangedSource(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	leaf := filepath.Join(base, "note.md")
	if err := os.WriteFile(leaf, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	plan, err := PrepareArchive(base, []string{"note.md"}, ArchiveBounds{MaxFiles: 2, MaxFileBytes: 1024, MaxTotalBytes: 2048})
	if err != nil {
		t.Fatal(err)
	}
	defer archiveClose(plan)
	if err := os.WriteFile(leaf, []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	if VerifyArchiveSource(plan) == nil {
		t.Fatal("changed source accepted")
	}
}

func TestArchiveRejectsUnsafeLeaves(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, leaf := range []string{"../outside", "/absolute", ".git/config", "a/../b", "a//b", "."} {
		t.Run(leaf, func(t *testing.T) {
			p, err := PrepareArchive(base, []string{leaf}, ArchiveBounds{MaxFiles: 2, MaxFileBytes: 1024, MaxTotalBytes: 2048})
			if p != nil {
				archiveClose(p)
			}
			if err == nil {
				t.Fatal("unsafe leaf accepted")
			}
		})
	}
}

func TestArchiveRefusesSymlinkAndReplacedRoot(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(base, "source")
	if err := os.Mkdir(source, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "note.md"), []byte("synthetic"), 0o600); err != nil {
		t.Fatal(err)
	}
	bounds := ArchiveBounds{MaxFiles: 2, MaxFileBytes: 1024, MaxTotalBytes: 2048}
	if err := os.Symlink("note.md", filepath.Join(source, "alias.md")); err != nil {
		t.Fatal(err)
	}
	if p, err := PrepareArchive(source, []string{"alias.md"}, bounds); err == nil {
		archiveClose(p)
		t.Fatal("symlink accepted")
	}
	p, err := PrepareArchive(source, []string{"note.md"}, bounds)
	if err != nil {
		t.Fatal(err)
	}
	defer archiveClose(p)
	if err := os.Rename(source, source+"-old"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(source, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "note.md"), []byte("synthetic"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := VerifyArchiveSource(p); err == nil {
		t.Fatal("replacement root accepted")
	}
}

func TestArchiveCapacityAndPublicDestinationRefused(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(base, "source")
	if err := os.Mkdir(source, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "note.md"), []byte("synthetic"), 0o600); err != nil {
		t.Fatal(err)
	}
	p, err := PrepareArchive(source, []string{"note.md"}, ArchiveBounds{MaxFiles: 2, MaxFileBytes: 1024, MaxTotalBytes: 2048, MinFreeBytes: 4096})
	if err != nil {
		t.Fatal(err)
	}
	defer archiveClose(p)
	dest := filepath.Join(base, "private")
	if _, err := WritePrivateArchive(p, dest, []byte(`{}`), []byte(`{}`), func(string) (int64, error) { return 0, nil }); err == nil {
		t.Fatal("capacity failure accepted")
	}
	entries, err := os.ReadDir(dest)
	if err != nil || len(entries) != 0 {
		t.Fatalf("failed capacity published files: %v %v", entries, err)
	}
	if err := os.Chmod(dest, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := WritePrivateArchive(p, dest, []byte(`{}`), []byte(`{}`), func(string) (int64, error) { return 1 << 30, nil }); err == nil {
		t.Fatal("public destination accepted")
	}
}

func TestArchiveRetainsOriginalInodeAndRebinds(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(base, "source")
	if err := os.Mkdir(source, 0o700); err != nil {
		t.Fatal(err)
	}
	leaf := filepath.Join(source, "note.md")
	if err := os.WriteFile(leaf, []byte("before"), 0o640); err != nil {
		t.Fatal(err)
	}
	writer, err := os.OpenFile(leaf, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer archiveClose(writer)
	p, err := PrepareArchive(source, []string{"note.md"}, ArchiveBounds{MaxFiles: 2, MaxFileBytes: 1024, MaxTotalBytes: 2048})
	if err != nil {
		t.Fatal(err)
	}
	defer archiveClose(p)
	result, err := WritePrivateArchive(p, filepath.Join(base, "archives"), []byte(`{}`), []byte(`{}`), func(string) (int64, error) { return 1 << 30, nil })
	if err != nil {
		t.Fatal(err)
	}
	quarantine := filepath.Join(base, "quarantine")
	if err := os.Rename(source, quarantine); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(source, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(leaf, []byte("new namespace"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := p.RebindRoot(quarantine); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.WriteAt([]byte("after!"), 0); err != nil {
		t.Fatal(err)
	}
	retained, err := os.ReadFile(filepath.Join(result.Path, "original-inodes/note.md"))
	if err != nil || string(retained) != "after!" {
		t.Fatalf("original inode not retained: %q %v", retained, err)
	}
	immutable, err := os.ReadFile(filepath.Join(result.Path, "data/note.md"))
	if err != nil || string(immutable) != "before" {
		t.Fatalf("immutable snapshot changed: %q %v", immutable, err)
	}
	recreated, err := os.ReadFile(leaf)
	if err != nil || string(recreated) != "new namespace" {
		t.Fatalf("new namespace touched: %q %v", recreated, err)
	}
	if err := VerifyArchiveSource(p); err == nil {
		t.Fatal("postarchive write accepted")
	}
}

func TestArchiveRejectsBoundsAndDestinationSwap(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(base, "source")
	if err := os.Mkdir(source, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "note.md"), []byte("synthetic"), 0o600); err != nil {
		t.Fatal(err)
	}
	if p, err := PrepareArchive(source, []string{"note.md"}, ArchiveBounds{MaxFiles: 1, MaxFileBytes: 2, MaxTotalBytes: 2}); err == nil {
		archiveClose(p)
		t.Fatal("oversize accepted")
	}
	p, err := PrepareArchive(source, []string{"note.md"}, ArchiveBounds{MaxFiles: 1, MaxFileBytes: 1024, MaxTotalBytes: 2048})
	if err != nil {
		t.Fatal(err)
	}
	defer archiveClose(p)
	dest := filepath.Join(base, "archives")
	_, err = WritePrivateArchive(p, dest, []byte(`{}`), []byte(`{}`), func(string) (int64, error) {
		if err := os.Rename(dest, dest+"-old"); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(dest, 0o700); err != nil {
			t.Fatal(err)
		}
		return 1 << 30, nil
	})
	if err == nil {
		t.Fatal("archive destination swap accepted")
	}
	for _, dir := range []string{dest, dest + "-old"} {
		entries, err := os.ReadDir(dir)
		if err != nil || len(entries) != 0 {
			t.Fatalf("swap left archive contents: %v %v", entries, err)
		}
	}
}

func TestArchiveRejectsSameBytesMetadataMutation(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	leaf := filepath.Join(base, "note.md")
	if err := os.WriteFile(leaf, []byte("same"), 0o600); err != nil {
		t.Fatal(err)
	}
	p, err := PrepareArchive(base, []string{"note.md"}, ArchiveBounds{MaxFiles: 1, MaxFileBytes: 1024, MaxTotalBytes: 2048})
	if err != nil {
		t.Fatal(err)
	}
	defer archiveClose(p)
	before, err := os.Stat(leaf)
	if err != nil {
		t.Fatal(err)
	}
	if archiveChangeStamp(before) == "" {
		t.Skip("platform lacks ctime evidence")
	}
	if err := os.Chmod(leaf, 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(leaf, 0o600); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(leaf)
	if err != nil {
		t.Fatal(err)
	}
	if archiveChangeStamp(before) == archiveChangeStamp(after) {
		t.Skip("filesystem ctime resolution did not expose mutation")
	}
	if err := VerifyArchiveSource(p); err == nil {
		t.Fatal("same-byte source metadata mutation accepted")
	}
}
