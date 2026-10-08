package fsys

import (
	"os"
	"testing"
)

type contentInfo struct {
	os.FileInfo
	sys any
}

func (f contentInfo) Sys() any { return f.sys }

func TestSameFileMetadataUnixShapes(t *testing.T) {
	fake := NewFake()
	if err := fake.WriteFile("/file", []byte("aaaa"), 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := fake.Stat("/file")
	if err != nil {
		t.Fatal(err)
	}
	type timestamp struct{ Sec, Nsec int64 }
	for _, shape := range []string{"Ctim", "Ctimespec"} {
		t.Run(shape, func(t *testing.T) {
			metadata := func(nsec int64) any {
				if shape == "Ctim" {
					return struct {
						Dev, Ino uint64
						Ctim     timestamp
					}{1, 2, timestamp{3, nsec}}
				}
				return struct {
					Dev, Ino  uint64
					Ctimespec timestamp
				}{1, 2, timestamp{3, nsec}}
			}
			before := contentInfo{info, metadata(4)}
			if !SameFileMetadata(before, contentInfo{info, metadata(4)}) {
				t.Fatal("identical Unix metadata rejected")
			}
			if SameFileMetadata(before, contentInfo{info, metadata(5)}) {
				t.Fatal("changed Unix ctime missed")
			}
		})
	}
}

func TestSameFileMetadata(t *testing.T) {
	fake := NewFake()
	path := "/file"
	if err := fake.WriteFile(path, []byte("aaaa"), 0o600); err != nil {
		t.Fatal(err)
	}
	before, err := fake.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	unchanged, err := fake.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !SameFileMetadata(before, unchanged) {
		t.Fatal("unchanged content not recognized")
	}
	mtime := fake.ModTimes[path]
	if err := fake.WriteFile(path, []byte("bbbb"), 0o600); err != nil {
		t.Fatal(err)
	}
	fake.ModTimes[path] = mtime
	after, err := fake.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if SameFileMetadata(before, after) {
		t.Fatal("inplace write with preserved mtime missed")
	}
	for _, sys := range []any{nil, "unknown", (*int)(nil), struct{}{}, struct{ Dev, Ino uint64 }{1, 2}} {
		info := contentInfo{before, sys}
		if SameFileMetadata(info, info) {
			t.Fatalf("unknown change metadata accepted: %T", sys)
		}
		_, hasIdentity := sys.(struct{ Dev, Ino uint64 })
		if SameFileIdentity(info, info) != hasIdentity {
			t.Fatalf("identity availability for %T differs from expected %t", sys, hasIdentity)
		}
	}
	if SameFileMetadata(nil, before) {
		t.Fatal("nil accepted")
	}
}
