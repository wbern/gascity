package beads_test

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/fsys"
)

// A retained reader must notice content changes even when size and mtime match.
func TestFileStorePreservedMtime(t *testing.T) {
	for _, replace := range []bool{false, true} {
		name := "inplace"
		if replace {
			name = "replacement"
		}
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "beads.json")
			writer, err := beads.OpenFileStore(fsys.OSFS{}, path)
			if err != nil {
				t.Fatal(err)
			}
			bead, err := writer.Create(beads.Bead{Title: "aaaa"})
			if err != nil {
				t.Fatal(err)
			}
			reader, err := beads.OpenFileStore(fsys.OSFS{}, path)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := reader.Get(bead.ID); err != nil {
				t.Fatal(err)
			}
			info, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			updated := bytes.Replace(data, []byte(`"aaaa"`), []byte(`"bbbb"`), 1)
			if bytes.Equal(data, updated) || len(data) != len(updated) {
				t.Fatal("invalid mutation")
			}
			target := path
			if replace {
				target += ".replacement"
			}
			if err := os.WriteFile(target, updated, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Chtimes(target, info.ModTime(), info.ModTime()); err != nil {
				t.Fatal(err)
			}
			if replace {
				if err := os.Rename(target, path); err != nil {
					t.Fatal(err)
				}
			}
			after, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			if after.Size() != info.Size() || !after.ModTime().Equal(info.ModTime()) {
				t.Fatal("size/mtime changed")
			}
			t.Logf("OS stat before=%+v after=%+v", info.Sys(), after.Sys())
			got, err := reader.Get(bead.ID)
			if err != nil {
				t.Fatal(err)
			}
			if got.Title != "bbbb" {
				t.Fatalf("retained reader title = %q, want bbbb", got.Title)
			}
		})
	}
}

type readRaceFS struct {
	fsys.FS
	afterRead func()
}

func (f *readRaceFS) ReadFile(path string) ([]byte, error) {
	data, err := f.FS.ReadFile(path)
	if err == nil && f.afterRead != nil {
		callback := f.afterRead
		f.afterRead = nil
		callback()
	}
	return data, err
}

// Metadata sampled after a read must never certify older bytes as current.
func TestFileStoreReadRaceDoesNotPoisonCache(t *testing.T) {
	base := fsys.NewFake()
	path := "/city/beads.json"
	writer, err := beads.OpenFileStore(base, path)
	if err != nil {
		t.Fatal(err)
	}
	bead, err := writer.Create(beads.Bead{Title: "aaaa"})
	if err != nil {
		t.Fatal(err)
	}
	wrapped := &readRaceFS{FS: base}
	reader, err := beads.OpenFileStore(wrapped, path)
	if err != nil {
		t.Fatal(err)
	}
	wrapped.afterRead = func() {
		if err := writer.Update(bead.ID, beads.UpdateOpts{Title: ptr("bbbb")}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := reader.Get(bead.ID); err != nil {
		t.Fatal(err)
	}
	got, err := reader.Get(bead.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Title != "bbbb" {
		t.Fatalf("reader cached stale bytes: %q", got.Title)
	}
}

type sysInfo struct {
	os.FileInfo
	sys any
}

func (f sysInfo) Sys() any { return f.sys }

type unknownSysFS struct {
	fsys.FS
	sys any
}

func (f unknownSysFS) Stat(path string) (os.FileInfo, error) {
	info, err := f.FS.Stat(path)
	if err != nil {
		return info, err
	}
	return sysInfo{info, f.sys}, nil
}

func TestFileStoreUnknownMetadataReloads(t *testing.T) {
	for _, sys := range []any{nil, "unknown", (*int)(nil), struct{}{}, struct{ Dev, Ino uint64 }{1, 2}} {
		t.Run(fmt.Sprintf("%T", sys), func(t *testing.T) {
			base := fsys.NewFake()
			path := "/city/beads.json"
			writer, err := beads.OpenFileStore(base, path)
			if err != nil {
				t.Fatal(err)
			}
			bead, err := writer.Create(beads.Bead{Title: "aaaa"})
			if err != nil {
				t.Fatal(err)
			}
			reader, err := beads.OpenFileStore(unknownSysFS{base, sys}, path)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := reader.Get(bead.ID); err != nil {
				t.Fatal(err)
			}
			mtime := base.ModTimes[path]
			data := bytes.Replace(base.Files[path], []byte(`"aaaa"`), []byte(`"bbbb"`), 1)
			if err := base.WriteFile(path, data, 0o600); err != nil {
				t.Fatal(err)
			}
			base.ModTimes[path] = mtime
			got, err := reader.Get(bead.ID)
			if err != nil {
				t.Fatal(err)
			}
			if got.Title != "bbbb" {
				t.Fatalf("unknown metadata retained %q", got.Title)
			}
		})
	}
}

// BenchmarkFileStoreSmallRead compares cached reads with safe metadata fallback.
func BenchmarkFileStoreSmallRead(b *testing.B) {
	for _, count := range []int{1, 32} {
		for _, mode := range []string{"cached", "statErrorReload", "unknownSysReload"} {
			b.Run(fmt.Sprintf("beads%d/%s", count, mode), func(b *testing.B) {
				path := filepath.Join(b.TempDir(), "beads.json")
				writer, err := beads.OpenFileStore(fsys.OSFS{}, path)
				if err != nil {
					b.Fatal(err)
				}
				var id string
				for i := 0; i < count; i++ {
					bead, err := writer.Create(beads.Bead{Title: "small store benchmark"})
					if err != nil {
						b.Fatal(err)
					}
					id = bead.ID
				}
				var filesystem fsys.FS = fsys.OSFS{}
				switch mode {
				case "statErrorReload":
					filesystem = &toggledErrorFS{FS: filesystem, path: path, statErr: os.ErrPermission}
				case "unknownSysReload":
					filesystem = unknownSysFS{FS: filesystem}
				}
				reader, err := beads.OpenFileStore(filesystem, path)
				if err != nil {
					b.Fatal(err)
				}
				if _, err := reader.Get(id); err != nil {
					b.Fatal(err)
				}
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					if _, err := reader.Get(id); err != nil {
						b.Fatal(err)
					}
				}
			})
		}
	}
}

type renameRaceFS struct {
	fsys.FS
	afterRename func()
}

func (f *renameRaceFS) Rename(old, newPath string) error {
	if err := f.FS.Rename(old, newPath); err != nil {
		return err
	}
	if f.afterRename != nil {
		callback := f.afterRename
		f.afterRename = nil
		callback()
	}
	return nil
}

func TestFileStoreSaveRaceDoesNotPoisonCache(t *testing.T) {
	base := fsys.NewFake()
	path := "/city/beads.json"
	wrapped := &renameRaceFS{FS: base}
	writer, err := beads.OpenFileStore(wrapped, path)
	if err != nil {
		t.Fatal(err)
	}
	other, err := beads.OpenFileStore(base, path)
	if err != nil {
		t.Fatal(err)
	}
	wrapped.afterRename = func() {
		if err := other.Update("gc-1", beads.UpdateOpts{Title: ptr("bbbb")}); err != nil {
			t.Fatal(err)
		}
	}
	bead, err := writer.Create(beads.Bead{Title: "aaaa"})
	if err != nil {
		t.Fatal(err)
	}
	got, err := writer.Get(bead.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Title != "bbbb" {
		t.Fatalf("writer cached another writer's metadata with title %q", got.Title)
	}
}
