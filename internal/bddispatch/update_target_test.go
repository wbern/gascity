package bddispatch

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gastownhall/gascity/internal/beadclient"
)

// TestDispatchUpdateTargetsTheIDNotAFlagValue pins which token the routed update
// writes to. cobra accepts flags before positionals, so `--set-metadata a=1 <id>`
// is a legitimate invocation that raw bd applies to <id>. Taking the first
// non-flag token as the id instead picked the metadata pair `a=1` and issued the
// write against a bead by that name.
func TestDispatchUpdateTargetsTheIDNotAFlagValue(t *testing.T) {
	var gotPath string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "ok"}) //nolint:errcheck
	}))
	defer ts.Close()
	client := beadclient.NewCityScopedClient(ts.URL, "alpha")

	var out, errb bytes.Buffer
	if code := DispatchViaAPI(client, "update", []string{"--set-metadata", "a=1", "gcg-7"}, &out, &errb); code != 0 {
		t.Fatalf("update via API: code=%d err=%s", code, errb.String())
	}
	if want := "/v0/city/alpha/bead/gcg-7/update"; gotPath != want {
		t.Fatalf("update -> %s, want %s", gotPath, want)
	}
}

// TestDispatchUpdateRefusesAmbiguousTarget pins the defensive floor at the
// dispatch boundary. ClassifyVerb already sends any non-single-id update to real
// bd, so reaching here with zero or several ids means the two disagreed; write
// nothing and fail loudly rather than guessing an id.
func TestDispatchUpdateRefusesAmbiguousTarget(t *testing.T) {
	cases := map[string][]string{
		"no id":       {"--status=closed"},
		"several ids": {"gcg-1", "gcg-2", "--status=closed"},
	}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			called := false
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				called = true
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{"status": "ok"}) //nolint:errcheck
			}))
			defer ts.Close()
			client := beadclient.NewCityScopedClient(ts.URL, "alpha")

			var out, errb bytes.Buffer
			if code := DispatchViaAPI(client, "update", args, &out, &errb); code == 0 {
				t.Fatalf("update %v: code=0, want non-zero", args)
			}
			if called {
				t.Fatalf("update %v issued a write; want none", args)
			}
		})
	}
}
