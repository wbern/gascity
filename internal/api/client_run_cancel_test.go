package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestClientCancelRun verifies Client.CancelRun issues the exact
// POST /v0/city/{cityName}/runs/{run_id}/cancel request the server's
// humaHandleRunCancel expects, attaches the anti-CSRF header itself (so no
// caller — CLI or otherwise — has to know about it), and decodes the 202
// RunCancelOutputBody into RunCancelResult.
func TestClientCancelRun(t *testing.T) {
	var gotMethod, gotPath, gotCSRF string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		gotCSRF = r.Header.Get("X-GC-Request")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck
			"run_id": "gcw-i5i90",
			"status": "canceled",
			"closed": int64(5),
		})
	}))
	defer ts.Close()
	c := NewCityScopedClient(ts.URL, "alpha")

	result, err := c.CancelRun("gcw-i5i90")
	if err != nil {
		t.Fatalf("CancelRun: %v", err)
	}
	if gotMethod != http.MethodPost || gotPath != "/v0/city/alpha/runs/gcw-i5i90/cancel" {
		t.Fatalf("CancelRun -> %s %s, want POST /v0/city/alpha/runs/gcw-i5i90/cancel", gotMethod, gotPath)
	}
	if gotCSRF == "" {
		t.Error("CancelRun did not attach the X-GC-Request CSRF header")
	}
	if result.RunID != "gcw-i5i90" || result.Status != "canceled" || result.Closed != 5 {
		t.Errorf("unexpected result: %+v", result)
	}
}

// TestClientCancelRun_AlreadyTerminal verifies a 409 from the server
// surfaces as a Go error rather than a successful RunCancelResult.
func TestClientCancelRun_AlreadyTerminal(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck
			"title":  "conflict",
			"detail": "run gcw-i5i90 is already terminal; nothing to cancel",
			"status": http.StatusConflict,
		})
	}))
	defer ts.Close()
	c := NewCityScopedClient(ts.URL, "alpha")

	if _, err := c.CancelRun("gcw-i5i90"); err == nil {
		t.Fatal("CancelRun: want error for already-terminal run, got nil")
	}
}
