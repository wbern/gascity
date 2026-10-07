package beads_test

import (
	"bytes"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/fsys"
)

// The fake advances ctime deterministically, independently of OS clock resolution.
func TestFileStorePreservedMtimeChangedCtime(t *testing.T) {
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
	reader, err := beads.OpenFileStore(base, path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reader.Get(bead.ID); err != nil {
		t.Fatal(err)
	}
	before, err := base.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	ctime := base.CTimes[path]
	data := bytes.Replace(base.Files[path], []byte(`"aaaa"`), []byte(`"bbbb"`), 1)
	if bytes.Equal(data, base.Files[path]) {
		t.Fatal("invalid mutation")
	}
	if err := base.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	base.ModTimes[path] = before.ModTime()
	after, err := base.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !fsys.SameFileIdentity(before, after) || before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) || !base.CTimes[path].After(ctime) {
		t.Fatal("fixture must change only content and ctime")
	}
	got, err := reader.Get(bead.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Title != "bbbb" {
		t.Fatalf("retained reader title = %q, want bbbb", got.Title)
	}
}
