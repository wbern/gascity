package scripts_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestPreCommitFormatterPreservesFileMode(t *testing.T) {
	repoRoot := repoRoot(t)
	binDir := t.TempDir()
	fakeLint := filepath.Join(binDir, "golangci-lint")
	writeExecutable(t, fakeLint, `#!/usr/bin/env bash
set -euo pipefail
if [ "$#" -ne 2 ] || [ "$1" != "fmt" ] || [ "$2" != "--stdin" ]; then
  echo "unexpected golangci-lint args: $*" >&2
  exit 2
fi
cat
printf '\n'
`)

	source := filepath.Join(t.TempDir(), "needs_format.go")
	if err := os.WriteFile(source, []byte("package main"), 0o644); err != nil {
		t.Fatalf("write source: %v", err)
	}

	cmd := exec.Command(filepath.Join(repoRoot, "scripts", "precommit-format-staged-go"))
	cmd.Dir = repoRoot
	cmd.Env = []string{
		"PATH=" + binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
		"HOME=" + t.TempDir(),
		"TMPDIR=" + t.TempDir(),
	}
	cmd.Stdin = strings.NewReader(source + "\n")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("precommit formatter failed: %v\n%s", err, out)
	}

	info, err := os.Stat(source)
	if err != nil {
		t.Fatalf("stat formatted source: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o644 {
		t.Fatalf("formatted source mode = %o, want 644", got)
	}
	content, err := os.ReadFile(source)
	if err != nil {
		t.Fatalf("read formatted source: %v", err)
	}
	if string(content) != "package main\n" {
		t.Fatalf("formatted content = %q, want package main with newline", content)
	}
}

func TestParallelLocalTestEntrypointsFailClosed(t *testing.T) {
	repoRoot := repoRoot(t)
	guard := "./scripts/refuse-local-parallel-test"
	makefile, err := os.ReadFile(filepath.Join(repoRoot, "Makefile"))
	if err != nil {
		t.Fatalf("read Makefile: %v", err)
	}
	for _, target := range []string{
		"test-fast-parallel",
		"test-cmd-gc-process-parallel",
		"test-integration-shards-parallel",
		"test-local-full-parallel",
	} {
		want := target + ":\n\t@$(LOCAL_PARALLEL_TEST_GUARD) \"make $@\""
		if !strings.Contains(string(makefile), want) {
			t.Fatalf("%s must fail closed before prerequisites or test work; missing %q", target, want)
		}
	}
	for _, target := range []string{
		"test",
		"test-mac",
		"test-cmd-gc-process",
		"test-acceptance-all",
		"test-integration",
		"test-integration-shards",
		"test-integration-shards-cover",
		"test-cover",
		"test-cover-mac",
	} {
		forbidden := target + ":\n\t@$(LOCAL_PARALLEL_TEST_GUARD)"
		if strings.Contains(string(makefile), forbidden) {
			t.Fatalf("%s is serial or explicitly scoped and must remain runnable", target)
		}
	}
	if !strings.Contains(string(makefile), "LOCAL_PARALLEL_TEST_GUARD := "+guard) {
		t.Fatalf("Makefile must use %s for every disabled parallel target", guard)
	}
	guardScript, err := os.ReadFile(filepath.Join(repoRoot, "scripts", "refuse-local-parallel-test"))
	if err != nil {
		t.Fatalf("read parallel-test guard: %v", err)
	}
	for _, forbidden := range []string{"go test", "xargs", "mktemp"} {
		if strings.Contains(string(guardScript), forbidden) {
			t.Fatalf("parallel-test guard must not invoke %q", forbidden)
		}
	}
	if !strings.Contains(string(guardScript), "exit 2") {
		t.Fatal("parallel-test guard must return a clear non-zero refusal")
	}

	localRunner, err := os.ReadFile(filepath.Join(repoRoot, "scripts", "test-local-parallel"))
	if err != nil {
		t.Fatalf("read local parallel runner: %v", err)
	}
	if !strings.Contains(string(localRunner), "refuse-local-parallel-test") {
		t.Fatal("direct scripts/test-local-parallel calls must use the same guard")
	}
	for _, forbidden := range []string{"gc_test_slice_reexec", "xargs", "go env", "mktemp"} {
		if strings.Contains(string(localRunner), forbidden) {
			t.Fatalf("disabled local runner must stop before %q", forbidden)
		}
	}
}

func TestPrePushDoesNotRunParallelLocalTests(t *testing.T) {
	repoRoot := repoRoot(t)
	script, err := os.ReadFile(filepath.Join(repoRoot, ".githooks", "pre-push"))
	if err != nil {
		t.Fatalf("read pre-push hook: %v", err)
	}
	content := string(script)
	for _, forbidden := range []string{"make test", "test-local-parallel", "go test"} {
		if strings.Contains(content, forbidden) {
			t.Fatalf("pre-push hook must not invoke %q", forbidden)
		}
	}
	if !strings.Contains(content, "disabled") {
		t.Fatal("pre-push hook must explain that parallel local tests are disabled")
	}

	workflow, err := os.ReadFile(filepath.Join(repoRoot, ".github", "workflows", "mac-regression.yml"))
	if err != nil {
		t.Fatalf("read Mac workflow: %v", err)
	}
	for _, want := range []string{"make test-mac", "make test-cover-mac"} {
		if !strings.Contains(string(workflow), want) {
			t.Fatalf("Mac CI must retain its broad coverage via %q", want)
		}
	}
	for _, unwanted := range []string{"make test-mac-ci", "make test-cover-mac-ci"} {
		if strings.Contains(string(workflow), unwanted) {
			t.Fatalf("Mac CI should use the ordinary serial target, not fork-only alias %q", unwanted)
		}
	}
}

func TestPreCommitRegeneratesDashboardClientOnSpecChange(t *testing.T) {
	repoRoot := repoRoot(t)
	script, err := os.ReadFile(filepath.Join(repoRoot, ".githooks", "pre-commit"))
	if err != nil {
		t.Fatalf("read pre-commit hook: %v", err)
	}
	content := string(script)

	npmBlockStart := strings.Index(content, "if command -v npm")
	if npmBlockStart < 0 {
		t.Fatal("pre-commit hook must guard dashboard regeneration on npm availability")
	}
	npmBlock := content[npmBlockStart:]

	genClientIdx := strings.Index(npmBlock, "npm run generate:client")
	if genClientIdx < 0 {
		t.Fatal("pre-commit hook must run 'npm run generate:client' when internal/api/openapi.json changes — " +
			"make dashboard-check only builds and typechecks against whatever client is already on disk, it never " +
			"regenerates it (that's make dashboard-ci's job, which the hook never calls). A spec-only commit " +
			"currently ships a stale generated TS client (see PR #4627, #4607)")
	}

	dashboardCheckIdx := strings.Index(npmBlock, "make dashboard-check")
	if dashboardCheckIdx < 0 {
		t.Fatal("pre-commit hook must still run make dashboard-check dashboard-smoke")
	}
	if genClientIdx > dashboardCheckIdx {
		t.Fatal("pre-commit hook must regenerate the dashboard client BEFORE typecheck/build, so a client that " +
			"doesn't match the new spec fails typecheck immediately instead of silently building against stale types")
	}

	clientAddNeedle := "git add internal/api/dashboardspa/web/shared/src/generated/gc-supervisor-client"
	genClientAddIdx := strings.Index(npmBlock, clientAddNeedle)
	if genClientAddIdx < 0 {
		t.Fatal("pre-commit hook must stage the regenerated dashboard client so a spec-only commit includes it")
	}
	if genClientAddIdx < genClientIdx {
		t.Fatal("pre-commit hook must stage the generated client after regenerating it, not before")
	}

	if strings.Contains(content, "regenerate the TS types, typecheck, and rebuild") {
		t.Fatal("pre-commit hook's dashboard block comment must not claim it regenerates the TS types unless it " +
			"actually calls npm run generate:client")
	}

	if !strings.Contains(content, `echo "warning: npm not on PATH`) {
		t.Fatal("pre-commit hook must still warn and no-op cleanly when npm is not on PATH")
	}
}

func TestPreCommitReachesDashboardBlockWhenOnlySpecFileStaged(t *testing.T) {
	repoRoot := repoRoot(t)
	hookPath := filepath.Join(repoRoot, ".githooks", "pre-commit")

	tmpRepo := t.TempDir()
	runGit := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = tmpRepo
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@test.invalid",
			"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@test.invalid",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}

	specPath := filepath.Join(tmpRepo, "internal", "api", "openapi.json")
	clientPath := filepath.Join(tmpRepo, "internal", "api", "dashboardspa", "web", "shared", "src", "generated", "gc-supervisor-client")
	distPath := filepath.Join(tmpRepo, "internal", "api", "dashboardspa", "dist", "placeholder")

	runGit("init")
	writeTestFile(t, specPath, "{}\n")
	writeTestFile(t, clientPath, "placeholder\n")
	writeTestFile(t, distPath, "placeholder\n")
	runGit("add", "-A")
	runGit("commit", "-m", "init")

	// Stage ONLY a change to openapi.json -- no .go, web-src, or doc files
	// are staged, matching the reviewer's criterion-2 repro scenario.
	writeTestFile(t, specPath, `{"changed":true}`+"\n")
	runGit("add", "internal/api/openapi.json")

	binDir := t.TempDir()
	npmLog := filepath.Join(binDir, "npm.log")
	writeExecutable(t, filepath.Join(binDir, "npm"), `#!/usr/bin/env bash
set -euo pipefail
echo "$*" >> "`+npmLog+`"
exit 0
`)
	// Stub make: this test verifies the control-flow reaches the dashboard
	// block at all (the reviewer's criterion-2 gap), not the real
	// dashboard-check/dashboard-smoke targets, which need the full repo.
	writeExecutable(t, filepath.Join(binDir, "make"), `#!/usr/bin/env bash
exit 0
`)

	cmd := exec.Command("bash", hookPath)
	cmd.Dir = tmpRepo
	cmd.Env = []string{
		"PATH=" + binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
		"HOME=" + t.TempDir(),
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("pre-commit hook failed: %v\n%s", err, out)
	}

	logContent, readErr := os.ReadFile(npmLog)
	if readErr != nil {
		t.Fatalf("pre-commit hook exited early and never invoked npm when only internal/api/openapi.json was "+
			"staged -- the go/web/docs early guard must not skip a spec-only commit (hook output: %s)", out)
	}
	if !strings.Contains(string(logContent), "generate:client") {
		t.Fatalf("pre-commit hook must run 'npm run generate:client' when only internal/api/openapi.json is "+
			"staged, got npm invocations:\n%s", logContent)
	}
}

func TestPreCommitFailsClosedWhenSpecStagedButNpmAbsent(t *testing.T) {
	repoRoot := repoRoot(t)
	hookPath := filepath.Join(repoRoot, ".githooks", "pre-commit")

	tmpRepo := t.TempDir()
	runGit := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = tmpRepo
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@test.invalid",
			"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@test.invalid",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}

	specPath := filepath.Join(tmpRepo, "internal", "api", "openapi.json")

	runGit("init")
	writeTestFile(t, specPath, "{}\n")
	runGit("add", "-A")
	runGit("commit", "-m", "init")

	// Stage ONLY a change to openapi.json -- same repro shape as
	// TestPreCommitReachesDashboardBlockWhenOnlySpecFileStaged, but this
	// time npm itself is unreachable on PATH.
	writeTestFile(t, specPath, `{"changed":true}`+"\n")
	runGit("add", "internal/api/openapi.json")

	cmd := exec.Command("bash", hookPath)
	cmd.Dir = tmpRepo
	cmd.Env = []string{
		"PATH=" + restrictedPathWithoutNpm(t, nil),
		"HOME=" + t.TempDir(),
	}
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("pre-commit hook must fail when internal/api/openapi.json is staged and npm is not on PATH "+
			"-- the generated TS client can't be regenerated, so the commit would silently ship a stale "+
			"client with no enforcement until CI runs. Hook exited 0, output:\n%s", out)
	}
	if !strings.Contains(string(out), "npm ci") || !strings.Contains(string(out), "generate:client") {
		t.Fatalf("pre-commit hook's npm-absent+spec-staged failure must name the exact recovery command "+
			"(cd internal/api/dashboardspa/web && npm ci && npm run generate:client), got:\n%s", out)
	}
}

func TestPreCommitFailsClosedWhenGoBlockStagesSpecAsSideEffectAndNpmAbsent(t *testing.T) {
	repoRoot := repoRoot(t)
	hookPath := filepath.Join(repoRoot, ".githooks", "pre-commit")

	tmpRepo := t.TempDir()
	runGit := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = tmpRepo
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@test.invalid",
			"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@test.invalid",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}

	goFilePath := filepath.Join(tmpRepo, "main.go")
	specPath := filepath.Join(tmpRepo, "internal", "api", "openapi.json")
	formatStagedGoPath := filepath.Join(tmpRepo, "scripts", "precommit-format-staged-go")
	// Every path the Go block unconditionally `git add`s after each
	// generation step must already exist on disk, or that `git add` fails
	// closed under `set -euo pipefail` before the hook ever reaches the
	// npm-absent branch this test targets.
	generatedPaths := []string{
		specPath,
		filepath.Join(tmpRepo, "docs", "reference", "schema", "openapi.json"),
		filepath.Join(tmpRepo, "docs", "reference", "schema", "openapi.txt"),
		filepath.Join(tmpRepo, "internal", "api", "genclient", "client_gen.go"),
		filepath.Join(tmpRepo, "docs", "reference", "schema", "city-schema.json"),
		filepath.Join(tmpRepo, "docs", "reference", "schema", "city-schema.txt"),
		filepath.Join(tmpRepo, "docs", "reference", "config.md"),
		filepath.Join(tmpRepo, "docs", "reference", "cli.md"),
	}

	runGit("init")
	writeTestFile(t, goFilePath, "package main\n\nfunc main() {}\n")
	for _, p := range generatedPaths {
		writeTestFile(t, p, "{}\n")
	}
	if err := os.MkdirAll(filepath.Dir(formatStagedGoPath), 0o755); err != nil {
		t.Fatalf("create parent for %s: %v", formatStagedGoPath, err)
	}
	writeExecutable(t, formatStagedGoPath, "#!/usr/bin/env bash\nexit 0\n")
	runGit("add", "-A")
	runGit("commit", "-m", "init")

	// Stage ONLY a .go file -- internal/api/openapi.json is untouched by the
	// user's own `git add`. The hook's own Go block (staged_go_files branch)
	// regenerates and stages openapi.json as a SIDE EFFECT via
	// `go run ./cmd/genspec`, which is exactly the #4627/#4607 staleness
	// trap the npm-present branch re-reads for (fresh spec_changed) but
	// which the npm-absent fail-closed branch used to miss (ga-jg89a5): it
	// checked a snapshot taken before the hook ran at all, so it never saw
	// the spec this commit was actually about to ship.
	writeTestFile(t, goFilePath, "package main\n\nfunc main() { println(1) }\n")
	runGit("add", "main.go")

	goStub := `#!/usr/bin/env bash
set -euo pipefail
if [ "$1" = "run" ] && [ "$2" = "./cmd/genspec" ]; then
  printf '{"changed":true}\n' > internal/api/openapi.json
fi
exit 0
`

	cmd := exec.Command("bash", hookPath)
	cmd.Dir = tmpRepo
	cmd.Env = []string{
		"PATH=" + restrictedPathWithoutNpm(t, map[string]string{
			"make": "#!/usr/bin/env bash\nexit 0\n",
			// Stands in for format/lint/genspec/genclient/genschema/vet.
			// Only `run ./cmd/genspec` has an observable side effect
			// (rewriting internal/api/openapi.json, which the hook's own
			// `git add` then stages), matching what the real cmd/genspec
			// does against a live Huma API -- the rest of the Go block is
			// exercised for control-flow only.
			"go": goStub,
		}),
		"HOME=" + t.TempDir(),
	}
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("pre-commit hook must fail when its own Go block stages internal/api/openapi.json as a side "+
			"effect (go run ./cmd/genspec, triggered by staging a .go file) and npm is not on PATH -- the "+
			"generated TS client can't be regenerated, so the commit would silently ship a stale client with "+
			"no enforcement until CI runs. Hook exited 0, output:\n%s", out)
	}
	if !strings.Contains(string(out), "npm ci") || !strings.Contains(string(out), "generate:client") {
		t.Fatalf("pre-commit hook's npm-absent+spec-staged-as-side-effect failure must name the exact "+
			"recovery command (cd internal/api/dashboardspa/web && npm ci && npm run generate:client), got:\n%s", out)
	}
}

func TestPreCommitWarnsOnlyWhenNpmAbsentAndSpecNotStaged(t *testing.T) {
	repoRoot := repoRoot(t)
	hookPath := filepath.Join(repoRoot, ".githooks", "pre-commit")

	tmpRepo := t.TempDir()
	runGit := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = tmpRepo
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@test.invalid",
			"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@test.invalid",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}

	docPath := filepath.Join(tmpRepo, "README.md")

	runGit("init")
	writeTestFile(t, docPath, "hello\n")
	runGit("add", "-A")
	runGit("commit", "-m", "init")

	// Stage a docs-only change -- internal/api/openapi.json is untouched,
	// so npm's absence must stay a warning, not a hard failure. staged_docs
	// being non-empty also exercises `make check-docs`, so stub `make` as a
	// no-op; the fixture repo has none of the real doc-lint machinery.
	writeTestFile(t, docPath, "hello again\n")
	runGit("add", "README.md")

	cmd := exec.Command("bash", hookPath)
	cmd.Dir = tmpRepo
	cmd.Env = []string{
		"PATH=" + restrictedPathWithoutNpm(t, map[string]string{
			"make": "#!/usr/bin/env bash\nexit 0\n",
		}),
		"HOME=" + t.TempDir(),
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("pre-commit hook must still succeed (warn-only) when npm is absent and "+
			"internal/api/openapi.json is NOT staged -- contributors without Node tooling must not be "+
			"blocked on unrelated commits, got exit error: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "npm not on PATH") {
		t.Fatalf("pre-commit hook should still warn when npm is absent, got:\n%s", out)
	}
}

// restrictedPathWithoutNpm builds a PATH containing only symlinks to the
// real bash and git (plus any provided stub scripts), guaranteeing npm is
// unreachable regardless of what's installed on the test host -- falling
// back to the ambient PATH would make these tests flaky on any machine
// that actually has npm installed.
func restrictedPathWithoutNpm(t *testing.T, stubs map[string]string) string {
	t.Helper()
	binDir := t.TempDir()
	for _, name := range []string{"bash", "git", "xargs"} {
		realPath, err := exec.LookPath(name)
		if err != nil {
			t.Fatalf("resolve real %s on test host PATH: %v", name, err)
		}
		if err := os.Symlink(realPath, filepath.Join(binDir, name)); err != nil {
			t.Fatalf("symlink %s: %v", name, err)
		}
	}
	for name, script := range stubs {
		writeExecutable(t, filepath.Join(binDir, name), script)
	}
	return binDir
}

func TestNativeDoltliteBeadsTargetRunsTaggedSuite(t *testing.T) {
	repoRoot := repoRoot(t)
	makefile, err := os.ReadFile(filepath.Join(repoRoot, "Makefile"))
	if err != nil {
		t.Fatalf("read Makefile: %v", err)
	}
	if err := validateNativeDoltliteMakefile(string(makefile)); err != nil {
		t.Fatalf("test-native-doltlite-beads recipe: %v", err)
	}

	cmd := exec.Command("make", "-n", "test-native-doltlite-beads")
	cmd.Dir = repoRoot
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("make -n test-native-doltlite-beads failed: %v\n%s", err, out)
	}
	command := string(out)
	if err := validateNativeDoltliteDryRun(command); err != nil {
		t.Fatalf("make -n test-native-doltlite-beads output: %v", err)
	}
	for _, want := range []string{
		"CGO_ENABLED=0",
		"-tags gascity_native_beads",
		"-run '^TestDoltlite'",
		"./internal/beads",
	} {
		if !strings.Contains(command, want) {
			t.Fatalf("test-native-doltlite-beads recipe missing %q:\n%s", want, command)
		}
	}
	for _, banned := range []string{
		"CGO_ENABLED=1",
		"cgo,gascity_native_beads",
	} {
		if strings.Contains(command, banned) {
			t.Fatalf("test-native-doltlite-beads recipe must not contain %q (doltlite store now uses pure-Go modernc):\n%s", banned, command)
		}
	}
	assertNativeDoltliteBeadsSelectionMatchesTaggedOwners(t, repoRoot)
}

func repoRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	return filepath.Dir(wd)
}

func writeExecutable(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatalf("write executable %s: %v", path, err)
	}
}
