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

// okRunCancelHandler emits a plausible 202 RunCancelOutputBody, asserting
// the CSRF header the client is responsible for attaching arrives on the
// request (the whole point of gcw-7dem9: the CLI must not make callers
// handle X-GC-Request themselves).
func okRunCancelHandler(t *testing.T) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("X-GC-Request"); got == "" {
			t.Errorf("request missing X-GC-Request CSRF header")
		}
		if !strings.HasSuffix(r.URL.Path, "/cancel") {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"run_id": "gcw-i5i90",
			"status": "canceled",
			"closed": int64(5),
		})
	})
}

func runCancelProblemHandler(status int, detail string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"title":  "error",
			"detail": detail,
			"status": status,
		})
	})
}

// TestRouteRunCancel_Success verifies that a successful cancel tears down
// the run end-to-end from the CLI's perspective — it reports the run id,
// resulting status, and closed count on both text and JSON paths, and
// exits 0. This is the acceptance criterion from gcw-7dem9: gc run cancel
// must report the closed count via the same endpoint the API already uses.
func TestRouteRunCancel_Success(t *testing.T) {
	srv := httptest.NewServer(okRunCancelHandler(t))
	defer srv.Close()
	c := api.NewCityScopedClient(srv.URL, "test-city")

	var stdout, stderr bytes.Buffer
	code := routeRunCancel(c, "", "gcw-i5i90", false, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%q", code, stderr.String())
	}
	got := stdout.String()
	for _, want := range []string{"gcw-i5i90", "canceled", "closed=5"} {
		if !strings.Contains(got, want) {
			t.Errorf("stdout missing %q:\n%s", want, got)
		}
	}
}

func TestRouteRunCancel_SuccessJSON(t *testing.T) {
	srv := httptest.NewServer(okRunCancelHandler(t))
	defer srv.Close()
	c := api.NewCityScopedClient(srv.URL, "test-city")

	var stdout, stderr bytes.Buffer
	code := routeRunCancel(c, "", "gcw-i5i90", true, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%q", code, stderr.String())
	}
	var out runCancelResult
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
		t.Fatalf("decode stdout: %v; stdout=%q", err, stdout.String())
	}
	if out.RunID != "gcw-i5i90" || out.Status != "canceled" || out.Closed != 5 || !out.OK {
		t.Errorf("unexpected result: %+v", out)
	}
}

// TestRouteRunCancel_NilClient verifies the no-controller path exits 2
// without contacting anything, matching the maintenance command's
// no-local-fallback convention (this bead's fix must not reimplement
// teardown locally).
func TestRouteRunCancel_NilClient(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := routeRunCancel(nil, "controller-down", "gcw-i5i90", false, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("exit = %d, want 2; stderr=%q", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "controller-down") {
		t.Errorf("stderr missing fallback reason:\n%s", stderr.String())
	}
}

// TestRouteRunCancel_AlreadyTerminal verifies a 409 from the server (the
// run is already terminal) surfaces as a CLI error rather than a false
// success.
func TestRouteRunCancel_AlreadyTerminal(t *testing.T) {
	srv := httptest.NewServer(runCancelProblemHandler(http.StatusConflict, "run gcw-i5i90 is already terminal; nothing to cancel"))
	defer srv.Close()
	c := api.NewCityScopedClient(srv.URL, "test-city")

	var stdout, stderr bytes.Buffer
	code := routeRunCancel(c, "", "gcw-i5i90", false, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("exit = %d, want 1; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "already terminal") {
		t.Errorf("stderr missing detail:\n%s", stderr.String())
	}
}

// TestRouteRunCancel_NotFound verifies a 404 from the server surfaces as a
// CLI error.
func TestRouteRunCancel_NotFound(t *testing.T) {
	srv := httptest.NewServer(runCancelProblemHandler(http.StatusNotFound, "run not found: gcw-missing"))
	defer srv.Close()
	c := api.NewCityScopedClient(srv.URL, "test-city")

	var stdout, stderr bytes.Buffer
	code := routeRunCancel(c, "", "gcw-missing", false, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("exit = %d, want 1; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "not found") {
		t.Errorf("stderr missing detail:\n%s", stderr.String())
	}
}
