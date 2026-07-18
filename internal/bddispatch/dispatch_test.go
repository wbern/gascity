package bddispatch

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/bdshim"
)

// TestDispatchViaAPICreate proves `bd create` routes to POST /v0/beads with the
// parsed fields and renders the created bead id like raw bd.
func TestDispatchViaAPICreate(t *testing.T) {
	var gotMethod, gotPath string
	var gotBody map[string]any
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		gotBody = nil
		_ = json.NewDecoder(r.Body).Decode(&gotBody) //nolint:errcheck
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		title, _ := gotBody["title"].(string)
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "gcg-9", "title": title}) //nolint:errcheck
	}))
	defer ts.Close()
	client := api.NewCityScopedClient(ts.URL, "alpha")

	var out, errb bytes.Buffer
	if code := DispatchViaAPI(client, "create", []string{"my task", "--type", "task", "--label", "x"}, &out, &errb); code != 0 {
		t.Fatalf("create via API: code=%d err=%s", code, errb.String())
	}
	if gotMethod != http.MethodPost || gotPath != "/v0/city/alpha/beads" {
		t.Fatalf("create -> %s %s, want POST /v0/city/alpha/beads", gotMethod, gotPath)
	}
	if gotBody["title"] != "my task" {
		t.Fatalf("create body title = %v, want 'my task'", gotBody["title"])
	}
	if !strings.Contains(out.String(), "Created bead: gcg-9") {
		t.Fatalf("create output = %q, want 'Created bead: gcg-9'", out.String())
	}
}

// TestDispatchViaAPIMol proves `bd mol current|progress <id>` routes to
// GET /beads/graph/{id} and renders step status indicators / progress from the
// returned topology (the routed source reaches SQLite-resident steps).
func TestDispatchViaAPIMol(t *testing.T) {
	graphJSON := map[string]any{
		"root": map[string]any{"id": "gcg-1", "title": "workflow", "status": "open"},
		"beads": []map[string]any{
			{"id": "gcg-1", "title": "workflow", "status": "open"},
			{"id": "gcg-2", "title": "step one", "status": "closed"},
			{"id": "gcg-3", "title": "step two", "status": "in_progress"},
		},
		"deps": []map[string]any{},
	}
	newServer := func() *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(graphJSON) //nolint:errcheck
		}))
	}

	t.Run("current", func(t *testing.T) {
		var gotMethod, gotPath string
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotMethod, gotPath = r.Method, r.URL.Path
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(graphJSON) //nolint:errcheck
		}))
		defer ts.Close()
		client := api.NewCityScopedClient(ts.URL, "alpha")
		var out, errb bytes.Buffer
		if code := DispatchViaAPI(client, "mol", []string{"current", "gcg-1"}, &out, &errb); code != 0 {
			t.Fatalf("mol current: code=%d err=%s", code, errb.String())
		}
		if gotMethod != http.MethodGet || gotPath != "/v0/city/alpha/beads/graph/gcg-1" {
			t.Fatalf("mol -> %s %s, want GET /v0/city/alpha/beads/graph/gcg-1", gotMethod, gotPath)
		}
		o := out.String()
		if !strings.Contains(o, "[done] gcg-2") || !strings.Contains(o, "[current] gcg-3") {
			t.Fatalf("mol current render = %q, want [done] gcg-2 + [current] gcg-3 (root excluded)", o)
		}
		if strings.Contains(o, "gcg-1 workflow\n") && strings.Contains(o, "[") && strings.Contains(o, "] gcg-1") {
			t.Fatalf("mol current rendered the root as a step: %q", o)
		}
	})

	t.Run("progress", func(t *testing.T) {
		ts := newServer()
		defer ts.Close()
		client := api.NewCityScopedClient(ts.URL, "alpha")
		var out, errb bytes.Buffer
		if code := DispatchViaAPI(client, "mol", []string{"progress", "gcg-1"}, &out, &errb); code != 0 {
			t.Fatalf("mol progress: code=%d err=%s", code, errb.String())
		}
		if !strings.Contains(out.String(), "1/2 steps complete (50%)") {
			t.Fatalf("mol progress render = %q, want 1/2 steps complete (50%%)", out.String())
		}
	})
}

// TestDispatchViaAPIQueryEphemeral proves `bd query --json 'ephemeral=true AND
// ...'` routes to GET /beads/ephemeral with the parsed filters and renders the
// wisp rows as a JSON array (like raw `bd query`).
func TestDispatchViaAPIQueryEphemeral(t *testing.T) {
	if !bdshim.QueryRoutingEnabled {
		t.Skip("v1: bd query routing is disabled until GET /beads/ephemeral is ported to this fork")
	}
	var gotMethod, gotPath, gotQuery string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath, gotQuery = r.Method, r.URL.Path, r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck
			"items": []map[string]any{{"id": "gcg-3", "title": "hb", "ephemeral": true}},
			"total": 1,
		})
	}))
	defer ts.Close()
	client := api.NewCityScopedClient(ts.URL, "alpha")

	var out, errb bytes.Buffer
	if code := DispatchViaAPI(client, "query", []string{"--json", "ephemeral=true AND status=open", "--limit", "0"}, &out, &errb); code != 0 {
		t.Fatalf("query via API: code=%d err=%s", code, errb.String())
	}
	if gotMethod != http.MethodGet || gotPath != "/v0/city/alpha/beads/ephemeral" {
		t.Fatalf("query -> %s %s, want GET /v0/city/alpha/beads/ephemeral", gotMethod, gotPath)
	}
	if !strings.Contains(gotQuery, "status=open") {
		t.Fatalf("query params = %q, want status=open", gotQuery)
	}
	if !strings.Contains(out.String(), "gcg-3") {
		t.Fatalf("query output = %q, want the ephemeral bead gcg-3", out.String())
	}
}

// TestParseQueryEphemeral covers the two in-repo `bd query` ephemeral shapes and
// the predicate/flag forms that must NOT route (closed allowlist).
func TestParseQueryEphemeral(t *testing.T) {
	cases := []struct {
		name string
		args []string
		ok   bool
		want api.EphemeralBeadsOpts
	}{
		{"listEphemeral shape", []string{"query", "--json", "ephemeral=true AND status=open AND label=wisp_type:ping", "--limit", "0"}, true, api.EphemeralBeadsOpts{Status: "open", Label: "wisp_type:ping"}},
		{"work_query literal", []string{"query", "--json", "ephemeral=true AND status=in_progress", "--limit=0"}, true, api.EphemeralBeadsOpts{Status: "in_progress"}},
		{"with --all", []string{"query", "--json", "ephemeral=true", "--all"}, true, api.EphemeralBeadsOpts{All: true}},
		{"missing --json", []string{"query", "ephemeral=true"}, false, api.EphemeralBeadsOpts{}},
		{"non-ephemeral predicate", []string{"query", "--json", "type=bug"}, false, api.EphemeralBeadsOpts{}},
		{"non-bare value", []string{"query", "--json", "ephemeral=true AND status=open OR x"}, false, api.EphemeralBeadsOpts{}},
		{"unknown flag", []string{"query", "--json", "ephemeral=true", "--weird"}, false, api.EphemeralBeadsOpts{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := ParseQueryEphemeral(tc.args)
			if ok != tc.ok {
				t.Fatalf("ParseQueryEphemeral(%v) ok=%v, want %v", tc.args, ok, tc.ok)
			}
			if ok && got != tc.want {
				t.Fatalf("ParseQueryEphemeral(%v) = %+v, want %+v", tc.args, got, tc.want)
			}
		})
	}
}

// TestDispatchViaAPIRoutesVerbs proves the shim's HTTP dispatch maps each routed
// bd verb onto the right city-scoped endpoint, verb, and body — the path a
// worker's bd op takes through the controller in the pure-HTTP redirect.
func TestDispatchViaAPIRoutesVerbs(t *testing.T) {
	var gotMethod, gotPath string
	var gotBody map[string]any
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		gotBody = nil
		_ = json.NewDecoder(r.Body).Decode(&gotBody) //nolint:errcheck
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "ok"}) //nolint:errcheck
	}))
	defer ts.Close()
	client := api.NewCityScopedClient(ts.URL, "alpha")

	var out, errb bytes.Buffer

	if code := DispatchViaAPI(client, "close", []string{"gcg-2"}, &out, &errb); code != 0 {
		t.Fatalf("close via API: code=%d err=%s", code, errb.String())
	}
	if gotMethod != http.MethodPost || gotPath != "/v0/city/alpha/bead/gcg-2/close" {
		t.Fatalf("close -> %s %s, want POST /v0/city/alpha/bead/gcg-2/close", gotMethod, gotPath)
	}

	out.Reset()
	errb.Reset()
	if code := DispatchViaAPI(client, "update", []string{"gcg-2", "--set-metadata", "gc.outcome=pass", "--status", "closed"}, &out, &errb); code != 0 {
		t.Fatalf("update via API: code=%d err=%s", code, errb.String())
	}
	if gotMethod != http.MethodPost || gotPath != "/v0/city/alpha/bead/gcg-2/update" {
		t.Fatalf("update -> %s %s", gotMethod, gotPath)
	}
	if gotBody["status"] != "closed" {
		t.Fatalf("update body status = %v, want closed", gotBody["status"])
	}
	if md, ok := gotBody["metadata"].(map[string]any); !ok || md["gc.outcome"] != "pass" {
		t.Fatalf("update body metadata = %v, want gc.outcome=pass", gotBody["metadata"])
	}

	out.Reset()
	errb.Reset()
	if code := DispatchViaAPI(client, "ready", []string{"--assignee=worker", "--json"}, &out, &errb); code != 0 {
		t.Fatalf("ready via API: code=%d err=%s", code, errb.String())
	}
	if gotMethod != http.MethodGet || gotPath != "/v0/city/alpha/beads/ready" {
		t.Fatalf("ready -> %s %s, want GET /v0/city/alpha/beads/ready", gotMethod, gotPath)
	}
}

// TestDispatchViaAPIList proves `bd list` routes to GET /v0/beads with the
// parsed status/assignee/limit filters — the GUPP-hook AssignedInProgressQuery.
func TestDispatchViaAPIList(t *testing.T) {
	var gotMethod, gotPath, gotQuery string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath, gotQuery = r.Method, r.URL.Path, r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"beads": []any{}}) //nolint:errcheck
	}))
	defer ts.Close()
	client := api.NewCityScopedClient(ts.URL, "alpha")

	var out, errb bytes.Buffer
	if code := DispatchViaAPI(client, "list",
		[]string{"--status", "in_progress", "--assignee=worker", "--json", "--limit", "25"}, &out, &errb); code != 0 {
		t.Fatalf("list via API: code=%d err=%s", code, errb.String())
	}
	if gotMethod != http.MethodGet || gotPath != "/v0/city/alpha/beads" {
		t.Fatalf("list -> %s %s, want GET /v0/city/alpha/beads", gotMethod, gotPath)
	}
	for _, want := range []string{"status=in_progress", "assignee=worker", "limit=25"} {
		if !strings.Contains(gotQuery, want) {
			t.Fatalf("list query %q missing %q", gotQuery, want)
		}
	}
}
