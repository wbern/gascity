package main

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// TestCityConfigLoadersDeclineTheRevisionSnapshot pins a file-level invariant:
// every city-config loader in cmd_agent.go discards the Provenance, so every one
// of them must decline the load-time revision snapshot.
//
// The snapshot content-hashes every pack directory so a later config.Revision()
// can compare against the tree as it was loaded. These loaders return only
// *config.City — they use the Provenance to emit warnings and then drop it — so
// nothing they load can observe the snapshot, and building it is pure cost on a
// one-shot command.
//
// This is a source-level guard rather than a behavioral one because the cost is
// invisible by construction: a loader that reverts to the default returns
// exactly the same config and passes every functional test, it just re-reads
// every pack file. Nothing fails; it only gets slower, which is precisely the
// regression a test suite does not otherwise catch.
//
// It also catches a mistake that compiles: passing the option to
// config.LoadWithIncludes, whose variadic is extra include PATHS, not options.
//
// If a loader here is ever changed to RETURN the Provenance it should keep the
// default instead, and this test should be updated to exempt it by name rather
// than deleted.
func TestCityConfigLoadersDeclineTheRevisionSnapshot(t *testing.T) {
	src, err := os.ReadFile("cmd_agent.go")
	if err != nil {
		t.Fatalf("reading cmd_agent.go: %v", err)
	}
	text := string(src)

	if strings.Contains(text, "config.LoadWithIncludes(") {
		t.Error("cmd_agent.go calls config.LoadWithIncludes( — that form always captures the revision snapshot; " +
			"use config.LoadWithIncludesOptions(..., skipRevisionSnapshot)")
	}

	calls := regexp.MustCompile(`config\.LoadWithIncludesOptions\([^)]*\)`).FindAllString(text, -1)
	if len(calls) == 0 {
		t.Fatal("no config.LoadWithIncludesOptions call in cmd_agent.go; this guard is no longer watching anything")
	}
	for _, call := range calls {
		if !strings.Contains(call, "skipRevisionSnapshot") {
			t.Errorf("loader does not decline the revision snapshot: %s", call)
		}
	}
}
