package main

import (
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

// defaultControllerBaseURL is the supervisor API default (bind 127.0.0.1, port
// 8372 — supervisor.Section.{BindOrDefault,PortOrDefault}). Used when neither
// GC_API_URL nor a supervisor.toml override is present.
const defaultControllerBaseURL = "http://127.0.0.1:8372"

// controllerBaseURL resolves the controller HTTP base URL from the lightest
// available source: the GC_API_URL env override, else the supervisor.toml
// bind/port, else the well-known default. It never loads the heavy SDK config.
func controllerBaseURL() string {
	if v := strings.TrimSpace(os.Getenv("GC_API_URL")); v != "" {
		return strings.TrimRight(v, "/")
	}
	if base, ok := supervisorTomlBaseURL(); ok {
		return base
	}
	return defaultControllerBaseURL
}

// supervisorTomlBaseURL reads just the [supervisor] bind/port from supervisor.toml
// (GC_HOME/supervisor.toml, else ~/.gc/supervisor.toml) to build the base URL. It
// returns ok=false when the file is absent or carries no port (defaults apply).
func supervisorTomlBaseURL() (string, bool) {
	var doc struct {
		Supervisor struct {
			Port int    `toml:"port"`
			Bind string `toml:"bind"`
		} `toml:"supervisor"`
	}
	found := false
	for _, path := range supervisorTomlCandidates() {
		if _, err := toml.DecodeFile(path, &doc); err == nil {
			found = true
			break
		}
	}
	if !found || doc.Supervisor.Port <= 0 {
		return "", false
	}
	bind := doc.Supervisor.Bind
	switch bind {
	case "", "0.0.0.0":
		bind = "127.0.0.1"
	case "::", "[::]":
		bind = "::1"
	}
	return fmt.Sprintf("http://%s", net.JoinHostPort(bind, strconv.Itoa(doc.Supervisor.Port))), true
}

// supervisorTomlCandidates lists the supervisor.toml paths to try, GC_HOME first.
func supervisorTomlCandidates() []string {
	var paths []string
	if home := strings.TrimSpace(os.Getenv("GC_HOME")); home != "" {
		paths = append(paths, filepath.Join(home, "supervisor.toml"))
	}
	if h, err := os.UserHomeDir(); err == nil {
		paths = append(paths, filepath.Join(h, ".gc", "supervisor.toml"))
	}
	return paths
}

// resolveCityName resolves the API-path city name: the --city override when
// present, else the basename of GC_CITY_PATH (or GC_CITY), which the supervisor
// registers a city under. Returns "" when nothing is resolvable (routing is then
// skipped and the caller falls back to passthrough).
func resolveCityName(override string) string {
	if o := strings.TrimSpace(override); o != "" {
		return o
	}
	for _, env := range []string{"GC_CITY_PATH", "GC_CITY"} {
		if v := strings.TrimSpace(os.Getenv(env)); v != "" {
			return filepath.Base(strings.TrimRight(v, "/"))
		}
	}
	return ""
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
