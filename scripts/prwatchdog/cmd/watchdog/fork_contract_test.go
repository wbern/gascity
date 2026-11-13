package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/scripts/prwatchdog"
)

// recordingTransport answers the one paginated check-runs request per poll
// (upstream fetches every run on the commit and filters to tracked names) with
// a passing run for each fork check, and records the requests it saw.
type recordingTransport struct {
	names []string
	paths []string
}

func (r *recordingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	r.paths = append(r.paths, req.URL.Path)
	if !strings.Contains(req.URL.Path, "/commits/exact-head/check-runs") {
		return nil, fmt.Errorf("wrong head URL %s", req.URL)
	}
	runs := make([]string, 0, len(r.names))
	for i, name := range r.names {
		runs = append(runs, fmt.Sprintf(`{"name":%q,"head_sha":"exact-head","status":"completed","conclusion":"success","id":%d}`, name, i+1))
	}
	body := `{"total_count":` + fmt.Sprint(len(runs)) + `,"check_runs":[` + strings.Join(runs, ",") + `]}`
	return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
}

func TestForkFetchEvaluateAndSummary(t *testing.T) {
	names, err := prwatchdog.CheckNames(prwatchdog.ContractFork)
	if err != nil {
		t.Fatal(err)
	}
	transport := &recordingTransport{names: names}
	fetcher := &githubFetcher{httpClient: &http.Client{Transport: transport}, repo: "wbern/gascity", checkNames: names}
	runs, err := fetcher.FetchCheckRuns(context.Background(), "exact-head")
	if err != nil {
		t.Fatal(err)
	}
	eval := prwatchdog.Evaluate(prwatchdog.Input{Contract: prwatchdog.ContractFork, HeadSHA: "exact-head", CheckRuns: runs})
	if !eval.Pass {
		t.Fatalf("fetched fork evidence failed: %+v", eval)
	}
	summary := renderSummary(eval)
	for _, name := range []string{prwatchdog.ForkVerifyName, prwatchdog.ForkLintName, prwatchdog.ForkProofName} {
		if !strings.Contains(summary, "| "+name+" | success |") {
			t.Fatalf("summary lost %q: %s", name, summary)
		}
	}
	if !strings.Contains(summary, "excludes comprehensive upstream CI coverage") || strings.Contains(summary, "| Check | success |") {
		t.Fatalf("misleading fork scope: %s", summary)
	}
	if len(transport.paths) != 1 {
		t.Fatalf("fetch made %d requests, want one paginated request for every tracked check", len(transport.paths))
	}
}

func TestUnknownContractRejectedBeforeFetching(t *testing.T) {
	t.Setenv("REPOSITORY", "wbern/gascity")
	t.Setenv("PR_HEAD_SHA", "exact-head")
	t.Setenv("GH_TOKEN", "test-token")
	t.Setenv("EVIDENCE_CONTRACT", "typo")
	if err := run(); err == nil || !strings.Contains(err.Error(), "unknown evidence contract") {
		t.Fatalf("unknown contract: %v", err)
	}
}
