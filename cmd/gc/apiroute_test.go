package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/supervisor"
)

// writeCityTOMLForRoute writes a minimal city.toml into dir and returns dir.
func writeCityTOMLForRoute(t *testing.T, dir, body string) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "city.toml"), []byte(body), 0o644); err != nil {
		t.Fatalf("writing city.toml: %v", err)
	}
	return dir
}

// TestStandaloneControllerClient covers standalone endpoint construction from
// city.toml: a configured port and a loopback bind (or allowed mutations).
// Hosting identity determines whether routing consults this endpoint at all.
func TestStandaloneControllerClient(t *testing.T) {
	cases := []struct {
		name    string
		toml    string
		write   bool
		wantNil bool
	}{
		{name: "no-city-toml", write: false, wantNil: true},
		{name: "no-api-section", toml: "name = \"t\"\n", write: true, wantNil: true},
		{name: "api-port-zero", toml: "name = \"t\"\n[api]\nport = 0\n", write: true, wantNil: true},
		{name: "loopback-port", toml: "name = \"t\"\n[api]\nport = 8080\n", write: true, wantNil: false},
		{name: "explicit-localhost", toml: "name = \"t\"\n[api]\nport = 8080\nbind = \"localhost\"\n", write: true, wantNil: false},
		{name: "non-loopback-no-mutations", toml: "name = \"t\"\n[api]\nport = 8080\nbind = \"0.0.0.0\"\n", write: true, wantNil: true},
		{name: "non-loopback-allow-mutations", toml: "name = \"t\"\n[api]\nport = 8080\nbind = \"0.0.0.0\"\nallow_mutations = true\n", write: true, wantNil: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if tc.write {
				writeCityTOMLForRoute(t, dir, tc.toml)
			}
			got := standaloneControllerClient(dir)
			if tc.wantNil && got != nil {
				t.Fatalf("standaloneControllerClient = non-nil, want nil")
			}
			if !tc.wantNil && got == nil {
				t.Fatalf("standaloneControllerClient = nil, want non-nil")
			}
		})
	}
}

// TestAPIClientRouting covers identity-based endpoint selection, supervisor
// grant requirements, legacy standalone routing and the GC_NO_API escape hatch.
func TestAPIClientRouting(t *testing.T) {
	t.Setenv("GC_HOME", t.TempDir())
	sentinel := api.NewClient("http://supervisor.sentinel:1")

	restore := func(alive func(string) controllerIdentityReply, sup func(string) *api.Client) {
		apiRouteControllerIdentityHook = alive
		apiRouteSupervisorClientHook = sup
	}
	origAlive, origSup := apiRouteControllerIdentityHook, apiRouteSupervisorClientHook
	t.Cleanup(func() { restore(origAlive, origSup) })

	t.Run("controller-alive-no-api-port-returns-nil", func(t *testing.T) {
		// General commands have a local fallback, so apiClient returns nil here
		// (no global supervisor fall-through).
		t.Setenv("GC_NO_API", "")
		restore(func(string) controllerIdentityReply { return controllerIdentityReply{PID: 4242} }, func(string) *api.Client { return sentinel })
		dir := writeCityTOMLForRoute(t, t.TempDir(), "name = \"t\"\n")
		if got := apiClient(dir); got != nil {
			t.Fatalf("apiClient = %p, want nil (general commands use local fallback)", got)
		}
	})

	t.Run("controller-alive-with-api-port-uses-standalone", func(t *testing.T) {
		t.Setenv("GC_NO_API", "")
		restore(func(string) controllerIdentityReply { return controllerIdentityReply{PID: 4242} }, func(string) *api.Client { return sentinel })
		dir := writeCityTOMLForRoute(t, t.TempDir(), "name = \"t\"\n[api]\nport = 8080\n")
		got := apiClient(dir)
		if got == nil {
			t.Fatalf("apiClient = nil, want standalone client")
		}
		if got == sentinel {
			t.Fatalf("apiClient returned supervisor sentinel, want standalone client (no regression)")
		}
	})

	t.Run("supervisor-hosted-controller-ignores-city-api-port", func(t *testing.T) {
		t.Setenv("GC_NO_API", "")
		restore(func(string) controllerIdentityReply { return controllerIdentityReply{PID: 4242} }, func(string) *api.Client { return sentinel })
		withAPIRouteHosting(t, 4242, controllerHostingSupervisor)
		dir := writeCityTOMLForRoute(t, t.TempDir(), "name = \"t\"\n[api]\nport = 8080\n")
		if got := apiClient(dir); got != sentinel {
			t.Fatalf("apiClient = %p, want supervisor sentinel %p for supervisor-hosted city despite [api] port", got, sentinel)
		}
	})

	t.Run("grant-gated-supervisor-does-not-route-general-calls", func(t *testing.T) {
		t.Setenv("GC_NO_API", "")
		t.Setenv("GC_HOME", t.TempDir())
		if err := os.MkdirAll(filepath.Dir(supervisor.ConfigPath()), 0o755); err != nil {
			t.Fatalf("create supervisor config directory: %v", err)
		}
		if err := os.WriteFile(supervisor.ConfigPath(), []byte("[supervisor]\nread_auth_required = true\n"), 0o600); err != nil {
			t.Fatalf("write grant-gated supervisor config: %v", err)
		}
		restore(func(string) controllerIdentityReply { return controllerIdentityReply{PID: 4242} }, func(string) *api.Client { return sentinel })
		withAPIRouteHosting(t, 4242, controllerHostingSupervisor)
		dir := writeCityTOMLForRoute(t, t.TempDir(), "name = \"t\"\n[api]\nport = 8080\n")
		if got := apiClient(dir); got != nil {
			t.Fatalf("apiClient = %p, want nil because the supervisor requires grants this CLI client cannot mint", got)
		}
	})

	t.Run("controller-down-uses-supervisor", func(t *testing.T) {
		t.Setenv("GC_NO_API", "")
		restore(func(string) controllerIdentityReply { return controllerIdentityReply{} }, func(string) *api.Client { return sentinel })
		dir := writeCityTOMLForRoute(t, t.TempDir(), "name = \"t\"\n[api]\nport = 8080\n")
		if got := apiClient(dir); got != sentinel {
			t.Fatalf("apiClient = %p, want supervisor sentinel %p", got, sentinel)
		}
	})

	t.Run("escape-hatch-returns-nil", func(t *testing.T) {
		t.Setenv("GC_NO_API", "1")
		restore(func(string) controllerIdentityReply { return controllerIdentityReply{PID: 4242} }, func(string) *api.Client { return sentinel })
		dir := writeCityTOMLForRoute(t, t.TempDir(), "name = \"t\"\n")
		if got := apiClient(dir); got != nil {
			t.Fatalf("apiClient = %p, want nil under GC_NO_API escape hatch", got)
		}
	})
}

// TestMaintenanceAPIClientRoutesToSupervisor preserves the maintenance route
// through the supervisor when no standalone port is configured.
func TestMaintenanceAPIClientRoutesToSupervisor(t *testing.T) {
	sentinel := api.NewClient("http://supervisor.sentinel:1")
	origAlive, origSup := apiRouteControllerIdentityHook, apiRouteSupervisorClientHook
	t.Cleanup(func() {
		apiRouteControllerIdentityHook = origAlive
		apiRouteSupervisorClientHook = origSup
	})

	t.Run("alive-no-api-port-routes-to-supervisor", func(t *testing.T) {
		t.Setenv("GC_NO_API", "")
		apiRouteControllerIdentityHook = func(string) controllerIdentityReply { return controllerIdentityReply{PID: 4242} }
		apiRouteSupervisorClientHook = func(string) *api.Client { return sentinel }
		dir := writeCityTOMLForRoute(t, t.TempDir(), "name = \"t\"\n")
		c, reason := maintenanceAPIClient(dir)
		if c != sentinel {
			t.Fatalf("maintenanceAPIClient client = %p, want supervisor sentinel %p", c, sentinel)
		}
		if reason != "" {
			t.Fatalf("maintenanceAPIClient reason = %q, want empty", reason)
		}
	})

	t.Run("escape-hatch-skips-supervisor", func(t *testing.T) {
		t.Setenv("GC_NO_API", "1")
		apiRouteControllerIdentityHook = func(string) controllerIdentityReply { return controllerIdentityReply{PID: 4242} }
		apiRouteSupervisorClientHook = func(string) *api.Client { return sentinel }
		dir := writeCityTOMLForRoute(t, t.TempDir(), "name = \"t\"\n")
		c, reason := maintenanceAPIClient(dir)
		if c != nil {
			t.Fatalf("maintenanceAPIClient client = %p, want nil under GC_NO_API", c)
		}
		if reason != "escape-hatch" {
			t.Fatalf("maintenanceAPIClient reason = %q, want \"escape-hatch\"", reason)
		}
	})
}

// TestCityStatusAPIClientRoutesToSupervisor is the ra-r9hm6v regression test.
// Before this fix, `gc status` resolved its client through plain apiClient,
// which returns nil whenever the per-city controller socket is alive but the
// city has no standalone [api] port — exactly the shape of a supervisor-
// managed city like a default `gc init`'d city (no [api] section in
// city.toml). That nil forced every `gc status` invocation onto the local
// fallback (open the bead/dolt store, rescan event archives to rebuild store
// health), measured at ~9.5s of CPU on a 26-agent/1.2GB city versus ~0.35s
// for the supervisor's already-cached response. cityStatusAPIClient uses
// statusReadAPIClient to retain that warm route with read-grant policy.
func TestCityStatusAPIClientRoutesToSupervisor(t *testing.T) {
	sentinel := api.NewClient("http://supervisor.sentinel:1")
	origAlive, origSup := apiRouteControllerIdentityHook, apiRouteSupervisorClientHook
	t.Cleanup(func() {
		apiRouteControllerIdentityHook = origAlive
		apiRouteSupervisorClientHook = origSup
	})

	t.Run("alive-no-api-port-routes-to-supervisor", func(t *testing.T) {
		t.Setenv("GC_NO_API", "")
		apiRouteControllerIdentityHook = func(string) controllerIdentityReply { return controllerIdentityReply{PID: 4242} }
		apiRouteSupervisorClientHook = func(string) *api.Client { return sentinel }
		dir := writeCityTOMLForRoute(t, t.TempDir(), "name = \"t\"\n")
		c, reason := cityStatusAPIClient(dir)
		if c != sentinel {
			t.Fatalf("cityStatusAPIClient client = %p, want supervisor sentinel %p (ra-r9hm6v regression: must not fall back to nil/local)", c, sentinel)
		}
		if reason != "" {
			t.Fatalf("cityStatusAPIClient reason = %q, want empty", reason)
		}
	})

	t.Run("escape-hatch-skips-supervisor", func(t *testing.T) {
		t.Setenv("GC_NO_API", "1")
		apiRouteControllerIdentityHook = func(string) controllerIdentityReply { return controllerIdentityReply{PID: 4242} }
		apiRouteSupervisorClientHook = func(string) *api.Client { return sentinel }
		dir := writeCityTOMLForRoute(t, t.TempDir(), "name = \"t\"\n")
		c, reason := cityStatusAPIClient(dir)
		if c != nil {
			t.Fatalf("cityStatusAPIClient client = %p, want nil under GC_NO_API", c)
		}
		if reason != "escape-hatch" {
			t.Fatalf("cityStatusAPIClient reason = %q, want \"escape-hatch\"", reason)
		}
	})

	t.Run("controller-fully-down-still-falls-back-local", func(t *testing.T) {
		// No socket at all (not even a supervisor-managed one): this is the
		// genuine "nothing is running" case, where the local fallback remains
		// correct and necessary — the fix must not paper over it.
		t.Setenv("GC_NO_API", "")
		apiRouteControllerIdentityHook = func(string) controllerIdentityReply { return controllerIdentityReply{} }
		apiRouteSupervisorClientHook = func(string) *api.Client { return nil }
		dir := writeCityTOMLForRoute(t, t.TempDir(), "name = \"t\"\n")
		c, reason := cityStatusAPIClient(dir)
		if c != nil {
			t.Fatalf("cityStatusAPIClient client = %p, want nil when nothing is running", c)
		}
		if reason != "controller-down" {
			t.Fatalf("cityStatusAPIClient reason = %q, want \"controller-down\"", reason)
		}
	})
}

// TestStatusAPIClientsRouteAliveNoAPICityToSupervisor covers the fork's rig
// status client alongside gc status: both use the warm supervisor view for an
// alive supervisor-managed city without a standalone [api] port.
func TestStatusAPIClientsRouteAliveNoAPICityToSupervisor(t *testing.T) {
	sentinel := api.NewClient("http://supervisor.sentinel:1")
	origAlive, origSup := apiRouteControllerIdentityHook, apiRouteSupervisorClientHook
	t.Cleanup(func() {
		apiRouteControllerIdentityHook = origAlive
		apiRouteSupervisorClientHook = origSup
	})

	t.Run("city-and-rig-status-use-the-warm-supervisor-view", func(t *testing.T) {
		t.Setenv("GC_NO_API", "")
		apiRouteControllerIdentityHook = func(string) controllerIdentityReply { return controllerIdentityReply{PID: 4242} }
		apiRouteSupervisorClientHook = func(string) *api.Client { return sentinel }
		dir := writeCityTOMLForRoute(t, t.TempDir(), "name = \"t\"\n")

		for name, clientForStatus := range map[string]func(string) (*api.Client, string){
			"city": cityStatusAPIClient,
			"rig":  rigStatusAPIClient,
		} {
			t.Run(name, func(t *testing.T) {
				got, reason := clientForStatus(dir)
				if got != sentinel || reason != "" {
					t.Fatalf("status client = (%p, %q), want supervisor client (%p, \"\")", got, reason, sentinel)
				}
			})
		}
	})

	t.Run("GC_NO_API-remains-an-escape-hatch", func(t *testing.T) {
		t.Setenv("GC_NO_API", "1")
		apiRouteControllerIdentityHook = func(string) controllerIdentityReply { return controllerIdentityReply{PID: 4242} }
		apiRouteSupervisorClientHook = func(string) *api.Client {
			t.Fatal("supervisor client must not bypass GC_NO_API")
			return nil
		}
		dir := writeCityTOMLForRoute(t, t.TempDir(), "name = \"t\"\n")

		for name, clientForStatus := range map[string]func(string) (*api.Client, string){
			"city": cityStatusAPIClient,
			"rig":  rigStatusAPIClient,
		} {
			t.Run(name, func(t *testing.T) {
				got, reason := clientForStatus(dir)
				if got != nil || reason != "escape-hatch" {
					t.Fatalf("status client = (%p, %q), want (nil, escape-hatch)", got, reason)
				}
			})
		}
	})

	t.Run("unavailable-supervisor-preserves-local-fallback", func(t *testing.T) {
		t.Setenv("GC_NO_API", "")
		apiRouteControllerIdentityHook = func(string) controllerIdentityReply { return controllerIdentityReply{PID: 4242} }
		apiRouteSupervisorClientHook = func(string) *api.Client { return nil }
		dir := writeCityTOMLForRoute(t, t.TempDir(), "name = \"t\"\n")

		got, reason := statusReadAPIClient(dir)
		if got != nil || reason != "controller-down" {
			t.Fatalf("status client = (%p, %q), want (nil, controller-down)", got, reason)
		}
	})
}

func TestSessionAPIClientsRouteAliveNoAPICityToSupervisor(t *testing.T) {
	sentinel := api.NewClient("http://supervisor.sentinel:1")
	origAlive, origSup := apiRouteControllerIdentityHook, apiRouteSupervisorClientHook
	t.Cleanup(func() {
		apiRouteControllerIdentityHook = origAlive
		apiRouteSupervisorClientHook = origSup
	})

	t.Run("session-list-and-peek-use-the-warm-supervisor-view", func(t *testing.T) {
		t.Setenv("GC_NO_API", "")
		apiRouteControllerIdentityHook = func(string) controllerIdentityReply { return controllerIdentityReply{PID: 4242} }
		apiRouteSupervisorClientHook = func(string) *api.Client { return sentinel }
		dir := writeCityTOMLForRoute(t, t.TempDir(), "name = \"t\"\n")

		for name, clientForSession := range map[string]func(string) (*api.Client, string){
			"list": sessionListAPIClient,
			"peek": sessionPeekAPIClient,
		} {
			t.Run(name, func(t *testing.T) {
				got, reason := clientForSession(dir)
				if got != sentinel || reason != "" {
					t.Fatalf("session client = (%p, %q), want supervisor client (%p, \"\")", got, reason, sentinel)
				}
			})
		}
	})

	t.Run("GC_NO_API-remains-an-escape-hatch-for-session", func(t *testing.T) {
		t.Setenv("GC_NO_API", "1")
		apiRouteControllerIdentityHook = func(string) controllerIdentityReply { return controllerIdentityReply{PID: 4242} }
		apiRouteSupervisorClientHook = func(string) *api.Client {
			t.Fatal("supervisor client must not bypass GC_NO_API")
			return nil
		}
		dir := writeCityTOMLForRoute(t, t.TempDir(), "name = \"t\"\n")

		for name, clientForSession := range map[string]func(string) (*api.Client, string){
			"list": sessionListAPIClient,
			"peek": sessionPeekAPIClient,
		} {
			t.Run(name, func(t *testing.T) {
				got, reason := clientForSession(dir)
				if got != nil || reason != "escape-hatch" {
					t.Fatalf("session client = (%p, %q), want (nil, escape-hatch)", got, reason)
				}
			})
		}
	})
}

func withAPIRouteHosting(t *testing.T, pid int, mode controllerHostingMode) {
	t.Helper()
	previous := apiRouteControllerIdentityHook
	apiRouteControllerIdentityHook = func(string) controllerIdentityReply { return controllerIdentityReply{PID: pid, HostingMode: mode} }
	t.Cleanup(func() { apiRouteControllerIdentityHook = previous })
}

func TestAPIHostingRoutingEdges(t *testing.T) {
	t.Setenv("GC_HOME", t.TempDir())
	t.Setenv("GC_NO_API", "")
	original := apiRouteSupervisorClientHook
	t.Cleanup(func() { apiRouteSupervisorClientHook = original })
	for _, tc := range []struct {
		name           string
		mode           controllerHostingMode
		port           bool
		prefer         bool
		available      bool
		wantSupervisor bool
	}{
		{"supervisor without port", controllerHostingSupervisor, false, false, true, true},
		{"supervisor unavailable ignores port", controllerHostingSupervisor, true, false, false, false},
		{"standalone never falls through", controllerHostingStandalone, false, true, true, false},
		{"legacy preferred fallback", controllerHostingUnknown, false, true, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			withAPIRouteHosting(t, 4242, tc.mode)
			sentinel := api.NewClient("http://supervisor.sentinel:1")
			calls := 0
			apiRouteSupervisorClientHook = func(string) *api.Client {
				calls++
				if tc.available {
					return sentinel
				}
				return nil
			}
			body := "name = \"test\"\n"
			if tc.port {
				body += "[api]\nport = 8080\n"
			}
			dir := writeCityTOMLForRoute(t, t.TempDir(), body)
			got := routeAPIClient(dir, tc.prefer)
			if tc.wantSupervisor && got != sentinel {
				t.Fatalf("got %p, want supervisor %p", got, sentinel)
			}
			if !tc.wantSupervisor && got != nil {
				t.Fatalf("got %p, want local fallback", got)
			}
			if calls > 1 || (tc.mode == controllerHostingStandalone && calls != 0) {
				t.Fatalf("supervisor consulted %d times", calls)
			}
		})
	}
}

func TestPreferredSupervisorClientRequiresGrants(t *testing.T) {
	t.Setenv("GC_NO_API", "")
	t.Setenv("GC_HOME", t.TempDir())
	if err := os.MkdirAll(filepath.Dir(supervisor.ConfigPath()), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(supervisor.ConfigPath(), []byte("[supervisor]\nread_auth_required = true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	withAPIRouteHosting(t, 4242, controllerHostingSupervisor)
	old := apiRouteSupervisorClientHook
	t.Cleanup(func() { apiRouteSupervisorClientHook = old })
	apiRouteSupervisorClientHook = func(string) *api.Client {
		t.Fatal("grant-protected supervisor was contacted without a grant")
		return nil
	}
	dir := writeCityTOMLForRoute(t, t.TempDir(), "name = \"test\"\n")
	if c, reason := statusReadAPIClient(dir); c != nil || reason != "grant-required" {
		t.Fatalf("preferred client=%p reason=%q, want local grant-required", c, reason)
	}
}

func TestStatusReadRoutingAllowsWriteOnlyGrants(t *testing.T) {
	t.Setenv("GC_NO_API", "")
	t.Setenv("GC_HOME", t.TempDir())
	for _, key := range []string{"GC_CITY_READ_REQUIRED", "GC_CITY_READ_PUBKEY", "GC_CITY_WRITE_REQUIRED", "GC_CITY_WRITE_PUBKEY"} {
		t.Setenv(key, "")
	}
	if err := os.MkdirAll(filepath.Dir(supervisor.ConfigPath()), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(supervisor.ConfigPath(), []byte("[supervisor]\nwrite_auth_required = true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := apiRouteSupervisorClientHook
	t.Cleanup(func() { apiRouteSupervisorClientHook = old })
	sentinel := api.NewClient("http://supervisor.sentinel:1")
	apiRouteSupervisorClientHook = func(string) *api.Client { return sentinel }
	dir := writeCityTOMLForRoute(t, t.TempDir(), "name = \"test\"\n")
	for _, tc := range []struct {
		name string
		pid  int
		mode controllerHostingMode
	}{
		{"hosted", 4242, controllerHostingSupervisor},
		{"controller down", 0, controllerHostingUnknown},
		{"legacy fallback", 4242, controllerHostingUnknown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			withAPIRouteHosting(t, tc.pid, tc.mode)
			for name, resolve := range map[string]func(string) (*api.Client, string){"status read": statusReadAPIClient, "city status": cityStatusAPIClient} {
				if got, reason := resolve(dir); got != sentinel || reason != "" {
					t.Errorf("%s = (%p, %q), want warm read client %p", name, got, reason, sentinel)
				}
			}
			if got := apiClient(dir); got != nil {
				t.Fatalf("general command bypassed write grant: %p", got)
			}
		})
	}
}
