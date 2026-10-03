package main

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// TestCmdAgentLoadersDeclineTheRevisionSnapshot pins a file-level invariant:
// every city-config loader in cmd_agent.go discards the Provenance, so every
// one of them must decline the load-time revision snapshot.
//
// The snapshot content-hashes every pack directory so a later config.Revision()
// can compare against the tree as loaded. These loaders return only
// *config.City — the Provenance is used for warnings and dropped — so nothing
// they load can observe it, and building it is pure cost on a one-shot command.
//
// A source-level guard rather than a behavioral one because the cost is
// invisible by construction: a loader that silently reverts to the default
// still returns exactly the same config, passes every functional test, and just
// re-reads every pack file. Nothing fails; it only gets slower. That is
// precisely the kind of regression a test suite does not otherwise catch.
//
// If a loader here is ever changed to RETURN the Provenance, it should keep the
// default instead — and this test should be updated to exempt it by name rather
// than deleted.
func TestCmdAgentLoadersDeclineTheRevisionSnapshot(t *testing.T) {
	src, err := os.ReadFile("cmd_agent.go")
	if err != nil {
		t.Fatalf("reading cmd_agent.go: %v", err)
	}
	text := string(src)

	// The plain LoadWithIncludes form takes no options, so it always captures
	// the snapshot. Its variadic is extra include PATHS, not options — a
	// tempting place to pass the option by mistake.
	if strings.Contains(text, "config.LoadWithIncludes(") {
		t.Error("cmd_agent.go calls config.LoadWithIncludes( — that form always captures the revision snapshot; " +
			"use config.LoadWithIncludesOptions(..., skipRevisionSnapshot)")
	}

	calls := regexp.MustCompile(`config\.LoadWithIncludesOptions\([^)]*\)`).FindAllString(text, -1)
	if len(calls) == 0 {
		t.Fatal("no config.LoadWithIncludesOptions call found in cmd_agent.go; this guard is no longer watching anything")
	}
	for _, call := range calls {
		if strings.Contains(call, "skipRevisionSnapshot") {
			continue
		}
		// loadPrematerializedCityConfig forwards its caller's options, so the
		// obligation moves to every caller: each must pass an options value
		// that declines the snapshot.
		if call == "config.LoadWithIncludesOptions(fs, tomlPath, opts)" {
			checkPrematerializedCallersDeclineSnapshot(t, text)
			continue
		}
		t.Errorf("loader does not decline the revision snapshot: %s", call)
	}
}

// checkPrematerializedCallersDeclineSnapshot verifies every caller of the
// options-forwarding loadPrematerializedCityConfig passes a package-level
// config.LoadOptions value that sets SkipRevisionSnapshot.
func checkPrematerializedCallersDeclineSnapshot(t *testing.T, text string) {
	t.Helper()
	declining := map[string]bool{}
	for _, m := range regexp.MustCompile(`var (\w+) = config\.LoadOptions\{([^}]*)\}`).FindAllStringSubmatch(text, -1) {
		if regexp.MustCompile(`\bSkipRevisionSnapshot:\s*true\b`).MatchString(m[2]) {
			declining[m[1]] = true
		}
	}
	callers := 0
	for _, m := range regexp.MustCompile(`(func )?loadPrematerializedCityConfig\(([^,]+),\s*([^,]+),\s*(\w+),`).FindAllStringSubmatch(text, -1) {
		if m[1] != "" {
			continue // the declaration, not a call
		}
		callers++
		if !declining[m[4]] {
			t.Errorf("loadPrematerializedCityConfig caller passes %s, which does not decline the revision snapshot", m[4])
		}
	}
	if callers == 0 {
		t.Fatal("loadPrematerializedCityConfig forwards opts but no caller was found; this guard is no longer watching it")
	}
}
