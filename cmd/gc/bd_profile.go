package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime/pprof"
	runtimetrace "runtime/trace"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const bdProfileDirEnv = "GC_BD_PROFILE_DIR"

var bdProfileSequence atomic.Uint64

type bdInvocationProfilePhase struct {
	Name       string  `json:"name"`
	DurationMS float64 `json:"duration_ms"`
}

// bdInvocationProfileReport deliberately records no invocation arguments:
// profiles are frequently shared for performance review, while bead IDs and
// command values can be sensitive.
type bdInvocationProfileReport struct {
	Version   int                        `json:"version"`
	Command   string                     `json:"command"`
	StartedAt string                     `json:"started_at"`
	TotalMS   float64                    `json:"total_ms"`
	Phases    []bdInvocationProfilePhase `json:"phases"`
}

func (r bdInvocationProfileReport) hasPhase(name string) bool {
	for _, phase := range r.Phases {
		if phase.Name == name {
			return true
		}
	}
	return false
}

func (r bdInvocationProfileReport) totalPhaseDuration() float64 {
	for _, phase := range r.Phases {
		if phase.Name == "total" {
			return phase.DurationMS
		}
	}
	return 0
}

// bdInvocationProfiler is a process-local, opt-in diagnostic for one gc bd
// invocation. It is intentionally separate from GC_BD_TRACE_JSON: that trace
// describes child bd calls, while this captures gc's own startup and routing
// phases as well as Go CPU and runtime traces.
type bdInvocationProfiler struct {
	enabled bool
	started time.Time
	dir     string
	prefix  string
	stderr  io.Writer

	mu     sync.Mutex
	phases []bdInvocationProfilePhase
	once   sync.Once

	cpuFile      *os.File
	runtimeFile  *os.File
	cpuStarted   bool
	traceStarted bool
}

type bdInvocationProfilerContextKey struct{}

var disabledBdInvocationProfiler = &bdInvocationProfiler{}

func newBdInvocationProfiler(args []string, stderr io.Writer) *bdInvocationProfiler {
	if !isBuiltinBdInvocation(args) {
		return disabledBdInvocationProfiler
	}
	dir := strings.TrimSpace(os.Getenv(bdProfileDirEnv))
	if dir == "" {
		return disabledBdInvocationProfiler
	}
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		bdProfileWarning(stderr, "profile directory %q is unavailable", dir)
		return disabledBdInvocationProfiler
	}

	started := time.Now()
	prefix := fmt.Sprintf("gc-bd-%s-%d-%d", started.UTC().Format("20060102T150405.000000000Z"), os.Getpid(), bdProfileSequence.Add(1))
	profiler := &bdInvocationProfiler{
		enabled: true,
		started: started,
		dir:     dir,
		prefix:  prefix,
		stderr:  stderr,
	}
	if err := profiler.start(); err != nil {
		bdProfileWarning(stderr, "%v", err)
		profiler.removeArtifacts()
		return disabledBdInvocationProfiler
	}
	return profiler
}

func (p *bdInvocationProfiler) start() error {
	cpuFile, err := os.OpenFile(filepath.Join(p.dir, p.prefix+".cpu.pprof"), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("creating CPU profile: %w", err)
	}
	p.cpuFile = cpuFile
	if err := pprof.StartCPUProfile(cpuFile); err != nil {
		return fmt.Errorf("starting CPU profile: %w", err)
	}
	p.cpuStarted = true

	runtimeFile, err := os.OpenFile(filepath.Join(p.dir, p.prefix+".runtime.trace"), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		pprof.StopCPUProfile()
		p.cpuStarted = false
		return fmt.Errorf("creating runtime trace: %w", err)
	}
	p.runtimeFile = runtimeFile
	if err := runtimetrace.Start(runtimeFile); err != nil {
		pprof.StopCPUProfile()
		p.cpuStarted = false
		return fmt.Errorf("starting runtime trace: %w", err)
	}
	p.traceStarted = true
	return nil
}

func (p *bdInvocationProfiler) phase(name string) func() {
	if p == nil || !p.enabled {
		return func() {}
	}
	started := time.Now()
	return func() {
		p.mu.Lock()
		p.phases = append(p.phases, bdInvocationProfilePhase{
			Name:       name,
			DurationMS: float64(time.Since(started)) / float64(time.Millisecond),
		})
		p.mu.Unlock()
	}
}

func (p *bdInvocationProfiler) close() {
	if p == nil || !p.enabled {
		return
	}
	p.once.Do(func() {
		totalDuration := time.Since(p.started)
		p.mu.Lock()
		p.phases = append(p.phases, bdInvocationProfilePhase{
			Name:       "total",
			DurationMS: float64(totalDuration) / float64(time.Millisecond),
		})
		phases := append([]bdInvocationProfilePhase(nil), p.phases...)
		p.mu.Unlock()

		if p.traceStarted {
			runtimetrace.Stop()
			p.traceStarted = false
		}
		if p.cpuStarted {
			pprof.StopCPUProfile()
			p.cpuStarted = false
		}
		p.closeArtifacts()

		report := bdInvocationProfileReport{
			Version:   1,
			Command:   "bd",
			StartedAt: p.started.UTC().Format(time.RFC3339Nano),
			TotalMS:   float64(totalDuration) / float64(time.Millisecond),
			Phases:    phases,
		}
		data, err := json.MarshalIndent(report, "", "  ")
		if err != nil {
			bdProfileWarning(p.stderr, "encoding phase report: %v", err)
			return
		}
		if err := os.WriteFile(filepath.Join(p.dir, p.prefix+".phases.json"), append(data, '\n'), 0o600); err != nil {
			bdProfileWarning(p.stderr, "writing phase report: %v", err)
		}
	})
}

func (p *bdInvocationProfiler) closeArtifacts() {
	if p.runtimeFile != nil {
		_ = p.runtimeFile.Close()
		p.runtimeFile = nil
	}
	if p.cpuFile != nil {
		_ = p.cpuFile.Close()
		p.cpuFile = nil
	}
}

func (p *bdInvocationProfiler) removeArtifacts() {
	var paths []string
	if p.runtimeFile != nil {
		paths = append(paths, p.runtimeFile.Name())
	}
	if p.cpuFile != nil {
		paths = append(paths, p.cpuFile.Name())
	}
	p.closeArtifacts()
	for _, path := range paths {
		_ = os.Remove(path)
	}
}

func bdProfileWarning(stderr io.Writer, format string, args ...any) {
	if stderr == nil {
		return
	}
	_, _ = fmt.Fprintf(stderr, "gc bd: profile: "+format+"\n", args...)
}

func withBdInvocationProfiler(ctx context.Context, profiler *bdInvocationProfiler) context.Context {
	if profiler == nil || !profiler.enabled {
		return ctx
	}
	return context.WithValue(ctx, bdInvocationProfilerContextKey{}, profiler)
}

func bdInvocationProfilerFromContext(ctx context.Context) *bdInvocationProfiler {
	if ctx != nil {
		if profiler, ok := ctx.Value(bdInvocationProfilerContextKey{}).(*bdInvocationProfiler); ok && profiler != nil {
			return profiler
		}
	}
	return disabledBdInvocationProfiler
}
