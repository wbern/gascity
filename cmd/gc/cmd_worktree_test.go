package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/testutil"
	"github.com/gastownhall/gascity/internal/worktree"
)

func TestWorktreeScanTextReportUsesManagedRootsAndSkipsLiveSessions(t *testing.T) {
	clearGCEnv(t)

	cityDir := t.TempDir()
	rigRoot := filepath.Join(cityDir, "rigs", "demo")
	liveDir := filepath.Join(cityDir, ".gc", "worktrees", "live")
	orphanDir := filepath.Join(cityDir, ".gc", "worktrees", "orphan")
	nestedStray := filepath.Join(rigRoot, "nested-stray")

	for _, dir := range []string{liveDir, orphanDir, nestedStray} {
		if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	restore := stubWorktreeScanDeps(t)
	worktreeResolveCity = func() (string, error) { return cityDir, nil }
	worktreeLoadCityConfig = func(string, ...io.Writer) (*config.City, error) {
		return &config.City{Rigs: []config.Rig{{Name: "demo", Path: filepath.Join("rigs", "demo")}}}, nil
	}
	worktreeOpenCityStoreAt = func(string) (beads.Store, error) { return nil, nil }
	worktreeListAllSessionBeads = func(beads.Store, beads.ListQuery) ([]beads.Bead, error) {
		return []beads.Bead{{ID: "gc-session-1", Metadata: map[string]string{"worker_dir": liveDir}}}, nil
	}
	newGitProbe = func(workDir string) gitProbe {
		switch filepath.Clean(workDir) {
		case filepath.Clean(orphanDir):
			return fakeStrayProbe{isRepo: true}
		case filepath.Clean(nestedStray):
			return fakeStrayProbe{isRepo: true, uncommitted: true}
		default:
			t.Fatalf("unexpected git probe for %q", workDir)
			return nil
		}
	}
	t.Cleanup(restore)

	var stdout, stderr bytes.Buffer
	cmd := newWorktreeCmd(&stdout, &stderr)
	cmd.SetArgs([]string{"scan"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute() = %v; stderr=%s", err, stderr.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}

	out := stdout.String()
	for _, want := range []string{
		"RECLAIMABLE",
		orphanDir,
		nestedStray,
		// gcw-kb6r (#41) widened this reason: the gate now also holds a
		// worktree whose only content is ignored files, and the text says so.
		"uncommitted or ignored work",
		"2 stray checkout(s): 1 reclaimable, 1 kept",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("stdout missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, liveDir) {
		t.Fatalf("stdout unexpectedly includes live-bound worktree %q:\n%s", liveDir, out)
	}
}

func TestWorktreeScanJSONOutput(t *testing.T) {
	clearGCEnv(t)

	restore := stubWorktreeScanDeps(t)
	worktreeResolveCity = func() (string, error) { return t.TempDir(), nil }
	worktreeLoadCityConfig = func(string, ...io.Writer) (*config.City, error) { return &config.City{}, nil }
	worktreeOpenCityStoreAt = func(string) (beads.Store, error) { return nil, nil }
	worktreeListAllSessionBeads = func(beads.Store, beads.ListQuery) ([]beads.Bead, error) { return nil, nil }
	worktreeScanStrayWorktrees = func([]string, map[string]bool, func(string) gitProbe) ([]strayWorktree, error) {
		return []strayWorktree{{Path: "/tmp/orphan", Reclaimable: true}}, nil
	}
	t.Cleanup(restore)

	var stdout, stderr bytes.Buffer
	cmd := newWorktreeCmd(&stdout, &stderr)
	cmd.SetArgs([]string{"scan", "--json"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute() = %v; stderr=%s", err, stderr.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}

	// The payload is an {schema_version, ok, ...} envelope, not a bare array:
	// the CLI's JSON contract rejects anything else, which is why this flag
	// was inert for its entire life despite this test passing.
	var got worktreeScanJSON
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("stdout is not valid JSON: %v\n%s", err, stdout.String())
	}
	if got.SchemaVersion != worktreeJSONSchemaVersion || !got.OK {
		t.Fatalf("envelope = %#v, want schema_version %q and ok true", got, worktreeJSONSchemaVersion)
	}
	if len(got.Strays) != 1 || got.Strays[0].Path != "/tmp/orphan" || !got.Strays[0].Reclaimable {
		t.Fatalf("decoded JSON = %#v, want one reclaimable orphan", got.Strays)
	}
}

func TestWorktreeScanEmptyCase(t *testing.T) {
	clearGCEnv(t)

	restore := stubWorktreeScanDeps(t)
	worktreeResolveCity = func() (string, error) { return t.TempDir(), nil }
	worktreeLoadCityConfig = func(string, ...io.Writer) (*config.City, error) { return &config.City{}, nil }
	worktreeOpenCityStoreAt = func(string) (beads.Store, error) { return nil, nil }
	worktreeListAllSessionBeads = func(beads.Store, beads.ListQuery) ([]beads.Bead, error) { return nil, nil }
	worktreeScanStrayWorktrees = func([]string, map[string]bool, func(string) gitProbe) ([]strayWorktree, error) {
		return nil, nil
	}
	t.Cleanup(restore)

	var stdout, stderr bytes.Buffer
	cmd := newWorktreeCmd(&stdout, &stderr)
	cmd.SetArgs([]string{"scan"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute() = %v; stderr=%s", err, stderr.String())
	}
	if got := strings.TrimSpace(stdout.String()); got != "No stray worktrees found under managed roots." {
		t.Fatalf("stdout = %q, want empty-case message", got)
	}
}

func TestWorktreeScanFailsClosedWhenStoreUnavailable(t *testing.T) {
	clearGCEnv(t)

	restore := stubWorktreeScanDeps(t)
	worktreeResolveCity = func() (string, error) { return t.TempDir(), nil }
	worktreeLoadCityConfig = func(string, ...io.Writer) (*config.City, error) { return &config.City{}, nil }
	worktreeOpenCityStoreAt = func(string) (beads.Store, error) { return nil, errors.New("store offline") }
	t.Cleanup(restore)

	var stdout, stderr bytes.Buffer
	cmd := newWorktreeCmd(&stdout, &stderr)
	cmd.SetArgs([]string{"scan"})
	err := cmd.Execute()
	if !errors.Is(err, errExit) {
		t.Fatalf("Execute() = %v, want errExit", err)
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q, want empty on failure", stdout.String())
	}
	if !strings.Contains(stderr.String(), "loading live session set: store offline") {
		t.Fatalf("stderr = %q, want live-set failure", stderr.String())
	}
}

func stubWorktreeScanDeps(t *testing.T) func() {
	t.Helper()
	origResolveCity := worktreeResolveCity
	origLoadCityConfig := worktreeLoadCityConfig
	origOpenCityStoreAt := worktreeOpenCityStoreAt
	origListAllSessionBeads := worktreeListAllSessionBeads
	origScanStrayWorktrees := worktreeScanStrayWorktrees
	origNewGitProbe := newGitProbe
	return func() {
		worktreeResolveCity = origResolveCity
		worktreeLoadCityConfig = origLoadCityConfig
		worktreeOpenCityStoreAt = origOpenCityStoreAt
		worktreeListAllSessionBeads = origListAllSessionBeads
		worktreeScanStrayWorktrees = origScanStrayWorktrees
		newGitProbe = origNewGitProbe
	}
}

func worktreeTestRepo(t *testing.T) (string, string) {
	t.Helper()
	return testutil.InitGitRepo(t)
}

func managedWorktreeCmdOpts(repo, root, path, base string) worktreeCmdOpts {
	return worktreeCmdOpts{
		Repo:       repo,
		Root:       root,
		Path:       path,
		Branch:     "work/gc-test",
		Base:       base,
		BeadID:     "gc-test",
		StoreRef:   "gascity",
		Creator:    "test",
		Owner:      "gc-sling",
		Generation: "1",
		Lifecycle:  worktree.LifecycleActive,
	}
}

func TestCmdWorktreeEnsureCreatesAndVerifies(t *testing.T) {
	repo, base := worktreeTestRepo(t)
	root := t.TempDir()
	wt := filepath.Join(root, "wt")
	var stdout, stderr bytes.Buffer

	opts := managedWorktreeCmdOpts(repo, root, wt, base)
	opts.JSON = true
	code := runWorktreeEnsure(opts, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("ensure exit = %d, stderr: %s", code, stderr.String())
	}
	var rep struct {
		Path          string `json:"path"`
		Branch        string `json:"branch"`
		Created       bool   `json:"created"`
		BranchCreated bool   `json:"branch_created"`
		Provenance    *struct {
			BaseSHA   string `json:"base_sha"`
			AttemptID string `json:"attempt_id"`
		} `json:"provenance"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &rep); err != nil {
		t.Fatalf("unmarshal ensure output %q: %v", stdout.String(), err)
	}
	if !rep.Created || !rep.BranchCreated || rep.Branch != "work/gc-test" ||
		rep.Provenance == nil || rep.Provenance.BaseSHA == "" || rep.Provenance.AttemptID == "" {
		t.Errorf("report = %+v, want created managed worktree with publishable provenance", rep)
	}

	// verify must pass on the ensured worktree.
	stdout.Reset()
	stderr.Reset()
	verifyOpts := opts
	verifyOpts.BaseSHA = rep.Provenance.BaseSHA
	code = runWorktreeVerify(verifyOpts, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("verify exit = %d, stderr: %s", code, stderr.String())
	}
}

func TestCmdWorktreeVerifyFailsOnMissing(t *testing.T) {
	repo, _ := worktreeTestRepo(t)
	root := t.TempDir()
	var stdout, stderr bytes.Buffer
	opts := managedWorktreeCmdOpts(repo, root, filepath.Join(root, "nope"), "main")
	code := runWorktreeVerify(opts, &stdout, &stderr)
	if code == 0 {
		t.Fatal("verify on missing worktree returned 0, want nonzero")
	}
	if !strings.Contains(stderr.String(), "does not exist") {
		t.Errorf("stderr %q does not explain the missing path", stderr.String())
	}
}

func TestCmdWorktreeEnsureDryRunIsPure(t *testing.T) {
	repo, base := worktreeTestRepo(t)
	root := t.TempDir()
	wt := filepath.Join(root, "wt")
	var stdout, stderr bytes.Buffer
	opts := managedWorktreeCmdOpts(repo, root, wt, base)
	opts.DryRun = true
	opts.JSON = true
	code := runWorktreeEnsure(opts, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("dry-run ensure exit = %d, stderr: %s", code, stderr.String())
	}
	if _, err := os.Stat(wt); !os.IsNotExist(err) {
		t.Error("dry-run ensure created the worktree path")
	}
	var rep struct {
		Planned []string `json:"planned"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &rep); err != nil {
		t.Fatalf("unmarshal dry-run output %q: %v", stdout.String(), err)
	}
	if len(rep.Planned) == 0 {
		t.Error("dry-run output has no planned actions")
	}
}

func TestCmdWorktreeManagedFlagsReachSpec(t *testing.T) {
	repo, base := worktreeTestRepo(t)
	root := t.TempDir()
	path := filepath.Join(root, "gc-test")
	opts := managedWorktreeCmdOpts(repo, root, path, base)
	opts.BaseSHA = strings.Repeat("a", 40)

	spec, err := opts.spec()
	if err != nil {
		t.Fatalf("spec: %v", err)
	}
	if spec.Root != root || spec.BeadID != opts.BeadID || spec.StoreRef != opts.StoreRef ||
		spec.BaseSHA != opts.BaseSHA || spec.Creator != opts.Creator || spec.Owner != opts.Owner ||
		spec.Generation != opts.Generation || spec.Lifecycle != opts.Lifecycle {
		t.Fatalf("spec = %+v, want all managed ownership fields from CLI", spec)
	}
}

func TestCmdWorktreeRegistered(t *testing.T) {
	root := newRootCmd(&bytes.Buffer{}, &bytes.Buffer{})
	for _, c := range root.Commands() {
		if c.Name() == "worktree" {
			return
		}
	}
	t.Fatal("gc worktree command is not registered on the root command")
}

func TestCmdWorktreeEnsureStrictJSONContract(t *testing.T) {
	t.Setenv("GC_JSON_CONTRACT_STRICT", "1")
	repo, base := worktreeTestRepo(t)
	root := t.TempDir()
	wt := filepath.Join(root, "wt")
	args := []string{
		"worktree", "ensure", "--json",
		"--repo", repo,
		"--root", root,
		"--path", wt,
		"--branch", "work/gc-test",
		"--base", base,
		"--bead", "gc-test",
		"--store-ref", "gascity",
		"--creator", "test",
		"--owner", "gc-sling",
		"--generation", "1",
	}
	var stdout, stderr bytes.Buffer
	if code := run(args, &stdout, &stderr); code != 0 {
		t.Fatalf("run(%v) failed: code=%d stdout=%q stderr=%q", args, code, stdout.String(), stderr.String())
	}
	if strings.Contains(stdout.String(), "json_unsupported") {
		t.Fatalf("gc worktree ensure still lacks declared JSON support: %s", stdout.String())
	}
	var result struct {
		SchemaVersion string `json:"schema_version"`
		OK            bool   `json:"ok"`
		Command       string `json:"command"`
		Action        string `json:"action"`
		Path          string `json:"path"`
		Provenance    *struct {
			AttemptID string `json:"attempt_id"`
		} `json:"provenance"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("unmarshal JSON output %q: %v", stdout.String(), err)
	}
	if result.SchemaVersion != "1" || !result.OK || result.Command != "worktree ensure" ||
		result.Action != "ensure" || result.Path != wt ||
		result.Provenance == nil || result.Provenance.AttemptID == "" {
		t.Fatalf("JSON result = %+v, want declared structured ensure output", result)
	}
	validateJSONAgainstResultSchema(t, []string{"worktree", "ensure"}, stdout.Bytes())
}

// ensureAttemptID provisions the worktree described by opts and returns the
// attempt id its ensure reported. Cleanup is authorized per provisioning
// attempt, so a caller that wants to clean up what it just created has to carry
// that id forward; routing the test through the CLI's own JSON output is also
// what proves the two commands agree on the field.
func ensureAttemptID(t *testing.T, opts *worktreeCmdOpts) string {
	t.Helper()
	jsonOpts := *opts
	jsonOpts.JSON = true
	var stdout, stderr bytes.Buffer
	if code := runWorktreeEnsure(jsonOpts, &stdout, &stderr); code != 0 {
		t.Fatalf("runWorktreeEnsure setup exit = %d (stderr: %s)", code, stderr.String())
	}
	var result struct {
		Provenance struct {
			AttemptID string `json:"attempt_id"`
		} `json:"provenance"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("unmarshal ensure output %q: %v", stdout.String(), err)
	}
	if result.Provenance.AttemptID == "" {
		t.Fatalf("ensure output %q carries no attempt id", stdout.String())
	}
	return result.Provenance.AttemptID
}

func TestCmdWorktreeCleanupJSONReportsPendingSafetyRefusal(t *testing.T) {
	repo, base := worktreeTestRepo(t)
	root := t.TempDir()
	wt := filepath.Join(root, "wt")
	opts := managedWorktreeCmdOpts(repo, root, wt, base)
	opts.AttemptID = ensureAttemptID(t, &opts)
	if err := os.WriteFile(filepath.Join(wt, "keep.txt"), []byte("keep"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	opts.JSON = true
	var stdout, stderr bytes.Buffer
	code := runWorktreeCleanup(opts, &stdout, &stderr)
	if code == 0 {
		t.Fatal("runWorktreeCleanup dirty worktree returned 0, want nonzero")
	}
	var result struct {
		SchemaVersion  string `json:"schema_version"`
		OK             bool   `json:"ok"`
		Command        string `json:"command"`
		Action         string `json:"action"`
		CleanupPending bool   `json:"cleanup_pending"`
		Error          *struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("unmarshal cleanup output %q: %v", stdout.String(), err)
	}
	if result.SchemaVersion != "1" || result.OK || result.Command != "worktree cleanup" ||
		result.Action != "cleanup" || !result.CleanupPending || result.Error == nil ||
		result.Error.Code != worktree.CleanupErrorDirty || result.Error.Message == "" {
		t.Fatalf("cleanup result = %+v, want structured cleanup_pending dirty refusal", result)
	}
	if _, err := os.Stat(filepath.Join(wt, "keep.txt")); err != nil {
		t.Fatalf("cleanup safety refusal removed WIP: %v", err)
	}
}

// TestCmdWorktreeEnsureDryRunJSONContract schema-validates the DRY-RUN output.
// Only the real-creation path was validated before, which is how planned
// provenance came to emit a zero timestamp and an empty attempt id while the
// doc comment promised both were absent. A consumer publishing that evidence
// onto a bead would have stored 0001-01-01T00:00:00Z as a creation time.
func TestCmdWorktreeEnsureDryRunJSONContract(t *testing.T) {
	t.Setenv("GC_JSON_CONTRACT_STRICT", "1")
	repo, base := worktreeTestRepo(t)
	root := t.TempDir()
	wt := filepath.Join(root, "wt")
	args := []string{
		"worktree", "ensure", "--json", "--dry-run",
		"--repo", repo,
		"--root", root,
		"--path", wt,
		"--branch", "work/gc-test",
		"--base", base,
		"--bead", "gc-test",
		"--store-ref", "gascity",
		"--creator", "test",
		"--owner", "gc-sling",
		"--generation", "1",
	}
	var stdout, stderr bytes.Buffer
	if code := run(args, &stdout, &stderr); code != 0 {
		t.Fatalf("run(%v) failed: code=%d stdout=%q stderr=%q", args, code, stdout.String(), stderr.String())
	}
	validateJSONAgainstResultSchema(t, []string{"worktree", "ensure"}, stdout.Bytes())

	if body := stdout.String(); strings.Contains(body, "0001-01-01") {
		t.Errorf("dry-run emitted a zero creation time: %s", body)
	}
	var result struct {
		Provenance map[string]any `json:"provenance"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	for _, absent := range []string{"created_at", "attempt_id"} {
		if _, present := result.Provenance[absent]; present {
			t.Errorf("dry-run provenance carries %q = %v, want it omitted until the worktree exists",
				absent, result.Provenance[absent])
		}
	}
}
