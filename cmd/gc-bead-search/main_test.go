package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRunUsesBoundedTypedSearchAndWritesItemsOnly(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v0/city/test-city/beads/search" {
			t.Fatalf("request = %s %s", r.Method, r.URL.Path)
		}
		var body struct {
			Metadata     map[string]string `json:"metadata"`
			ExcludeTypes []string          `json:"exclude_types"`
			Rig          string            `json:"rig"`
			Limit        int               `json:"limit"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		if body.Metadata["pr_number"] != "9" || len(body.ExcludeTypes) != 1 || body.ExcludeTypes[0] != "epic" || body.Rig != "crm" || body.Limit != 1 {
			t.Fatalf("request body = %+v", body)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"items":[{"id":"gc-1","title":"first","status":"open","issue_type":"task","created_at":"2026-07-21T12:00:00Z"}],"total":1}`))
	}))
	defer server.Close()
	t.Setenv("GC_API_URL", server.URL)
	t.Setenv("GC_CITY_PATH", "/tmp/test-city")

	var stdout, stderr bytes.Buffer
	if got := run([]string{"--metadata", "pr_number=9", "--exclude-type", "epic", "--rig", "crm"}, &stdout, &stderr); got != 0 {
		t.Fatalf("run exit=%d stderr=%s", got, stderr.String())
	}
	var rows []struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &rows); err != nil {
		t.Fatalf("decode stdout: %v", err)
	}
	if len(rows) != 1 || rows[0].ID != "gc-1" {
		t.Fatalf("rows=%+v", rows)
	}
}

func TestRunRejectsPartialAndInvalidRequests(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"items":[],"total":0,"partial":true,"partial_errors":["rig bad: unavailable"]}`))
	}))
	defer server.Close()
	t.Setenv("GC_API_URL", server.URL)
	t.Setenv("GC_CITY_PATH", "/tmp/test-city")

	for _, args := range [][]string{
		{},
		{"--metadata", "broken"},
		{"--metadata", "k=v", "--limit", "0"},
		{"--metadata", "k=v"},
	} {
		var stdout, stderr bytes.Buffer
		if got := run(args, &stdout, &stderr); got == 0 {
			t.Fatalf("run(%v) succeeded, stderr=%s", args, stderr.String())
		}
		if stdout.Len() != 0 {
			t.Fatalf("run(%v) wrote partial/invalid output %q", args, stdout.String())
		}
	}
}

func TestParseMetadataPreservesValueEqualsAndRejectsDuplicate(t *testing.T) {
	got, err := parseMetadata([]string{"url=https://a/?x=y", "empty="})
	if err != nil || got["url"] != "https://a/?x=y" || got["empty"] != "" {
		t.Fatalf("parseMetadata = %#v, %v", got, err)
	}
	if _, err := parseMetadata([]string{"a=1", "a=2"}); err == nil {
		t.Fatal("duplicate key succeeded")
	}
}
