package worktree

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// ArchiveBounds limits selected artifacts and reserves free space before writing.
type ArchiveBounds struct {
	MaxFiles                                  int
	MaxFileBytes, MaxTotalBytes, MinFreeBytes int64
}

// ArchiveEntry identifies one immutable, bounded source-file snapshot.
type ArchiveEntry struct {
	Path   string
	SHA256 string
	Size   int64
	Mode   os.FileMode
}

// ArchiveResult identifies a fully verified private archive and its manifest.
type ArchiveResult struct {
	Path           string
	ManifestSHA256 string
	Files          int
	Bytes          int64
}

// ArchivePlan pins the source directory and selected file identities until Close.
type ArchivePlan struct {
	mu         sync.Mutex
	root       *os.Root
	path       string
	identity   os.FileInfo
	entries    []ArchiveEntry
	identities []os.FileInfo
	bounds     ArchiveBounds
	digest     string
	total      int64
}

// Entries returns a copy of the selected artifact inventory.
func (p *ArchivePlan) Entries() []ArchiveEntry { return append([]ArchiveEntry(nil), p.entries...) }

// Digest binds the selected paths, content hashes, sizes and modes.
func (p *ArchivePlan) Digest() string { return p.digest }

// TotalBytes returns the total selected payload size.
func (p *ArchivePlan) TotalBytes() int64 { return p.total }

// Close releases the pinned source directory descriptor.
func (p *ArchivePlan) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.root == nil {
		return nil
	}
	err := p.root.Close()
	p.root = nil
	return err
}

// archiveClose releases read-only handles; payload writers check Close directly.
func archiveClose(c io.Closer) { _ = c.Close() }

func archiveHash(b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }
func archiveLeaf(path string) bool {
	if len(path) > 1024 || len(strings.Split(path, "/")) > 32 || path == "" || strings.ContainsAny(path, "\\\x00") || filepath.IsAbs(path) {
		return false
	}
	for _, c := range strings.Split(path, "/") {
		if c == "" || c == "." || c == ".." || strings.EqualFold(c, ".git") {
			return false
		}
	}
	return true
}

// archiveOpenRoot refuses symlink components, including namespace replacement
// between the component observation and opening its directory descriptor.
func archiveOpenRoot(path string) (*os.Root, error) {
	if !filepath.IsAbs(path) {
		return nil, errors.New("archive root must be absolute")
	}
	r, err := os.OpenRoot(string(filepath.Separator))
	if err != nil {
		return nil, err
	}
	if path == string(filepath.Separator) {
		return r, nil
	}
	for _, part := range strings.Split(strings.TrimPrefix(path, string(filepath.Separator)), string(filepath.Separator)) {
		if part == "" || part == "." || part == ".." {
			archiveClose(r)
			return nil, errors.New("ambiguous root path")
		}
		before, e := r.Lstat(part)
		if e != nil || !before.IsDir() || before.Mode()&os.ModeSymlink != 0 {
			archiveClose(r)
			return nil, fmt.Errorf("unsafe root component %q", part)
		}
		next, e := r.OpenRoot(part)
		if e != nil {
			archiveClose(r)
			return nil, e
		}
		after, e := next.Stat(".")
		archiveClose(r)
		if e != nil || !os.SameFile(before, after) {
			archiveClose(next)
			return nil, errors.New("root component changed")
		}
		r = next
	}
	return r, nil
}

func archiveRead(root *os.Root, path string, limit int64) ([]byte, os.FileInfo, error) {
	return archiveReadAfterProbe(root, path, limit, nil)
}

func archiveReadAfterProbe(root *os.Root, path string, limit int64, afterProbe func()) ([]byte, os.FileInfo, error) {
	parts := strings.Split(path, "/")
	parent := root
	var opened []*os.Root
	defer func() {
		for _, r := range opened {
			archiveClose(r)
		}
	}()
	for _, part := range parts[:len(parts)-1] {
		before, err := parent.Lstat(part)
		if err != nil {
			return nil, nil, err
		}
		if !before.IsDir() || before.Mode()&os.ModeSymlink != 0 {
			return nil, nil, errors.New("non-directory artifact ancestor")
		}
		next, err := parent.OpenRoot(part)
		if err != nil {
			return nil, nil, err
		}
		opened = append(opened, next)
		after, err := next.Stat(".")
		if err != nil || !os.SameFile(before, after) {
			return nil, nil, errors.New("artifact ancestor changed")
		}
		parent = next
	}
	name := parts[len(parts)-1]
	before, err := parent.Lstat(name)
	if err != nil {
		return nil, nil, err
	}
	if !before.Mode().IsRegular() || before.Size() > limit {
		return nil, nil, errors.New("artifact is not bounded regular file")
	}
	if afterProbe != nil {
		afterProbe()
	}
	f, err := archiveOpenRegular(parent, name)
	if err != nil {
		return nil, nil, err
	}
	defer archiveClose(f)
	openedInfo, err := f.Stat()
	if err != nil || !os.SameFile(before, openedInfo) {
		return nil, nil, errors.New("artifact replaced before read")
	}
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, nil, err
	}
	after, err := f.Stat()
	if err != nil {
		return nil, nil, err
	}
	current, err := parent.Lstat(name)
	if err != nil {
		return nil, nil, err
	}
	if int64(len(data)) > limit || !archiveSame(before, after) || !archiveSame(after, current) {
		return nil, nil, errors.New("artifact changed during read")
	}
	return data, after, nil
}

func archiveSame(a, b os.FileInfo) bool {
	return archiveSameContentMetadata(a, b) && archiveChangeStamp(a) == archiveChangeStamp(b)
}

func archiveSameContentMetadata(a, b os.FileInfo) bool {
	return os.SameFile(a, b) && a.Size() == b.Size() && a.Mode() == b.Mode() && a.ModTime().Equal(b.ModTime())
}

// PrepareArchive captures bounded regular-file snapshots without writing.
func PrepareArchive(root string, exactLeaves []string, bounds ArchiveBounds) (*ArchivePlan, error) {
	if bounds.MaxFiles <= 0 || bounds.MaxFileBytes <= 0 || bounds.MaxFileBytes == int64(^uint64(0)>>1) || bounds.MaxTotalBytes <= 0 || bounds.MinFreeBytes < 0 || len(exactLeaves) > bounds.MaxFiles {
		return nil, errors.New("invalid archive bounds")
	}
	r, err := archiveOpenRoot(root)
	if err != nil {
		return nil, err
	}
	p := &ArchivePlan{root: r, path: root, bounds: bounds}
	ok := false
	defer func() {
		if !ok {
			archiveClose(r)
		}
	}()
	p.identity, err = r.Stat(".")
	if err != nil {
		return nil, err
	}
	leaves := append([]string(nil), exactLeaves...)
	sort.Strings(leaves)
	for i, leaf := range leaves {
		if !archiveLeaf(leaf) || (i > 0 && (leaf == leaves[i-1] || strings.HasPrefix(leaf, leaves[i-1]+"/"))) {
			return nil, errors.New("invalid or duplicate artifact path")
		}
		data, info, e := archiveRead(r, leaf, bounds.MaxFileBytes)
		if e != nil {
			return nil, e
		}
		if int64(len(data)) > bounds.MaxTotalBytes-p.total {
			return nil, errors.New("archive total bound exceeded")
		}
		p.total += int64(len(data))
		p.entries = append(p.entries, ArchiveEntry{leaf, archiveHash(data), int64(len(data)), info.Mode()})
		p.identities = append(p.identities, info)
	}
	encoded, err := json.Marshal(p.entries)
	if err != nil {
		return nil, err
	}
	p.digest = archiveHash(encoded)
	ok = true
	return p, nil
}

func verifyArchiveSource(p *ArchivePlan) error {
	if p.root == nil {
		return errors.New("archive plan closed")
	}
	current, err := archiveOpenRoot(p.path)
	if err != nil {
		return err
	}
	defer archiveClose(current)
	info, err := current.Stat(".")
	if err != nil || !os.SameFile(p.identity, info) {
		return errors.New("source root replaced")
	}
	for i, e := range p.entries {
		data, info, err := archiveRead(p.root, e.Path, p.bounds.MaxFileBytes)
		if err != nil {
			return err
		}
		if !archiveSame(info, p.identities[i]) || archiveHash(data) != e.SHA256 {
			return errors.New("archive source changed")
		}
	}
	return nil
}

// RebindRoot accepts only a new namespace for the already held source inode.
func (p *ArchivePlan) RebindRoot(newRoot string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.root == nil {
		return errors.New("archive plan closed")
	}
	r, err := archiveOpenRoot(newRoot)
	if err != nil {
		return err
	}
	defer archiveClose(r)
	info, err := r.Stat(".")
	if err != nil {
		return err
	}
	if !os.SameFile(p.identity, info) {
		return errors.New("rebind would adopt different source root")
	}
	old := p.path
	p.path = newRoot
	if err := verifyArchiveSource(p); err != nil {
		p.path = old
		return err
	}
	return nil
}

// VerifyArchiveSource refuses changed source identity, metadata or content.
func VerifyArchiveSource(p *ArchivePlan) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return verifyArchiveSource(p)
}

type archiveManifest struct {
	Version        int            `json:"version"`
	Source         string         `json:"source"`
	PlanSHA256     string         `json:"plan_sha256"`
	Entries        []ArchiveEntry `json:"entries"`
	EvidenceSHA256 string         `json:"evidence_sha256"`
	RequestSHA256  string         `json:"request_sha256"`
	RetainedInodes []string       `json:"retained_inode_identities"`
}

func archiveWrite(r *os.Root, path string, data []byte) error {
	dir := filepath.Dir(path)
	if dir != "." {
		if err := r.MkdirAll(dir, 0o700); err != nil {
			return err
		}
	}
	f, err := r.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	_, writeErr := f.Write(data)
	if writeErr == nil {
		writeErr = f.Sync()
	}
	closeErr := f.Close()
	if writeErr != nil {
		return writeErr
	}
	return closeErr
}

func archiveSync(r *os.Root) error {
	f, err := r.Open(".")
	if err != nil {
		return err
	}
	defer archiveClose(f)
	return f.Sync()
}

// WritePrivateArchive preserves an immutable copied snapshot. The completion
// marker is created exclusively only after readback, private restoration, and
// source revalidation. It grants no authorization to remove the source.
func WritePrivateArchive(p *ArchivePlan, archiveRoot string, evidenceJSON, requestJSON []byte, freeBytes func(string) (int64, error)) (ArchiveResult, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	var result ArchiveResult
	if err := verifyArchiveSource(p); err != nil {
		return result, err
	}
	if len(evidenceJSON) > 1<<20 || len(requestJSON) > 1<<20 || !json.Valid(evidenceJSON) || !json.Valid(requestJSON) || freeBytes == nil {
		return result, errors.New("invalid archive context or capacity probe")
	}
	if !filepath.IsAbs(archiveRoot) {
		return result, errors.New("archive root must be absolute")
	}
	for _, part := range strings.Split(strings.TrimPrefix(archiveRoot, string(filepath.Separator)), string(filepath.Separator)) {
		if part == "" || part == "." || part == ".." {
			return result, errors.New("ambiguous archive root path")
		}
	}
	if archiveRoot == p.path || strings.HasPrefix(archiveRoot, p.path+string(filepath.Separator)) {
		return result, errors.New("archive must be outside source")
	}
	parent, err := archiveOpenRoot(filepath.Dir(archiveRoot))
	if err != nil {
		return result, err
	}
	defer archiveClose(parent)
	// Reject inode aliases into the source, including case-insensitive aliases.
	for ancestor := filepath.Dir(archiveRoot); ; ancestor = filepath.Dir(ancestor) {
		r, e := archiveOpenRoot(ancestor)
		if e != nil {
			return result, e
		}
		info, e := r.Stat(".")
		archiveClose(r)
		if e != nil {
			return result, e
		}
		if os.SameFile(info, p.identity) {
			return result, errors.New("archive ancestor is source root")
		}
		if ancestor == string(filepath.Separator) {
			break
		}
	}
	base := filepath.Base(archiveRoot)
	if base == "." || base == ".." || base == string(filepath.Separator) {
		return result, errors.New("invalid archive root")
	}
	if err := parent.Mkdir(base, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return result, err
	}
	ar, err := archiveOpenRoot(archiveRoot)
	if err != nil {
		return result, err
	}
	defer archiveClose(ar)
	arInfo, err := ar.Stat(".")
	if err != nil {
		return result, err
	}
	if os.SameFile(arInfo, p.identity) {
		return result, errors.New("archive aliases source root")
	}
	if arInfo.Mode().Perm()&0o077 != 0 || !archiveOwned(arInfo) {
		return result, errors.New("archive root must be private")
	}
	// Reserve two payload copies plus bounded manifests and contexts. Avoid
	// overflow before adding the caller's minimum free-space reserve.
	overhead := int64(len(evidenceJSON)+len(requestJSON)) + int64(len(p.entries))*(512<<10) + 16384
	maxInt := int64(^uint64(0) >> 1)
	if p.total > (maxInt-overhead)/2 {
		return result, errors.New("archive capacity overflow")
	}
	needed := p.total*2 + overhead
	if p.bounds.MinFreeBytes > maxInt-needed {
		return result, errors.New("archive reserve overflow")
	}
	needed += p.bounds.MinFreeBytes
	available, err := freeBytes(archiveRoot)
	if err != nil {
		return result, err
	}
	if available < needed {
		return result, errors.New("insufficient private archive capacity")
	}
	// A callback may run external code: verify the namespace again before writes.
	check, err := archiveOpenRoot(archiveRoot)
	if err != nil {
		return result, err
	}
	checkInfo, statErr := check.Stat(".")
	archiveClose(check)
	if statErr != nil || !os.SameFile(arInfo, checkInfo) {
		return result, errors.New("archive root replaced")
	}
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return result, err
	}
	name := "archive-" + hex.EncodeToString(random[:])
	if err := ar.Mkdir(name, 0o700); err != nil {
		return result, err
	}
	published := false
	defer func() {
		if !published {
			_ = ar.RemoveAll(name)
		}
	}()
	dest, err := ar.OpenRoot(name)
	if err != nil {
		return result, err
	}
	defer archiveClose(dest)
	destIdentity, err := dest.Stat(".")
	if err != nil {
		return result, err
	}
	if err := dest.Mkdir("data", 0o700); err != nil {
		return result, err
	}
	for _, entry := range p.entries {
		data, info, err := archiveRead(p.root, entry.Path, p.bounds.MaxFileBytes)
		if err != nil {
			return result, err
		}
		if archiveHash(data) != entry.SHA256 || info.Mode() != entry.Mode {
			return result, errors.New("source changed while copying")
		}
		if err := archiveWrite(dest, "data/"+entry.Path, data); err != nil {
			return result, err
		}
	}
	if err := archiveWrite(dest, "evidence.json", evidenceJSON); err != nil {
		return result, err
	}
	if err := archiveWrite(dest, "request.json", requestJSON); err != nil {
		return result, err
	}
	retainedIdentities := make([]string, len(p.identities))
	for i, info := range p.identities {
		retainedIdentities[i] = archiveInodeIdentity(info)
	}
	manifest := archiveManifest{1, p.path, p.digest, p.Entries(), archiveHash(evidenceJSON), archiveHash(requestJSON), retainedIdentities}
	encoded, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return result, err
	}
	if err := archiveWrite(dest, "manifest.json", encoded); err != nil {
		return result, err
	}
	// Restore into a separate private directory using only archived bytes, then
	// reread each restored leaf. The source is never a restore destination.
	if err := dest.Mkdir("restore-check", 0o700); err != nil {
		return result, err
	}
	restore, err := dest.OpenRoot("restore-check")
	if err != nil {
		return result, err
	}
	restoreErr := func() error {
		for _, entry := range p.entries {
			data, _, err := archiveRead(dest, "data/"+entry.Path, p.bounds.MaxFileBytes)
			if err != nil {
				return err
			}
			if archiveHash(data) != entry.SHA256 {
				return errors.New("archive readback mismatch")
			}
			if err := archiveWrite(restore, entry.Path, data); err != nil {
				return err
			}
			again, _, err := archiveRead(restore, entry.Path, p.bounds.MaxFileBytes)
			if err != nil {
				return err
			}
			if archiveHash(again) != entry.SHA256 {
				return errors.New("restoration verification mismatch")
			}
		}
		return archiveSync(restore)
	}()
	archiveClose(restore)
	if restoreErr != nil {
		return result, restoreErr
	}
	if err := dest.RemoveAll("restore-check"); err != nil {
		return result, err
	}
	for path, want := range map[string]string{"manifest.json": archiveHash(encoded), "evidence.json": archiveHash(evidenceJSON), "request.json": archiveHash(requestJSON)} {
		data, _, err := archiveRead(dest, path, 3<<20)
		if err != nil {
			return result, err
		}
		if archiveHash(data) != want {
			return result, errors.New("archive metadata readback mismatch")
		}
	}
	if err := verifyArchiveSource(p); err != nil {
		return result, err
	}
	// Retain original inodes independently of the immutable copied snapshot.
	// Link failure is fatal; copying would lose preservation of open-FD writes.
	if err := dest.Mkdir("original-inodes", 0o700); err != nil {
		return result, err
	}
	for i, entry := range p.entries {
		target := filepath.Join(archiveRoot, name, "original-inodes", entry.Path)
		if err := dest.MkdirAll(filepath.Dir("original-inodes/"+entry.Path), 0o700); err != nil {
			return result, err
		}
		if err := archiveRetainOriginal(p.root, entry.Path, filepath.Join(p.path, entry.Path), target, p.identities[i]); err != nil {
			return result, err
		}
		data, info, err := archiveRead(p.root, entry.Path, p.bounds.MaxFileBytes)
		if err != nil {
			return result, err
		}
		if archiveHash(data) != entry.SHA256 || !archiveSameContentMetadata(info, p.identities[i]) {
			return result, errors.New("source changed while retaining inode")
		}
		p.identities[i] = info // acknowledge only our verified hardlink-induced ctime change
	}
	if err := verifyArchiveSource(p); err != nil {
		return result, err
	}
	// Sync every containing directory before publishing the completion marker.
	dirs := map[string]bool{".": true, "data": true, "original-inodes": true}
	for _, entry := range p.entries {
		for _, prefix := range []string{"data/", "original-inodes/"} {
			for dir := filepath.Dir(prefix + entry.Path); dir != "."; dir = filepath.Dir(dir) {
				dirs[dir] = true
			}
		}
	}
	for dir := range dirs {
		r, err := dest.OpenRoot(dir)
		if err != nil {
			return result, err
		}
		err = archiveSync(r)
		archiveClose(r)
		if err != nil {
			return result, err
		}
	}
	namespace, err := archiveOpenRoot(filepath.Join(archiveRoot, name))
	if err != nil {
		return result, err
	}
	namespaceInfo, namespaceErr := namespace.Stat(".")
	archiveClose(namespace)
	if namespaceErr != nil || !os.SameFile(destIdentity, namespaceInfo) {
		return result, errors.New("archive namespace replaced before publication")
	}
	marker, err := json.Marshal(struct {
		ManifestSHA256 string `json:"manifest_sha256"`
	}{archiveHash(encoded)})
	if err != nil {
		return result, err
	}
	if err := archiveWrite(dest, "complete.tmp", marker); err != nil {
		return result, err
	}
	if err := dest.Link("complete.tmp", "complete.json"); err != nil {
		return result, err
	}
	if err := dest.Remove("complete.tmp"); err != nil {
		return result, err
	}
	if err := archiveSync(dest); err != nil {
		return result, err
	}
	if err := archiveSync(ar); err != nil {
		return result, err
	}
	published = true
	return ArchiveResult{filepath.Join(archiveRoot, name), archiveHash(encoded), len(p.entries), p.total}, nil
}
