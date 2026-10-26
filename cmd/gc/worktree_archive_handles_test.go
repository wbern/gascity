package main

import (
	"fmt"
	"path/filepath"
	"testing"
)

func TestArchiveHandleInventoryFailsClosed(t *testing.T) {
	root, err := normalizeReapBorrowPath(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	own := 42
	base := fmt.Sprintf("p%d\x00\nf3\x00ar\x00tDIR\x00n%s\x00\n", own, root)
	for _, tc := range []struct {
		name, extra string
		wantClear   bool
	}{
		{"own_anchor", "", true},
		{"other_file", fmt.Sprintf("p43\x00\nf4\x00au\x00tREG\x00n%s\x00\n", filepath.Join(root, "config.json")), false},
		{"other_directory", fmt.Sprintf("p43\x00\nf4\x00ar\x00tDIR\x00n%s\x00\n", root), false},
		{"own_file", fmt.Sprintf("f5\x00ar\x00tREG\x00n%s\x00\n", filepath.Join(root, "notes.md")), false},
		{"unknown_path", "f5\x00ar\x00tREG\x00nunknown\x00\n", false},
		{"sibling", fmt.Sprintf("p43\x00\nf4\x00au\x00tREG\x00n%s\x00\n", root+"-sibling"), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := inspectArchiveHandles([]byte(base+tc.extra), root, own)
			if (err == nil) != tc.wantClear {
				t.Fatalf("error=%v", err)
			}
		})
	}
	if err := inspectArchiveHandles([]byte("p43\x00\nf4\x00tDIR\x00n/\x00"), root, own); err == nil {
		t.Fatal("inventory omitting own process accepted")
	}
}

func TestArchiveDescriptorCoverageRejectsSilentlyOmittedLiveProcess(t *testing.T) {
	before := map[int]bool{1: true, 2: true, 3: true}
	after := map[int]bool{1: true, 2: true, 4: true}
	if err := archiveProcessCoverage([]byte("p1\x00p3\x00"), before, after); err == nil {
		t.Fatal("live process omitted without refusal")
	}
	if err := archiveProcessCoverage([]byte("p1\x00p2\x00"), before, after); err != nil {
		t.Fatalf("exited/new process misclassified: %v", err)
	}
}

func TestArchiveDescriptorUnknownAndRevokedLabelsAreNotProof(t *testing.T) {
	root, err := normalizeReapBorrowPath(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, record := range []string{"f10\x00a \x00n(revoked)\x00", "f10\x00ar\x00tunknown\x00n/\x00"} {
		if err := inspectArchiveHandles([]byte("p42\x00"+record), root, 42); err == nil {
			t.Fatal("unknown descriptor admitted")
		}
	}
}

func TestArchiveHandleFailureCompleteness(t *testing.T) {
	report := worktreeArchiveReport{Complete: true, ArchivePath: "retained-archive"}
	held := archiveHandleFailure(report, fmt.Errorf("%w (pid 43)", errArchiveHeldHandle))
	if !held.Complete || held.ReasonCode != "open_handles" || held.Outcome != "protected" {
		t.Fatalf("held=%+v", held)
	}
	unknown := archiveHandleFailure(report, fmt.Errorf("inventory omitted a process"))
	if unknown.Complete || unknown.ReasonCode != "handle_inventory_incomplete" || unknown.ArchivePath != "retained-archive" {
		t.Fatalf("unknown=%+v", unknown)
	}
}
