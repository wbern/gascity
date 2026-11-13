package scripts_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// localParallelRunnerGuard is the refusal script the fork's
// scripts/test-local-parallel delegates to. The fork disables the local
// parallel fan-out runner because its concurrent compiler load exhausts a
// workstation (see TESTING.md); TestParallelLocalTestEntrypointsFailClosed
// pins that refusal.
const localParallelRunnerGuard = "refuse-local-parallel-test"

// skipIfLocalParallelRunnerDisabled skips upstream assertions about the
// fan-out runner's internals when scripts/test-local-parallel is the fork's
// refusal stub. A runner that is restored to a real fan-out implementation
// falls through to those assertions again.
func skipIfLocalParallelRunnerDisabled(t *testing.T, content string) {
	t.Helper()
	if strings.Contains(content, localParallelRunnerGuard) {
		t.Skipf("scripts/test-local-parallel delegates to %s; the fan-out runner is disabled in this fork", localParallelRunnerGuard)
	}
}

func readLocalParallelRunner(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(repoRoot(t), "scripts", "test-local-parallel"))
	if err != nil {
		t.Fatalf("read scripts/test-local-parallel: %v", err)
	}
	return string(data)
}

// prePushSuiteHook is the upstream script the pre-push hook delegates to for
// the push-time test suite.
const prePushSuiteHook = "push-suite.sh"

// skipIfPrePushSuiteDisabled skips upstream assertions about the push-time
// test suite when the fork's pre-push hook does not run it. The fork keeps the
// hook non-blocking for tests (TestPrePushDoesNotRunParallelLocalTests pins
// that); .githooks/lib/push-suite.sh is retained so a hook that is restored to
// delegate to it falls through to these assertions again.
func skipIfPrePushSuiteDisabled(t *testing.T) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(repoRoot(t), ".githooks", "pre-push"))
	if err != nil {
		t.Fatalf("read pre-push hook: %v", err)
	}
	if !strings.Contains(string(data), prePushSuiteHook) {
		t.Skipf(".githooks/pre-push does not delegate to %s; the push-time suite is disabled in this fork", prePushSuiteHook)
	}
}
