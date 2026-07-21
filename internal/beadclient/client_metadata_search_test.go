package beadclient

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestMetadataSearchMapsTypedResponseAndRequest(t *testing.T) {
	called := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		if r.Method != http.MethodPost || r.URL.Path != "/v0/city/test-city/beads/search" {
			t.Fatalf("request = %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("X-GC-Request") == "" {
			t.Fatal("missing X-GC-Request header")
		}
		var body struct {
			Metadata map[string]string `json:"metadata"`
			Limit    int               `json:"limit"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		if body.Metadata["pr_number"] != "9" || body.Limit != 1 {
			t.Fatalf("request body = %+v, want metadata and limit", body)
		}
		w.Header().Set(cacheAgeHeader, "2.5")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"items":[{"id":"gc-1","title":"selected","status":"open","issue_type":"task","created_at":"2026-07-21T12:00:00Z"}],"total":3,"partial":true,"partial_errors":["rig bad: unavailable"]}`))
	}))
	defer server.Close()

	got, err := NewCityScopedClient(server.URL, "test-city").MetadataSearch(MetadataSearchOpts{
		Metadata: map[string]string{"pr_number": "9"},
		Limit:    1,
	})
	if err != nil {
		t.Fatalf("MetadataSearch: %v", err)
	}
	if !called || got.AgeSeconds != 2.5 || got.Body.Total != 3 || !got.Body.Partial || len(got.Body.PartialErrors) != 1 {
		t.Fatalf("response = %+v, called=%v", got, called)
	}
	if len(got.Body.Items) != 1 || got.Body.Items[0].ID != "gc-1" || got.Body.Items[0].Type != "task" {
		t.Fatalf("items = %+v", got.Body.Items)
	}
}

func TestMetadataSearchRejectsUnboundedInputBeforeRequest(t *testing.T) {
	client := NewCityScopedClient("http://127.0.0.1:1", "test-city")
	for _, opts := range []MetadataSearchOpts{
		{Limit: 1},
		{Metadata: map[string]string{"k": "v"}, Limit: 0},
		{Metadata: map[string]string{"k": "v"}, Limit: metadataSearchMaxLimit + 1},
	} {
		if _, err := client.MetadataSearch(opts); err == nil {
			t.Fatalf("MetadataSearch(%+v) succeeded, want validation error", opts)
		}
	}
}
