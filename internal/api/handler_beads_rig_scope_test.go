package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// The bead-read endpoints share one rig-scope contract: a rig that the city
// does not have a store for is an error, and a rig that exists but holds no
// matching beads is an ordinary empty result. Before this contract existed the
// two were indistinguishable — every endpoint answered 200 with an empty list,
// which is the same silent-wrong shape as gascity commit 0227f3a42 (a scoped
// read answering from the wrong store). `gc bd list --rig <unknown>` already
// exits 1 with "rig not found"; routing that read through the controller must
// not downgrade the error into a bare empty page.

type rigScopeListResponse struct {
	Items []beads.Bead `json:"items"`
	Total int          `json:"total"`
}

func decodeRigScopeList(t *testing.T, rec *httptest.ResponseRecorder) rigScopeListResponse {
	t.Helper()
	var resp rigScopeListResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	return resp
}

// Each beads.MemStore numbers its own beads from gc-1, so two stores in one
// test produce colliding IDs. Assert on titles, which stay distinct.
func titlesOf(items []beads.Bead) []string {
	out := make([]string, 0, len(items))
	for _, b := range items {
		out = append(out, b.Title)
	}
	sort.Strings(out)
	return out
}

func getRigScope(t *testing.T, state *fakeState, path string) *httptest.ResponseRecorder {
	t.Helper()
	h := newTestCityHandler(t, state)
	req := httptest.NewRequest("GET", cityURL(state, path), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestBeadListUnknownRigReturnsNotFound(t *testing.T) {
	state := newFakeState(t)
	state.stores["myrig"].Create(beads.Bead{Title: "in myrig"}) //nolint:errcheck

	rec := getRigScope(t, state, "/beads?rig=no-such-rig")

	if rec.Code != http.StatusNotFound {
		t.Fatalf("GET /beads?rig=no-such-rig = %d, want %d (body %s)",
			rec.Code, http.StatusNotFound, rec.Body.String())
	}
	// The message must name BOTH causes. A declared-but-unbound rig is skipped
	// by buildStores and lands here looking identical to a typo, and it is
	// reachable from a UI whose rig picker is built from the declared rig list.
	// A bare "not found" would send an operator hunting for a misspelling.
	if body := rec.Body.String(); !strings.Contains(body, "unbound") {
		t.Errorf("unknown-rig error should name the unbound case too, got %s", body)
	}
}

// A rig that exists but has no beads is NOT an error. This is the case that
// makes an unconditional 404-on-empty wrong, and it is why the check must be
// on store presence rather than on result count.
func TestBeadListKnownRigWithNoBeadsReturnsEmptyOK(t *testing.T) {
	state := newFakeState(t)
	state.stores["emptyrig"] = beads.NewMemStore()
	state.stores["myrig"].Create(beads.Bead{Title: "in myrig"}) //nolint:errcheck

	rec := getRigScope(t, state, "/beads?rig=emptyrig")

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /beads?rig=emptyrig = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if resp := decodeRigScopeList(t, rec); resp.Total != 0 {
		t.Errorf("known-but-empty rig: Total = %d, want 0", resp.Total)
	}
}

func TestBeadListKnownRigFiltersToThatRig(t *testing.T) {
	state := newFakeState(t)
	other := beads.NewMemStore()
	state.stores["rig2"] = other
	state.stores["myrig"].Create(beads.Bead{Title: "in myrig"}) //nolint:errcheck
	if _, err := other.Create(beads.Bead{Title: "in rig2"}); err != nil {
		t.Fatalf("Create(rig2 bead): %v", err)
	}

	rec := getRigScope(t, state, "/beads?rig=rig2")

	got := titlesOf(decodeRigScopeList(t, rec).Items)
	if want := []string{"in rig2"}; !slices.Equal(got, want) {
		t.Errorf("rig-scoped list titles = %v, want %v", got, want)
	}
}

// GET /beads/ready federated every store unconditionally and silently dropped
// an unknown `rig` query parameter, so a scoped ready read answered with the
// whole city. Measured against the live controller before the fix:
// /beads/ready?rig=gas-city-wbern returned 402 beads spanning four rigs.
func TestBeadReadyRigFiltersToRequestedRig(t *testing.T) {
	state := newFakeState(t)
	other := beads.NewMemStore()
	state.stores["rig2"] = other
	state.stores["myrig"].Create(beads.Bead{Title: "ready in myrig"}) //nolint:errcheck
	if _, err := other.Create(beads.Bead{Title: "ready in rig2"}); err != nil {
		t.Fatalf("Create(rig2 bead): %v", err)
	}

	rec := getRigScope(t, state, "/beads/ready?rig=rig2")

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /beads/ready?rig=rig2 = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	got := titlesOf(decodeRigScopeList(t, rec).Items)
	if want := []string{"ready in rig2"}; !slices.Equal(got, want) {
		t.Errorf("rig-scoped ready titles = %v, want %v", got, want)
	}
}

// A rig-scoped ready read must not fall back to the city store: the city store
// is federated only for an unscoped read.
func TestBeadReadyRigExcludesCityStore(t *testing.T) {
	state := newFakeState(t)
	state.cityBeadStore = beads.NewMemStore()
	if _, err := state.cityBeadStore.Create(beads.Bead{Title: "city-scope ready"}); err != nil {
		t.Fatalf("Create(cityBead): %v", err)
	}
	state.stores["myrig"].Create(beads.Bead{Title: "ready in myrig"}) //nolint:errcheck

	rec := getRigScope(t, state, "/beads/ready?rig=myrig")

	got := titlesOf(decodeRigScopeList(t, rec).Items)
	if want := []string{"ready in myrig"}; !slices.Equal(got, want) {
		t.Errorf("rig-scoped ready titles = %v, want %v (city bead must not leak)", got, want)
	}
}

func TestBeadReadyUnknownRigReturnsNotFound(t *testing.T) {
	state := newFakeState(t)
	state.stores["myrig"].Create(beads.Bead{Title: "ready in myrig"}) //nolint:errcheck

	rec := getRigScope(t, state, "/beads/ready?rig=no-such-rig")

	if rec.Code != http.StatusNotFound {
		t.Fatalf("GET /beads/ready?rig=no-such-rig = %d, want %d (body %s)",
			rec.Code, http.StatusNotFound, rec.Body.String())
	}
}

// GET /beads/ephemeral had the same silent-drop shape as /beads/ready: verified
// live before the fix, ?rig=gas-city-wbern, ?rig=nonsense and no rig at all
// returned byte-identical results.
func TestBeadEphemeralRigFiltersToRequestedRig(t *testing.T) {
	state := newFakeState(t)
	other := beads.NewMemStore()
	state.stores["rig2"] = other
	state.stores["myrig"].Create(beads.Bead{Title: "wisp in myrig", Ephemeral: true}) //nolint:errcheck
	if _, err := other.Create(beads.Bead{Title: "wisp in rig2", Ephemeral: true}); err != nil {
		t.Fatalf("Create(rig2 wisp): %v", err)
	}

	rec := getRigScope(t, state, "/beads/ephemeral?rig=rig2")

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /beads/ephemeral?rig=rig2 = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	got := titlesOf(decodeRigScopeList(t, rec).Items)
	if want := []string{"wisp in rig2"}; !slices.Equal(got, want) {
		t.Errorf("rig-scoped ephemeral titles = %v, want %v", got, want)
	}
}

func TestBeadEphemeralUnknownRigReturnsNotFound(t *testing.T) {
	state := newFakeState(t)

	rec := getRigScope(t, state, "/beads/ephemeral?rig=no-such-rig")

	if rec.Code != http.StatusNotFound {
		t.Fatalf("GET /beads/ephemeral?rig=no-such-rig = %d, want %d (body %s)",
			rec.Code, http.StatusNotFound, rec.Body.String())
	}
}
