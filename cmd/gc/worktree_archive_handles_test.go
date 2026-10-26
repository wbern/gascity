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

func TestArchiveProcessUIDRendering(t *testing.T) {
	for _, tc := range []struct {
		name string
		uid  uint32
		data string
		want []int
	}{
		{"normal", 501, "42 501 S\n43 501 S\n77 0 S\n", []int{42, 43}},
		{"foreign_nobody_forms", 501, "77 -2 S\n78 4294967294 S\n42 501 S\n", []int{42}},
		{"current_nobody_forms", 4294967294, "42 -2 S\n43 4294967294 S\n77 501 S\n", []int{42, 43}},
		{"signed_boundary", 2147483648, "42 -2147483648 S\n43 2147483648 S\n", []int{42, 43}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ids, err := parseArchiveProcessIDs([]byte(tc.data), tc.uid, 42, true)
			if err != nil || len(ids) != len(tc.want) {
				t.Fatalf("valid UID rendering refused or cohort changed: %v %v", ids, err)
			}
			for _, pid := range tc.want {
				if !ids[pid] {
					t.Fatalf("current-user PID %d omitted: %v", pid, ids)
				}
			}
		})
	}
}

func TestArchiveProcessUIDMalformedInventoryRefused(t *testing.T) {
	for _, row := range []string{
		"77 nobody", "77 ?", "77 -2147483649", "77 4294967296",
		"77 9223372036854775808", "-1 501", "unknown 501", "77 501 extra",
	} {
		t.Run(row, func(t *testing.T) {
			if ids, err := parseArchiveProcessIDs([]byte("42 501 S\n"+row+" S\n"), 501, 42, true); err == nil {
				t.Fatalf("malformed foreign/current row was silently accepted: %v", ids)
			}
		})
	}
	if _, err := parseArchiveProcessIDs([]byte("77 -2 S\n"), 501, 42, true); err == nil {
		t.Fatal("inventory omitting current process accepted")
	}
}

func TestArchiveZombieProcessCoverage(t *testing.T) {
	parse := func(rows string) map[int]bool {
		t.Helper()
		ids, err := parseArchiveProcessIDs([]byte("42 501 S\n"+rows), 501, 42, true)
		if err != nil {
			t.Fatal(err)
		}
		return ids
	}
	zombie := parse("77 501 Z\n")
	if live, exists := zombie[77]; !exists || live {
		t.Fatalf("zombie identity must remain present but not descriptor-bearing: %v", zombie)
	}
	if err := archiveProcessCoverage([]byte("p42\x00"), zombie, zombie); err != nil {
		t.Fatalf("validated zombie incorrectly requires descriptors: %v", err)
	}
	for _, state := range []string{"R", "S", "T", "U", "I", "H", "D", "t", "X", "W", "P", "Ss", "SN+", "R<"} {
		t.Run(state, func(t *testing.T) {
			live := parse("77 501 " + state + "\n")
			if err := archiveProcessCoverage([]byte("p42\x00"), live, live); err == nil {
				t.Fatal("omitted non-zombie process admitted")
			}
			if err := archiveProcessCoverage([]byte("p42\x00"), zombie, live); err == nil {
				t.Fatal("stale zombie observation exempted a later live process")
			}
		})
	}
	for _, rows := range []string{"77 501 S\n77 501 Z\n", "77 501 Z\n77 501 S\n"} {
		duplicate := parse(rows)
		if err := archiveProcessCoverage([]byte("p42\x00"), duplicate, duplicate); err == nil {
			t.Fatal("duplicate zombie record erased live coverage requirement")
		}
	}
	live := parse("77 501 S\n")
	if err := archiveProcessCoverage([]byte("p42\x00"), live, zombie); err != nil {
		t.Fatalf("process that became a validated zombie still requires descriptors: %v", err)
	}
	if _, err := parseArchiveProcessIDs([]byte("42 501 Z\n"), 501, 42, true); err == nil {
		t.Fatal("current process accepted as zombie")
	}
}

func TestArchiveProcessStateMalformedInventoryRefused(t *testing.T) {
	for _, row := range []string{"77 501", "77 501 ?", "77 501 Q", "77 501 <defunct>", "77 501 Z?", "77 501 Zunknown", "77 501 Z extra"} {
		t.Run(row, func(t *testing.T) {
			if _, err := parseArchiveProcessIDs([]byte("42 501 S\n"+row+"\n"), 501, 42, true); err == nil {
				t.Fatal("unknown or malformed process state accepted")
			}
		})
	}
}

func TestArchiveZombieExemptionRequiresPlatformProof(t *testing.T) {
	// Linux can expose a zombie thread-group leader with live sibling threads.
	// Without Darwin's process-wide SZOMB guarantee, retain FD coverage.
	ids, err := parseArchiveProcessIDs([]byte("42 501 S\n77 501 Z\n"), 501, 42, false)
	if err != nil || !ids[77] {
		t.Fatalf("unproven zombie exemption applied: %v %v", ids, err)
	}
	if err := archiveProcessCoverage([]byte("p42\x00"), ids, ids); err == nil {
		t.Fatal("unproven zombie process omitted without protection")
	}
}
