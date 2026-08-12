package bddispatch

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/bdshim"
	"github.com/gastownhall/gascity/internal/beadclient"
	"github.com/gastownhall/gascity/internal/beads"
)

func TestWriteReadyJSONWithBudgetReplacesOversizedPayloadWithManifest(t *testing.T) {
	var stdout, stderr bytes.Buffer
	beadsOut := []beads.Bead{{ID: "gcg-oversized", Description: strings.Repeat("secret-value", 64)}}

	if code := WriteReadyJSONWithBudget(beadsOut, &stdout, &stderr, 512); code != 0 {
		t.Fatalf("WriteReadyJSONWithBudget() = %d, stderr = %q", code, stderr.String())
	}
	if got := stdout.Len(); got > 512 {
		t.Fatalf("stdout is %d bytes, want at most 512", got)
	}
	if !json.Valid(stdout.Bytes()) {
		t.Fatalf("stdout is not valid JSON: %q", stdout.String())
	}
	if strings.Contains(stdout.String(), "secret-value") {
		t.Fatalf("stdout leaked withheld content: %q", stdout.String())
	}
	var manifest struct {
		SchemaVersion   string `json:"schema_version"`
		Kind            string `json:"kind"`
		Reason          string `json:"reason"`
		CommandClass    string `json:"command_class"`
		BudgetBytes     int    `json:"budget_bytes"`
		SerializedBytes int    `json:"serialized_bytes"`
		SHA256          string `json:"sha256"`
		Spill           struct {
			Mode      string `json:"mode"`
			Path      string `json:"path"`
			ExpiresAt string `json:"expires_at"`
		} `json:"spill"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &manifest); err != nil {
		t.Fatalf("unmarshal manifest: %v", err)
	}
	if manifest.SchemaVersion != "1" || manifest.Kind != "gc.output_firewall" || manifest.Reason != "byte_budget_exceeded" || manifest.CommandClass != "managed_bd_read" {
		t.Fatalf("manifest = %#v", manifest)
	}
	if manifest.BudgetBytes != 512 || manifest.SerializedBytes <= manifest.BudgetBytes || manifest.SHA256 == "" {
		t.Fatalf("manifest = %#v", manifest)
	}
	if manifest.Spill.Mode == "" || manifest.Spill.ExpiresAt == "" {
		t.Fatalf("spill manifest is incomplete: %#v", manifest.Spill)
	}
}

func TestWriteReadyJSONWithBudgetGiantSingleFieldNeverReachesStdout(t *testing.T) {
	var stdout, stderr bytes.Buffer
	giant := strings.Repeat("giant-secret", managedReadOutputBudget)
	if code := WriteReadyJSONWithBudget([]beads.Bead{{ID: "gcg-giant", Description: giant}}, &stdout, &stderr, managedReadOutputBudget); code != 0 {
		t.Fatalf("code=%d", code)
	}
	if stdout.Len() > managedReadOutputBudget || strings.Contains(stdout.String(), "giant-secret") || !json.Valid(stdout.Bytes()) {
		t.Fatalf("giant field escaped firewall: %d bytes", stdout.Len())
	}
}

func TestWriteReadyJSONWithBudgetEncodeFailureWritesBoundedValidManifest(t *testing.T) {
	original := marshalOutputJSON
	marshalOutputJSON = func(any) ([]byte, error) { return nil, errors.New("encode failed") }
	t.Cleanup(func() { marshalOutputJSON = original })
	var stdout, stderr bytes.Buffer
	if code := WriteReadyJSONWithBudget([]beads.Bead{{ID: "gcg"}}, &stdout, &stderr, 512); code != 0 {
		t.Fatalf("code=%d", code)
	}
	if stdout.Len() > 512 || !json.Valid(stdout.Bytes()) || strings.Contains(stdout.String(), "encode failed") || !strings.Contains(stdout.String(), "gc.output_firewall") {
		t.Fatalf("stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
}

func TestWriteReadyJSONPreservesDirectUserOutput(t *testing.T) {
	t.Parallel()

	var stdout, stderr bytes.Buffer
	beadsOut := []beads.Bead{{ID: "gcg-direct", Description: strings.Repeat("direct-user-body", 3000)}}
	if code := WriteReadyJSON(beadsOut, &stdout, &stderr); code != 0 {
		t.Fatalf("WriteReadyJSON() = %d, stderr = %q", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "direct-user-body") {
		t.Fatal("direct compatibility writer replaced the body")
	}
	if !json.Valid(stdout.Bytes()) {
		t.Fatalf("direct output is not valid JSON: %q", stdout.String())
	}
}

func TestWriteManagedReadJSONUsesBudgetOnlyWhenManaged(t *testing.T) {
	t.Setenv(managedOutputFirewallEnv, "1")

	var stdout, stderr bytes.Buffer
	beadsOut := []beads.Bead{{ID: "gcg-managed", Description: strings.Repeat("managed-secret", 4000)}}
	if code := WriteManagedReadJSON(beadsOut, &stdout, &stderr); code != 0 {
		t.Fatalf("WriteManagedReadJSON() = %d, stderr = %q", code, stderr.String())
	}
	if stdout.Len() > managedReadOutputBudget || strings.Contains(stdout.String(), "managed-secret") {
		t.Fatalf("managed output leaked body or exceeded budget (%d bytes)", stdout.Len())
	}
}

func TestWriteReadyJSONWithBudgetCountsEscapedUTF8AndManyRows(t *testing.T) {
	cases := []struct {
		name string
		out  []beads.Bead
	}{
		{"escaped utf8", []beads.Bead{{ID: "gcg-utf8", Description: strings.Repeat("\"\\漢", 80)}}},
		{"many rows", func() []beads.Bead {
			out := make([]beads.Bead, 80)
			for i := range out {
				out[i] = beads.Bead{ID: "gcg-row", Title: strings.Repeat("row", 20)}
			}
			return out
		}()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if code := WriteReadyJSONWithBudget(tc.out, &stdout, &stderr, 512); code != 0 {
				t.Fatalf("code=%d stderr=%q", code, stderr.String())
			}
			if stdout.Len() > 512 || !json.Valid(stdout.Bytes()) {
				t.Fatalf("invalid bounded output: %d bytes %q", stdout.Len(), stdout.String())
			}
			if !strings.Contains(stdout.String(), "gc.output_firewall") {
				t.Fatalf("want manifest, got %q", stdout.String())
			}
		})
	}
}

func TestWriteReadyJSONWithBudgetDisabledSpillDoesNotCreateArtifact(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	setOutputFirewallSpillPath(t, dir)
	t.Setenv("GC_MANAGED_OUTPUT_FIREWALL_SPILL_MODE", "disabled")
	var stdout, stderr bytes.Buffer
	if code := WriteReadyJSONWithBudget([]beads.Bead{{ID: "gcg", Description: strings.Repeat("body", 1000)}}, &stdout, &stderr, 512); code != 0 {
		t.Fatalf("code=%d", code)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("disabled spill created %d artifacts", len(entries))
	}
}

func TestWriteReadyJSONWithBudgetUnavailableSpillReturnsNoSpillManifest(t *testing.T) {
	blocked := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(blocked, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	setOutputFirewallSpillPath(t, blocked)
	var stdout, stderr bytes.Buffer
	if code := WriteReadyJSONWithBudget([]beads.Bead{{ID: "gcg", Description: strings.Repeat("body", 1000)}}, &stdout, &stderr, 512); code != 0 {
		t.Fatalf("code=%d", code)
	}
	var manifest struct {
		Spill struct{ Mode, Path string } `json:"spill"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.Spill.Mode != "unavailable" || manifest.Spill.Path != "" {
		t.Fatalf("spill=%#v", manifest.Spill)
	}
}

type failingOutputWriter struct{ writes int }

func setOutputFirewallSpillPath(t *testing.T, dir string) {
	t.Helper()
	t.Setenv(managedOutputFirewallSpillRootEnv, filepath.Dir(dir))
	t.Setenv(managedOutputFirewallSpillPathEnv, filepath.Base(dir))
}

func (w *failingOutputWriter) Write([]byte) (int, error) { w.writes++; return 0, os.ErrClosed }

func TestWriteReadyJSONWithBudgetWriteFailureDoesNotRetryOrLeakPayload(t *testing.T) {
	w := &failingOutputWriter{}
	var stderr bytes.Buffer
	if code := WriteReadyJSONWithBudget([]beads.Bead{{ID: "gcg", Description: strings.Repeat("secret", 1000)}}, w, &stderr, 512); code != 1 {
		t.Fatalf("code=%d", code)
	}
	if w.writes != 1 {
		t.Fatalf("writes=%d, want one final publish attempt", w.writes)
	}
	if strings.Contains(stderr.String(), "secret") {
		t.Fatalf("stderr leaked payload: %q", stderr.String())
	}
}

func TestWriteReadyJSONWithBudgetSpillsWithPrivatePermissions(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatalf("chmod spill dir: %v", err)
	}
	setOutputFirewallSpillPath(t, dir)
	dirInfo, err := os.Lstat(dir)
	if err != nil {
		t.Fatalf("stat temp dir: %v", err)
	}
	t.Logf("spill dir mode=%o", dirInfo.Mode().Perm())

	var stdout, stderr bytes.Buffer
	if code := WriteReadyJSONWithBudget([]beads.Bead{{ID: "gcg-oversized", Description: strings.Repeat("body", 1000)}}, &stdout, &stderr, 512); code != 0 {
		t.Fatalf("WriteReadyJSONWithBudget() = %d, stderr = %q", code, stderr.String())
	}
	var manifest struct {
		SerializedBytes int    `json:"serialized_bytes"`
		SHA256          string `json:"sha256"`
		Spill           struct {
			Mode string `json:"mode"`
			Path string `json:"path"`
		} `json:"spill"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &manifest); err != nil {
		t.Fatalf("unmarshal manifest: %v", err)
	}
	if manifest.Spill.Mode != "secure" || manifest.Spill.Path == "" {
		t.Fatalf("spill manifest = %#v", manifest.Spill)
	}
	info, err := os.Stat(manifest.Spill.Path)
	if err != nil {
		t.Fatalf("stat spill %q: %v; stdout=%q", manifest.Spill.Path, err, stdout.String())
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("spill mode = %o, want 600", got)
	}
	artifact, err := os.ReadFile(manifest.Spill.Path)
	if err != nil {
		t.Fatal(err)
	}
	want, err := json.Marshal([]beads.Bead{{ID: "gcg-oversized", Description: strings.Repeat("body", 1000)}})
	if err != nil {
		t.Fatal(err)
	}
	want = append(want, '\n')
	if !bytes.Equal(artifact, want) {
		t.Fatalf("artifact does not match serialized payload")
	}
	digest := sha256.Sum256(artifact)
	if manifest.SerializedBytes != len(artifact) || manifest.SHA256 != fmt.Sprintf("%x", digest) {
		t.Fatalf("manifest bytes/digest mismatch: %#v", manifest)
	}
	if strings.Contains(stdout.String(), "bodybody") || strings.Contains(stderr.String(), "bodybody") {
		t.Fatal("withheld payload leaked")
	}
}

func TestWriteReadyJSONWithBudgetUsesConfiguredRetentionTTL(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	setOutputFirewallSpillPath(t, dir)
	t.Setenv(managedOutputFirewallRetentionEnv, "1h")
	before := time.Now().UTC()
	var stdout, stderr bytes.Buffer
	if code := WriteReadyJSONWithBudget([]beads.Bead{{ID: "gcg", Description: strings.Repeat("body", 1000)}}, &stdout, &stderr, 512); code != 0 {
		t.Fatalf("code=%d stderr=%q", code, stderr.String())
	}
	var manifest struct {
		Spill struct {
			ExpiresAt string `json:"expires_at"`
		} `json:"spill"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &manifest); err != nil {
		t.Fatal(err)
	}
	expiresAt, err := time.Parse(time.RFC3339, manifest.Spill.ExpiresAt)
	if err != nil {
		t.Fatal(err)
	}
	if got := expiresAt.Sub(before); got < 59*time.Minute || got > 61*time.Minute {
		t.Fatalf("expires_at interval = %s, want about 1h", got)
	}
}

func TestWriteManagedJSONCancelledBeforePublishWritesNoOutput(t *testing.T) {
	t.Setenv(managedOutputFirewallEnv, "1")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var stdout, stderr bytes.Buffer
	if code := WriteManagedJSON(ctx, "managed_hook_claim", "hook", []string{strings.Repeat("secret", 1000)}, &stdout, &stderr); code != 1 {
		t.Fatalf("code=%d", code)
	}
	if stdout.Len() != 0 || !strings.Contains(stderr.String(), "canceled") {
		t.Fatalf("stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
}

func TestWriteManagedOutputReplacesMalformedJSON(t *testing.T) {
	t.Setenv(managedOutputFirewallEnv, "1")
	var stdout, stderr bytes.Buffer
	if code := WriteManagedOutput(context.Background(), "managed_bd_passthrough", "show", []byte("not-json"), true, &stdout, &stderr); code != 0 {
		t.Fatalf("code=%d stderr=%q", code, stderr.String())
	}
	if !json.Valid(stdout.Bytes()) || strings.Contains(stdout.String(), "not-json") || !strings.Contains(stdout.String(), "invalid_json") {
		t.Fatalf("stdout=%q", stdout.String())
	}
}

func TestWriteManagedReadJSONForVerbHonorsConfiguredScope(t *testing.T) {
	t.Setenv(managedOutputFirewallEnv, "1")
	t.Setenv(managedOutputFirewallBudgetEnv, "512")
	t.Setenv(managedOutputFirewallReadVerbsEnv, "ready")
	var stdout, stderr bytes.Buffer
	body := strings.Repeat("show-body", 200)
	if code := WriteManagedReadJSONForVerb("show", []beads.Bead{{ID: "gcg", Description: body}}, &stdout, &stderr); code != 0 {
		t.Fatalf("code=%d stderr=%q", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), body) {
		t.Fatalf("out-of-scope show was firewalled: %q", stdout.String())
	}
}

// TestDispatchViaAPICreate proves `bd create` routes to POST /v0/beads with the
// parsed fields and renders the created bead id like raw bd.
func TestDispatchViaAPICreate(t *testing.T) {
	t.Setenv(managedOutputFirewallEnv, "1")
	var gotMethod, gotPath string
	var gotBody map[string]any
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		gotBody = nil
		_ = json.NewDecoder(r.Body).Decode(&gotBody) //nolint:errcheck
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		title, _ := gotBody["title"].(string)
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "gcg-9", "title": title}) //nolint:errcheck
	}))
	defer ts.Close()
	client := beadclient.NewCityScopedClient(ts.URL, "alpha")

	var out, errb bytes.Buffer
	if code := DispatchViaAPI(client, "create", []string{"my task", "--type", "task", "--label", "x"}, &out, &errb); code != 0 {
		t.Fatalf("create via API: code=%d err=%s", code, errb.String())
	}
	if gotMethod != http.MethodPost || gotPath != "/v0/city/alpha/beads" {
		t.Fatalf("create -> %s %s, want POST /v0/city/alpha/beads", gotMethod, gotPath)
	}
	if gotBody["title"] != "my task" {
		t.Fatalf("create body title = %v, want 'my task'", gotBody["title"])
	}
	if !strings.Contains(out.String(), "Created bead: gcg-9") {
		t.Fatalf("create output = %q, want 'Created bead: gcg-9'", out.String())
	}
}

// TestDispatchViaAPIMol proves `bd mol current|progress <id>` routes to
// GET /beads/graph/{id} and renders step status indicators / progress from the
// returned topology (the routed source reaches SQLite-resident steps).
func TestDispatchViaAPIMol(t *testing.T) {
	graphJSON := map[string]any{
		"root": map[string]any{"id": "gcg-1", "title": "workflow", "status": "open"},
		"beads": []map[string]any{
			{"id": "gcg-1", "title": "workflow", "status": "open"},
			{"id": "gcg-2", "title": "step one", "status": "closed"},
			{"id": "gcg-3", "title": "step two", "status": "in_progress"},
		},
		"deps": []map[string]any{},
	}
	newServer := func() *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(graphJSON) //nolint:errcheck
		}))
	}

	t.Run("current", func(t *testing.T) {
		var gotMethod, gotPath string
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotMethod, gotPath = r.Method, r.URL.Path
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(graphJSON) //nolint:errcheck
		}))
		defer ts.Close()
		client := beadclient.NewCityScopedClient(ts.URL, "alpha")
		var out, errb bytes.Buffer
		if code := DispatchViaAPI(client, "mol", []string{"current", "gcg-1"}, &out, &errb); code != 0 {
			t.Fatalf("mol current: code=%d err=%s", code, errb.String())
		}
		if gotMethod != http.MethodGet || gotPath != "/v0/city/alpha/beads/graph/gcg-1" {
			t.Fatalf("mol -> %s %s, want GET /v0/city/alpha/beads/graph/gcg-1", gotMethod, gotPath)
		}
		o := out.String()
		if !strings.Contains(o, "[done] gcg-2") || !strings.Contains(o, "[current] gcg-3") {
			t.Fatalf("mol current render = %q, want [done] gcg-2 + [current] gcg-3 (root excluded)", o)
		}
		if strings.Contains(o, "gcg-1 workflow\n") && strings.Contains(o, "[") && strings.Contains(o, "] gcg-1") {
			t.Fatalf("mol current rendered the root as a step: %q", o)
		}
	})

	t.Run("progress", func(t *testing.T) {
		ts := newServer()
		defer ts.Close()
		client := beadclient.NewCityScopedClient(ts.URL, "alpha")
		var out, errb bytes.Buffer
		if code := DispatchViaAPI(client, "mol", []string{"progress", "gcg-1"}, &out, &errb); code != 0 {
			t.Fatalf("mol progress: code=%d err=%s", code, errb.String())
		}
		if !strings.Contains(out.String(), "1/2 steps complete (50%)") {
			t.Fatalf("mol progress render = %q, want 1/2 steps complete (50%%)", out.String())
		}
	})
}

func TestRenderBdMolBoundsManagedPlainOutput(t *testing.T) {
	t.Setenv(managedOutputFirewallEnv, "1")
	t.Setenv(managedOutputFirewallBudgetEnv, "512")
	t.Setenv(managedOutputFirewallReadVerbsEnv, "mol")
	t.Setenv("GC_MANAGED_OUTPUT_FIREWALL_SPILL_MODE", "disabled")
	g := beadclient.BeadGraph{Root: beads.Bead{ID: "gcw-root", Title: strings.Repeat("root-secret", 200)}, Beads: []beads.Bead{{ID: "gcw-root"}, {ID: "gcw-step", Title: strings.Repeat("step-secret", 200)}}}
	var stdout, stderr bytes.Buffer
	if code := renderBdMol("current", g, false, &stdout, &stderr); code != 0 {
		t.Fatalf("code=%d stderr=%q", code, stderr.String())
	}
	if stdout.Len() > 512 || !json.Valid(stdout.Bytes()) || strings.Contains(stdout.String(), "root-secret") || strings.Contains(stdout.String(), "step-secret") {
		t.Fatalf("stdout=%q", stdout.String())
	}
}

func TestRenderBdMolPreservesUnderBudgetPlainOutput(t *testing.T) {
	t.Setenv(managedOutputFirewallEnv, "1")
	t.Setenv(managedOutputFirewallReadVerbsEnv, "mol")
	g := beadclient.BeadGraph{Root: beads.Bead{ID: "gcw-root", Title: "Root"}, Beads: []beads.Bead{{ID: "gcw-root"}, {ID: "gcw-step", Title: "Step", Status: "open"}}}
	var stdout, stderr bytes.Buffer
	if code := renderBdMol("current", g, false, &stdout, &stderr); code != 0 {
		t.Fatalf("code=%d stderr=%q", code, stderr.String())
	}
	if got, want := stdout.String(), "Molecule gcw-root — Root (0/1 done)\n  [pending] gcw-step Step\n"; got != want {
		t.Fatalf("stdout=%q want=%q", got, want)
	}
}

// TestDispatchViaAPIQueryEphemeral proves `bd query --json 'ephemeral=true AND
// ...'` routes to GET /beads/ephemeral with the parsed filters and renders the
// wisp rows as a JSON array (like raw `bd query`).
func TestDispatchViaAPIQueryEphemeral(t *testing.T) {
	if !bdshim.QueryRoutingEnabled {
		t.Skip("v1: bd query routing is disabled until GET /beads/ephemeral is ported to this fork")
	}
	var gotMethod, gotPath, gotQuery string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath, gotQuery = r.Method, r.URL.Path, r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck
			"items": []map[string]any{{"id": "gcg-3", "title": "hb", "ephemeral": true}},
			"total": 1,
		})
	}))
	defer ts.Close()
	client := beadclient.NewCityScopedClient(ts.URL, "alpha")

	var out, errb bytes.Buffer
	if code := DispatchViaAPI(client, "query", []string{"--json", "ephemeral=true AND status=open", "--limit", "0"}, &out, &errb); code != 0 {
		t.Fatalf("query via API: code=%d err=%s", code, errb.String())
	}
	if gotMethod != http.MethodGet || gotPath != "/v0/city/alpha/beads/ephemeral" {
		t.Fatalf("query -> %s %s, want GET /v0/city/alpha/beads/ephemeral", gotMethod, gotPath)
	}
	if !strings.Contains(gotQuery, "status=open") {
		t.Fatalf("query params = %q, want status=open", gotQuery)
	}
	if !strings.Contains(out.String(), "gcg-3") {
		t.Fatalf("query output = %q, want the ephemeral bead gcg-3", out.String())
	}
}

// TestParseQueryEphemeral covers the two in-repo `bd query` ephemeral shapes and
// the predicate/flag forms that must NOT route (closed allowlist).
func TestParseQueryEphemeral(t *testing.T) {
	cases := []struct {
		name string
		args []string
		ok   bool
		want beadclient.EphemeralBeadsOpts
	}{
		{"listEphemeral shape", []string{"query", "--json", "ephemeral=true AND status=open AND label=wisp_type:ping", "--limit", "0"}, true, beadclient.EphemeralBeadsOpts{Status: "open", Label: "wisp_type:ping"}},
		{"work_query literal", []string{"query", "--json", "ephemeral=true AND status=in_progress", "--limit=0"}, true, beadclient.EphemeralBeadsOpts{Status: "in_progress"}},
		{"with --all", []string{"query", "--json", "ephemeral=true", "--all"}, true, beadclient.EphemeralBeadsOpts{All: true}},
		{"missing --json", []string{"query", "ephemeral=true"}, false, beadclient.EphemeralBeadsOpts{}},
		{"non-ephemeral predicate", []string{"query", "--json", "type=bug"}, false, beadclient.EphemeralBeadsOpts{}},
		{"non-bare value", []string{"query", "--json", "ephemeral=true AND status=open OR x"}, false, beadclient.EphemeralBeadsOpts{}},
		{"unknown flag", []string{"query", "--json", "ephemeral=true", "--weird"}, false, beadclient.EphemeralBeadsOpts{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := ParseQueryEphemeral(tc.args)
			if ok != tc.ok {
				t.Fatalf("ParseQueryEphemeral(%v) ok=%v, want %v", tc.args, ok, tc.ok)
			}
			if ok && got != tc.want {
				t.Fatalf("ParseQueryEphemeral(%v) = %+v, want %+v", tc.args, got, tc.want)
			}
		})
	}
}

func TestApplyReadyParamsHonorsAssignee(t *testing.T) {
	params, err := ParseReadyParams([]string{"--assignee=worker-a", "--json"})
	if err != nil {
		t.Fatalf("ParseReadyParams: %v", err)
	}
	got := applyReadyParams([]beads.Bead{
		{ID: "global-p1", Assignee: "someone-else"},
		{ID: "unassigned", Assignee: ""},
		{ID: "mine", Assignee: "worker-a"},
	}, params)
	if len(got) != 1 || got[0].ID != "mine" {
		t.Fatalf("applyReadyParams(--assignee) = %+v, want only mine", got)
	}
}

func TestApplyReadyParamsHonorsExplicitEmptyAssignee(t *testing.T) {
	params, err := ParseReadyParams([]string{"--assignee=", "--json"})
	if err != nil {
		t.Fatalf("ParseReadyParams: %v", err)
	}
	got := applyReadyParams([]beads.Bead{
		{ID: "assigned", Assignee: "worker-a"},
		{ID: "unassigned", Assignee: ""},
	}, params)
	if len(got) != 1 || got[0].ID != "unassigned" {
		t.Fatalf("applyReadyParams(--assignee=) = %+v, want only unassigned", got)
	}
}

// TestDispatchViaAPIRoutesVerbs proves the shim's HTTP dispatch maps each routed
// bd verb onto the right city-scoped endpoint, verb, and body — the path a
// worker's bd op takes through the controller in the pure-HTTP redirect.
func TestDispatchViaAPIRoutesVerbs(t *testing.T) {
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
	client := beadclient.NewCityScopedClient(ts.URL, "alpha")

	var out, errb bytes.Buffer

	if code := DispatchViaAPI(client, "close", []string{"gcg-2"}, &out, &errb); code != 0 {
		t.Fatalf("close via API: code=%d err=%s", code, errb.String())
	}
	if gotMethod != http.MethodPost || gotPath != "/v0/city/alpha/bead/gcg-2/close" {
		t.Fatalf("close -> %s %s, want POST /v0/city/alpha/bead/gcg-2/close", gotMethod, gotPath)
	}

	out.Reset()
	errb.Reset()
	if code := DispatchViaAPI(client, "update", []string{"gcg-2", "--set-metadata", "gc.outcome=pass", "--status", "closed"}, &out, &errb); code != 0 {
		t.Fatalf("update via API: code=%d err=%s", code, errb.String())
	}
	if gotMethod != http.MethodPost || gotPath != "/v0/city/alpha/bead/gcg-2/update" {
		t.Fatalf("update -> %s %s", gotMethod, gotPath)
	}
	if gotBody["status"] != "closed" {
		t.Fatalf("update body status = %v, want closed", gotBody["status"])
	}
	if md, ok := gotBody["metadata"].(map[string]any); !ok || md["gc.outcome"] != "pass" {
		t.Fatalf("update body metadata = %v, want gc.outcome=pass", gotBody["metadata"])
	}

	out.Reset()
	errb.Reset()
	if code := DispatchViaAPI(client, "update", []string{"gcg-2", "-d", "replacement description"}, &out, &errb); code != 0 {
		t.Fatalf("short description update via API: code=%d err=%s", code, errb.String())
	}
	if gotMethod != http.MethodPost || gotPath != "/v0/city/alpha/bead/gcg-2/update" {
		t.Fatalf("short description update -> %s %s", gotMethod, gotPath)
	}
	if gotBody["description"] != "replacement description" {
		t.Fatalf("short description update body = %v, want replacement description", gotBody["description"])
	}

	out.Reset()
	errb.Reset()
	if code := DispatchViaAPI(client, "ready", []string{"--assignee=worker", "--json"}, &out, &errb); code != 0 {
		t.Fatalf("ready via API: code=%d err=%s", code, errb.String())
	}
	if gotMethod != http.MethodGet || gotPath != "/v0/city/alpha/beads/ready" {
		t.Fatalf("ready -> %s %s, want GET /v0/city/alpha/beads/ready", gotMethod, gotPath)
	}
}

func TestParseUpdateOptsDescription(t *testing.T) {
	opts, err := ParseUpdateOpts([]string{"gcg-2", "-d", "--body-file"})
	if err != nil {
		t.Fatalf("ParseUpdateOpts(-d): %v", err)
	}
	if opts.Description == nil || *opts.Description != "--body-file" {
		t.Fatalf("ParseUpdateOpts(-d) description = %v, want %q", opts.Description, "--body-file")
	}

	if _, err := ParseUpdateOpts([]string{"gcg-2", "-d"}); err == nil {
		t.Fatal("ParseUpdateOpts(-d without value) = nil error, want error")
	}
}

// TestDispatchViaAPIList proves `bd list` routes to GET /v0/beads with the
// parsed status/assignee/limit filters — the GUPP-hook AssignedInProgressQuery.
func TestDispatchViaAPIList(t *testing.T) {
	var gotMethod, gotPath, gotQuery string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath, gotQuery = r.Method, r.URL.Path, r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"beads": []any{}}) //nolint:errcheck
	}))
	defer ts.Close()
	client := beadclient.NewCityScopedClient(ts.URL, "alpha")

	var out, errb bytes.Buffer
	if code := DispatchViaAPI(client, "list",
		[]string{"--status", "in_progress", "--assignee=worker", "--json", "--limit", "25"}, &out, &errb); code != 0 {
		t.Fatalf("list via API: code=%d err=%s", code, errb.String())
	}
	if gotMethod != http.MethodGet || gotPath != "/v0/city/alpha/beads" {
		t.Fatalf("list -> %s %s, want GET /v0/city/alpha/beads", gotMethod, gotPath)
	}
	for _, want := range []string{"status=in_progress", "assignee=worker", "limit=25"} {
		if !strings.Contains(gotQuery, want) {
			t.Fatalf("list query %q missing %q", gotQuery, want)
		}
	}
}
