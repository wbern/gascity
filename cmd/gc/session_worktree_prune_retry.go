package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/contract"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/git"
	"github.com/gastownhall/gascity/internal/worktree"
)

const (
	workerDirRetryDirectory = "worker-dir-retirement"
	workerDirRetryLimit     = 1024
	workerDirRetryBatch     = 8
	workerDirRetryInterval  = time.Minute
	workerDirRetryBudget    = 10 * time.Second
)

func (cr *CityRuntime) workerDirRetryDue(now time.Time) bool {
	if !cr.cfg.Daemon.AutoPruneWorkerDirEnabled() || now.Before(cr.workerDirRetryNext) {
		return false
	}
	cr.workerDirRetryNext = now.Add(workerDirRetryInterval)
	return true
}

type workerDirRetirement struct {
	Version       int    `json:"version"`
	SessionID     string `json:"session_id"`
	Rig           string `json:"rig"`
	Path          string `json:"path"`
	Branch        string `json:"branch"`
	CommonDir     string `json:"common_dir"`
	PrivateDir    string `json:"private_dir"`
	PointerDevice uint64 `json:"pointer_device"`
	PointerInode  uint64 `json:"pointer_inode"`
	PointerChange string `json:"pointer_change"`
}

type pendingWorkerDir struct {
	name    string
	data    []byte
	record  workerDirRetirement
	failure error
}

// The record identifies a worktree incarnation and branch, not an old HEAD:
// committing previously dirty work is allowed to make a retired tree safe.
func workerDirRetirementIdentity(city, rigName, rigRoot, path, sessionID string) (workerDirRetirement, error) {
	if sessionID == "" {
		return workerDirRetirement{}, errors.New("retirement identity is incomplete")
	}
	r, err := linkedWorktreeRetirementIdentity(city, rigName, rigRoot, path)
	r.SessionID = sessionID
	if err == nil && r.Branch == "" {
		err = errors.New("retirement branch unavailable")
	}
	return r, err
}

// linkedWorktreeRetirementIdentity binds Git inspection to the exact physical
// linked checkout and its pointer incarnation, independently of owner kind.
// Detached worktrees are valid here; the worker-dir record requires a branch.
func linkedWorktreeRetirementIdentity(city, rigName, rigRoot, path string) (workerDirRetirement, error) {
	r := workerDirRetirement{Version: 1, Rig: rigName}
	if rigName == "" || rigRoot == "" {
		return r, errors.New("retirement identity is incomplete")
	}
	root, err := normalizeReapBorrowPath(city)
	if err != nil {
		return r, err
	}
	r.Path, err = normalizeReapBorrowPath(path)
	if err != nil {
		return r, err
	}
	boundary := filepath.Join(root, ".gc", "worktrees", rigName)
	if r.Path == boundary || !pathAtOrUnder(boundary, r.Path) {
		return r, errors.New("retirement path is outside its rig workspace")
	}
	g := git.NewInspection(r.Path)
	workRoot, err := g.TopLevel()
	if err != nil {
		return r, err
	}
	workRoot, err = normalizeReapBorrowPath(workRoot)
	if err != nil || workRoot != r.Path {
		return r, errors.New("retirement subject is not the exact worktree root")
	}
	r.CommonDir, err = g.CommonDir()
	if err != nil {
		return r, err
	}
	r.CommonDir, err = normalizeReapBorrowPath(r.CommonDir)
	if err != nil {
		return r, err
	}
	common, err := git.NewInspection(rigRoot).CommonDir()
	if err != nil {
		return r, err
	}
	common, err = normalizeReapBorrowPath(common)
	if err != nil || common != r.CommonDir {
		return r, errors.New("retirement belongs to another repository")
	}
	r.PrivateDir, err = g.GitDir()
	if err != nil {
		return r, err
	}
	r.PrivateDir, err = normalizeReapBorrowPath(r.PrivateDir)
	if err != nil || r.PrivateDir == r.CommonDir {
		return r, errors.New("retirement is not a linked worktree")
	}
	_, pointer, err := fsys.ReadRegularFileStableBounded(fsys.OSFS{}, filepath.Join(r.Path, ".git"), 16<<10)
	if err != nil {
		return r, err
	}
	var ok bool
	r.PointerDevice, r.PointerInode, ok = fsys.FileIdentity(pointer)
	if !ok {
		return r, errors.New("worktree pointer identity unavailable")
	}
	r.PointerChange = workerDirChangeStamp(pointer)
	if r.PointerChange == "" {
		return r, errors.New("worktree pointer change time unavailable")
	}
	backlink, _, err := fsys.ReadRegularFileStableBounded(fsys.OSFS{}, filepath.Join(r.PrivateDir, "gitdir"), 16<<10)
	if err != nil {
		return r, err
	}
	target := strings.TrimSpace(string(backlink))
	if !filepath.IsAbs(target) {
		target = filepath.Join(r.PrivateDir, target)
	}
	target, err = normalizeReapBorrowPath(target)
	if err != nil || target != filepath.Join(r.Path, ".git") {
		return r, errors.New("retirement worktree backlink mismatch")
	}
	registered, err := git.NewInspection(rigRoot).WorktreeList()
	if err != nil {
		return r, err
	}
	found := false
	for _, wt := range registered {
		p, e := normalizeReapBorrowPath(wt.Path)
		if e == nil && p == r.Path {
			found = true
			break
		}
	}
	if !found {
		return r, errors.New("retirement worktree is not registered")
	}
	if _, err := os.Lstat(filepath.Join(r.CommonDir, "info", "grafts")); err == nil || !os.IsNotExist(err) {
		return r, errors.New("retirement repository has grafts or unreadable graft state")
	}

	r.Branch, err = g.CurrentBranch()
	if err != nil {
		return r, errors.New("retirement branch unavailable")
	}
	return r, nil
}

func workerDirChangeStamp(info os.FileInfo) string {
	v := reflect.Indirect(reflect.ValueOf(info.Sys()))
	if !v.IsValid() || v.Kind() != reflect.Struct {
		return ""
	}
	for _, name := range []string{"Ctim", "Ctimespec"} {
		field := v.FieldByName(name)
		if field.IsValid() && field.Kind() == reflect.Struct {
			sec, nsec := field.FieldByName("Sec"), field.FieldByName("Nsec")
			if sec.IsValid() && nsec.IsValid() && sec.Kind() == reflect.Int64 && nsec.Kind() == reflect.Int64 {
				return fmt.Sprintf("%d:%d", sec.Int(), nsec.Int())
			}
		}
	}
	return ""
}

// openWorkerDirRetryRoot checks each component before and after opening its
// descriptor. All subsequent IO stays relative to the pinned private root.
func openWorkerDirRetryRoot(city string, create bool) (*os.Root, error) {
	if err := workerDirRetryPlatformSupported(); err != nil {
		return nil, err
	}
	physical, err := normalizeReapBorrowPath(city)
	if err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(physical)
	if err != nil {
		return nil, err
	}
	for i, name := range []string{".gc", "runtime", workerDirRetryDirectory} {
		if create {
			if err := root.Mkdir(name, 0o700); err != nil && !os.IsExist(err) {
				_ = root.Close()
				return nil, err
			} else if err == nil {
				if err := syncWorkerDirRetryRoot(root); err != nil {
					_ = root.Close()
					return nil, err
				}
			}
		}
		before, err := root.Lstat(name)
		if err != nil {
			_ = root.Close()
			return nil, err
		}
		if !before.IsDir() || before.Mode()&os.ModeSymlink != 0 {
			_ = root.Close()
			return nil, errors.New("retirement registry has an aliased parent")
		}
		next, err := root.OpenRoot(name)
		if err != nil {
			_ = root.Close()
			return nil, err
		}
		inside, e1 := next.Stat(".")
		after, e2 := root.Lstat(name)
		_ = root.Close()
		if e1 != nil || e2 != nil || !fsys.SameFileIdentity(before, inside) || !fsys.SameFileIdentity(before, after) || after.Mode()&os.ModeSymlink != 0 {
			_ = next.Close()
			return nil, errors.New("retirement registry identity changed")
		}
		if i == 2 {
			if !workerDirRetryOwned(inside) || inside.Mode().Perm() != 0o700 {
				_ = next.Close()
				return nil, errors.New("retirement registry is not private and owned")
			}
		}
		root = next
	}
	return root, nil
}

// Metadata comparisons also fence ownership, mode and link-count changes;
// content equality is checked separately before admission after recovery.
func sameWorkerDirRetryMetadata(first, second os.FileInfo) bool {
	if !fsys.SameFileMetadata(first, second) || first.Mode() != second.Mode() {
		return false
	}
	return sameWorkerDirRetryOwnerLinks(first, second)
}

func readWorkerDirRetryFile(root *os.Root, name string) ([]byte, error) {
	data, info, err := readWorkerDirRetryMetadata(root, name)
	if err != nil {
		return nil, err
	}
	if workerDirRetryLinkCount(info) != 1 {
		return nil, errors.New("retirement registry file has unresolved aliases")
	}
	return data, nil
}

// This bounded read does not admit a record. Multiple links are examined only
// after strict JSON/hash validation by readCompletedWorkerDirRetryRecord.
func readWorkerDirRetryMetadata(root *os.Root, name string) ([]byte, os.FileInfo, error) {
	f, err := openWorkerDirRetryPlatformFile(root, name, os.O_RDONLY, 0)
	if err != nil {
		return nil, nil, err
	}
	defer f.Close() //nolint:errcheck // Read-only metadata.
	before, err := f.Stat()
	if err != nil {
		return nil, nil, err
	}
	if !workerDirRetryOwned(before) || workerDirRetryLinkCount(before) == 0 || !before.Mode().IsRegular() || before.Mode().Perm() != 0o600 || before.Size() > 16<<10 {
		return nil, nil, errors.New("unsafe retirement registry file")
	}
	data, err := io.ReadAll(io.LimitReader(f, 16<<10+1))
	if err != nil || len(data) > 16<<10 {
		return nil, nil, errors.New("retirement registry read failed or exceeded bound")
	}
	after, err := f.Stat()
	if err != nil || !sameWorkerDirRetryMetadata(before, after) {
		return nil, nil, errors.New("retirement registry file changed")
	}
	return data, after, nil
}

func decodeWorkerDirRetryRecord(name string, data []byte) (workerDirRetirement, error) {
	var record workerDirRetirement
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&record) != nil || record.Version != 1 {
		return record, errors.New("invalid retirement record")
	}
	if decoder.Decode(new(any)) != io.EOF {
		return record, errors.New("trailing retirement record data")
	}
	digest := sha256.Sum256(data)
	if hex.EncodeToString(digest[:])+".json" != name {
		return record, errors.New("retirement record digest mismatch")
	}
	return record, nil
}

func readCompletedWorkerDirRetryRecord(ctx context.Context, root *os.Root, name string) ([]byte, workerDirRetirement, error) {
	data, info, err := readWorkerDirRetryMetadata(root, name)
	if err != nil {
		return nil, workerDirRetirement{}, err
	}
	record, err := decodeWorkerDirRetryRecord(name, data)
	if err != nil {
		return nil, record, err
	}
	if workerDirRetryLinkCount(info) != 1 {
		if err := recoverWorkerDirPendingLinks(ctx, root, name, info); err != nil {
			return nil, record, err
		}
		// Recovery never relaxes admission: the original single-link reader
		// must now accept exactly the complete bytes validated above.
		current, err := readWorkerDirRetryFile(root, name)
		if err != nil || !bytes.Equal(current, data) {
			return nil, record, errors.New("recovered retirement record changed")
		}
	}
	return data, record, nil
}

func workerDirPendingName(name string) bool {
	suffix, ok := strings.CutPrefix(name, ".pending-")
	return ok && len(suffix) >= 26 && len(suffix) <= 64 && strings.Trim(suffix, "ABCDEFGHIJKLMNOPQRSTUVWXYZ234567") == ""
}

// Only a complete, digest-validated published record reaches this function.
// Account for EVERY kernel link before removing any reserved private temp name;
// an outside or unknown alias leaves all names untouched. The held descriptor
// pins the inode across recovery. No checkout path or user marker is mutated.
func recoverWorkerDirPendingLinks(ctx context.Context, root *os.Root, name string, expected os.FileInfo) error {
	f, err := openWorkerDirRetryPlatformFile(root, name, os.O_RDONLY, 0)
	if err != nil {
		return err
	}
	defer f.Close() //nolint:errcheck // Read-only metadata anchor.
	current, err := f.Stat()
	if err != nil || !sameWorkerDirRetryMetadata(expected, current) {
		return errors.New("retirement publication changed before recovery")
	}
	entries, err := workerDirRetryEntries(root)
	if err != nil {
		return err
	}
	var aliases []string
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		if entry.Name() == name {
			continue
		}
		info, err := root.Lstat(entry.Name())
		if err != nil {
			return err
		}
		if strings.HasPrefix(entry.Name(), ".pending-") && !info.Mode().IsRegular() {
			return errors.New("unsafe pending retirement metadata")
		}
		if !fsys.SameFileIdentity(current, info) {
			continue
		}
		if !workerDirPendingName(entry.Name()) || !sameWorkerDirRetryMetadata(current, info) {
			return errors.New("unknown retirement metadata alias")
		}
		aliases = append(aliases, entry.Name())
	}
	observed, err := f.Stat()
	if err != nil || !sameWorkerDirRetryMetadata(current, observed) || len(aliases) == 0 || int64(workerDirRetryLinkCount(observed)) != int64(len(aliases)+1) {
		return errors.New("retirement links are not completely accounted for")
	}
	// All aliases were validated before the first unlink. If a crash interrupts
	// cleanup, the remaining links form the same recoverable state on restart.
	for _, alias := range aliases {
		if err := ctx.Err(); err != nil {
			return err
		}
		held, e1 := f.Stat()
		published, e2 := root.Lstat(name)
		pending, e3 := root.Lstat(alias)
		if e1 != nil || e2 != nil || e3 != nil || !sameWorkerDirRetryMetadata(observed, held) || !sameWorkerDirRetryMetadata(held, published) || !sameWorkerDirRetryMetadata(held, pending) {
			return errors.New("retirement alias changed before recovery")
		}
		links := workerDirRetryLinkCount(held)
		if err := root.Remove(alias); err != nil {
			return err
		}
		observed, err = f.Stat()
		if err != nil || !fsys.SameFileIdentity(held, observed) || held.Mode() != observed.Mode() || held.Size() != observed.Size() || !held.ModTime().Equal(observed.ModTime()) || workerDirRetryLinkCount(observed) != links-1 {
			return errors.New("retirement metadata changed during recovery")
		}
	}
	return syncWorkerDirRetryRoot(root)
}

func writeWorkerDirRetryFile(root *os.Root, name string, data []byte, replace bool) error {
	temp := ".pending-" + rand.Text()
	f, err := openWorkerDirRetryPlatformFile(root, temp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	defer func() { _ = root.Remove(temp) }()
	n, writeErr := f.Write(data)
	if writeErr == nil && n != len(data) {
		writeErr = io.ErrShortWrite
	}
	syncErr := f.Sync()
	closeErr := f.Close()
	if writeErr != nil {
		return writeErr
	}
	if syncErr != nil {
		return syncErr
	}
	if closeErr != nil {
		return closeErr
	}
	if replace {
		if err := root.Rename(temp, name); err != nil {
			return err
		}
		return syncWorkerDirRetryRoot(root)
	}
	if err := root.Link(temp, name); err != nil {
		if os.IsExist(err) {
			current, readErr := readWorkerDirRetryFile(root, name)
			if readErr == nil && bytes.Equal(current, data) {
				return nil
			}
		}
		return err
	}
	return syncWorkerDirRetryRoot(root)
}

func syncWorkerDirRetryRoot(root *os.Root) error {
	f, err := root.Open(".")
	if err != nil {
		return err
	}
	err = f.Sync()
	closeErr := f.Close()
	if err != nil {
		return err
	}
	return closeErr
}

func recordDeferredWorkerDirPrune(city, rigRoot, path, sessionID, template string, stderr io.Writer) {
	rig, _, ok := strings.Cut(template, "/")
	if !ok {
		return
	}
	record, err := workerDirRetirementIdentity(city, rig, rigRoot, path, sessionID)
	if err == nil {
		var root *os.Root
		root, err = openWorkerDirRetryRoot(city, true)
		if err == nil {
			defer root.Close() //nolint:errcheck // Metadata root has no buffered writes.
			var entries []os.DirEntry
			entries, err = workerDirRetryEntries(root)
			if err == nil && len(entries) >= workerDirRetryLimit {
				err = errors.New("retirement registry is full")
			}
			if err == nil {
				data, encodeErr := json.Marshal(record)
				err = encodeErr
				if err == nil {
					sum := sha256.Sum256(data)
					err = writeWorkerDirRetryFile(root, hex.EncodeToString(sum[:])+".json", data, false)
				}
			}
		}
	}
	if err != nil {
		fmt.Fprintf(stderr, "session reconciler: could not defer worker_dir %s: %v\n", path, err) //nolint:errcheck // Diagnostic writes do not change the safety decision.
	}
}

func workerDirRetryEntries(root *os.Root) ([]os.DirEntry, error) {
	f, err := root.Open(".")
	if err != nil {
		return nil, err
	}
	defer f.Close() //nolint:errcheck // Read-only directory.
	entries, err := f.ReadDir(workerDirRetryLimit + 2)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	if len(entries) > workerDirRetryLimit+1 {
		return nil, errors.New("retirement registry exceeds entry budget")
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	return entries, nil
}

// pendingWorkerDirs rotates a bounded batch. The cursor is private scheduler
// state, never evidence authorizing removal, so dirty entries cannot starve
// later entries indefinitely.
func pendingWorkerDirs(root *os.Root) ([]pendingWorkerDir, error) {
	return pendingWorkerDirsCtx(context.Background(), root)
}

func pendingWorkerDirsCtx(ctx context.Context, root *os.Root) ([]pendingWorkerDir, error) {
	entries, err := workerDirRetryEntries(root)
	if err != nil {
		return nil, err
	}
	cursor, err := readWorkerDirRetryFile(root, ".cursor")
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	if len(cursor) != 0 {
		if len(cursor) != 69 || !strings.HasSuffix(string(cursor), ".json") {
			return nil, errors.New("invalid retirement cursor")
		}
		if _, err := hex.DecodeString(string(cursor[:64])); err != nil {
			return nil, errors.New("invalid retirement cursor")
		}
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		if len(name) == 69 && strings.HasSuffix(name, ".json") {
			if _, err := hex.DecodeString(name[:64]); err == nil {
				names = append(names, name)
			}
		}
	}
	if len(names) == 0 {
		return nil, nil
	}
	start := sort.SearchStrings(names, string(cursor))
	if start < len(names) && names[start] == string(cursor) {
		start++
	}
	var out []pendingWorkerDir
	for i := 0; i < len(names) && i < workerDirRetryBatch; i++ {
		name := names[(start+i)%len(names)]
		p := pendingWorkerDir{name: name}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		p.data, p.record, p.failure = readCompletedWorkerDirRetryRecord(ctx, root, name)
		out = append(out, p)
	}
	return out, nil
}

func retiredWorkerDirOwner(ctx context.Context, store beads.Store, id string) (beads.Bead, error) {
	if err := ctx.Err(); err != nil {
		return beads.Bead{}, err
	}
	q := beads.ListQuery{IDs: []string{id}, IncludeClosed: true, TierMode: beads.TierBoth, Live: true}
	var rows []beads.Bead
	var err error
	if contextual, ok := store.(interface {
		ListCtx(context.Context, beads.ListQuery) ([]beads.Bead, error)
	}); ok {
		rows, err = contextual.ListCtx(ctx, q)
	} else {
		rows, err = store.List(q)
	}
	if err != nil {
		return beads.Bead{}, err
	}
	if err := ctx.Err(); err != nil {
		return beads.Bead{}, err
	}
	if len(rows) != 1 || rows[0].ID != id {
		return beads.Bead{}, errors.New("retired session missing or ambiguous")
	}
	return rows[0], nil
}

func retiredWorkerDirOwnerMatches(owner beads.Bead, record workerDirRetirement) bool {
	if owner.Status != "closed" || owner.ID != record.SessionID || owner.Metadata["gc.user_paused"] == "true" {
		return false
	}
	path, err := normalizeReapBorrowPath(contract.WorkerDirFromMetadata(owner.Metadata))
	if err != nil || path != record.Path {
		return false
	}
	rig, _, ok := strings.Cut(owner.Metadata["template"], "/")
	return ok && rig == record.Rig
}

// retryDeferredWorkerDirPrunes is the periodic counterpart of session
// retirement. Only recorded, still-closed retirement attempts enter this path.
func retryDeferredWorkerDirPrunes(city string, cfg *config.City, scope reapStoreScope, sessions beads.Store, liveDirs []string, stderr io.Writer) int {
	return retryDeferredWorkerDirPrunesCtx(context.Background(), city, cfg, scope, sessions, liveDirs, stderr)
}

// The shared context is checked between phases and before mutation. Existing
// store/Git probes retain their own deadlines; this is cooperative cancellation,
// not a promise to interrupt a synchronous filesystem/provider call.
func retryDeferredWorkerDirPrunesCtx(parent context.Context, city string, cfg *config.City, scope reapStoreScope, sessions beads.Store, liveDirs []string, stderr io.Writer) int {
	ctx, cancel := context.WithTimeout(parent, workerDirRetryBudget)
	defer cancel()
	if ctx.Err() != nil {
		return 0
	}
	if stderr == nil {
		stderr = io.Discard
	}
	if cfg == nil || !cfg.Daemon.AutoPruneWorkerDirEnabled() || sessions == nil {
		return 0
	}
	root, err := openWorkerDirRetryRoot(city, false)
	if os.IsNotExist(err) {
		return 0
	}
	if err != nil {
		fmt.Fprintf(stderr, "retired worker-dir retry unavailable: %v\n", err) //nolint:errcheck // Diagnostic writes do not change the safety decision.
		return 0
	}
	defer root.Close() //nolint:errcheck // Metadata root has no buffered writes.
	if pct := cfg.Daemon.AutoReapClosedBeadWorktreesMaxLoadPercent(); pct > 0 {
		load, err := reapLoadAverageFn()
		if err != nil || load > float64(runtime.NumCPU())*float64(pct)/100 {
			return 0
		}
	}
	pending, err := pendingWorkerDirsCtx(ctx, root)
	if err != nil {
		fmt.Fprintf(stderr, "retired worker-dir records unavailable: %v\n", err) //nolint:errcheck // Diagnostic writes do not change the safety decision.
		return 0
	}
	if len(pending) == 0 {
		return 0
	}
	lastAttempted := ""
	defer func() {
		if lastAttempted != "" {
			if err := writeWorkerDirRetryFile(root, ".cursor", []byte(lastAttempted), true); err != nil {
				fmt.Fprintf(stderr, "retired worker-dir cursor unavailable: %v\n", err) //nolint:errcheck // Diagnostic writes do not change the safety decision.
			}
		}
	}()
	removed := 0
	for _, p := range pending {
		if ctx.Err() != nil {
			break
		}
		if removalLimit := cfg.Daemon.AutoReapClosedBeadWorktreesMaxPerPass(); removalLimit > 0 && removed >= removalLimit {
			break
		}
		lastAttempted = p.name
		if p.failure != nil {
			fmt.Fprintf(stderr, "retired worker-dir record %s unavailable: %v\n", p.name, p.failure) //nolint:errcheck // Diagnostic writes do not change the safety decision.
			continue
		}
		record := p.record
		owner, err := retiredWorkerDirOwner(ctx, sessions, record.SessionID)
		if err != nil || !retiredWorkerDirOwnerMatches(owner, record) {
			continue
		}
		rig := rigRootByName(cfg, record.Rig)
		if rig == "" {
			continue
		}
		unlock, err := worktree.TryLockRetirement(rig, record.Path)
		if err != nil {
			continue
		}
		didRemove := func() bool {
			defer unlock()
			if ctx.Err() != nil {
				return false
			}
			current, err := workerDirRetirementIdentity(city, record.Rig, rig, record.Path, record.SessionID)
			if err != nil || current != record || ctx.Err() != nil {
				return false
			}
			gp := git.NewInspection(record.Path)
			if workerDirGitStateSkip(gp, record.Path, stderr) != "" {
				return false
			}
			if err := archivePlatformHandlesClear(ctx, record.Path); err != nil {
				fmt.Fprintf(stderr, "retired worker-dir descriptor evidence protects %s: %v\n", record.Path, err) //nolint:errcheck
				return false
			}
			// Read current borrowers and cwd evidence after the Git/descriptor
			// probes, under the canonical workspace lock. No initial stale
			// pass snapshot authorizes this newly reachable destructive path.
			if ctx.Err() != nil {
				return false
			}
			refs, incomplete := collectReapBorrowScope(city, cfg, scope, stderr)
			if len(incomplete) > 0 || ctx.Err() != nil {
				return false
			}
			borrowers, err := reapBorrowersUnder(refs, record.Path)
			if err != nil || len(borrowers) > 0 {
				return false
			}
			live := collectLiveWorktreeStateFn()
			if !live.scanned || ctx.Err() != nil {
				return false
			}
			if yes, _ := worktreeIsLive(record.Path, live, liveDirs); yes {
				return false
			}
			owner, err = retiredWorkerDirOwner(ctx, sessions, record.SessionID)
			if err != nil || !retiredWorkerDirOwnerMatches(owner, record) {
				return false
			}
			current, err = workerDirRetirementIdentity(city, record.Rig, rig, record.Path, record.SessionID)
			if err != nil || current != record || ctx.Err() != nil {
				return false
			}
			if ctx.Err() != nil || workerDirGitStateSkip(gp, record.Path, stderr) != "" || ctx.Err() != nil {
				return false
			}
			if err := git.NewInspection(rig).WorktreeRemove(record.Path, false); err != nil {
				fmt.Fprintf(stderr, "retired worker-dir removal failed for %s: %v\n", record.Path, err) //nolint:errcheck // Diagnostic writes do not change the safety decision.
				return false
			}
			return true
		}()
		if didRemove {
			removed++
			current, err := readWorkerDirRetryFile(root, p.name)
			if err == nil && bytes.Equal(current, p.data) {
				if err := root.Remove(p.name); err != nil {
					fmt.Fprintf(stderr, "retired worker-dir record removal failed: %v\n", err) //nolint:errcheck // Diagnostic writes do not change the safety decision.
				}
			}
			fmt.Fprintf(stderr, "session reconciler: retried retirement of %s (session %s)\n", record.Path, record.SessionID) //nolint:errcheck
			if pace := cfg.Daemon.AutoReapClosedBeadWorktreesPace(); pace > 0 {
				select {
				case <-ctx.Done():
					return removed
				case <-time.After(pace):
				}
			}
		}
	}
	return removed
}
