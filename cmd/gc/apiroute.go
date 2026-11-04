package main

import (
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/supervisor"
)

// Routing probes are indirect so tests need no live controller or supervisor.
var (
	apiRouteControllerIdentityHook = probeControllerIdentity
	apiRouteSupervisorClientHook   = supervisorCityAPIClient
)

// apiClient routes general commands through the serving controller. A
// supervisor-hosted city ignores its standalone API configuration. Commands
// retain their local fallback when the supervisor requires grants
// the bundled client cannot mint.
func apiClient(cityPath string) *api.Client {
	return routeAPIClient(cityPath, false)
}

func routeAPIClient(cityPath string, preferSupervisor bool) *api.Client {
	return routeAPIClientWithGrantPolicy(cityPath, preferSupervisor, supervisorRequiresCityGrants)
}

func routeAPIClientWithGrantPolicy(cityPath string, preferSupervisor bool, requiresGrants func() bool) *api.Client {
	if disabled, warn := classifyGCNoAPI(os.Getenv("GC_NO_API")); disabled {
		return nil
	} else if warn != "" {
		fmt.Fprintln(os.Stderr, "warning: "+warn) //nolint:errcheck // best-effort stderr
	}
	identity := apiRouteControllerIdentityHook(cityPath)
	switch {
	case identity.PID == 0 || identity.HostingMode == controllerHostingSupervisor:
		if requiresGrants() {
			return nil
		}
		return apiRouteSupervisorClientHook(cityPath)
	default:
		c := standaloneControllerClient(cityPath)
		if c == nil && preferSupervisor && identity.HostingMode == controllerHostingUnknown {
			if requiresGrants() {
				return nil
			}
			return apiRouteSupervisorClientHook(cityPath)
		}
		return c
	}
}

// supervisorRequiresCityGrants reports whether general commands need a local
// fallback because the supervisor requires a read or write grant. Unreadable
// supervisor policy fails closed instead of sending an unauthenticated request.
func supervisorRequiresCityGrants() bool {
	cfg, err := supervisor.LoadConfig(supervisor.ConfigPath())
	if err != nil {
		return true
	}
	policy := cfg.Supervisor
	return policy.WriteAuthRequired || policy.ReadAuthRequired ||
		strings.TrimSpace(policy.WriteAuthVerifyKey) != "" || strings.TrimSpace(policy.ReadAuthVerifyKey) != "" ||
		strings.TrimSpace(os.Getenv("GC_CITY_WRITE_PUBKEY")) != "" || strings.TrimSpace(os.Getenv("GC_CITY_READ_PUBKEY")) != "" ||
		os.Getenv("GC_CITY_WRITE_REQUIRED") == "1" || os.Getenv("GC_CITY_READ_REQUIRED") == "1"
}

// supervisorRequiresCityReadGrants applies only read policy to status requests.
// A write-only grant policy must not disable the supervisor's warm GET views.
func supervisorRequiresCityReadGrants() bool {
	cfg, err := supervisor.LoadConfig(supervisor.ConfigPath())
	if err != nil {
		return true
	}
	return cfg.Supervisor.ReadAuthRequired || strings.TrimSpace(cfg.Supervisor.ReadAuthVerifyKey) != "" ||
		strings.TrimSpace(os.Getenv("GC_CITY_READ_PUBKEY")) != "" || os.Getenv("GC_CITY_READ_REQUIRED") == "1"
}

// standaloneControllerClient builds an API client for a standalone controller
// that binds cfg.API.Port from city.toml. It returns nil when no usable
// standalone HTTP endpoint exists: the config is unreadable, [api] port is
// unset, or the listener binds a non-loopback address without allow_mutations.
func standaloneControllerClient(cityPath string) *api.Client {
	tomlPath := filepath.Join(cityPath, "city.toml")
	cfg, err := config.Load(fsys.OSFS{}, tomlPath)
	if err != nil || cfg.API.Port <= 0 {
		return nil
	}
	// Non-localhost bind means API runs read-only — skip API routing
	// (unless allow_mutations is set).
	bind := cfg.API.BindOrDefault()
	if bind != "127.0.0.1" && bind != "localhost" && bind != "::1" && !cfg.API.AllowMutations {
		return nil
	}
	baseURL := fmt.Sprintf("http://%s", net.JoinHostPort(bind, strconv.Itoa(cfg.API.Port)))
	// Standalone controller serves /v0/city/{cityName}/... routes via
	// api.NewSupervisorMux, so per-city method calls need a city-scoped
	// client. Derive the city name from config; the controller only
	// serves one city in standalone mode.
	return api.NewCityScopedClient(baseURL, standaloneControllerCityName(cfg, cityPath))
}

// standaloneControllerCityName resolves the effective city name for a
// standalone controller API client. In standalone mode the controller serves
// exactly one city, so the client must match the runtime identity.
func standaloneControllerCityName(cfg *config.City, cityPath string) string {
	return loadedCityName(cfg, cityPath)
}

// supervisorFallthroughAPIClient prefers the supervisor for commands with no
// local fallback, or a costly one. Explicit standalone hosting never falls
// through to the supervisor; unknown legacy hosting may do so without a port.
func supervisorFallthroughAPIClient(cityPath string) (*api.Client, string) {
	if c := routeAPIClient(cityPath, true); c != nil {
		return c, ""
	}
	return nil, routeFallbackReason(cityPath, true)
}

// statusReadAPIClient resolves warm API views for status-shaped read commands.
// Read grant requirements fail closed; write-only requirements leave GETs usable.
func statusReadAPIClient(cityPath string) (*api.Client, string) {
	if c := routeAPIClientWithGrantPolicy(cityPath, true, supervisorRequiresCityReadGrants); c != nil {
		return c, ""
	}
	return nil, routeFallbackReasonWithGrantPolicy(cityPath, true, supervisorRequiresCityReadGrants)
}

// apiClientFallbackReason explains why general API routing fell back locally.
func apiClientFallbackReason(cityPath string) string {
	return routeFallbackReason(cityPath, false)
}

func routeFallbackReason(cityPath string, preferSupervisor bool) string {
	return routeFallbackReasonWithGrantPolicy(cityPath, preferSupervisor, supervisorRequiresCityGrants)
}

func routeFallbackReasonWithGrantPolicy(cityPath string, preferSupervisor bool, requiresGrants func() bool) string {
	if disabled, _ := classifyGCNoAPI(os.Getenv("GC_NO_API")); disabled {
		return "escape-hatch"
	}
	identity := apiRouteControllerIdentityHook(cityPath)
	if identity.PID > 0 && identity.HostingMode == controllerHostingUnknown && preferSupervisor && standaloneControllerClient(cityPath) == nil && requiresGrants() {
		return "grant-required"
	}
	if identity.PID == 0 || identity.HostingMode == controllerHostingSupervisor {
		if requiresGrants() {
			return "grant-required"
		}
		return "controller-down"
	}
	tomlPath := filepath.Join(cityPath, "city.toml")
	if cfg, err := config.Load(fsys.OSFS{}, tomlPath); err == nil && cfg.API.Port > 0 {
		bind := cfg.API.BindOrDefault()
		if bind != "127.0.0.1" && bind != "localhost" && bind != "::1" && !cfg.API.AllowMutations {
			return "non-loopback-bind"
		}
	}
	return "controller-down"
}

// resolveAgentForAPI resolves a bare agent name (e.g., "worker") to its
// qualified form (e.g., "myrig/worker") using the current rig context, so
// the API server can find the agent. If already qualified or resolution
// fails, the original name is returned.
func resolveAgentForAPI(cityPath, name string) string {
	cfg, err := loadCityConfig(cityPath, io.Discard)
	if err != nil {
		cfg, err = loadCityConfigForEditFS(fsys.OSFS{}, filepath.Join(cityPath, "city.toml"))
		if err != nil {
			return name
		}
	}
	resolved, ok := resolveAgentIdentity(cfg, name, currentRigContext(cfg))
	if !ok {
		return name
	}
	return resolved.QualifiedName()
}
