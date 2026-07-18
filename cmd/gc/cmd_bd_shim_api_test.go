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
