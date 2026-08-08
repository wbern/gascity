package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/bdshim"
)

// routeLogLine is one structured bdshim dispatch record, appended as JSONL for
// post-hoc routing/latency insight (which verbs route vs passthrough, and how
// long the in-proxy work took — excludes this binary's own OS spawn cost).
type routeLogLine struct {
	TS          string `json:"ts"`
	Verb        string `json:"verb"`
	Disposition string `json:"disposition"` // route | passthrough | refuse
	Exit        int    `json:"exit"`
	DurMS       int64  `json:"dur_ms"`
	Shape       string `json:"shape,omitempty"`
}

// logDisposition appends a best-effort JSONL record of one dispatch. It never
// fails the call: any error (no path, unwritable file, encode error) is silently
// dropped, because logging must not break a bd invocation.
func logDisposition(verb string, args []string, disposition string, exit int, start time.Time) {
	path := routeLogPath()
	if path == "" {
		return
	}
	line := routeLogLine{
		TS:          time.Now().UTC().Format(time.RFC3339Nano),
		Verb:        bdshim.CommandVerb(verb),
		Disposition: disposition,
		Exit:        exit,
		DurMS:       time.Since(start).Milliseconds(),
		Shape:       bdshim.CommandShape(args),
	}
	enc, err := json.Marshal(line)
	if err != nil {
		return
	}
	rotateIfOversized(path)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return
	}
	defer f.Close() //nolint:errcheck // best-effort log
	_, _ = f.Write(append(enc, '\n'))
}

// defaultRouteLogMaxBytes caps the route log. It is written on EVERY bd
// dispatch and had no bound at all: on gc2 it reached 73.5 MiB between
// 2026-07-19 and 2026-08-08 and was still growing (gcw-yr0o.8).
const defaultRouteLogMaxBytes = 32 << 20 // 32 MiB

// rotateIfOversized renames path to path+".1" once it exceeds the cap, so the
// live log restarts empty and at most one generation is kept. Best-effort, like
// the rest of this file: every failure path simply leaves the log as it was
// rather than risking a bd invocation.
//
// One generation, not N, on purpose — this is routing telemetry, not an audit
// trail, and an unbounded ring of generations would reintroduce the growth this
// fixes. Any earlier path+".1" is replaced; os.Rename overwrites on POSIX.
//
// Concurrency: many agents invoke bd at once, so two processes can decide to
// rotate together. os.Rename is atomic, so the loser renames an already-rotated
// (now short) file and at worst a few telemetry lines are lost. That is
// acceptable for a best-effort log and is why this takes no lock — a lock here
// would sit on the hot path of every bd call in the fleet.
func rotateIfOversized(path string) {
	maxBytes := int64(defaultRouteLogMaxBytes)
	if v := strings.TrimSpace(os.Getenv("GC_BDSHIM_LOG_MAX_BYTES")); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n <= 0 {
			return // malformed or disabled: leave the log alone
		}
		maxBytes = n
	}
	fi, err := os.Stat(path)
	if err != nil || fi.Size() < maxBytes {
		return
	}
	_ = os.Rename(path, path+".1")
}

// routeLogPath resolves the JSONL log path: the GC_BDSHIM_LOG override, else
// $GC_CITY_PATH/.gc/bdshim.log when a city is in scope, else empty (logging off).
func routeLogPath() string {
	if v := strings.TrimSpace(os.Getenv("GC_BDSHIM_LOG")); v != "" {
		return v
	}
	if city := strings.TrimSpace(os.Getenv("GC_CITY_PATH")); city != "" {
		return filepath.Join(city, ".gc", "bdshim.log")
	}
	return ""
}
