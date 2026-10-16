// Package exec implements [events.Provider] by delegating each operation to
// a user-supplied script via fork/exec. This follows the same pattern as
// the mail and session exec providers: a single script receives the operation
// name as its first argument and communicates via JSON on stdin/stdout.
//
// Record is fire-and-forget (fork per event, errors to stderr). List and
// LatestSeq fork once per call. Watch starts a long-running subprocess
// that streams NDJSON events on stdout.
package exec //nolint:revive // internal package, always imported with alias

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/gastownhall/gascity/internal/events"
)

// Provider implements [events.Provider] by delegating to a user-supplied script.
type Provider struct {
	script  string
	timeout time.Duration
	ready   sync.Once // ensure-running called once
	stderr  io.Writer
}

type listScriptFilter struct {
	Type     string
	Actor    string
	Since    time.Time
	AfterSeq uint64
}

// NewProvider returns an exec events provider that delegates to the given script.
// Errors from best-effort operations (Record) are logged to stderr.
func NewProvider(script string, stderr io.Writer) *Provider {
	return &Provider{
		script:  script,
		timeout: 30 * time.Second,
		stderr:  stderr,
	}
}

// Record delegates to: script record with JSON event on stdin.
// Best-effort — errors are printed to stderr, never returned.
func (p *Provider) Record(e events.Event) {
	p.ensureRunning()
	data, err := json.Marshal(e)
	if err != nil {
		p.logErr("record: marshal: %v", err)
		return
	}
	if _, err := p.run(data, "record"); err != nil {
		p.logErr("record: %v", err)
	}
}

// List delegates to: script list with JSON filter on stdin, then applies the
// SDK filter locally so optional script filtering cannot weaken the contract.
func (p *Provider) List(filter events.Filter) ([]events.Event, error) {
	p.ensureRunning()
	scriptFilter := listScriptFilter{
		Type:     filter.Type,
		Actor:    filter.Actor,
		Since:    filter.Since,
		AfterSeq: filter.AfterSeq,
	}
	data, err := json.Marshal(scriptFilter)
	if err != nil {
		return nil, fmt.Errorf("exec events provider: marshal filter: %w", err)
	}
	out, err := p.run(data, "list")
	if err != nil {
		return nil, err
	}
	if out == "" {
		return nil, nil
	}
	evts, err := unmarshalEvents(out)
	if err != nil {
		return nil, err
	}
	return events.ApplyFilter(evts, filter), nil
}

// LatestSeq delegates to: script latest-seq
func (p *Provider) LatestSeq() (uint64, error) {
	p.ensureRunning()
	out, err := p.run(nil, "latest-seq")
	if err != nil {
		return 0, err
	}
	if out == "" {
		return 0, nil
	}
	var seq uint64
	if _, err := fmt.Sscanf(out, "%d", &seq); err != nil {
		return 0, fmt.Errorf("exec events provider: parse seq %q: %w", out, err)
	}
	return seq, nil
}

// Watch starts a long-running subprocess: script watch <afterSeq>
// The subprocess streams NDJSON events on stdout. Each line is a
// complete JSON event.
func (p *Provider) Watch(ctx context.Context, afterSeq uint64) (events.Watcher, error) {
	p.ensureRunning()
	child, cancel := context.WithCancel(ctx)
	cmd := pollCommand(child, p.script, "watch", fmt.Sprint(afterSeq))
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return nil, err
	}
	if err = cmd.Start(); err != nil {
		cancel()
		_ = stdout.Close()
		return nil, err
	}
	stop := context.AfterFunc(child, func() { _ = stdout.Close() })
	return &execWatcher{cmd: cmd, reader: bufio.NewReader(stdout), stdout: stdout, ctx: child, cancel: cancel, stop: stop}, nil
}

// Close is a no-op for the exec provider.
func (p *Provider) Close() error {
	return nil
}

// ensureRunning calls "ensure-running" on the script once per provider
// lifetime. Exit 2 (unknown op) is treated as success.
func (p *Provider) ensureRunning() {
	p.ready.Do(func() {
		_, _ = p.run(nil, "ensure-running")
	})
}

// run executes the script with the given args, optionally piping stdinData
// to its stdin. Returns the trimmed stdout on success.
func (p *Provider) run(stdinData []byte, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), p.timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, p.script, args...)
	cmd.WaitDelay = 2 * time.Second

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if stdinData != nil {
		cmd.Stdin = bytes.NewReader(stdinData)
	}

	err := cmd.Run()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			if exitErr.ExitCode() == 2 {
				return "", nil
			}
		}
		errMsg := strings.TrimSpace(stderr.String())
		if errMsg == "" {
			errMsg = err.Error()
		}
		return "", fmt.Errorf("exec events provider %s %s: %s", p.script, strings.Join(args, " "), errMsg)
	}

	return strings.TrimRight(stdout.String(), "\n"), nil
}

// logErr logs an error to stderr (best-effort).
func (p *Provider) logErr(format string, args ...any) {
	if p.stderr != nil {
		fmt.Fprintf(p.stderr, "events exec: "+format+"\n", args...) //nolint:errcheck // best-effort stderr
	}
}

// unmarshalEvents decodes a JSON array of Events.
func unmarshalEvents(data string) ([]events.Event, error) {
	var evts []events.Event
	if err := json.Unmarshal([]byte(data), &evts); err != nil {
		return nil, fmt.Errorf("exec events provider: unmarshal events: %w", err)
	}
	return evts, nil
}

// execWatcher reads NDJSON events from a long-running subprocess.
type execWatcher struct {
	cmd       *exec.Cmd
	reader    *bufio.Reader
	stdout    io.ReadCloser
	ctx       context.Context
	cancel    context.CancelFunc
	stop      func() bool
	closeOnce sync.Once
}

// Next reads one unbounded-size NDJSON row from the subprocess stdout.
func (w *execWatcher) Next() (events.Event, error) {
	line, err := w.reader.ReadBytes('\n')
	if e := w.ctx.Err(); e != nil {
		return events.Event{}, e
	}
	if len(line) == 0 && err != nil {
		return events.Event{}, fmt.Errorf("exec events watcher: stream ended: %w", err)
	}
	var e events.Event
	if err := json.Unmarshal(bytes.TrimRight(line, "\r\n"), &e); err != nil {
		return events.Event{}, fmt.Errorf("exec events watcher: unmarshal: %w", err)
	}
	return e, nil
}

// Close cancels the owned process group, closes stdout, and joins exactly once.
func (w *execWatcher) Close() error {
	w.closeOnce.Do(func() { w.cancel(); _ = w.stdout.Close(); w.stop(); _ = w.cmd.Wait() })
	return nil
}

// Compile-time interface check.
var _ events.Provider = (*Provider)(nil)

// pollOutput shares event and ACK ordering. An ACK cannot overtake a produced
// suffix, including when the consumer drains only part of the bounded queue.
type pollOutput struct {
	event *events.Event
	ack   bool
	err   error
}

type pollScanner struct {
	ctx       context.Context
	cancel    context.CancelFunc
	joined    chan struct{}
	grants    chan events.PollBudget
	output    chan pollOutput
	mu        sync.Mutex // consumer state only; never held by worker
	active    bool
	terminal  error
	remaining events.PollBudget // worker-owned
}

func newPollScanner(ctx context.Context, run func(*pollScanner) error) *pollScanner {
	ctx, cancel := context.WithCancel(ctx)
	w := &pollScanner{ctx: ctx, cancel: cancel, joined: make(chan struct{}), grants: make(chan events.PollBudget, 1), output: make(chan pollOutput, 1)}
	go func() {
		defer close(w.joined)
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

func validPollBudget(b events.PollBudget) bool {
	// No arithmetic combines these independently consumed counters.
	return b.CompressedBytes > 0 && b.RawBytes > 0 && b.Records > 0 && b.Segments > 0
}

func (w *pollScanner) Poll(ctx context.Context, b events.PollBudget) (events.PollBatch, error) {
	if !validPollBudget(b) {
		return events.PollBatch{}, fmt.Errorf("event poll: budgets must be positive")
	}
	if err := ctx.Err(); err != nil {
		return events.PollBatch{}, err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := w.ctx.Err(); err != nil {
		return events.PollBatch{}, err
	}
	if w.terminal != nil {
		return events.PollBatch{}, w.terminal
	}
	if !w.active {
		w.active = true
		w.grants <- b
	}
	batch := events.PollBatch{Pending: true}
	for i := 0; i < 256; i++ {
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
		return nil
	case <-w.ctx.Done():
		return w.ctx.Err()
	}
}

func (w *pollScanner) boundary() error {
	if err := w.send(pollOutput{ack: true}); err != nil {
		return err
	}
	return w.awaitGrant()
}

// take suspends the worker's stack, not the decoder, at grant exhaustion.
// Incomplete record and gzip framing survive the ACK to the next grant.
func (w *pollScanner) take(field func(*events.PollBudget) *int) error {
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
	if err := w.take(func(b *events.PollBudget) *int { return &b.Records }); err != nil {
		return err
	}
	var e events.Event
	if err := json.Unmarshal(bytes.TrimRight(line, "\r\n"), &e); err != nil {
		return fmt.Errorf("exec events watcher: unmarshal: %w", err)
	}
	if e.Seq > *maxSeq {
		*maxSeq = e.Seq
		return w.send(pollOutput{event: &e})
	}
	return nil
}

// BeginWatch starts cancellable initialization and retained NDJSON replay off the
// caller. Close owns the subprocess group, stdout and worker join exactly once.
func (p *Provider) BeginWatch(ctx context.Context, afterSeq uint64) (events.PollWatcher, error) {
	return newPollScanner(ctx, func(w *pollScanner) error {
		initCtx, cancel := context.WithTimeout(w.ctx, p.timeout)
		init := pollCommand(initCtx, p.script, "ensure-running")
		err := init.Run()
		cancel()
		if err != nil {
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ExitCode() != 2 {
				return fmt.Errorf("exec events initialization: %w", err)
			}
		}
		if err := w.take(func(b *events.PollBudget) *int { return &b.Segments }); err != nil {
			return err
		}
		cmd := pollCommand(w.ctx, p.script, "watch", fmt.Sprint(afterSeq))
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			return err
		}
		defer stdout.Close() //nolint:errcheck // owned pipe cleanup
		if err = cmd.Start(); err != nil {
			return err
		}
		stop := context.AfterFunc(w.ctx, func() { _ = stdout.Close() })
		defer stop()
		// Wait exactly once, after closing the pipe and killing the owned process
		// group. Descendants holding stdout cannot keep a read or Wait alive.
		defer func() { _ = stdout.Close(); _ = cmd.Cancel(); _ = cmd.Wait() }()
		var partial []byte
		var one [1]byte
		input := bufio.NewReaderSize(&execGrantReader{w: w, r: stdout}, 4096)
		for {
			n, err := input.Read(one[:])
			if n > 0 {
				if e := w.take(func(b *events.PollBudget) *int { return &b.RawBytes }); e != nil {
					return e
				}
				partial = append(partial, one[0])
				if one[0] == '\n' {
					if e := w.record(partial, &afterSeq); e != nil {
						return e
					}
					partial = partial[:0]
				}
			}
			if err != nil {
				if err == io.EOF && len(partial) > 0 {
					if e := w.record(partial, &afterSeq); e != nil {
						return e
					}
				}
				if w.ctx.Err() != nil {
					return w.ctx.Err()
				}
				return fmt.Errorf("exec events watcher: stream ended: %w", err)
			}
		}
	}), nil
}

func pollCommand(ctx context.Context, script string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, script, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	return cmd
}

type execGrantReader struct {
	w *pollScanner
	r io.Reader
}

func (r *execGrantReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	for r.w.remaining.CompressedBytes == 0 {
		if err := r.w.boundary(); err != nil {
			return 0, err
		}
	}
	if err := r.w.ctx.Err(); err != nil {
		return 0, err
	}
	n := min(len(p), r.w.remaining.CompressedBytes, 4096)
	n, err := r.r.Read(p[:n])
	r.w.remaining.CompressedBytes -= n
	return n, err
}
