package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// TestClientBeadWriteMethodsIssueExpectedRequests is the direct client-level
// v1.1 TDD for the bd-shim write methods (gcw-vosx): each method must issue the
// exact city-scoped HTTP request the server handler expects. The reads-first v1
// set is Close/Reopen/Delete/Update/ReadyBeads (Create + GetBeadGraph are covered
// end-to-end by the shim dispatch tests in cmd/gc/cmd_bd_shim_api_test.go).
// ClaimBead/ReleaseBeadIfCurrent are deferred to v2 (gcw-muvg/gcw-wxt2) and are
// intentionally not exercised here.
func TestClientBeadWriteMethodsIssueExpectedRequests(t *testing.T) {
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
	c := NewCityScopedClient(ts.URL, "alpha")

	if err := c.CloseBead("gcg-1"); err != nil {
		t.Fatalf("CloseBead: %v", err)
	}
	if gotMethod != http.MethodPost || gotPath != "/v0/city/alpha/bead/gcg-1/close" {
		t.Fatalf("CloseBead -> %s %s, want POST /v0/city/alpha/bead/gcg-1/close", gotMethod, gotPath)
	}

	if err := c.ReopenBead("gcg-1"); err != nil {
		t.Fatalf("ReopenBead: %v", err)
	}
	if gotMethod != http.MethodPost || gotPath != "/v0/city/alpha/bead/gcg-1/reopen" {
		t.Fatalf("ReopenBead -> %s %s, want POST /v0/city/alpha/bead/gcg-1/reopen", gotMethod, gotPath)
	}

	if err := c.DeleteBead("gcg-1"); err != nil {
		t.Fatalf("DeleteBead: %v", err)
	}
	if gotMethod != http.MethodDelete || gotPath != "/v0/city/alpha/bead/gcg-1" {
		t.Fatalf("DeleteBead -> %s %s, want DELETE /v0/city/alpha/bead/gcg-1", gotMethod, gotPath)
	}

	pass := "closed"
	if err := c.UpdateBead("gcg-1", beads.UpdateOpts{Status: &pass, Metadata: map[string]string{"gc.outcome": "pass"}}); err != nil {
		t.Fatalf("UpdateBead: %v", err)
	}
	if gotMethod != http.MethodPost || gotPath != "/v0/city/alpha/bead/gcg-1/update" {
		t.Fatalf("UpdateBead -> %s %s, want POST /v0/city/alpha/bead/gcg-1/update", gotMethod, gotPath)
	}
	if gotBody["status"] != "closed" {
		t.Fatalf("UpdateBead body status = %v, want closed", gotBody["status"])
	}
	if md, ok := gotBody["metadata"].(map[string]any); !ok || md["gc.outcome"] != "pass" {
		t.Fatalf("UpdateBead body metadata = %v, want gc.outcome=pass", gotBody["metadata"])
	}

	if _, err := c.ReadyBeads(); err != nil {
		t.Fatalf("ReadyBeads: %v", err)
	}
	if gotMethod != http.MethodGet || gotPath != "/v0/city/alpha/beads/ready" {
		t.Fatalf("ReadyBeads -> %s %s, want GET /v0/city/alpha/beads/ready", gotMethod, gotPath)
	}
}
