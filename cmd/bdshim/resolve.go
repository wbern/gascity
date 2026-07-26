package main

import (
	"net/http"
	"os"
	"time"

	"github.com/gastownhall/gascity/internal/bdroute"
)

// defaultControllerBaseURL is the supervisor API default (bind 127.0.0.1, port
// 8372 — supervisor.Section.{BindOrDefault,PortOrDefault}). Used when neither
// GC_API_URL nor a supervisor.toml override is present.
const defaultControllerBaseURL = "http://127.0.0.1:8372"

// controllerBaseURL resolves the controller HTTP base URL from the lightest
// available source: the GC_API_URL env override, else the supervisor.toml
// bind/port, else the well-known default. It never loads the heavy SDK config.
func controllerBaseURL() string {
	return bdroute.ControllerBaseURL(os.Getenv, nil)
}

// resolveCityName resolves the API-path city name: the --city override when
// present, else the basename of GC_CITY_PATH (or GC_CITY), which the supervisor
// registers a city under. Returns "" when nothing is resolvable (routing is then
// skipped and the caller falls back to passthrough).
func resolveCityName(override string) string {
	target, ok := bdroute.Resolve(override, os.Getenv, nil)
	if !ok {
		return ""
	}
	return target.City
}

// controllerReachable reports whether the controller answers an HTTP request at
// base within a short timeout. Any HTTP status (even 404) proves the listener is
// up. Used ONLY by the infrequent claim path to decide whether to fall back to
// the real bd's atomic claim — where a spurious miss is harmless because the
// fallback claim is correct. The hot read/write path deliberately does not probe
// (a probe can spuriously trip under load and mis-route a read to bd.real).
func controllerReachable(base string) bool {
	client := &http.Client{Timeout: 400 * time.Millisecond}
	resp, err := client.Get(base + "/")
	if err != nil {
		return false
	}
	_ = resp.Body.Close()
	return true
}
