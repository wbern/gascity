// Package controllerendpoint resolves the lightweight controller address and
// city scope used by standalone controller clients. It intentionally avoids
// loading the full SDK configuration so hot-path helper binaries stay small.
package controllerendpoint

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"
)

// DefaultBaseURL is the default local supervisor API listener.
const DefaultBaseURL = "http://127.0.0.1:8372"

// BaseURL resolves the controller address from GC_API_URL, then from the
// lightweight supervisor.toml, then from DefaultBaseURL.
func BaseURL() string {
	if v := strings.TrimSpace(os.Getenv("GC_API_URL")); v != "" {
		return strings.TrimRight(v, "/")
	}
	if base, ok := supervisorTomlBaseURL(); ok {
		return base
	}
	return DefaultBaseURL
}

// CityName resolves an API city name from an explicit override or the managed
// session's GC_CITY_PATH / GC_CITY environment. It returns an empty string
// when no city scope is available.
func CityName(override string) string {
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
