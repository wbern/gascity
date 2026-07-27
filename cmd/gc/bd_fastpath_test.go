package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/beads"
)

const testFastpathCity = "testcity"

// stubEarlyBdController serves the single bead endpoint the fast path reads and
// points earlyBdAPIClient at it for the duration of the test.
func stubEarlyBdController(t *testing.T, handler http.HandlerFunc) {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	previous := earlyBdAPIClient
	earlyBdAPIClient = func(string) *api.Client {
		return api.NewCityScopedClient(server.URL, testFastpathCity)
	}
	t.Cleanup(func() { earlyBdAPIClient = previous })
}

func beadResponseHandler(t *testing.T, wantPath string, bead beads.Bead) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != wantPath {
			t.Errorf("request path = %q, want %q", r.URL.Path, wantPath)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(bead) //nolint:errcheck // test handler
	}
}

func TestBdFastpathIsOptIn(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  string
		want bool
	}{
		{name: "unset defaults off"},
		{name: "zero", raw: "0"},
		{name: "unknown value fails closed", raw: "maybe"},
		{name: "empty after trim", raw: "   "},
		{name: "one", raw: "1", want: true},
		{name: "true", raw: "true", want: true},
		{name: "yes uppercase", raw: "YES", want: true},
		{name: "padded", raw: " 1 ", want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := bdFastpathEnabled(tc.raw); got != tc.want {
				t.Fatalf("bdFastpathEnabled(%q) = %t, want %t", tc.raw, got, tc.want)
			}
		})
	}
}

func TestEarlyBdShowIDAcceptsOnlyTheJSONPointLookup(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{name: "json show", args: []string{"bd", "show", "ab-1234", "--json"}, want: "ab-1234"},
		{name: "no json flag", args: []string{"bd", "show", "ab-1234"}},
		{name: "extra flag", args: []string{"bd", "show", "ab-1234", "--json", "--limit", "1"}},
		{name: "different verb", args: []string{"bd", "list", "--json", "--status", "open"}},
		{name: "mutation", args: []string{"bd", "update", "ab-1234", "--json"}},
		{name: "close", args: []string{"bd", "close", "ab-1234", "--json"}},
		{name: "explicit city scope", args: []string{"bd", "--city", "other", "show", "ab-1234", "--json"}},
		{name: "explicit rig scope", args: []string{"bd", "--rig", "other", "show", "ab-1234", "--json"}},
		{name: "city scope assignment form", args: []string{"bd", "--city=other", "show", "ab-1234"}},
		{name: "flag shaped id", args: []string{"bd", "show", "--all", "--json"}},
		{name: "padded id", args: []string{"bd", "show", " ab-1234", "--json"}},
		{name: "id with space", args: []string{"bd", "show", "ab 1234", "--json"}},
		{name: "empty id", args: []string{"bd", "show", "", "--json"}},
		{name: "not bd", args: []string{"status", "show", "ab-1234", "--json"}},
		{name: "no args", args: []string{"bd"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := earlyBdShowID(tc.args)
			if ok != (tc.want != "") {
				t.Fatalf("earlyBdShowID(%q) ok = %t, want %t", tc.args, ok, tc.want != "")
			}
			if got != tc.want {
				t.Fatalf("earlyBdShowID(%q) = %q, want %q", tc.args, got, tc.want)
			}
		})
	}
}

func TestHasForeignDoltEndpointAdmitsOnlyTheProjectionGCStamped(t *testing.T) {
	for _, tc := range []struct {
		name          string
		host, port    string
		canonicalHost string
		canonicalPort string
		want          bool
	}{
		{name: "no inherited endpoint"},
		{name: "matches canonical projection", host: "127.0.0.1", port: "3306", canonicalHost: "127.0.0.1", canonicalPort: "3306"},
		{name: "port only projection", port: "3306", canonicalPort: "3306"},
		{name: "host differs", host: "db.example.test", port: "3306", canonicalHost: "127.0.0.1", canonicalPort: "3306", want: true},
		{name: "port differs", host: "127.0.0.1", port: "3307", canonicalHost: "127.0.0.1", canonicalPort: "3306", want: true},
		{name: "no provenance stamped", host: "127.0.0.1", port: "3306", want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("GC_DOLT_HOST", tc.host)
			t.Setenv("GC_DOLT_PORT", tc.port)
			t.Setenv(canonicalDoltHostEnv, tc.canonicalHost)
			t.Setenv(canonicalDoltPortEnv, tc.canonicalPort)
			if got := hasForeignDoltEndpoint(); got != tc.want {
				t.Fatalf("hasForeignDoltEndpoint() = %t, want %t", got, tc.want)
			}
		})
	}
}

func TestEarlyBdCityPathRequiresAnAbsolutePathInTheEnvironment(t *testing.T) {
	for _, tc := range []struct {
		name     string
		cityPath string
		city     string
		want     string
	}{
		{name: "city path wins", cityPath: "/cities/alpha", city: "/cities/beta", want: "/cities/alpha"},
		{name: "absolute city", city: "/cities/beta", want: "/cities/beta"},
		{name: "registered name is not enough", city: "beta"},
		{name: "relative path is not enough", cityPath: "./alpha"},
		{name: "unset"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("GC_CITY_PATH", tc.cityPath)
			t.Setenv("GC_CITY", tc.city)
			if got := earlyBdCityPath(); got != tc.want {
				t.Fatalf("earlyBdCityPath() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestTryEarlyBdReadServesTheShowLookupFromTheController(t *testing.T) {
	t.Setenv(bdFastpathEnv, "1")
	t.Setenv("GC_CITY_PATH", "/cities/"+testFastpathCity)
	t.Setenv("GC_BEADS_SCOPE_ROOT", "/cities/"+testFastpathCity)
	t.Setenv("GC_DOLT_HOST", "")
	t.Setenv("GC_DOLT_PORT", "")

	stubEarlyBdController(t, beadResponseHandler(t, "/v0/city/"+testFastpathCity+"/bead/ab-1234", beads.Bead{
		ID:     "ab-1234",
		Title:  "example",
		Status: "open",
		Type:   "task",
	}))

	var stdout, stderr bytes.Buffer
	code, handled := tryEarlyBdRead([]string{"bd", "show", "ab-1234", "--json"}, &stdout, &stderr)
	if !handled || code != 0 {
		t.Fatalf("tryEarlyBdRead() = (%d, %t), want (0, true); stderr=%q", code, handled, stderr.String())
	}

	var got []beads.Bead
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("decode stdout %q: %v", stdout.String(), err)
	}
	if len(got) != 1 || got[0].ID != "ab-1234" {
		t.Fatalf("stdout = %q, want a single-element array holding ab-1234", stdout.String())
	}
	if !strings.HasSuffix(stdout.String(), "]\n") {
		t.Fatalf("stdout = %q, want a newline-terminated JSON array", stdout.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
}

func TestTryEarlyBdReadFallsThroughWithAnUntouchedStdout(t *testing.T) {
	unreachable := func(*testing.T) http.HandlerFunc {
		return func(w http.ResponseWriter, _ *http.Request) {
			t.Error("fast path dispatched a request it should have declined")
			w.WriteHeader(http.StatusInternalServerError)
		}
	}

	for _, tc := range []struct {
		name    string
		env     map[string]string
		args    []string
		handler http.HandlerFunc
	}{
		{
			name:    "not opted in",
			env:     map[string]string{bdFastpathEnv: ""},
			args:    []string{"bd", "show", "ab-1234", "--json"},
			handler: unreachable(t),
		},
		{
			name:    "shape outside the allowlist",
			args:    []string{"bd", "list", "--json"},
			handler: unreachable(t),
		},
		{
			name:    "foreign dolt endpoint",
			env:     map[string]string{"GC_DOLT_HOST": "db.example.test"},
			args:    []string{"bd", "show", "ab-1234", "--json"},
			handler: unreachable(t),
		},
		{
			name:    "no city in the environment",
			env:     map[string]string{"GC_CITY_PATH": "", "GC_CITY": ""},
			args:    []string{"bd", "show", "ab-1234", "--json"},
			handler: unreachable(t),
		},
		{
			name:    "rig scoped invocation",
			env:     map[string]string{"GC_BEADS_SCOPE_ROOT": "/repos/some-rig"},
			args:    []string{"bd", "show", "ab-1234", "--json"},
			handler: unreachable(t),
		},
		{
			name:    "unprojected bead scope",
			env:     map[string]string{"GC_BEADS_SCOPE_ROOT": ""},
			args:    []string{"bd", "show", "ab-1234", "--json"},
			handler: unreachable(t),
		},
		{
			name: "controller reports the bead missing",
			args: []string{"bd", "show", "ab-1234", "--json"},
			handler: func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/problem+json")
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(`{"title":"not found"}`)) //nolint:errcheck // test handler
			},
		},
		{
			name: "controller fails",
			args: []string{"bd", "show", "ab-1234", "--json"},
			handler: func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/problem+json")
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = w.Write([]byte(`{"title":"controller unavailable"}`)) //nolint:errcheck // test handler
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(bdFastpathEnv, "1")
			t.Setenv("GC_CITY_PATH", "/cities/"+testFastpathCity)
			t.Setenv("GC_BEADS_SCOPE_ROOT", "/cities/"+testFastpathCity)
			t.Setenv("GC_CITY", "")
			t.Setenv("GC_DOLT_HOST", "")
			t.Setenv("GC_DOLT_PORT", "")
			t.Setenv(canonicalDoltHostEnv, "")
			t.Setenv(canonicalDoltPortEnv, "")
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			stubEarlyBdController(t, tc.handler)

			var stdout, stderr bytes.Buffer
			code, handled := tryEarlyBdRead(tc.args, &stdout, &stderr)
			if handled || code != 0 {
				t.Fatalf("tryEarlyBdRead() = (%d, %t), want (0, false)", code, handled)
			}
			if stdout.Len() != 0 {
				t.Fatalf("stdout = %q, want empty so the ordinary path owns the output", stdout.String())
			}
		})
	}
}

func TestTryEarlyBdReadFallsThroughWithoutAControllerRoute(t *testing.T) {
	t.Setenv(bdFastpathEnv, "1")
	t.Setenv("GC_CITY_PATH", "/cities/"+testFastpathCity)
	t.Setenv("GC_BEADS_SCOPE_ROOT", "/cities/"+testFastpathCity)
	t.Setenv("GC_DOLT_HOST", "")
	t.Setenv("GC_DOLT_PORT", "")

	previous := earlyBdAPIClient
	earlyBdAPIClient = func(string) *api.Client { return nil }
	t.Cleanup(func() { earlyBdAPIClient = previous })

	var stdout, stderr bytes.Buffer
	if code, handled := tryEarlyBdRead([]string{"bd", "show", "ab-1234", "--json"}, &stdout, &stderr); handled || code != 0 {
		t.Fatalf("tryEarlyBdRead() = (%d, %t), want (0, false)", code, handled)
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q, want empty", stdout.String())
	}
}

// TestEarlyBdShowOmitsFieldsGascityDoesNotModel pins the one behavioral
// difference between the fast path and the bd read passthrough: the response
// carries gascity's modeled bead, so bd fields gascity deliberately does not
// model are absent. internal/beads.TestCorpusFieldsAreModeledOrExplicitlyIgnored
// is the drift detector for that ignore set; this test states the consequence
// at the gc bd boundary, which is why the fast path is opt-in.
func TestEarlyBdShowOmitsFieldsGascityDoesNotModel(t *testing.T) {
	t.Setenv(bdFastpathEnv, "1")
	t.Setenv("GC_CITY_PATH", "/cities/"+testFastpathCity)
	t.Setenv("GC_BEADS_SCOPE_ROOT", "/cities/"+testFastpathCity)
	t.Setenv("GC_DOLT_HOST", "")
	t.Setenv("GC_DOLT_PORT", "")

	stubEarlyBdController(t, beadResponseHandler(t, "/v0/city/"+testFastpathCity+"/bead/ab-1234", beads.Bead{
		ID:     "ab-1234",
		Title:  "example",
		Status: "open",
		Type:   "task",
	}))

	var stdout, stderr bytes.Buffer
	if code, handled := tryEarlyBdRead([]string{"bd", "show", "ab-1234", "--json"}, &stdout, &stderr); !handled || code != 0 {
		t.Fatalf("tryEarlyBdRead() = (%d, %t), want (0, true)", code, handled)
	}

	var rows []map[string]json.RawMessage
	if err := json.Unmarshal(stdout.Bytes(), &rows); err != nil {
		t.Fatalf("decode stdout: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}

	modeled := map[string]bool{}
	for _, name := range beadJSONFieldNames() {
		modeled[name] = true
	}
	for field := range rows[0] {
		if !modeled[field] {
			t.Errorf("fast path emitted %q, which beads.Bead does not model", field)
		}
	}

	// Fields bd emits that gascity does not model, and which the fast path
	// therefore cannot return. Changing this set is a deliberate contract
	// change, not an incidental one.
	unmodeled := []string{"comment_count", "created_by", "dependency_count", "dependent_count", "owner", "started_at"}
	for _, field := range unmodeled {
		if modeled[field] {
			t.Errorf("beads.Bead now models %q; the fast path no longer omits it — update this contract and the opt-in rationale", field)
		}
	}
}

// beadJSONFieldNames returns the wire field names beads.Bead marshals, derived
// by reflection so it stays in sync as the struct evolves.
func beadJSONFieldNames() []string {
	tp := reflect.TypeOf(beads.Bead{})
	names := make([]string, 0, tp.NumField())
	for i := 0; i < tp.NumField(); i++ {
		name := strings.Split(tp.Field(i).Tag.Get("json"), ",")[0]
		if name != "" && name != "-" {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

func TestRunServesTheShowLookupBeforeBuildingTheCommandTree(t *testing.T) {
	t.Setenv(bdFastpathEnv, "1")
	t.Setenv("GC_CITY_PATH", "/cities/"+testFastpathCity)
	t.Setenv("GC_BEADS_SCOPE_ROOT", "/cities/"+testFastpathCity)
	t.Setenv("GC_DOLT_HOST", "")
	t.Setenv("GC_DOLT_PORT", "")

	stubEarlyBdController(t, beadResponseHandler(t, "/v0/city/"+testFastpathCity+"/bead/ab-1234", beads.Bead{
		ID:     "ab-1234",
		Title:  "example",
		Status: "open",
		Type:   "task",
	}))

	var stdout bytes.Buffer
	if code := run([]string{"bd", "show", "ab-1234", "--json"}, &stdout, io.Discard); code != 0 {
		t.Fatalf("run() = %d, want 0", code)
	}
	if !strings.Contains(stdout.String(), `"ab-1234"`) {
		t.Fatalf("stdout = %q, want the controller-served bead", stdout.String())
	}
}
