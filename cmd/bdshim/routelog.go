package main

import (
	"encoding/json"
	"os"
	"path/filepath"
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
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return
	}
	defer f.Close() //nolint:errcheck // best-effort log
	_, _ = f.Write(append(enc, '\n'))
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
