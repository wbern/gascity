//go:build aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package fsys

import (
	"io"
	"os"

	"golang.org/x/sys/unix"
)

// ReadRegularFile reads name without following a final symlink.
func (OSFS) ReadRegularFile(name string) ([]byte, error) {
	snapshot, err := (OSFS{}).readRegularFileSnapshot(name)
	if err != nil {
		return nil, err
	}
	return snapshot.data, nil
}

// readRegularFileSnapshot reads name without following a final symlink and
// returns the opened file identity for post-read stability checks.
func (OSFS) readRegularFileSnapshot(name string) (regularFileSnapshot, error) {
	return (OSFS{}).readRegularFileSnapshotLimit(name, -1)
}

func (OSFS) readRegularFileSnapshotLimit(name string, limit int64) (regularFileSnapshot, error) {
	fd, err := unix.Open(name, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return regularFileSnapshot{}, &os.PathError{Op: "open", Path: name, Err: err}
	}
	file := os.NewFile(uintptr(fd), name)
	if file == nil {
		_ = unix.Close(fd)
		return regularFileSnapshot{}, &os.PathError{Op: "open", Path: name, Err: os.ErrInvalid}
	}
	defer func() {
		_ = file.Close()
	}()

	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return regularFileSnapshot{}, &os.PathError{Op: "stat", Path: name, Err: err}
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG {
		return regularFileSnapshot{}, &os.PathError{Op: "open", Path: name, Err: os.ErrInvalid}
	}
	var reader io.Reader = file
	if limit >= 0 {
		if stat.Size > limit {
			return regularFileSnapshot{}, &os.PathError{Op: "read", Path: name, Err: os.ErrInvalid}
		}
		reader = io.LimitReader(file, limit+1)
	}
	data, err := io.ReadAll(reader)
	if err != nil {
		return regularFileSnapshot{}, &os.PathError{Op: "read", Path: name, Err: err}
	}
	if limit >= 0 && int64(len(data)) > limit {
		return regularFileSnapshot{}, &os.PathError{Op: "read", Path: name, Err: os.ErrInvalid}
	}
	return regularFileSnapshot{
		data:  data,
		id:    fileIdentity{dev: uint64(stat.Dev), ino: stat.Ino}, //nolint:unconvert // int32 on darwin, uint64 on linux
		hasID: true,
	}, nil
}
