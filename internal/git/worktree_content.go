package git

import (
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	trackedContentMaxFiles     = 50000
	trackedContentMaxBytes     = int64(512 << 20)
	trackedContentMaxFileBytes = int64(128 << 20)
)

// VerifyTrackedContent independently checks actual bytes against one committed
// tree. Git's index/stat cache is deliberately not evidence here. Call after
// cheap hold gates, immediately before destructive retirement. This reads no
// filters and changes no index: transformed/CRLF/LFS worktree bytes that differ
// from their committed blob conservatively require review.
func (g *Git) VerifyTrackedContent(expectedHEAD string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	probe := NewInspection(g.workDir)
	head, err := probe.RevParseVerifyCommit("HEAD")
	if err != nil || head != expectedHEAD {
		return fmt.Errorf("tracked-content HEAD expectation changed")
	}
	listing, err := probe.runCtx(ctx, "ls-tree", "-r", "-l", "-z", "--full-tree", expectedHEAD)
	if err != nil {
		return fmt.Errorf("reading committed tracked inventory: %w", err)
	}
	if len(listing) > 16<<20 {
		return fmt.Errorf("tracked inventory exceeds byte budget")
	}
	rootPath, err := probe.TopLevel()
	if err != nil {
		return err
	}
	rootPath, err = filepath.EvalSymlinks(rootPath)
	if err != nil {
		return err
	}
	root, err := os.OpenRoot(rootPath)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	rootInfo, err := root.Stat(".")
	if err != nil {
		return err
	}
	var total int64
	files := 0
	buffer := make([]byte, 64<<10)
	for _, record := range strings.Split(listing, "\x00") {
		if record == "" {
			continue
		}
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("tracked verification budget: %w", err)
		}
		header, name, ok := strings.Cut(record, "\t")
		parts := strings.Fields(header)
		if !ok || len(parts) != 4 || !trackedLocalPath(name) {
			return fmt.Errorf("unsafe committed tracked inventory")
		}
		if parts[0] == "160000" || parts[1] != "blob" {
			return fmt.Errorf("nested tracked repository or unsupported tree entry: %s", name)
		}
		size, err := strconv.ParseInt(parts[3], 10, 64)
		if err != nil || size < 0 || size > trackedContentMaxFileBytes || size > trackedContentMaxBytes-total {
			return fmt.Errorf("tracked content exceeds byte budget: %s", name)
		}
		files++
		if files > trackedContentMaxFiles {
			return fmt.Errorf("tracked content exceeds file budget")
		}
		total += size
		if err := verifyTrackedEntry(ctx, root, name, parts[0], parts[2], size, buffer); err != nil {
			return err
		}
	}
	current, err := os.Lstat(rootPath)
	if err != nil || !os.SameFile(rootInfo, current) {
		return fmt.Errorf("tracked source root changed")
	}
	head, err = probe.RevParseVerifyCommit("HEAD")
	if err != nil || head != expectedHEAD {
		return fmt.Errorf("tracked-content HEAD changed during verification")
	}
	return nil
}

func trackedLocalPath(name string) bool {
	if !filepath.IsLocal(name) || filepath.ToSlash(filepath.Clean(name)) != name {
		return false
	}
	for _, part := range strings.Split(name, "/") {
		if strings.EqualFold(part, ".git") {
			return false
		}
	}
	return true
}

type trackedParent struct {
	path string
	info os.FileInfo
}

func trackedParents(root *os.Root, name string) ([]trackedParent, error) {
	var parents []trackedParent
	for dir := filepath.Dir(name); ; dir = filepath.Dir(dir) {
		info, err := root.Lstat(dir)
		if err != nil {
			return nil, err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("tracked path traverses a symlink or non-directory")
		}
		parents = append(parents, trackedParent{dir, info})
		if dir == "." {
			break
		}
	}
	return parents, nil
}

func sameTrackedFile(a, b os.FileInfo) bool {
	stamp := trackedContentChangeStamp(a)
	return stamp != "" && os.SameFile(a, b) && a.Mode() == b.Mode() && a.Size() == b.Size() && a.ModTime().Equal(b.ModTime()) && stamp == trackedContentChangeStamp(b)
}

func trackedBlobHasher(oid string) (hash.Hash, error) {
	switch len(oid) {
	case 40:
		return sha1.New(), nil
	case 64:
		return sha256.New(), nil
	default:
		return nil, fmt.Errorf("unsupported Git object identity")
	}
}

type trackedDeadlineReader struct {
	ctx context.Context
	r   io.Reader
}

func (r trackedDeadlineReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(p)
}

func verifyTrackedEntry(ctx context.Context, root *os.Root, name, mode, oid string, size int64, buffer []byte) error {
	parents, err := trackedParents(root, name)
	if err != nil {
		return fmt.Errorf("tracked parent changed for %s: %w", name, err)
	}
	before, err := root.Lstat(name)
	if err != nil {
		return fmt.Errorf("tracked file unavailable: %s: %w", name, err)
	}
	digest, err := trackedBlobHasher(oid)
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintf(digest, "blob %d\x00", size)
	switch mode {
	case "100644", "100755":
		if !before.Mode().IsRegular() || before.Size() != size || (before.Mode().Perm()&0o100 != 0) != (mode == "100755") {
			return fmt.Errorf("tracked file type, size or executable mode changed: %s", name)
		}
		file, err := openTrackedRegular(root, name)
		if err != nil {
			return fmt.Errorf("opening tracked file %s: %w", name, err)
		}
		opened, err := file.Stat()
		if err != nil || !sameTrackedFile(before, opened) {
			_ = file.Close()
			return fmt.Errorf("tracked file changed before reading: %s", name)
		}
		read, readErr := io.CopyBuffer(digest, io.LimitReader(trackedDeadlineReader{ctx, file}, size+1), buffer)
		after, statErr := file.Stat()
		closeErr := file.Close()
		if readErr != nil || statErr != nil || closeErr != nil || read != size || !sameTrackedFile(opened, after) {
			return fmt.Errorf("tracked file changed or could not be read: %s", name)
		}
	case "120000":
		if before.Mode()&os.ModeSymlink == 0 {
			return fmt.Errorf("tracked symlink replaced: %s", name)
		}
		target, err := root.Readlink(name)
		if err != nil || int64(len(target)) != size {
			return fmt.Errorf("tracked symlink changed: %s", name)
		}
		_, _ = io.WriteString(digest, target)
	default:
		return fmt.Errorf("unsupported tracked file mode: %s", name)
	}
	after, err := root.Lstat(name)
	if err != nil || !sameTrackedFile(before, after) {
		return fmt.Errorf("tracked file identity or metadata changed: %s", name)
	}
	if hex.EncodeToString(digest.Sum(nil)) != oid {
		return fmt.Errorf("tracked content differs from committed blob: %s", name)
	}
	for _, parent := range parents {
		after, err := root.Lstat(parent.path)
		if err != nil || !after.IsDir() || !os.SameFile(parent.info, after) {
			return fmt.Errorf("tracked parent changed: %s", name)
		}
	}
	return nil
}
