// Package bdroute resolves the dependency-light controller route shared by gc
// early reads and the standalone bdshim client.
package bdroute

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"
)

const defaultControllerBaseURL = "http://127.0.0.1:8372"

// Target is the city-scoped controller route for a routed bead operation.
type Target struct {
	BaseURL string
	City    string
}

// Resolve finds a controller route without loading the SDK config. The caller
// may supply a city override; otherwise the city path environment determines
// the supervisor city name. It returns false when no city can be established.
func Resolve(cityOverride string, getenv func(string) string, supervisorBase func() string) (Target, bool) {
	if getenv == nil {
		getenv = os.Getenv
	}
	city := strings.TrimSpace(cityOverride)
	if city == "" {
		for _, key := range []string{"GC_CITY_PATH", "GC_CITY"} {
			if raw := strings.TrimSpace(getenv(key)); raw != "" {
				city = filepath.Base(strings.TrimRight(raw, "/"))
				break
			}
		}
	}
	if city == "" || city == "." {
		return Target{}, false
	}
	return Target{BaseURL: ControllerBaseURL(getenv, supervisorBase), City: city}, true
}

// ControllerBaseURL resolves the controller HTTP base URL without SDK config.
func ControllerBaseURL(getenv func(string) string, supervisorBase func() string) string {
	if getenv == nil {
		getenv = os.Getenv
	}
	base := strings.TrimSpace(getenv("GC_API_URL"))
	if base == "" && supervisorBase != nil {
		base = strings.TrimSpace(supervisorBase())
	}
	if base == "" {
		base = supervisorTomlBaseURL(getenv)
	}
	if base == "" {
		base = defaultControllerBaseURL
	}
	return strings.TrimRight(base, "/")
}

func supervisorTomlBaseURL(getenv func(string) string) string {
	var doc struct {
		Supervisor struct {
			Port int    `toml:"port"`
			Bind string `toml:"bind"`
		} `toml:"supervisor"`
	}
	paths := make([]string, 0, 2)
	if home := strings.TrimSpace(getenv("GC_HOME")); home != "" {
		paths = append(paths, filepath.Join(home, "supervisor.toml"))
	}
	if home, err := os.UserHomeDir(); err == nil {
		paths = append(paths, filepath.Join(home, ".gc", "supervisor.toml"))
	}
	for _, path := range paths {
		if _, err := toml.DecodeFile(path, &doc); err != nil {
			continue
		}
		if doc.Supervisor.Port <= 0 {
			continue
		}
		bind := doc.Supervisor.Bind
		switch bind {
		case "", "0.0.0.0":
			bind = "127.0.0.1"
		case "::", "[::]":
			bind = "::1"
		}
		return fmt.Sprintf("http://%s", net.JoinHostPort(bind, strconv.Itoa(doc.Supervisor.Port)))
	}
	return ""
}
