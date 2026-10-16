package events

import (
	"bufio"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// backfillConcurrency bounds how many watchers may actively read archive/rotating
// segments concurrently. A cold resume gunzips archives; without a cap, N
// simultaneous resumes multiply the CPU/IO cost. Excess resumes queue on this
// semaphore. The slot is held only during a single bounded batch read (not
// across the consumer's drain), so it caps concurrent decode work without
// pinning memory.
const backfillConcurrency = 2

// backfillBatch bounds how many events one backfill step buffers before yielding
// to the consumer. It caps a watcher's resident backfill memory to O(batch)
// events (not a whole archive segment), so many concurrent cold resumes cannot
// aggregate into an OOM.
const backfillBatch = 256

var backfillSlots = make(chan struct{}, backfillConcurrency)

// acquireBackfillSlot blocks for a decode slot, returning early if ctx is
// canceled or done is closed (a Close mid-wait must unblock Next).
func acquireBackfillSlot(ctx context.Context, done <-chan struct{}) error {
	select {
	case backfillSlots <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-done:
		return errWatcherClosed
	}
}

func releaseBackfillSlot() { <-backfillSlots }

// backfillSourceKind distinguishes a gzip archive from a plain-JSONL segment.
type backfillSourceKind int

const (
	sourceArchive backfillSourceKind = iota
	sourceRotating
)

// backfillSource is one on-disk segment contributing to a resume backfill,
// ordered by its first sequence. fallbackPath, set for rotating files, is the
// canonical .gz archive the recorder promotes the file to: the promotion
// (rename .gz into place, THEN remove the rotating source) can land between the
// directory listing and the open, in which case the archive did not exist at
// list time and the rotating file no longer does — reading the derived archive
// path is the only way not to lose that window.
type backfillSource struct {
	path         string
	fallbackPath string
	kind         backfillSourceKind
	firstSeq     uint64
	lastSeq      uint64
}

// listBackfillSources returns the .gz archives and in-flight rotating-* files in
// dir whose seq window may contain events with Seq > afterSeq, sorted by first
// sequence. A .gz archive and its not-yet-removed rotating source share a seq
// window; the streamed monotonic guard drops the duplicate, so both are safe to
// include.
func listBackfillSources(dir string, afterSeq uint64) ([]backfillSource, error) {
	entries, err := readRotationDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var srcs []backfillSource
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		switch {
		case isCanonicalArchiveBasename(name):
			info, perr := parseArchiveBasename(name)
			if perr != nil {
				continue
			}
			if afterSeq > 0 && info.LastSeq <= afterSeq {
				continue
			}
			srcs = append(srcs, backfillSource{
				path: filepath.Join(dir, name), kind: sourceArchive,
				firstSeq: info.FirstSeq, lastSeq: info.LastSeq,
			})
		case hasRotatingPrefix(name):
			ts, first, last, ok := parseRotatingBasename(name)
			if !ok {
				continue // legacy rotating file without a window; reaper promotes it
			}
			if afterSeq > 0 && last <= afterSeq {
				continue
			}
			srcs = append(srcs, backfillSource{
				path:         filepath.Join(dir, name),
				fallbackPath: filepath.Join(dir, formatArchiveBasename(ts, first, last)),
				kind:         sourceRotating,
				firstSeq:     first, lastSeq: last,
			})
		}
	}
	sort.Slice(srcs, func(i, j int) bool {
		if srcs[i].firstSeq != srcs[j].firstSeq {
			return srcs[i].firstSeq < srcs[j].firstSeq
		}
		return srcs[i].kind < srcs[j].kind
	})
	return srcs, nil
}

// isCanonicalArchiveBasename reports whether name is a canonical .gz archive.
func isCanonicalArchiveBasename(name string) bool {
	return strings.HasPrefix(name, "events.jsonl.archive-") && strings.HasSuffix(name, ".gz")
}

// segmentReader streams events from one open backfill segment line by line via
// bufio.Reader.ReadBytes — which, unlike bufio.Scanner, imposes no maximum line
// length, so an event larger than 1 MiB cannot poison the resume. It owns the
// underlying file (and gzip stream, for archives) and is closed exactly once.
type segmentReader struct {
	f  *os.File
	gz *gzip.Reader
	br *bufio.Reader
}

// openSegmentReader opens one backfill segment. A rotating file that vanished
// between listing and open was promoted (the recorder renames the .gz into place
// BEFORE removing the rotating source), so its derived archive path is read
// instead — skipping would silently lose the window whenever the promotion lands
// in that gap, because the .gz did not exist at list time. A missing archive
// yields (nil, nil): archives are only removed by retention reaping, which never
// touches windows a live backfill can still need (and the monotonic cursor makes
// a re-listed duplicate harmless).
func openSegmentReader(src backfillSource) (*segmentReader, error) {
	f, err := os.Open(src.path)
	kind := src.kind
	if err != nil && os.IsNotExist(err) && src.fallbackPath != "" {
		f, err = os.Open(src.fallbackPath)
		kind = sourceArchive
	}
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	sr := &segmentReader{f: f}
	var r io.Reader = f
	if kind == sourceArchive {
		gz, gerr := gzip.NewReader(f)
		if gerr != nil {
			_ = f.Close()
			return nil, gerr
		}
		sr.gz = gz
		r = gz
	}
	sr.br = bufio.NewReaderSize(r, 64*1024)
	return sr, nil
}

// activeSegmentReader wraps a captured active fd (0..size) as a segment reader.
func activeSegmentReader(f *os.File, size int64, afterSeq uint64) (*segmentReader, error) {
	start := activeScanStart(f, size, afterSeq)
	if _, err := f.Seek(start, io.SeekStart); err != nil {
		return nil, err
	}
	return &segmentReader{f: nil, br: bufio.NewReaderSize(&io.LimitedReader{R: f, N: size - start}, 64*1024)}, nil
}

// readInto reads up to batch filter-matching events with Seq > *maxSeq from the
// segment into out, advancing *maxSeq. It returns done=true at end of segment.
// Malformed and oversized-but-unparseable lines are skipped, never fatal.
func (sr *segmentReader) readInto(filter Filter, maxSeq *uint64, out *[]Event, batch int) (bool, error) {
	added := 0
	for added < batch {
		line, err := sr.br.ReadBytes('\n')
		if len(line) > 0 {
			var e Event
			if json.Unmarshal(trimLine(line), &e) == nil && matchesFilter(e, filter) && e.Seq > *maxSeq {
				*maxSeq = e.Seq
				*out = append(*out, e)
				added++
			}
		}
		if err != nil {
			if err == io.EOF {
				return true, nil
			}
			return false, err
		}
	}
	return false, nil
}

func trimLine(b []byte) []byte {
	for len(b) > 0 && (b[len(b)-1] == '\n' || b[len(b)-1] == '\r') {
		b = b[:len(b)-1]
	}
	return b
}

// close releases the segment's gzip stream and file. Safe to call once; the
// owning fileWatcher is single-consumer, so no concurrent close occurs.
func (sr *segmentReader) close() {
	if sr == nil {
		return
	}
	if sr.gz != nil {
		_ = sr.gz.Close()
	}
	// A nil f means the reader wraps a caller-owned active fd (closed elsewhere).
	if sr.f != nil {
		_ = sr.f.Close()
	}
}

// pollOutput shares event and ACK ordering. An ACK cannot overtake a produced
// suffix, including when the consumer drains only part of the bounded queue.
type pollOutput struct {
	event *Event
	ack   bool
	err   error
}

type pollScanner struct {
	ctx       context.Context
	cancel    context.CancelFunc
	joined    chan struct{}
	grants    chan PollBudget
	output    chan pollOutput
	mu        sync.Mutex // consumer state only; never held by worker
	active    bool
	terminal  error
	remaining PollBudget // worker-owned
	slotted   bool       // worker-owned backfill concurrency slot
}

func newPollScanner(ctx context.Context, run func(*pollScanner) error) *pollScanner {
	ctx, cancel := context.WithCancel(ctx)
	w := &pollScanner{ctx: ctx, cancel: cancel, joined: make(chan struct{}), grants: make(chan PollBudget, 1), output: make(chan pollOutput, 1)}
	go func() {
		defer close(w.joined)
		defer func() {
			if w.slotted {
				releaseBackfillSlot()
			}
		}()
		err := w.awaitGrant()
		if err == nil {
			err = run(w)
		}
		if err != nil {
			_ = w.send(pollOutput{err: err})
		}
	}()
	return w
}

func validPollBudget(b PollBudget) bool {
	// No arithmetic combines these independently consumed counters.
	return b.CompressedBytes > 0 && b.RawBytes > 0 && b.Records > 0 && b.Segments > 0
}

func (w *pollScanner) Poll(ctx context.Context, b PollBudget) (PollBatch, error) {
	if !validPollBudget(b) {
		return PollBatch{}, fmt.Errorf("event poll: budgets must be positive")
	}
	if err := ctx.Err(); err != nil {
		return PollBatch{}, err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := w.ctx.Err(); err != nil {
		return PollBatch{}, err
	}
	if w.terminal != nil {
		return PollBatch{}, w.terminal
	}
	if !w.active {
		w.active = true
		w.grants <- b
	}
	batch := PollBatch{Pending: true}
	for i := 0; i < backfillBatch; i++ {
		select {
		case out := <-w.output:
			if out.event != nil {
				batch.Events = append(batch.Events, *out.event)
			}
			if out.err != nil {
				w.terminal = out.err
				return batch, out.err
			}
			if out.ack {
				w.active = false
				batch.Pending = false
				return batch, nil
			}
		default:
			return batch, nil
		}
	}
	return batch, nil
}

func (w *pollScanner) Close() error { w.cancel(); <-w.joined; return nil }
func (w *pollScanner) send(out pollOutput) error {
	select {
	case w.output <- out:
		return nil
	case <-w.ctx.Done():
		return w.ctx.Err()
	}
}

func (w *pollScanner) awaitGrant() error {
	select {
	case b := <-w.grants:
		w.remaining = b
		if err := acquireBackfillSlot(w.ctx, w.ctx.Done()); err != nil {
			return err
		}
		w.slotted = true
		return nil
	case <-w.ctx.Done():
		return w.ctx.Err()
	}
}

func (w *pollScanner) boundary() error {
	if w.slotted {
		releaseBackfillSlot()
		w.slotted = false
	}
	if err := w.send(pollOutput{ack: true}); err != nil {
		return err
	}
	return w.awaitGrant()
}

// take suspends the worker's stack, not the decoder, at grant exhaustion.
// Incomplete record and gzip framing survive the ACK to the next grant.
func (w *pollScanner) take(field func(*PollBudget) *int) error {
	for *field(&w.remaining) == 0 {
		if err := w.boundary(); err != nil {
			return err
		}
	}
	if err := w.ctx.Err(); err != nil {
		return err
	}
	*field(&w.remaining)--
	return nil
}

func (w *pollScanner) record(line []byte, maxSeq *uint64) error {
	if err := w.take(func(b *PollBudget) *int { return &b.Records }); err != nil {
		return err
	}
	var e Event
	if json.Unmarshal(trimLine(line), &e) == nil && e.Seq > *maxSeq {
		*maxSeq = e.Seq
		return w.send(pollOutput{event: &e})
	}
	return nil
}

// grantReader never returns synthetic EOF/errors on exhaustion: gzip retains
// its decoder stack while this bounded input reader awaits the next grant.
// The bufio reader above it owns at most one 4 KiB compressed fragment.
type grantReader struct {
	w *pollScanner
	r io.Reader
}

func (r *grantReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	field := func(b *PollBudget) *int { return &b.CompressedBytes }
	for *field(&r.w.remaining) == 0 {
		if err := r.w.boundary(); err != nil {
			return 0, err
		}
	}
	if err := r.w.ctx.Err(); err != nil {
		return 0, err
	}
	n := min(len(p), *field(&r.w.remaining), 4096)
	// This reader has no nested grants; only the framing reader above it can
	// suspend inside gzip. Charge actual input even for a short read.
	n, err := r.r.Read(p[:n])
	*field(&r.w.remaining) -= n
	return n, err
}

func (r *grantReader) ReadByte() (byte, error) {
	var b [1]byte
	n, err := r.Read(b[:])
	if n == 1 {
		return b[0], nil
	}
	return 0, err
}

// scanPollLines incrementally frames arbitrary valid row sizes. Only partial
// framing can grow to record size; produced-event buffering remains bounded.
func (w *pollScanner) scanPollLines(r io.Reader, maxSeq *uint64, partial *[]byte) error {
	var one [1]byte
	for {
		n, err := r.Read(one[:])
		if n > 0 {
			// Count admitted raw bytes after the underlying read: a gzip input grant
			// may have exhausted while producing this single byte.
			if takeErr := w.take(func(b *PollBudget) *int { return &b.RawBytes }); takeErr != nil {
				return takeErr
			}
			*partial = append(*partial, one[0])
			if one[0] == '\n' {
				if e := w.record(*partial, maxSeq); e != nil {
					return e
				}
				*partial = (*partial)[:0]
			}
		}
		if err != nil {
			return err
		}
	}
}
