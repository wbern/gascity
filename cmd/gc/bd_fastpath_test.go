package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestHasExplicitBdScopeFlagHandlesEmptyArgs is the regression guard for a live
// crash: bare `gc` with no arguments panicked with "slice bounds out of range
// [1:0]".
//
// hasExplicitBdScopeFlag skips args[0] (the subcommand) by slicing args[1:],
// which panics rather than yielding an empty slice when args itself is empty.
// It is the FIRST call in tryEarlyBdShimReadOutcome after the enable check, and
// the fastpath is on by default, so every no-argument invocation reached it.
// A user typing `gc` to see the help got a panic and a stack trace.
func TestHasExplicitBdScopeFlagHandlesEmptyArgs(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want bool
	}{
		{"nil args", nil, false},
		{"empty args", []string{}, false},
		{"subcommand only", []string{"bd"}, false},
		{"subcommand alone is never scanned as a flag", []string{"--city"}, false},
		{"explicit city flag", []string{"bd", "--city"}, true},
		{"explicit rig flag", []string{"bd", "--rig"}, true},
		{"explicit city assignment", []string{"bd", "--city=demo"}, true},
		{"explicit rig assignment", []string{"bd", "--rig=frontend"}, true},
		{"unrelated flags", []string{"bd", "list", "--json"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := hasExplicitBdScopeFlag(tc.args); got != tc.want {
				t.Errorf("hasExplicitBdScopeFlag(%q) = %v, want %v", tc.args, got, tc.want)
			}
		})
	}
}

func TestBdFastpathEnabledDefaultsOnWithExplicitOptOut(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  string
		want bool
	}{
		{name: "unset", want: true},
		{name: "enabled", raw: "1", want: true},
		{name: "enabled trimmed", raw: " 1 ", want: true},
		{name: "disabled", raw: "0", want: false},
		{name: "disabled trimmed", raw: " 0 ", want: false},
		{name: "invalid fails closed", raw: "yes", want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := bdFastpathEnabled(tc.raw); got != tc.want {
				t.Fatalf("bdFastpathEnabled(%q) = %t, want %t", tc.raw, got, tc.want)
			}
		})
	}
}

func TestRunEarlyBdDirectDispatchesApprovedList(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got, want := r.URL.Path, "/v0/city/gc2/beads"; got != want {
			t.Fatalf("request path = %q, want %q", got, want)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"beads": []any{}})
	}))
	defer server.Close()

	var stdout, stderr bytes.Buffer
	if code := runEarlyBdDirect(server.URL, "gc2", "list", []string{"--json"}, &stdout, &stderr); code != 0 {
		t.Fatalf("runEarlyBdDirect() = %d, stderr=%q", code, stderr.String())
	}
	if got, want := stdout.String(), "[]\n"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
}

func TestRunEarlyBdDirectFailsLoudlyOnControllerError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"title":"controller unavailable"}`))
	}))
	defer server.Close()

	var stdout, stderr bytes.Buffer
	if code := runEarlyBdDirect(server.URL, "gc2", "list", []string{"--json"}, &stdout, &stderr); code == 0 {
		t.Fatal("runEarlyBdDirect() succeeded after controller failure")
	}
	if stdout.Len() != 0 || !strings.Contains(stderr.String(), "list via API") {
		t.Fatalf("controller failure output = stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
}

func TestTryEarlyBdShimReadUsesDirectExperimentArm(t *testing.T) {
	t.Setenv("GC_BD_FASTPATH", "1")
	t.Setenv("GC_CITY_PATH", "/tmp/gc2")
	t.Setenv("GC_BD_EXPERIMENT_ARMS", "shim=0,direct=100,legacy=0")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"beads": []any{}})
	}))
	defer server.Close()
	t.Setenv("GC_API_URL", server.URL)

	shim := writeEarlyBdShim(t, "exit 99\n")
	previous := earlyBdShimPath
	earlyBdShimPath = func(string) (string, error) { return shim, nil }
	t.Cleanup(func() { earlyBdShimPath = previous })
	setEarlyBDLookPath(t, shim)

	var stdout, stderr bytes.Buffer
	if code, handled := tryEarlyBdShimRead([]string{"bd", "query", "--json", "ephemeral=true"}, strings.NewReader(""), &stdout, &stderr); !handled || code != 0 {
		t.Fatalf("tryEarlyBdShimRead() = (%d, %t), stderr=%q", code, handled, stderr.String())
	}
	if got, want := stdout.String(), "[]\n"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
}

func TestDirectExperimentObservationRedactsInvocationValues(t *testing.T) {
	t.Setenv("GC_BD_FASTPATH", "1")
	t.Setenv("GC_CITY_PATH", "/tmp/gc2")
	t.Setenv("GC_BD_EXPERIMENT_ARMS", "shim=0,direct=100,legacy=0")
	secret := "private-agent-and-path"
	logPath := filepath.Join(t.TempDir(), "experiment.jsonl")
	t.Setenv("GC_BD_EXPERIMENT_LOG", logPath)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"beads": []any{}})
	}))
	defer server.Close()
	t.Setenv("GC_API_URL", server.URL)

	shim := writeEarlyBdShim(t, "exit 99\n")
	previous := earlyBdShimPath
	earlyBdShimPath = func(string) (string, error) { return shim, nil }
	t.Cleanup(func() { earlyBdShimPath = previous })
	setEarlyBDLookPath(t, shim)

	if code, handled := tryEarlyBdShimRead([]string{"bd", "query", "--json", "ephemeral=true AND assignee=" + secret}, strings.NewReader(""), io.Discard, io.Discard); !handled || code != 0 {
		t.Fatalf("tryEarlyBdShimRead() = (%d, %t)", code, handled)
	}
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read observation: %v", err)
	}
	if strings.Contains(string(data), secret) || strings.Contains(string(data), server.URL) {
		t.Fatalf("observation leaked invocation data: %s", data)
	}
}

func TestTryEarlyBdShimReadLegacyArmDefersObservationUntilCommandExit(t *testing.T) {
	t.Setenv("GC_BD_FASTPATH", "1")
	t.Setenv("GC_CITY_PATH", "/tmp/gc2")
	t.Setenv("GC_BD_EXPERIMENT_ARMS", "shim=100,direct=0,legacy=0")
	t.Setenv("GC_BD_EXPERIMENT_FORCE_ARM", "legacy")
	logPath := filepath.Join(t.TempDir(), "experiment.jsonl")
	t.Setenv("GC_BD_EXPERIMENT_LOG", logPath)

	shim := writeEarlyBdShim(t, "exit 99\n")
	previous := earlyBdShimPath
	earlyBdShimPath = func(string) (string, error) { return shim, nil }
	t.Cleanup(func() { earlyBdShimPath = previous })
	setEarlyBDLookPath(t, shim)

	_, handled, legacy := tryEarlyBdShimReadOutcome([]string{"bd", "query", "--json", "ephemeral=true"}, strings.NewReader(""), io.Discard, io.Discard, time.Now())
	if handled || legacy == nil {
		t.Fatalf("legacy outcome = (handled=%t, observation=%v), want unhandled observation", handled, legacy)
	}
	observeLegacyBdExperiment(*legacy, 17, time.Now(), 7)
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read observation: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("decode observation: %v", err)
	}
	if got["arm"] != "legacy" || got["exit"] != float64(7) || got["stdout_bytes"] != float64(17) {
		t.Fatalf("legacy observation = %v", got)
	}
}

// TestTryEarlyBdShimDeclinesShapesTheShimNoLongerRoutes pins the invariant that
// the early path only serves shapes the shim's classifier actually routes
// through the controller. show and list carry bd-computed IssueDetails /
// IssueWithCounts fields the controller Bead does not retain, so the classifier
// passes them through to real bd. Fast-pathing such a shape would either serve
// that lossy controller projection (direct arm) or exec raw bd with the
// caller's own cwd/env instead of gc's resolved scope (shim arm). Both are
// wrong, so the early path must decline and leave them on the full gc bd path.
func TestTryEarlyBdShimDeclinesShapesTheShimNoLongerRoutes(t *testing.T) {
	t.Setenv("GC_BD_FASTPATH", "1")
	t.Setenv("GC_CITY_PATH", "/tmp/gc2")
	// Force the direct arm: if a lossy shape still reached the experiment, this
	// is the arm that would answer it from the controller projection.
	t.Setenv("GC_BD_EXPERIMENT_ARMS", "shim=0,direct=100,legacy=0")

	shim := writeEarlyBdShim(t, "printf 'shim:%s\\n' \"$*\"\n")
	previous := earlyBdShimPath
	earlyBdShimPath = func(_ string) (string, error) { return shim, nil }
	t.Cleanup(func() { earlyBdShimPath = previous })
	setEarlyBDLookPath(t, shim)

	for _, tc := range []struct {
		name string
		args []string
		call func([]string, io.Reader, io.Writer, io.Writer) (int, bool)
	}{
		{"show_json", []string{"bd", "show", "gcw-123", "--json"}, tryEarlyBdShim},
		{"list_json", []string{"bd", "list", "--json"}, tryEarlyBdShimRead},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code, handled := tc.call(tc.args, strings.NewReader(""), &stdout, &stderr)
			if handled {
				t.Fatalf("%v was fast-pathed (code=%d, stdout=%q); want handled=false so it stays on the full gc bd path",
					tc.args, code, stdout.String())
			}
			if stdout.Len() != 0 {
				t.Fatalf("%v wrote %q to stdout while declining", tc.args, stdout.String())
			}
		})
	}
}

func TestTryEarlyBdShimReadRoutesOnlyThroughTheManagedShimAlreadyOnPath(t *testing.T) {
	t.Setenv("GC_BD_FASTPATH", "1")
	t.Setenv("GC_CITY_PATH", "/tmp/gc2")

	shim := writeEarlyBdShim(t, "printf 'shim:%s\\n' \"$*\"\n")
	previousShimPath := earlyBdShimPath
	earlyBdShimPath = func(_ string) (string, error) { return shim, nil }
	t.Cleanup(func() { earlyBdShimPath = previousShimPath })
	previousLookPath := earlyBdLookPath
	earlyBdLookPath = func(name string) (string, error) {
		if name != "bd" {
			t.Fatalf("LookPath(%q), want bd", name)
		}
		return shim, nil
	}
	t.Cleanup(func() { earlyBdLookPath = previousLookPath })

	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{
			name: "ready preserves bd flags",
			args: []string{"bd", "ready", "--limit", "5", "--json"},
			want: "shim:ready --limit 5 --json\n",
		},
		{
			name: "ready preserves bd global flags",
			args: []string{"bd", "--readonly", "ready", "--json"},
			want: "shim:--readonly ready --json\n",
		},
		{
			name: "query preserves the ephemeral predicate",
			args: []string{"bd", "query", "--json", "ephemeral=true AND status=open", "--limit", "5"},
			want: "shim:query --json ephemeral=true AND status=open --limit 5\n",
		},
		{
			name: "mol current preserves its rendered output flags",
			args: []string{"bd", "mol", "current", "gcw-root", "--json"},
			want: "shim:mol current gcw-root --json\n",
		},
		{
			name: "mol progress preserves its rendered output flags",
			args: []string{"bd", "mol", "progress", "gcw-root"},
			want: "shim:mol progress gcw-root\n",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code, handled := tryEarlyBdShimRead(tc.args, strings.NewReader(""), &stdout, &stderr)
			if !handled || code != 0 {
				t.Fatalf("tryEarlyBdShimRead() = (%d, %t), want (0, true); stderr=%q", code, handled, stderr.String())
			}
			if got := stdout.String(); got != tc.want {
				t.Fatalf("stdout = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestTryEarlyBdShimReadFailsClosedWhenManagedShimIsNotOnPath(t *testing.T) {
	t.Setenv("GC_BD_FASTPATH", "1")
	t.Setenv("GC_CITY_PATH", "/tmp/gc2")

	shim := writeEarlyBdShim(t, "exit 0\n")
	foreign := writeEarlyBdShim(t, "exit 0\n")
	previousShimPath := earlyBdShimPath
	earlyBdShimPath = func(_ string) (string, error) { return shim, nil }
	t.Cleanup(func() { earlyBdShimPath = previousShimPath })
	previousLookPath := earlyBdLookPath
	earlyBdLookPath = func(string) (string, error) { return foreign, nil }
	t.Cleanup(func() { earlyBdLookPath = previousLookPath })

	if code, handled := tryEarlyBdShimRead([]string{"bd", "list", "--json"}, strings.NewReader(""), io.Discard, io.Discard); handled || code != 0 {
		t.Fatalf("tryEarlyBdShimRead() = (%d, %t), want (0, false)", code, handled)
	}
}

func TestTryEarlyBdShimReadFailsClosedForShimPassthroughShapes(t *testing.T) {
	t.Setenv("GC_BD_FASTPATH", "1")
	t.Setenv("GC_CITY_PATH", "/tmp/gc2")

	previousShimPath := earlyBdShimPath
	earlyBdShimPath = func(_ string) (string, error) {
		t.Fatal("shim path must not be resolved for a passthrough shape")
		return "", nil
	}
	t.Cleanup(func() { earlyBdShimPath = previousShimPath })

	for _, args := range [][]string{
		{"bd", "list"},
		{"bd", "list", "--json", "--offset", "10"},
		{"bd", "ready", "--label", "blocked"},
		{"bd", "query", "--json", "status=open"},
		{"bd", "mol", "pour", "example"},
		{"bd", "--city", "/city", "list", "--json"},
		{"bd", "--rig", "rig", "ready", "--json"},
	} {
		if code, handled := tryEarlyBdShimRead(args, strings.NewReader(""), io.Discard, io.Discard); handled || code != 0 {
			t.Fatalf("tryEarlyBdShimRead(%v) = (%d, %t), want (0, false)", args, code, handled)
		}
	}
}

func TestTryEarlyBdShimReadNeverBypassesMutationGuards(t *testing.T) {
	t.Setenv("GC_BD_FASTPATH", "1")
	t.Setenv("GC_CITY_PATH", "/tmp/gc2")

	previousShimPath := earlyBdShimPath
	earlyBdShimPath = func(_ string) (string, error) {
		t.Fatal("mutation must not resolve the early bdshim path")
		return "", nil
	}
	t.Cleanup(func() { earlyBdShimPath = previousShimPath })

	for _, args := range [][]string{
		{"bd", "close", "gcw-123"},
		{"bd", "create", "new work"},
		{"bd", "update", "gcw-123", "--status", "closed"},
		{"bd", "delete", "gcw-123"},
		{"bd", "reopen", "gcw-123"},
	} {
		if code, handled := tryEarlyBdShimRead(args, strings.NewReader(""), io.Discard, io.Discard); handled || code != 0 {
			t.Fatalf("tryEarlyBdShimRead(%v) = (%d, %t), want (0, false)", args, code, handled)
		}
	}
}

func TestTryEarlyBdShimReadPreservesExternalDoltOverrideAndAllowsCanonicalProjection(t *testing.T) {
	t.Setenv("GC_BD_FASTPATH", "1")
	t.Setenv("GC_CITY_PATH", "/tmp/gc2")

	shim := writeEarlyBdShim(t, "printf 'shim:%s\\n' \"$*\"\n")
	previousShimPath := earlyBdShimPath
	earlyBdShimPath = func(_ string) (string, error) { return shim, nil }
	t.Cleanup(func() { earlyBdShimPath = previousShimPath })
	setEarlyBDLookPath(t, shim)

	for _, tc := range []struct {
		name        string
		env         map[string]string
		wantHandled bool
	}{
		{
			name:        "external override stays on the full path",
			env:         map[string]string{"GC_DOLT_HOST": "db.example.test"},
			wantHandled: false,
		},
		{
			name: "canonical projected port uses the managed shim",
			env: map[string]string{
				"GC_DOLT_PORT":                       "49813",
				"GC_BD_FASTPATH_CANONICAL_DOLT_PORT": "49813",
			},
			wantHandled: true,
		},
		{
			name: "stale canonical projection stays on the full path",
			env: map[string]string{
				"GC_DOLT_PORT":                       "49813",
				"GC_BD_FASTPATH_CANONICAL_DOLT_PORT": "49812",
			},
			wantHandled: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for key, value := range tc.env {
				t.Setenv(key, value)
			}

			code, handled := tryEarlyBdShimRead([]string{"bd", "ready", "--json"}, strings.NewReader(""), io.Discard, io.Discard)
			if handled != tc.wantHandled || code != 0 {
				t.Fatalf("tryEarlyBdShimRead() = (%d, %t), want (0, %t)", code, handled, tc.wantHandled)
			}
		})
	}
}

// TestTryEarlyBdShimKeepsReadOnTheFullPathWhenTheManagedShimIsNotSelected pins
// that the early path declines when PATH's `bd` resolves to a different file
// than the trusted managed shim, so a terminal whose bd is not the city's shim
// keeps gc bd's full rig-local contract.
func TestTryEarlyBdShimKeepsReadOnTheFullPathWhenTheManagedShimIsNotSelected(t *testing.T) {
	t.Setenv("GC_BD_FASTPATH", "1")
	t.Setenv("GC_CITY_PATH", "/tmp/gc2")

	shim := writeEarlyBdShim(t, "exit 0\n")
	foreign := writeEarlyBdShim(t, "exit 0\n")
	previousShimPath := earlyBdShimPath
	earlyBdShimPath = func(_ string) (string, error) { return shim, nil }
	t.Cleanup(func() { earlyBdShimPath = previousShimPath })
	previousLookPath := earlyBdLookPath
	earlyBdLookPath = func(string) (string, error) { return foreign, nil }
	t.Cleanup(func() { earlyBdLookPath = previousLookPath })

	if code, handled := tryEarlyBdShimRead([]string{"bd", "ready", "--json"}, strings.NewReader(""), io.Discard, io.Discard); handled || code != 0 {
		t.Fatalf("tryEarlyBdShimRead() = (%d, %t), want (0, false)", code, handled)
	}
}

func TestRunUsesEarlyBdShimReadBeforeNormalBdPath(t *testing.T) {
	t.Setenv("GC_BD_FASTPATH", "1")
	t.Setenv("GC_CITY_PATH", "/tmp/gc2")

	shim := writeEarlyBdShim(t, "printf 'shim:%s\\n' \"$*\"\n")
	previousShimPath := earlyBdShimPath
	earlyBdShimPath = func(_ string) (string, error) { return shim, nil }
	t.Cleanup(func() { earlyBdShimPath = previousShimPath })
	previousLookPath := earlyBdLookPath
	earlyBdLookPath = func(string) (string, error) { return shim, nil }
	t.Cleanup(func() { earlyBdLookPath = previousLookPath })
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{name: "ready", args: []string{"bd", "ready", "--json"}, want: "shim:ready --json\n"},
		{name: "query", args: []string{"bd", "query", "--json", "ephemeral=true"}, want: "shim:query --json ephemeral=true\n"},
		{name: "mol", args: []string{"bd", "mol", "current", "gcw-root", "--json"}, want: "shim:mol current gcw-root --json\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if code := run(tc.args, &stdout, &stderr); code != 0 {
				t.Fatalf("run() = %d, want 0; stderr=%q", code, stderr.String())
			}
			if got := stdout.String(); got != tc.want {
				t.Fatalf("stdout = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestTryEarlyBdShimHonorsExplicitOptOut(t *testing.T) {
	t.Setenv("GC_BD_FASTPATH", "0")
	t.Setenv("GC_CITY_PATH", "/tmp/gc2")

	previous := earlyBdShimPath
	earlyBdShimPath = func(_ string) (string, error) {
		t.Fatal("explicit opt-out must not resolve the bdshim path")
		return "", nil
	}
	t.Cleanup(func() { earlyBdShimPath = previous })

	// Uses a shape the shim still routes, so a decline can only come from the
	// opt-out itself rather than from the shape being unroutable.
	if code, handled := tryEarlyBdShimRead([]string{"bd", "ready", "--json"}, strings.NewReader(""), io.Discard, io.Discard); handled || code != 0 {
		t.Fatalf("tryEarlyBdShimRead() = (%d, %t) when disabled, want (0, false)", code, handled)
	}
}

func TestTryEarlyBdShimPreservesManagedBDEntrypointName(t *testing.T) {
	t.Setenv("GC_BD_FASTPATH", "1")
	t.Setenv("GC_CITY_PATH", "/tmp/gc2")
	managedPath := filepath.Join(t.TempDir(), ".gc", "shimbin", "bd")
	if err := os.MkdirAll(filepath.Dir(managedPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(managedPath, []byte("#!/bin/sh\nprintf '%s\\n' \"${0##*/}\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	previous := earlyBdShimPath
	earlyBdShimPath = func(_ string) (string, error) { return managedPath, nil }
	t.Cleanup(func() { earlyBdShimPath = previous })
	setEarlyBDLookPath(t, managedPath)

	var stdout bytes.Buffer
	code, handled := tryEarlyBdShimRead([]string{"bd", "ready", "--json"}, strings.NewReader(""), &stdout, io.Discard)
	if !handled || code != 0 {
		t.Fatalf("tryEarlyBdShimRead() = (%d, %t), want (0, true)", code, handled)
	}
	if got, want := stdout.String(), "bd\n"; got != want {
		t.Fatalf("managed entrypoint name = %q, want %q", got, want)
	}
}

func TestEarlyBdShimBesideGCRequiresExecutableSibling(t *testing.T) {
	dir := t.TempDir()
	gcPath := filepath.Join(dir, "gc")
	if err := os.WriteFile(gcPath, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := earlyBdShimBesideGC(gcPath); got != "" {
		t.Fatalf("without sibling: got %q, want empty", got)
	}
	shimPath := filepath.Join(dir, "bdshim")
	if err := os.WriteFile(shimPath, []byte("#!/bin/sh\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := earlyBdShimBesideGC(gcPath); got != "" {
		t.Fatalf("non-executable sibling: got %q, want empty", got)
	}
	if err := os.Chmod(shimPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if got := earlyBdShimBesideGC(gcPath); got != shimPath {
		t.Fatalf("executable sibling: got %q, want %q", got, shimPath)
	}
}

func TestEarlyBdShimForCityUsesOnlyTrustedManagedBDAlias(t *testing.T) {
	binDir := t.TempDir()
	gcPath := filepath.Join(binDir, "gc")
	trustedPath := filepath.Join(binDir, "bdshim")
	for _, path := range []string{gcPath, trustedPath} {
		if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	city := t.TempDir()
	managedDir := filepath.Join(city, ".gc", "shimbin")
	if err := os.MkdirAll(managedDir, 0o755); err != nil {
		t.Fatal(err)
	}
	managedPath := filepath.Join(managedDir, "bd")
	foreignPath := filepath.Join(t.TempDir(), "bdshim")
	if err := os.WriteFile(foreignPath, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(foreignPath, managedPath); err != nil {
		t.Fatal(err)
	}
	if got := earlyBdShimForCity(gcPath, city); got != "" {
		t.Fatalf("foreign managed entry = %q, want fallback", got)
	}
	if err := os.Remove(managedPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(trustedPath, managedPath); err != nil {
		t.Fatal(err)
	}
	if got := earlyBdShimForCity(gcPath, city); got != managedPath {
		t.Fatalf("trusted managed entry = %q, want %q", got, managedPath)
	}
}

func TestTryEarlyBdShimFailsClosedForUnsafeShapes(t *testing.T) {
	t.Setenv("GC_BD_FASTPATH", "1")
	t.Setenv("GC_CITY_PATH", "/tmp/gc2")

	called := false
	previous := earlyBdShimPath
	earlyBdShimPath = func(_ string) (string, error) {
		called = true
		return "", nil
	}
	t.Cleanup(func() { earlyBdShimPath = previous })

	for _, args := range [][]string{
		{"bd", "show", "gcw-123"},                            // non-JSON output is not yet parity-pinned
		{"bd", "show", "gcw-123", "--json", "--rig", "x"},    // rig scope must remain on full gc bd
		{"--rig", "x", "bd", "show", "gcw-123", "--json"},    // root-level rig scope must remain on full gc bd
		{"bd", "--city", "gc2", "show", "gcw-123", "--json"}, // explicit city uses gc bd's path validation
		{"bd", "list", "--json"},                             // list scope is not necessarily city-wide
		{"bd", "update", "gcw-123", "--status", "closed"},    // writes retain gc bd guards
		{"bd", "show", "gcw-123", "--json", "--verbose"},     // unknown rendering flag
		{"bd", "show", "gcw-123", "--json", "extra"},         // extra positional
		{"bd", "show", "gcw 123", "--json"},                  // malformed bead ID
		{"bd", "show", " gcw-123 ", "--json"},                // do not normalize malformed input
	} {
		if code, handled := tryEarlyBdShim(args, strings.NewReader(""), io.Discard, io.Discard); handled || code != 0 {
			t.Fatalf("tryEarlyBdShim(%v) = (%d, %t), want (0, false)", args, code, handled)
		}
	}
	if called {
		t.Fatal("unsafe shape resolved bdshim path")
	}
}

func TestTryEarlyBdShimFallsBackWithoutManagedCityOrShim(t *testing.T) {
	t.Setenv("GC_BD_FASTPATH", "1")
	t.Setenv("GC_CITY_PATH", "")
	t.Setenv("GC_CITY", "")

	previous := earlyBdShimPath
	earlyBdShimPath = func(_ string) (string, error) {
		t.Fatal("bdshim path must not be resolved without managed city context")
		return "", nil
	}
	t.Cleanup(func() { earlyBdShimPath = previous })

	if code, handled := tryEarlyBdShimRead([]string{"bd", "ready", "--json"}, strings.NewReader(""), io.Discard, io.Discard); handled || code != 0 {
		t.Fatalf("without city: got (%d, %t), want (0, false)", code, handled)
	}
}

func TestTryEarlyBdShimPreservesShimExitCode(t *testing.T) {
	t.Setenv("GC_BD_FASTPATH", "1")
	t.Setenv("GC_CITY_PATH", "/tmp/gc2")

	shim := writeEarlyBdShim(t, "printf controller-down >&2\nexit 7\n")
	previous := earlyBdShimPath
	earlyBdShimPath = func(_ string) (string, error) { return shim, nil }
	t.Cleanup(func() { earlyBdShimPath = previous })
	setEarlyBDLookPath(t, shim)

	var stderr bytes.Buffer
	if code, handled := tryEarlyBdShimRead([]string{"bd", "ready", "--json"}, strings.NewReader(""), io.Discard, &stderr); !handled || code != 7 {
		t.Fatalf("tryEarlyBdShimRead() = (%d, %t), want (7, true); stderr=%q", code, handled, stderr.String())
	}
}

func TestTryEarlyBdShimNormalizesSignalExitToFailure(t *testing.T) {
	t.Setenv("GC_BD_FASTPATH", "1")
	t.Setenv("GC_CITY_PATH", "/tmp/gc2")

	shim := writeEarlyBdShim(t, "kill -TERM $$\n")
	previous := earlyBdShimPath
	earlyBdShimPath = func(_ string) (string, error) { return shim, nil }
	t.Cleanup(func() { earlyBdShimPath = previous })
	setEarlyBDLookPath(t, shim)

	if code, handled := tryEarlyBdShimRead([]string{"bd", "ready", "--json"}, strings.NewReader(""), io.Discard, io.Discard); !handled || code != 1 {
		t.Fatalf("tryEarlyBdShimRead() = (%d, %t), want (1, true)", code, handled)
	}
}

func writeEarlyBdShim(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "bdshim")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func setEarlyBDLookPath(t *testing.T, path string) {
	t.Helper()
	previous := earlyBdLookPath
	earlyBdLookPath = func(name string) (string, error) {
		if name != "bd" {
			t.Fatalf("LookPath(%q), want bd", name)
		}
		return path, nil
	}
	t.Cleanup(func() { earlyBdLookPath = previous })
}

// Retiring bdshim (gcw-yr0o) removes the binary. Today the whole fast path is
// gated on that binary existing AND being the `bd` on PATH — the gate runs
// BEFORE the experiment is consulted, so even the in-process direct arm, which
// never execs the shim, is unreachable without it. Deleting bdshim would
// therefore silently drop every gc bd call onto the 600-780ms doBd path,
// including the verbs that are fast today.
//
// An approved shape must be served in-process when no shim is installed.
func TestTryEarlyBdShimReadServesApprovedShapeWithoutAShimBinary(t *testing.T) {
	t.Setenv("GC_BD_FASTPATH", "1")
	t.Setenv("GC_CITY_PATH", "/tmp/gc2")
	t.Setenv("GC_BD_EXPERIMENT_ARMS", "shim=100,direct=0,legacy=0") // shim arm selected...
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"beads": []any{}})
	}))
	defer server.Close()
	t.Setenv("GC_API_URL", server.URL)

	// No bdshim anywhere.
	previous := earlyBdShimPath
	earlyBdShimPath = func(string) (string, error) { return "", exec.ErrNotFound }
	t.Cleanup(func() { earlyBdShimPath = previous })
	previousLook := earlyBdLookPath
	earlyBdLookPath = func(string) (string, error) { return "", exec.ErrNotFound }
	t.Cleanup(func() { earlyBdLookPath = previousLook })

	var stdout, stderr bytes.Buffer
	code, handled := tryEarlyBdShimRead([]string{"bd", "query", "--json", "ephemeral=true"}, strings.NewReader(""), &stdout, &stderr)
	if !handled || code != 0 {
		t.Fatalf("without a shim binary: got (%d, %t), want (0, true) served in-process; stderr=%q", code, handled, stderr.String())
	}
	if got, want := stdout.String(), "[]\n"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
}

// A shape the experiment does not cover has no in-process implementation, so
// without a shim it must DECLINE to doBd rather than fabricate a result.
func TestTryEarlyBdShimReadDeclinesUnapprovedShapeWithoutAShimBinary(t *testing.T) {
	t.Setenv("GC_BD_FASTPATH", "1")
	t.Setenv("GC_CITY_PATH", "/tmp/gc2")

	previous := earlyBdShimPath
	earlyBdShimPath = func(string) (string, error) { return "", exec.ErrNotFound }
	t.Cleanup(func() { earlyBdShimPath = previous })
	previousLook := earlyBdLookPath
	earlyBdLookPath = func(string) (string, error) { return "", exec.ErrNotFound }
	t.Cleanup(func() { earlyBdLookPath = previousLook })

	// `ready` is fastpath-eligible but is NOT an approved experiment shape, so it
	// has no in-process path.
	if code, handled := tryEarlyBdShimRead([]string{"bd", "ready", "--json"}, strings.NewReader(""), io.Discard, io.Discard); handled || code != 0 {
		t.Fatalf("unapproved shape without a shim: got (%d, %t), want (0, false) so doBd handles it", code, handled)
	}
}
