package bddispatch

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gastownhall/gascity/internal/beadclient"
)

// TestDispatchDeleteIsNotRouted pins that no `bd delete` reaches the controller.
//
// The API exposes only a SOFT-delete — DELETE /v0/city/{city}/bead/{id} is
// implemented as store.Close, and "Hard-delete is not exposed through the API"
// (internal/api/huma_handlers_beads.go:970-973). Dispatching bd's hard-delete
// onto it was wrong in both directions, measured live on the deployed shim:
// `bd delete <id>` — a read-only PREVIEW in raw bd — CLOSED the bead at exit 0
// with no output, and `bd delete <id> --force` also merely closed it, leaving
// the bead in the store while reporting success.
//
// The classifier now keeps delete off this path entirely. This is the floor
// under that: if a delete ever arrives here anyway, it must fail loudly rather
// than quietly close a bead the caller asked to destroy.
func TestDispatchDeleteIsNotRouted(t *testing.T) {
	for _, args := range [][]string{{"gcw-1"}, {"gcw-1", "--force"}} {
		called := false
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			called = true
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"status": "ok"}) //nolint:errcheck
		}))
		client := beadclient.NewCityScopedClient(ts.URL, "alpha")

		var out, errb bytes.Buffer
		code := DispatchViaAPI(client, "delete", args, &out, &errb)
		ts.Close()

		if code == 0 {
			t.Errorf("delete %v: code=0, want non-zero", args)
		}
		if called {
			t.Errorf("delete %v reached the controller; want no request", args)
		}
	}
}
