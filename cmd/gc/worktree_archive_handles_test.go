package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/fsys"
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
	if err := archiveProcessCoverage([]byte("p1\x00p2\x00"), before, after); err == nil {
		t.Fatal("new live process omitted without refusal")
	}
	if err := archiveProcessCoverage([]byte("p1\x00p2\x00p4\x00"), before, after); err != nil {
		t.Fatalf("complete live cohort refused: %v", err)
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

// archiveTestHandleRecord models the device/inode fields of a real lsof row.
func archiveTestHandleRecord(t *testing.T, pid int, fd, access, kind, path, name string) string {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	device, inode, ok := fsys.FileIdentity(info)
	if !ok {
		t.Fatal("file identity unavailable")
	}
	if runtime.GOOS == "darwin" {
		device = uint64(uint32(device))
	}
	return fmt.Sprintf("p%d\x00f%s\x00a%s\x00t%s\x00D0x%x\x00i%d\x00n%s\x00", pid, fd, access, kind, device, inode, name)
}

func TestArchiveInventoryExcludesOnlySuccessfullyWaitedPS(t *testing.T) {
	dir := t.TempDir()
	// A real child publishes its own PID as ps does. An unrelated after-only
	// process must remain required, even though both rows are new to lsof.
	body := fmt.Sprintf("#!/bin/sh\nprintf '%%s %%s S\\n' '%d' '%d' \"$$\" '%d' '1999999999' '%d'\n", os.Getpid(), os.Geteuid(), os.Geteuid(), os.Geteuid())
	path := filepath.Join(dir, "ps")
	if err := os.WriteFile(path, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	ids, err := archiveProcessIDs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 2 || !ids[os.Getpid()] || !ids[1999999999] {
		t.Fatalf("waited child exemption broadened or missing: %v", ids)
	}
	if err := archiveProcessCoverage([]byte(fmt.Sprintf("p%d\x00", os.Getpid())), nil, ids); err == nil {
		t.Fatal("unwaited new process exempted")
	}
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 7\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := archiveProcessIDs(ctx); err == nil {
		t.Fatal("failed child treated as complete census")
	}
}

func TestArchiveInventoryTimeoutIsIncomplete(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	data, pid, err := archiveInventoryOutput(ctx, 1024, "/bin/sh", "-c", "exec /bin/sleep 5")
	if err == nil || pid != 0 || data != nil {
		t.Fatalf("timeout supplied exit proof: pid=%d err=%v", pid, err)
	}
	report := archiveHandleFailure(worktreeArchiveReport{Complete: true}, err)
	if report.Complete || report.ReasonCode != "handle_inventory_incomplete" || report.ArchivePath != "" {
		t.Fatalf("timeout classification: %+v", report)
	}
}
