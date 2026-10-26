package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

type archiveHandleIdentity struct {
	device uint32
	inode  uint64
}
type archiveHandleEntry struct {
	identity archiveHandleIdentity
	mode     uint16
	// Directories' change timestamps fence membership changes during the census.
	// File bytes are checked separately by the tracked/artifact safety guards.
	changeSec, changeNsec int64
}
type archiveHandleTree map[string]archiveHandleEntry

func archivePlatformHandlesClear(ctx context.Context, root string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	// This permission-independent proof is limited to Darwin's validated kernel
	// dev_t/ino_t representation. Other platforms retain pathname inspection.
	anchor, err := os.OpenFile(root, os.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return fmt.Errorf("candidate identity anchor unavailable: %w", err)
	}
	defer anchor.Close() //nolint:errcheck // Read-only anchor; no buffered writes.
	deadline, ok := ctx.Deadline()
	if !ok {
		return errors.New("descriptor census has no deadline")
	}
	before, err := captureArchiveHandleTree(anchor, root, 250000, deadline)
	if err != nil {
		return err
	}
	data, err := archiveHandleSnapshot(ctx)
	if err != nil {
		return err
	}
	after, err := captureArchiveHandleTree(anchor, root, 250000, deadline)
	if err != nil {
		return err
	}
	if !maps.Equal(before, after) {
		return errors.New("candidate identity inventory changed during descriptor census")
	}
	identities := make(map[archiveHandleIdentity]bool, len(after))
	for _, entry := range after {
		if entry.mode&unix.S_IFMT == unix.S_IFREG || entry.mode&unix.S_IFMT == unix.S_IFDIR {
			identities[entry.identity] = true
		}
	}
	match := func(device, inode string) (bool, bool, error) {
		if err := ctx.Err(); err != nil {
			return false, false, err
		}
		id, err := parseArchiveDarwinIdentity(device, inode)
		if err != nil {
			return false, false, err
		}
		return identities[id], id == after["."].identity, nil
	}
	if err := inspectArchiveHandlesWithIdentity(data, root, os.Getpid(), match); err != nil {
		return err
	}
	return ctx.Err()
}

func parseArchiveDarwinIdentity(device, inode string) (archiveHandleIdentity, error) {
	if !strings.HasPrefix(device, "0x") || len(device) <= 2 || inode == "" {
		return archiveHandleIdentity{}, errors.New("descriptor device/inode unavailable")
	}
	dev, e1 := strconv.ParseUint(device[2:], 16, 32)
	ino, e2 := strconv.ParseUint(inode, 10, 64)
	if e1 != nil || e2 != nil || dev == 0 || ino == 0 {
		return archiveHandleIdentity{}, errors.New("invalid descriptor device/inode")
	}
	return archiveHandleIdentity{uint32(dev), ino}, nil
}

func archiveDarwinHandleEntry(st *unix.Stat_t) archiveHandleEntry {
	entry := archiveHandleEntry{identity: archiveHandleIdentity{uint32(st.Dev), st.Ino}, mode: st.Mode}
	if st.Mode&unix.S_IFMT == unix.S_IFDIR {
		entry.changeSec = st.Ctim.Sec
		entry.changeNsec = st.Ctim.Nsec
	}
	return entry
}

func captureArchiveHandleTree(anchor *os.File, root string, limit int, deadline time.Time) (archiveHandleTree, error) {
	physical, err := normalizeReapBorrowPath(root)
	if err != nil || physical != root {
		return nil, errors.New("candidate identity namespace is not canonical")
	}
	var held, namespace unix.Stat_t
	if err := unix.Fstat(int(anchor.Fd()), &held); err != nil {
		return nil, err
	}
	if err := unix.Lstat(root, &namespace); err != nil {
		return nil, err
	}
	var filesystem unix.Statfs_t
	if err := unix.Fstatfs(int(anchor.Fd()), &filesystem); err != nil {
		return nil, err
	}
	if filesystem.Flags&unix.MNT_LOCAL == 0 {
		return nil, errors.New("candidate identity inventory requires a local filesystem")
	}
	expected := archiveDarwinHandleEntry(&held)
	if expected.mode&unix.S_IFMT != unix.S_IFDIR || expected != archiveDarwinHandleEntry(&namespace) {
		return nil, errors.New("candidate identity anchor changed")
	}
	// Open a fresh directory stream through the pinned descriptor. dup would
	// share its directory offset, making the second census appear empty.
	fd, err := unix.Openat(int(anchor.Fd()), ".", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	dir := os.NewFile(uintptr(fd), root)
	defer dir.Close() //nolint:errcheck // Read-only directory.
	entries := make(archiveHandleTree)
	pathBytes := 0
	if err := walkArchiveHandleTree(dir, ".", expected.identity.device, entries, limit, deadline, 0, &pathBytes); err != nil {
		return nil, err
	}
	if err := unix.Fstat(int(anchor.Fd()), &held); err != nil {
		return nil, err
	}
	if err := unix.Lstat(root, &namespace); err != nil {
		return nil, err
	}
	if expected != archiveDarwinHandleEntry(&held) || expected != archiveDarwinHandleEntry(&namespace) {
		return nil, errors.New("candidate identity anchor changed during walk")
	}
	physical, err = normalizeReapBorrowPath(root)
	if err != nil || physical != root {
		return nil, errors.New("candidate identity namespace changed")
	}
	return entries, nil
}

func walkArchiveHandleTree(dir *os.File, path string, device uint32, entries archiveHandleTree, limit int, deadline time.Time, depth int, pathBytes *int) error {
	if depth > 128 || len(entries) >= limit || time.Now().After(deadline) {
		return errors.New("candidate identity inventory exceeds budget")
	}
	var before unix.Stat_t
	if err := unix.Fstat(int(dir.Fd()), &before); err != nil {
		return err
	}
	entry := archiveDarwinHandleEntry(&before)
	if entry.mode&unix.S_IFMT != unix.S_IFDIR || entry.identity.device != device {
		return errors.New("candidate identity inventory crosses mount or changed directory")
	}
	entries[path] = entry
	for {
		children, err := dir.ReadDir(128)
		if err != nil && err != io.EOF {
			return fmt.Errorf("candidate identity directory unreadable: %w", err)
		}
		for _, child := range children {
			if len(entries) >= limit || time.Now().After(deadline) {
				return errors.New("candidate identity inventory exceeds budget")
			}
			name := child.Name()
			if name == "." || name == ".." || strings.ContainsAny(name, "/\x00") {
				return errors.New("invalid candidate directory entry")
			}
			relative := filepath.Join(path, name)
			*pathBytes += len(relative)
			if *pathBytes > 32<<20 {
				return errors.New("candidate identity inventory path budget exceeded")
			}
			if _, duplicate := entries[relative]; duplicate {
				return errors.New("duplicate candidate directory entry")
			}
			var st unix.Stat_t
			if err := unix.Fstatat(int(dir.Fd()), name, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
				return err
			}
			current := archiveDarwinHandleEntry(&st)
			if current.identity.device != device || current.identity.inode == 0 {
				return errors.New("candidate identity inventory crosses mount or lacks identity")
			}
			switch current.mode & unix.S_IFMT {
			case unix.S_IFDIR:
				childFD, err := unix.Openat(int(dir.Fd()), name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
				if err != nil {
					return err
				}
				opened := os.NewFile(uintptr(childFD), relative)
				var actual unix.Stat_t
				statErr := unix.Fstat(childFD, &actual)
				if statErr != nil || current != archiveDarwinHandleEntry(&actual) {
					_ = opened.Close()
					return errors.New("candidate directory changed before traversal")
				}
				walkErr := walkArchiveHandleTree(opened, relative, device, entries, limit, deadline, depth+1, pathBytes)
				closeErr := opened.Close()
				if walkErr != nil {
					return walkErr
				}
				if closeErr != nil {
					return closeErr
				}
			case unix.S_IFREG, unix.S_IFLNK:
				entries[relative] = current
			default:
				return errors.New("candidate identity inventory contains unsupported file type")
			}
		}
		if err == io.EOF {
			break
		}
	}
	var after unix.Stat_t
	if err := unix.Fstat(int(dir.Fd()), &after); err != nil {
		return err
	}
	if entry != archiveDarwinHandleEntry(&after) {
		return errors.New("candidate directory changed during traversal")
	}
	return nil
}
