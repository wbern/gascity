package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// archiveHandleSnapshot is stricter than the ordinary cwd-only scan: every
// current-user regular-file and directory descriptor must be inspectable.
var archiveHandleSnapshot = collectArchiveHandleSnapshot

var errArchiveHeldHandle = errors.New("checkout has an open file or directory descriptor")

type archiveBoundedOutput struct {
	bytes.Buffer
	limit int
}

func (b *archiveBoundedOutput) Write(p []byte) (int, error) {
	if len(p) > b.limit-b.Len() {
		return 0, fmt.Errorf("descriptor inventory exceeds byte budget")
	}
	return b.Buffer.Write(p)
}

func collectArchiveHandleSnapshot() ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	before, err := archiveProcessIDs(ctx)
	if err != nil {
		return nil, err
	}
	data, err := archiveInventoryOutput(ctx, 8<<20, "lsof", "-n", "-P", "-a", "-u", strconv.Itoa(os.Geteuid()), "-F0pftan")
	if err != nil {
		return nil, err
	}
	after, err := archiveProcessIDs(ctx)
	if err != nil {
		return nil, err
	}
	if err := archiveProcessCoverage(data, before, after); err != nil {
		return nil, err
	}
	return data, nil
}

func archiveInventoryOutput(ctx context.Context, limit int, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.WaitDelay = 100 * time.Millisecond
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if err == syscall.ESRCH {
			return nil
		}
		return err
	}
	out := &archiveBoundedOutput{limit: limit}
	diagnostics := &archiveBoundedOutput{limit: 64 << 10}
	cmd.Stdout = out
	cmd.Stderr = diagnostics
	if err := cmd.Run(); err != nil || ctx.Err() != nil || diagnostics.Len() != 0 {
		return nil, fmt.Errorf("%s process inventory unavailable or incomplete", name)
	}
	return out.Bytes(), nil
}

func archiveProcessIDs(ctx context.Context) (map[int]bool, error) {
	data, err := archiveInventoryOutput(ctx, 1<<20, "ps", "-axo", "pid=,uid=")
	if err != nil {
		return nil, err
	}
	return parseArchiveProcessIDs(data, uint32(os.Geteuid()), os.Getpid())
}

func parseArchiveProcessIDs(data []byte, ownUID uint32, ownPID int) (map[int]bool, error) {
	ids := make(map[int]bool)
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		if len(fields) != 2 {
			return nil, fmt.Errorf("invalid process ownership inventory")
		}
		pid, e1 := strconv.Atoi(fields[0])
		uid, e2 := strconv.ParseInt(fields[1], 10, 64)
		// BSD ps can render unsigned uid_t through signed int32 (nobody is
		// -2). Bound both representations before conversion so wider malformed
		// values cannot wrap into the current user's ownership cohort.
		if e1 != nil || e2 != nil || pid < 0 || uid < -(1<<31) || uid > (1<<32)-1 {
			return nil, fmt.Errorf("invalid process ownership record")
		}
		if uint32(uid) == ownUID {
			ids[pid] = true
		}
	}
	if !ids[ownPID] {
		return nil, fmt.Errorf("process ownership inventory omitted current process")
	}
	return ids, nil
}

// Darwin lsof may silently omit a process when its initial information query
// is denied. Require coverage of every current-user PID surviving both sides
// of enumeration; processes that exited have released their descriptors.
func archiveProcessCoverage(data []byte, before, after map[int]bool) error {
	seen := make(map[int]bool)
	for _, raw := range bytes.Split(data, []byte{0}) {
		field := strings.TrimLeft(string(raw), "\r\n")
		if !strings.HasPrefix(field, "p") {
			continue
		}
		pid, err := strconv.Atoi(field[1:])
		if err != nil || pid <= 0 {
			return fmt.Errorf("invalid process descriptor record")
		}
		seen[pid] = true
	}
	for pid := range before {
		if after[pid] && !seen[pid] {
			return fmt.Errorf("descriptor inventory omitted live current-user process %d", pid)
		}
	}
	return nil
}

func archiveHandlesClear(root string) error {
	data, err := archiveHandleSnapshot()
	if err != nil {
		return err
	}
	return inspectArchiveHandles(data, root, os.Getpid())
}

func inspectArchiveHandles(data []byte, root string, ownPID int) error {
	if len(data) == 0 || len(data) > 8<<20 {
		return fmt.Errorf("empty or oversized descriptor inventory")
	}
	pid := 0
	fd, kind, access, name := "", "", "", ""
	processes, files := 0, 0
	seenOwn := false
	flush := func() error {
		if fd == "" {
			return nil
		}
		files++
		if kind == "" {
			return fmt.Errorf("descriptor type unavailable (pid %d, fd %s)", pid, fd)
		}
		switch kind {
		case "REG", "DIR":
		case "CHR", "BLK", "FIFO", "PIPE", "IPv4", "IPv6", "unix", "sock", "KQUEUE", "PSXSEM", "PSXSHM", "NPOLICY", "NEXUS", "CHAN", "systm", "a_inode":
			return nil
		default:
			return fmt.Errorf("unrecognized descriptor type %q (pid %d, fd %s)", kind, pid, fd)
		}
		if name == "" || !strings.HasPrefix(name, "/") || lsofErrAnnotation.MatchString(name) {
			return fmt.Errorf("file descriptor path unavailable")
		}
		name = strings.TrimSuffix(name, " (deleted)")
		physical, err := normalizeReapBorrowPath(name)
		if err != nil {
			return fmt.Errorf("file descriptor path unresolved")
		}
		if !pathAtOrUnder(root, physical) {
			return nil
		}
		// The archive plan's held read-only root descriptor deliberately pins the
		// source inode. No other descriptor owned by this process is exempt.
		if pid == ownPID && kind == "DIR" && access == "r" && physical == root && fd != "cwd" {
			return nil
		}
		return fmt.Errorf("%w (pid %d)", errArchiveHeldHandle, pid)
	}
	for _, raw := range bytes.Split(data, []byte{0}) {
		field := strings.TrimLeft(string(raw), "\r\n")
		if field == "" {
			continue
		}
		switch field[0] {
		case 'p':
			if err := flush(); err != nil {
				return err
			}
			fd, kind, access, name = "", "", "", ""
			value, err := strconv.Atoi(field[1:])
			if err != nil || value <= 0 {
				return fmt.Errorf("invalid process descriptor record")
			}
			pid = value
			processes++
			seenOwn = seenOwn || pid == ownPID
		case 'f':
			if pid == 0 {
				return fmt.Errorf("descriptor without process")
			}
			if err := flush(); err != nil {
				return err
			}
			fd = field[1:]
			kind, access, name = "", "", ""
		case 't':
			kind = field[1:]
		case 'a':
			access = field[1:]
		case 'n':
			name = field[1:]
		default:
			return fmt.Errorf("unexpected descriptor inventory field")
		}
	}
	if err := flush(); err != nil {
		return err
	}
	if processes == 0 || files == 0 || !seenOwn {
		return fmt.Errorf("incomplete current-user descriptor inventory")
	}
	return nil
}

func archiveHandleFailure(report worktreeArchiveReport, err error) worktreeArchiveReport {
	if errors.Is(err, errArchiveHeldHandle) {
		return archiveReportStop(report, "protected", "open_handles", err.Error())
	}
	report.Complete = false
	return archiveReportStop(report, "protected", "handle_inventory_incomplete", err.Error())
}
