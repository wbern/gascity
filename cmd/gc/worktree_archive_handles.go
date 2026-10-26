package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
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

func collectArchiveHandleSnapshot(ctx context.Context) ([]byte, error) {
	before, err := archiveProcessIDs(ctx)
	if err != nil {
		return nil, err
	}
	data, _, err := archiveInventoryOutput(ctx, 8<<20, "lsof", "-n", "-P", "-a", "-u", strconv.Itoa(os.Geteuid()), "-F0pftanDi")
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

func archiveInventoryOutput(ctx context.Context, limit int, name string, args ...string) ([]byte, int, error) {
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
		return nil, 0, fmt.Errorf("%s process inventory unavailable or incomplete", name)
	}
	// Run includes successful Wait. Only this exact subprocess is proven exited.
	return out.Bytes(), cmd.Process.Pid, nil
}

func archiveProcessIDs(ctx context.Context) (map[int]bool, error) {
	data, waitedPID, err := archiveInventoryOutput(ctx, 1<<20, "ps", "-axo", "pid=,uid=,state=")
	if err != nil {
		return nil, err
	}
	// Darwin's process-wide SZOMB follows descriptor-table teardown. Linux can
	// retain a zombie group leader with live sibling threads, so stays strict.
	ids, err := parseArchiveProcessIDs(data, uint32(os.Geteuid()), os.Getpid(), runtime.GOOS == "darwin")
	if err != nil {
		return nil, err
	}
	// ps reports itself, but its successfully waited child cannot retain handles.
	delete(ids, waitedPID)
	return ids, nil
}

func parseArchiveProcessIDs(data []byte, ownUID uint32, ownPID int, zombieFDsClosed bool) (map[int]bool, error) {
	ids := make(map[int]bool)
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		if len(fields) != 3 {
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
		if !archiveProcessStateValid(fields[2]) {
			return nil, fmt.Errorf("invalid or unknown process state")
		}
		if uint32(uid) == ownUID {
			// Darwin publishes SZOMB only after closing its descriptor table.
			// Keep the key: a later live observation must still require coverage.
			// A duplicate live row wins over any stale zombie observation.
			ids[pid] = ids[pid] || !zombieFDsClosed || fields[2][0] != 'Z'
		}
	}
	if !ids[ownPID] {
		return nil, fmt.Errorf("process ownership inventory omitted current process")
	}
	return ids, nil
}

// state= includes documented BSD/procps modifiers. Unknown output cannot
// establish exit; do not mistake command names such as <defunct> for state.
func archiveProcessStateValid(state string) bool {
	if state == "" || len(state) > 16 || !strings.ContainsRune("DHIPRSTtUWXZ", rune(state[0])) {
		return false
	}
	return strings.Trim(state[1:], "<>AELNSVWXsl+") == ""
}

// Darwin lsof may silently omit a process when its initial information query
// is denied. Require coverage of every live PID in the final census, including
// processes that appeared after the initial census. An earlier zombie record
// never exempts a later live observation.
func archiveProcessCoverage(data []byte, _ map[int]bool, after map[int]bool) error {
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
	for pid, live := range after {
		if live && !seen[pid] {
			return fmt.Errorf("descriptor inventory omitted live current-user process %d", pid)
		}
	}
	return nil
}

func archiveHandlesClear(root string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return archivePlatformHandlesClear(ctx, root)
}

func inspectArchiveHandles(data []byte, root string, ownPID int) error {
	return inspectArchiveHandlesWithIdentity(data, root, ownPID, nil)
}

// identityMatch is supplied only after a complete, stable candidate census.
// A positive match protects aliases even when their pathname is outside root.
func inspectArchiveHandlesWithIdentity(data []byte, root string, ownPID int, identityMatch func(string, string) (bool, bool, error)) error {
	if len(data) == 0 || len(data) > 8<<20 {
		return fmt.Errorf("empty or oversized descriptor inventory")
	}
	pid := 0
	fd, kind, access, name, device, inode := "", "", "", "", "", ""
	deviceSeen, inodeSeen := false, false
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
		matched, rootMatched := false, false
		if identityMatch != nil {
			var err error
			matched, rootMatched, err = identityMatch(device, inode)
			if err != nil {
				return err
			}
		}
		name = strings.TrimSuffix(name, " (deleted)")
		physical, err := normalizeReapBorrowPath(name)
		if err != nil {
			if identityMatch == nil {
				return fmt.Errorf("file descriptor path unresolved")
			}
			if !matched {
				return nil
			}
			return fmt.Errorf("%w (pid %d)", errArchiveHeldHandle, pid)
		}
		if !matched && !pathAtOrUnder(root, physical) {
			return nil
		}
		// The archive plan's held read-only root descriptor deliberately pins the
		// source inode. No other descriptor owned by this process is exempt.
		if pid == ownPID && kind == "DIR" && access == "r" && physical == root && fd != "cwd" && (identityMatch == nil || rootMatched) {
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
			fd, kind, access, name, device, inode = "", "", "", "", "", ""
			deviceSeen, inodeSeen = false, false
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
			kind, access, name, device, inode = "", "", "", "", ""
			deviceSeen, inodeSeen = false, false
		case 't':
			kind = field[1:]
		case 'a':
			access = field[1:]
		case 'n':
			name = field[1:]
		case 'D':
			if deviceSeen {
				return fmt.Errorf("duplicate descriptor device")
			}
			deviceSeen = true
			device = field[1:]
		case 'i':
			if inodeSeen {
				return fmt.Errorf("duplicate descriptor inode")
			}
			inodeSeen = true
			inode = field[1:]
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
