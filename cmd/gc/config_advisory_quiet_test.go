package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
)

// deliberateAlwaysFreshSessions is the number of intentional mode="always" +
// wake_mode="fresh" named sessions the fixture city declares. Five matches the
// live gc2 city that motivated gcw-qap3.4.3, where every ordinary command
// prepended one advisory line per qualifying session.
const deliberateAlwaysFreshSessions = 5

// writeDeliberateAlwaysFreshCity writes a city whose config deliberately pairs
// mode="always" named sessions with wake_mode="fresh" templates. That pairing
// is a supported, intentional restart-per-cycle policy, so config.Load reports
// it as an advisory rather than an error.
func writeDeliberateAlwaysFreshCity(t *testing.T, dir string) {
	t.Helper()
	var city strings.Builder
	city.WriteString("[workspace]\nname = \"demo\"\n")
	for i := 1; i <= deliberateAlwaysFreshSessions; i++ {
		name := fmt.Sprintf("watch%d", i)
		fmt.Fprintf(&city, "\n[[named_session]]\nname = %q\ntemplate = %q\nmode = \"always\"\nscope = \"city\"\n", name, name)

		agentDir := filepath.Join(dir, "agents", name)
		if err := os.MkdirAll(agentDir, 0o755); err != nil {
			t.Fatalf("MkdirAll(%s): %v", agentDir, err)
		}
		if err := os.WriteFile(filepath.Join(agentDir, "agent.toml"),
			[]byte("scope = \"city\"\nstart_command = \"true\"\nwake_mode = \"fresh\"\n"), 0o644); err != nil {
			t.Fatalf("write agent.toml for %s: %v", name, err)
		}
	}
	writeCityToml(t, dir, city.String())
	// The city is the local (root) pack; without pack.toml its agents/
	// directory is never scanned and the named sessions resolve to nothing.
	writePackToml(t, dir, "[pack]\nname = \"demo\"\nschema = 1\n")
}

// countAlwaysFreshAdvisoryLines counts advisory lines by classifier rather than
// by hardcoded text, so the count cannot silently drop to zero because the
// advisory wording changed.
func countAlwaysFreshAdvisoryLines(s string) int {
	n := 0
	for _, line := range strings.Split(s, "\n") {
		if config.IsAlwaysFreshWakeModeWarning(line) {
			n++
		}
	}
	return n
}

// setupDeliberateAlwaysFreshCity prepares a temp city and returns its path.
func setupDeliberateAlwaysFreshCity(t *testing.T) string {
	t.Helper()
	clearGCEnv(t)
	dir := resolvedTempDir(t)
	t.Chdir(dir)
	t.Setenv("GC_CITY_PATH", dir)
	if err := os.MkdirAll(filepath.Join(dir, ".gc"), 0o755); err != nil {
		t.Fatalf("MkdirAll(.gc): %v", err)
	}
	writeDeliberateAlwaysFreshCity(t, dir)
	return dir
}

// TestDeliberateAlwaysFreshAdvisoryIsProducedByTheFixture is the positive
// marker for the two suppression tests below. Without it, a fixture that
// stopped tripping the advisory would make "zero advisory bytes" pass
// vacuously — the silent-absence failure this epic exists to prevent.
func TestDeliberateAlwaysFreshAdvisoryIsProducedByTheFixture(t *testing.T) {
	dir := setupDeliberateAlwaysFreshCity(t)

	_, prov, err := config.LoadWithIncludesOptions(fsys.OSFS{}, filepath.Join(dir, "city.toml"), skipRevisionSnapshot)
	if err != nil {
		t.Fatalf("config.LoadWithIncludesOptions: %v", err)
	}
	advisories := 0
	for _, w := range prov.Warnings {
		if config.IsAlwaysFreshWakeModeWarning(w) {
			advisories++
		}
	}
	if advisories != deliberateAlwaysFreshSessions {
		t.Fatalf("config load produced %d always+fresh advisories, want %d; warnings=%v",
			advisories, deliberateAlwaysFreshSessions, prov.Warnings)
	}
}

// TestRoutineConfigLoadsDoNotRepeatAlwaysFreshAdvisory is the reproduction for
// gcw-qap3.4.3: an ordinary command must not spend the operator's (or an
// agent's) context on a notice about configuration they chose on purpose. Each
// CLI call is a new process, so per-process dedupe cannot help — the advisory
// has to leave the routine path entirely.
func TestRoutineConfigLoadsDoNotRepeatAlwaysFreshAdvisory(t *testing.T) {
	dir := setupDeliberateAlwaysFreshCity(t)

	var stderr bytes.Buffer
	const invocations = 20
	for i := range invocations {
		if _, err := loadCityConfig(dir, &stderr); err != nil {
			t.Fatalf("loadCityConfig invocation %d: %v (stderr=%q)", i, err, stderr.String())
		}
	}
	if got := countAlwaysFreshAdvisoryLines(stderr.String()); got != 0 {
		t.Fatalf("%d ordinary config loads emitted %d always+fresh advisory lines, want 0\nstderr=%q",
			invocations, got, stderr.String())
	}
}

// TestConfigValidationStillReportsEachAlwaysFreshAdvisoryOnce holds the other
// half of the contract: quieting the routine path must not make the advisory
// unreachable. `gc config show --validate` is the explicit diagnostic surface,
// and it must report every unique advisory exactly once.
func TestConfigValidationStillReportsEachAlwaysFreshAdvisoryOnce(t *testing.T) {
	setupDeliberateAlwaysFreshCity(t)

	var stdout, stderr bytes.Buffer
	code := run([]string{"config", "show", "--validate"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("run(config show --validate) = %d, want 0\nstdout=%q\nstderr=%q", code, stdout.String(), stderr.String())
	}
	if got := countAlwaysFreshAdvisoryLines(stderr.String()); got != deliberateAlwaysFreshSessions {
		t.Fatalf("config validation reported %d always+fresh advisory lines, want %d (each unique advisory once)\nstderr=%q",
			got, deliberateAlwaysFreshSessions, stderr.String())
	}
}

// TestEmitLoadCityConfigWarnings_KeepsActionableWarningsAndDropsAdvisories
// pins the discrimination itself: a warning that names a defect the operator
// must fix still reaches stderr; the deliberate-policy advisory does not.
func TestEmitLoadCityConfigWarnings_KeepsActionableWarningsAndDropsAdvisories(t *testing.T) {
	advisory := alwaysFreshAdvisoryFixture(t)
	actionable := `named_session "ghost": backing template "ghost" not found after pack expansion; named session disabled until its template resolves`

	var buf bytes.Buffer
	emitLoadCityConfigWarnings(&buf, &config.Provenance{Warnings: []string{
		advisory, actionable, advisory, actionable,
	}})

	out := buf.String()
	if got := countAlwaysFreshAdvisoryLines(out); got != 0 {
		t.Errorf("emitted %d advisory lines on the routine path, want 0: %q", got, out)
	}
	if got := strings.Count(out, actionable); got != 1 {
		t.Errorf("actionable warning emitted %d times, want exactly 1 (duplicates collapsed): %q", got, out)
	}
}

// TestEmitLoadCityConfigWarnings_JSONWriterStaysEmpty proves the advisory
// change does not reintroduce bytes on a --json path, where any stderr line
// would still be a behavioral surprise for scripted callers.
func TestEmitLoadCityConfigWarnings_JSONWriterStaysEmpty(t *testing.T) {
	advisory := alwaysFreshAdvisoryFixture(t)

	var stderr bytes.Buffer
	w := configWarnWriter(true, &stderr)
	if w != io.Discard {
		t.Fatalf("configWarnWriter(jsonOut=true) = %T, want io.Discard", w)
	}
	emitLoadCityConfigWarnings(w, &config.Provenance{Warnings: []string{advisory}})
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty on a --json path", stderr.String())
	}
}

// TestSplitStrictConfigWarnings_AdvisoryStaysNonFatalBesideAFatal keeps the
// strict-mode classification honest: quieting the advisory must not change
// whether `gc start` treats it as fatal, and must not swallow a real fatal
// warning that arrives in the same batch.
func TestSplitStrictConfigWarnings_AdvisoryStaysNonFatalBesideAFatal(t *testing.T) {
	advisory := alwaysFreshAdvisoryFixture(t)
	fatalWarning := `city agent "mayor" shadows agent of the same name from import "gs"`

	fatal, nonFatal := splitStrictConfigWarnings([]string{advisory, fatalWarning})
	if len(fatal) != 1 || fatal[0] != fatalWarning {
		t.Fatalf("fatal = %v, want only the shadow warning", fatal)
	}
	if len(nonFatal) != 1 || nonFatal[0] != advisory {
		t.Fatalf("nonFatal = %v, want the always+fresh advisory", nonFatal)
	}
}

// alwaysFreshAdvisoryFixture derives the advisory text from the validator that
// emits it, so these tests cannot pass against a string the validator no longer
// produces.
func alwaysFreshAdvisoryFixture(t *testing.T) string {
	t.Helper()
	warnings, err := config.ValidateNamedSessions(&config.City{
		Workspace:     config.Workspace{Name: "test-city"},
		Agents:        []config.Agent{{Name: "watchdog", WakeMode: "fresh"}},
		NamedSessions: []config.NamedSession{{Template: "watchdog", Mode: "always"}},
	})
	if err != nil {
		t.Fatalf("config.ValidateNamedSessions: %v", err)
	}
	if len(warnings) != 1 || !config.IsAlwaysFreshWakeModeWarning(warnings[0]) {
		t.Fatalf("warnings = %v, want exactly the always+fresh advisory", warnings)
	}
	return warnings[0]
}
