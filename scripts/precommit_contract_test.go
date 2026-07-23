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

func TestAggregateLocalTestEntrypointsFailClosed(t *testing.T) {
	repoRoot := repoRoot(t)
	guard := "./scripts/refuse-local-aggregate-test"
	makefile, err := os.ReadFile(filepath.Join(repoRoot, "Makefile"))
	if err != nil {
		t.Fatalf("read Makefile: %v", err)
	}
	for _, target := range []string{
		"test",
		"test-mac",
		"test-fast-parallel",
		"test-cmd-gc-process",
		"test-cmd-gc-process-parallel",
		"test-acceptance-all",
		"test-integration",
		"test-integration-shards",
		"test-integration-shards-parallel",
		"test-integration-shards-cover",
		"test-local-full-parallel",
		"test-cover",
		"test-cover-mac",
	} {
		want := target + ":\n\t@$(LOCAL_AGGREGATE_TEST_GUARD) \"make $@\""
		if !strings.Contains(string(makefile), want) {
			t.Fatalf("%s must fail closed before prerequisites or test work; missing %q", target, want)
		}
	}
	if !strings.Contains(string(makefile), "LOCAL_AGGREGATE_TEST_GUARD := "+guard) {
		t.Fatalf("Makefile must use %s for every disabled aggregate target", guard)
	}
	guardScript, err := os.ReadFile(filepath.Join(repoRoot, "scripts", "refuse-local-aggregate-test"))
	if err != nil {
		t.Fatalf("read aggregate-test guard: %v", err)
	}
	for _, forbidden := range []string{"go test", "xargs", "mktemp"} {
		if strings.Contains(string(guardScript), forbidden) {
			t.Fatalf("aggregate-test guard must not invoke %q", forbidden)
		}
	}
	if !strings.Contains(string(guardScript), "exit 2") {
		t.Fatal("aggregate-test guard must return a clear non-zero refusal")
	}

	localRunner, err := os.ReadFile(filepath.Join(repoRoot, "scripts", "test-local-parallel"))
	if err != nil {
		t.Fatalf("read local parallel runner: %v", err)
	}
	if !strings.Contains(string(localRunner), "refuse-local-aggregate-test") {
		t.Fatal("direct scripts/test-local-parallel calls must use the same guard")
	}
	for _, forbidden := range []string{"gc_test_slice_reexec", "xargs", "go env", "mktemp"} {
		if strings.Contains(string(localRunner), forbidden) {
			t.Fatalf("disabled local runner must stop before %q", forbidden)
		}
	}
}

func TestPrePushDoesNotRunAggregateLocalTests(t *testing.T) {
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
		t.Fatal("pre-push hook must explain that aggregate local tests are disabled")
	}

	workflow, err := os.ReadFile(filepath.Join(repoRoot, ".github", "workflows", "mac-regression.yml"))
	if err != nil {
		t.Fatalf("read Mac workflow: %v", err)
	}
	for _, want := range []string{"make test-mac-ci", "make test-cover-mac-ci"} {
		if !strings.Contains(string(workflow), want) {
			t.Fatalf("Mac CI must retain its broad coverage via %q", want)
		}
	}
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
