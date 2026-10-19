package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
)

// A canonical singleton created without an alias must persist its qualified
// name as alias and the alias-derived runtime session name, not the
// s-<beadID> fallback that the reconciler's later alias adoption cannot undo.
func TestHandleSessionCreate_CanonicalSingletonWithoutAliasUsesQualifiedNameAlias(t *testing.T) {
	fs := newSessionFakeState(t)
	fs.cfg.Agents = append(fs.cfg.Agents, config.Agent{
		Name:              "marketer",
		Provider:          "test-agent",
		MaxActiveSessions: intPtr(1),
	})
	srv := New(fs)
	h := newTestCityHandlerWith(t, fs, srv)

	req := newPostRequest(cityURL(fs, "/sessions"), strings.NewReader(`{"kind":"agent","name":"marketer"}`))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusAccepted {
		t.Fatalf("got status %d, want %d; body: %s", w.Code, http.StatusAccepted, w.Body.String())
	}

	accepted := decodeAsyncAccepted(t, w.Body)
	success, failure := waitForSessionCreateResult(t, fs.eventProv, accepted.RequestID)
	if success == nil {
		t.Fatalf("session create failed: %s: %s", failure.ErrorCode, failure.ErrorMessage)
	}
	if got := success.Session.Alias; got != "marketer" {
		t.Fatalf("Alias = %q, want marketer", got)
	}
	if got := success.Session.SessionName; got != "marketer" {
		t.Fatalf("SessionName = %q, want alias-derived marketer", got)
	}
}
