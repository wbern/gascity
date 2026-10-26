package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestArchiveDarwinHandleIdentityMembership(t *testing.T) {
	for _, scenario := range []string{"unrelated_protected_path", "held_inode_protected_path", "outside_hardlink", "held_directory", "symlink_alias", "inside_path_wrong_identity", "own_root_anchor", "same_inode_other_device", "directory_ctime_change", "transient_namespace", "replaced_leaf", "missing_identity", "conflicting_identity", "changed_namespace", "replaced_root", "outside_symlink_not_followed"} {
		t.Run(scenario, func(t *testing.T) {
			parent, err := normalizeReapBorrowPath(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			root := filepath.Join(parent, "checkout")
			if err := os.Mkdir(root, 0o700); err != nil {
				t.Fatal(err)
			}
			inside := filepath.Join(root, "config.json")
			if err := os.WriteFile(inside, []byte("saved"), 0o600); err != nil {
				t.Fatal(err)
			}
			outside := filepath.Join(parent, "external")
			if err := os.WriteFile(outside, []byte("external"), 0o600); err != nil {
				t.Fatal(err)
			}
			// ELOOP deterministically models an unresolvable pathname without depending
			// on host TCC policy or the test runner's privilege.
			unresolved := filepath.Join(parent, "loop")
			if err := os.Symlink("loop", unresolved); err != nil {
				t.Fatal(err)
			}
			old := archiveHandleSnapshot
			t.Cleanup(func() { archiveHandleSnapshot = old })
			wantClear := scenario == "unrelated_protected_path" || scenario == "outside_symlink_not_followed" || scenario == "own_root_anchor" || scenario == "same_inode_other_device"
			archiveHandleSnapshot = func(_ context.Context) ([]byte, error) {
				base := archiveTestHandleRecord(t, os.Getpid(), "cwd", "r", "DIR", parent, parent)
				path, name := outside, unresolved
				switch scenario {
				case "held_inode_protected_path":
					path = inside
				case "inside_path_wrong_identity":
					path = outside
					name = inside
				case "symlink_alias":
					name = filepath.Join(parent, "alias")
					if err := os.Symlink(inside, name); err != nil {
						t.Fatal(err)
					}
					path = inside
				case "transient_namespace":
					transient := filepath.Join(root, "transient")
					if err := os.WriteFile(transient, []byte("temporary"), 0o600); err != nil {
						t.Fatal(err)
					}
					if err := os.Remove(transient); err != nil {
						t.Fatal(err)
					}
				case "directory_ctime_change":
					if err := os.Chmod(root, 0o755); err != nil {
						t.Fatal(err)
					}
					if err := os.Chmod(root, 0o700); err != nil {
						t.Fatal(err)
					}
				case "replaced_leaf":
					if err := os.Rename(inside, filepath.Join(parent, "retained")); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(inside, []byte("saved"), 0o600); err != nil {
						t.Fatal(err)
					}
				case "outside_hardlink":
					name = filepath.Join(parent, "hardlink")
					if err := os.Link(inside, name); err != nil {
						t.Fatal(err)
					}
					path = name
				case "held_directory":
					path = root
					name = unresolved
				case "changed_namespace":
					if err := os.WriteFile(filepath.Join(root, "late.txt"), []byte("new"), 0o600); err != nil {
						t.Fatal(err)
					}
				case "replaced_root":
					if err := os.Rename(root, root+"-old"); err != nil {
						t.Fatal(err)
					}
					if err := os.Mkdir(root, 0o700); err != nil {
						t.Fatal(err)
					}
				}
				if scenario == "missing_identity" {
					return []byte(base + fmt.Sprintf("p%d\x00f5\x00tREG\x00n%s\x00", os.Getpid()+1, unresolved)), nil
				}
				record := archiveTestHandleRecord(t, os.Getpid()+1, "5", "r", "REG", path, name)
				if scenario == "held_directory" {
					record = archiveTestHandleRecord(t, os.Getpid()+1, "5", "r", "DIR", path, name)
				}
				if scenario == "own_root_anchor" {
					record = archiveTestHandleRecord(t, os.Getpid(), "7", "r", "DIR", root, root)
				}
				if scenario == "same_inode_other_device" {
					var st unix.Stat_t
					if err := unix.Lstat(inside, &st); err != nil {
						t.Fatal(err)
					}
					record = fmt.Sprintf("p%d\x00f5\x00ar\x00tREG\x00D0x%x\x00i%d\x00n%s\x00", os.Getpid()+1, uint32(st.Dev)^1, st.Ino, unresolved)
				}
				if scenario == "conflicting_identity" {
					record += "i1\x00"
				}
				return []byte(base + record), nil
			}
			if scenario == "outside_symlink_not_followed" {
				if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
					t.Fatal(err)
				}
			}
			err = archiveHandlesClear(root)
			if (err == nil) != wantClear {
				t.Fatalf("clear=%v want=%v: %v", err == nil, wantClear, err)
			}
			if scenario == "held_inode_protected_path" || scenario == "outside_hardlink" || scenario == "held_directory" || scenario == "inside_path_wrong_identity" || scenario == "symlink_alias" {
				if !errors.Is(err, errArchiveHeldHandle) {
					t.Fatalf("known inode hold lost: %v", err)
				}
			}
		})
	}
}

func TestArchiveDarwinIdentityEncoding(t *testing.T) {
	for _, tc := range []struct {
		device, inode string
		valid         bool
	}{
		{"0xffffffff", "18446744073709551615", true},
		{"0x1000011", "2", true},
		{"", "2", false},
		{"0x0", "2", false},
		{"0x100000000", "2", false},
		{"0x1", "0", false},
		{"0x1", "18446744073709551616", false},
		{"0x1", "-1", false},
		{"0x1", "2x", false},
	} {
		if _, err := parseArchiveDarwinIdentity(tc.device, tc.inode); (err == nil) != tc.valid {
			t.Fatalf("%q/%q: %v", tc.device, tc.inode, err)
		}
	}
	stat := unix.Stat_t{Dev: -1, Ino: ^uint64(0), Mode: unix.S_IFREG}
	actual := archiveDarwinHandleEntry(&stat)
	expected, err := parseArchiveDarwinIdentity("0xffffffff", "18446744073709551615")
	if err != nil || actual.identity != expected {
		t.Fatalf("signed device or wide inode mismatched: %+v %+v %v", actual.identity, expected, err)
	}
}

func TestArchiveDarwinTreeRefusesIncompleteCensus(t *testing.T) {
	for _, scenario := range []string{"entry_budget", "deadline", "special", "unreadable", "mount_device", "root_symlink"} {
		t.Run(scenario, func(t *testing.T) {
			root, err := normalizeReapBorrowPath(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(filepath.Join(root, "child"), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(root, "child", "payload"), []byte("bytes"), 0o600); err != nil {
				t.Fatal(err)
			}
			anchor, err := os.Open(root)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := anchor.Close(); err != nil {
					t.Error(err)
				}
			})
			limit, deadline := 100, time.Now().Add(time.Second)
			switch scenario {
			case "entry_budget":
				limit = 1
			case "deadline":
				deadline = time.Now().Add(-time.Second)
			case "special":
				if err := unix.Mkfifo(filepath.Join(root, "fifo"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "unreadable":
				if os.Geteuid() == 0 {
					t.Skip("root bypasses Unix mode denial")
				}
				p := filepath.Join(root, "child")
				if err := os.Chmod(p, 0); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = os.Chmod(p, 0o700) })
			case "mount_device":
				var st unix.Stat_t
				if err := unix.Fstat(int(anchor.Fd()), &st); err != nil {
					t.Fatal(err)
				}
				pathBytes := 0
				if err := walkArchiveHandleTree(anchor, ".", uint32(st.Dev)^1, archiveHandleTree{}, 100, deadline, 0, &pathBytes); err == nil {
					t.Fatal("foreign device traversed")
				}
				return
			case "root_symlink":
				alias := root + "-alias"
				if err := os.Symlink(root, alias); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = os.Remove(alias) })
				root = alias
			}
			if _, err := captureArchiveHandleTree(anchor, root, limit, deadline); err == nil {
				t.Fatal("incomplete census accepted")
			}
		})
	}
}
