package bddispatch

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gastownhall/gascity/internal/beadclient"
)

// TestDispatchGateListQueriesTypeGate pins that a routed `bd gate list` asks the
// controller for gate beads and renders them as bd's JSON array.
func TestDispatchGateListQueriesTypeGate(t *testing.T) {
	var gotPath, gotQuery string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotQuery = r.URL.Path, r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck
			"items": []map[string]any{{
				"id": "gcw-1", "title": "Gate: human", "status": "open",
				"issue_type": "gate", "await_type": "human",
			}},
			"total": 1,
		})
	}))
	defer ts.Close()
	client := beadclient.NewCityScopedClient(ts.URL, "alpha")

	var out, errb bytes.Buffer
	if code := DispatchViaAPI(client, "gate", []string{"list", "--json", "--limit", "50"}, &out, &errb); code != 0 {
		t.Fatalf("gate list: code=%d err=%s", code, errb.String())
	}
	if want := "/v0/city/alpha/beads"; gotPath != want {
		t.Fatalf("path = %s, want %s", gotPath, want)
	}
	if !bytes.Contains([]byte(gotQuery), []byte("type=gate")) {
		t.Fatalf("query = %q, want it to filter type=gate", gotQuery)
	}

	var got []map[string]any
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("output is not a JSON array: %v (%s)", err, out.String())
	}
	if len(got) != 1 || got[0]["await_type"] != "human" {
		t.Fatalf("gate row = %v; want await_type preserved", got)
	}
}

// TestDispatchGateRefusesNonListShapes pins the floor under the classifier: a
// gate mutation arriving here must not reach the controller. `bd gate check`
// closes resolved gates and has no controller equivalent.
func TestDispatchGateRefusesNonListShapes(t *testing.T) {
	for _, args := range [][]string{{"check"}, {"check", "--escalate"}, {"list"}, {"create", "--blocks", "x"}} {
		called := false
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			called = true
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"items": []any{}, "total": 0}) //nolint:errcheck
		}))
		client := beadclient.NewCityScopedClient(ts.URL, "alpha")
		var out, errb bytes.Buffer
		code := DispatchViaAPI(client, "gate", args, &out, &errb)
		ts.Close()
		if code == 0 {
			t.Errorf("gate %v: code=0, want non-zero", args)
		}
		if called {
			t.Errorf("gate %v reached the controller; want no request", args)
		}
	}
}
