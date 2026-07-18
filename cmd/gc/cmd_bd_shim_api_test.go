package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/api"
)

// TestDispatchBdShimVerbViaAPICreate proves `bd create` routes to POST /v0/beads
// with the parsed fields and renders the created bead id like raw bd.
func TestDispatchBdShimVerbViaAPICreate(t *testing.T) {
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
	if code := dispatchBdShimVerbViaAPI(client, "create", []string{"my task", "--type", "task", "--label", "x"}, &out, &errb); code != 0 {
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

// TestDispatchBdShimClaim proves `bd update <id> --claim` routes to
// POST /v0/city/{city}/bead/{id}/claim, conveys the actor, and reproduces the
// `bd update --claim --json` output contract BdStore.Claim parses.
func TestDispatchBdShimClaim(t *testing.T) {
	t.Run("success prints the claimed bead JSON and parses back", func(t *testing.T) {
		var gotMethod, gotPath string
		var gotBody map[string]any
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotMethod, gotPath = r.Method, r.URL.Path
			_ = json.NewDecoder(r.Body).Decode(&gotBody) //nolint:errcheck
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"claimed": true,
				"bead":    map[string]any{"id": "gcg-2", "title": "review", "status": "in_progress", "assignee": "reviewer-1", "issue_type": "task"},
			}) //nolint:errcheck
		}))
		defer ts.Close()
		client := api.NewCityScopedClient(ts.URL, "alpha")

		var out, errb bytes.Buffer
		code, handled := dispatchBdShimClaim(client, "gcg-2", "reviewer-1", &out, &errb)
		if !handled || code != 0 {
			t.Fatalf("claim: handled=%v code=%d err=%s", handled, code, errb.String())
		}
		if gotMethod != http.MethodPost || gotPath != "/v0/city/alpha/bead/gcg-2/claim" {
			t.Fatalf("claim -> %s %s, want POST /v0/city/alpha/bead/gcg-2/claim", gotMethod, gotPath)
		}
		if gotBody["actor"] != "reviewer-1" {
			t.Fatalf("claim body actor = %v, want reviewer-1", gotBody["actor"])
		}
		// The emitted JSON must be a bd-shaped issue array (id/status/assignee
		// under bd's wire keys) so gc hook's BdStore.Claim parser consumes it.
		// The parser-level round-trip is proven in the beads package
		// (TestParseBDMutationBeadParsesMarshaledBead).
		var parsed []struct {
			ID       string `json:"id"`
			Status   string `json:"status"`
			Assignee string `json:"assignee"`
		}
		if err := json.Unmarshal(out.Bytes(), &parsed); err != nil {
			t.Fatalf("claim output is not a JSON issue array: %v (%q)", err, out.String())
		}
		if len(parsed) != 1 || parsed[0].ID != "gcg-2" || parsed[0].Assignee != "reviewer-1" || parsed[0].Status != "in_progress" {
			t.Fatalf("claim output = %+v, want one in_progress bead gcg-2 held by reviewer-1", parsed)
		}
	})

	t.Run("lost race emits a conflict message bd recognizes", func(t *testing.T) {
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"claimed": false,
				"bead":    map[string]any{"id": "gcg-2", "status": "in_progress", "assignee": "reviewer-9", "issue_type": "task"},
			}) //nolint:errcheck
		}))
		defer ts.Close()
		client := api.NewCityScopedClient(ts.URL, "alpha")

		var out, errb bytes.Buffer
		code, handled := dispatchBdShimClaim(client, "gcg-2", "reviewer-1", &out, &errb)
		if !handled || code != 1 {
			t.Fatalf("lost race: handled=%v code=%d, want handled/1", handled, code)
		}
		if !strings.Contains(strings.ToLower(errb.String()), "already claimed by reviewer-9") {
			t.Fatalf("conflict message = %q, want 'already claimed by reviewer-9'", errb.String())
		}
	})

	t.Run("501 signals fallback to real bd", func(t *testing.T) {
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/problem+json")
			w.WriteHeader(http.StatusNotImplemented)
			_ = json.NewEncoder(w).Encode(map[string]any{"title": "Not Implemented", "detail": "cannot claim"}) //nolint:errcheck
		}))
		defer ts.Close()
		client := api.NewCityScopedClient(ts.URL, "alpha")

		var out, errb bytes.Buffer
		_, handled := dispatchBdShimClaim(client, "gcg-2", "reviewer-1", &out, &errb)
		if handled {
			t.Fatalf("501 should signal fallback (handled=false), got handled=true err=%s", errb.String())
		}
	})
}

// TestDispatchBdShimVerbViaAPIMol proves `bd mol current|progress <id>` routes
// to GET /beads/graph/{id} and renders step status indicators / progress from
// the returned topology (the routed source reaches SQLite-resident steps).
func TestDispatchBdShimVerbViaAPIMol(t *testing.T) {
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
		if code := dispatchBdShimVerbViaAPI(client, "mol", []string{"current", "gcg-1"}, &out, &errb); code != 0 {
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
		if code := dispatchBdShimVerbViaAPI(client, "mol", []string{"progress", "gcg-1"}, &out, &errb); code != 0 {
			t.Fatalf("mol progress: code=%d err=%s", code, errb.String())
		}
		if !strings.Contains(out.String(), "1/2 steps complete (50%)") {
			t.Fatalf("mol progress render = %q, want 1/2 steps complete (50%%)", out.String())
		}
	})
}

// TestBdMolRoutable covers the routable read shapes and the forms that must not
// route (other subcommands, omitted id, view flags).
func TestBdMolRoutable(t *testing.T) {
	cases := []struct {
		args []string
		ok   bool
	}{
		{[]string{"current", "gcg-1"}, true},
		{[]string{"progress", "gcg-1"}, true},
		{[]string{"current", "gcg-1", "--json"}, true},
		{[]string{"current"}, false},                            // id omitted (bd infers it)
		{[]string{"pour", "proto"}, false},                      // not a read subcommand
		{[]string{"current", "--for", "agent", "gcg-1"}, false}, // view flag: not routable
		{nil, false},
	}
	for _, tc := range cases {
		if got := bdMolRoutableArgs(tc.args); got != tc.ok {
			t.Errorf("bdMolRoutableArgs(%v) = %v, want %v", tc.args, got, tc.ok)
		}
	}
}

// TestDispatchBdShimVerbViaAPIQueryEphemeral proves `bd query --json
// 'ephemeral=true AND ...'` routes to GET /beads/ephemeral with the parsed
// filters and renders the wisp rows as a JSON array (like raw `bd query`).
func TestDispatchBdShimVerbViaAPIQueryEphemeral(t *testing.T) {
	if !bdQueryRoutingEnabled {
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
	if code := dispatchBdShimVerbViaAPI(client, "query", []string{"--json", "ephemeral=true AND status=open", "--limit", "0"}, &out, &errb); code != 0 {
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

// TestParseBdQueryEphemeral covers the two in-repo `bd query` ephemeral shapes
// and the predicate/flag forms that must NOT route (closed allowlist).
func TestParseBdQueryEphemeral(t *testing.T) {
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
			got, ok := parseBdQueryEphemeral(tc.args)
			if ok != tc.ok {
				t.Fatalf("parseBdQueryEphemeral(%v) ok=%v, want %v", tc.args, ok, tc.ok)
			}
			if ok && got != tc.want {
				t.Fatalf("parseBdQueryEphemeral(%v) = %+v, want %+v", tc.args, got, tc.want)
			}
		})
	}
}

// TestClassifyBdShimVerbQueryRoutes: a mappable ephemeral query routes in both
// phases; an unmappable one refuses under split (would miss SQLite wisps) and
// passes through in the identity phase.
func TestClassifyBdShimVerbQueryRoutes(t *testing.T) {
	routable := []string{"--json", "ephemeral=true AND status=open"}
	unmappable := []string{"--json", "type=bug"}
	if bdQueryRoutingEnabled {
		// v2: a mappable ephemeral query routes in both phases.
		if got := classifyBdShimVerb("query", routable, true); got != bdRoute {
			t.Fatalf("routable query (split) = %v, want bdRoute", got)
		}
		if got := classifyBdShimVerb("query", routable, false); got != bdRoute {
			t.Fatalf("routable query (identity) = %v, want bdRoute", got)
		}
	} else {
		// v1: query never routes (GET /beads/ephemeral is not ported); it passes
		// through in the identity phase and refuses under split, so a
		// SQLite-resident wisp is never silently missed.
		if got := classifyBdShimVerb("query", routable, false); got != bdPassthrough {
			t.Fatalf("routable query (identity, v1) = %v, want bdPassthrough", got)
		}
		if got := classifyBdShimVerb("query", routable, true); got != bdRefuse {
			t.Fatalf("routable query (split, v1) = %v, want bdRefuse", got)
		}
	}
	// The unmappable-query contract is identical regardless of routing.
	if got := classifyBdShimVerb("query", unmappable, true); got != bdRefuse {
		t.Fatalf("unmappable query (split) = %v, want bdRefuse", got)
	}
	if got := classifyBdShimVerb("query", unmappable, false); got != bdPassthrough {
		t.Fatalf("unmappable query (identity) = %v, want bdPassthrough", got)
	}
}

// TestDispatchBdShimVerbViaAPIRoutesVerbs proves the shim's HTTP dispatch maps
// each routed bd verb onto the right city-scoped endpoint, verb, and body — the
// path a worker's bd op takes through the controller in the pure-HTTP redirect.
func TestDispatchBdShimVerbViaAPIRoutesVerbs(t *testing.T) {
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

	if code := dispatchBdShimVerbViaAPI(client, "close", []string{"gcg-2"}, &out, &errb); code != 0 {
		t.Fatalf("close via API: code=%d err=%s", code, errb.String())
	}
	if gotMethod != http.MethodPost || gotPath != "/v0/city/alpha/bead/gcg-2/close" {
		t.Fatalf("close -> %s %s, want POST /v0/city/alpha/bead/gcg-2/close", gotMethod, gotPath)
	}

	out.Reset()
	errb.Reset()
	if code := dispatchBdShimVerbViaAPI(client, "update", []string{"gcg-2", "--set-metadata", "gc.outcome=pass", "--status", "closed"}, &out, &errb); code != 0 {
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
	if code := dispatchBdShimVerbViaAPI(client, "ready", []string{"--assignee=worker", "--json"}, &out, &errb); code != 0 {
		t.Fatalf("ready via API: code=%d err=%s", code, errb.String())
	}
	if gotMethod != http.MethodGet || gotPath != "/v0/city/alpha/beads/ready" {
		t.Fatalf("ready -> %s %s, want GET /v0/city/alpha/beads/ready", gotMethod, gotPath)
	}
}

// TestDispatchBdShimVerbViaAPIList proves `bd list` routes to GET /v0/beads with
// the parsed status/assignee/limit filters — the GUPP-hook AssignedInProgressQuery.
func TestDispatchBdShimVerbViaAPIList(t *testing.T) {
	var gotMethod, gotPath, gotQuery string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath, gotQuery = r.Method, r.URL.Path, r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"beads": []any{}}) //nolint:errcheck
	}))
	defer ts.Close()
	client := api.NewCityScopedClient(ts.URL, "alpha")

	var out, errb bytes.Buffer
	if code := dispatchBdShimVerbViaAPI(client, "list",
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
