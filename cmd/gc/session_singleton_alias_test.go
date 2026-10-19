package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/session"
)

// A canonical singleton (max_active_sessions = 1) converges to its qualified
// name as alias: the reconciler adopts that alias one tick after create. A
// manual create without --alias used to persist the s-<beadID> fallback as
// session_name before the alias arrived, so the tmux session kept the
// synthetic name forever while every other singleton ran under its alias.

func writeSingletonSessionCityTOML(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, ".gc"), 0o755); err != nil {
		t.Fatalf("MkdirAll(.gc): %v", err)
	}
	rigRoot := filepath.Join(dir, "repos", "demo")
	if err := os.MkdirAll(rigRoot, 0o755); err != nil {
		t.Fatalf("MkdirAll(rig root): %v", err)
	}
	data := []byte(fmt.Sprintf(`[workspace]
name = "test-city"

[beads]
provider = "file"

[providers.codex]
base = "builtin:codex"

[[rigs]]
name = "demo"
path = %q

[[agent]]
name = "marketer"
provider = "codex"
start_command = "echo"
work_dir = ".gc/workspaces/marketer"
wake_mode = "fresh"
min_active_sessions = 0
max_active_sessions = 1

[[agent]]
name = "witness"
dir = "demo"
provider = "codex"
start_command = "echo"
min_active_sessions = 0
max_active_sessions = 1
`, rigRoot))
	if err := os.WriteFile(filepath.Join(dir, "city.toml"), data, 0o644); err != nil {
		t.Fatalf("WriteFile(city.toml): %v", err)
	}
}

func TestCmdSessionNew_CanonicalSingletonWithoutAliasUsesQualifiedNameAlias(t *testing.T) {
	tests := []struct {
		template        string
		wantAlias       string
		wantSessionName string
	}{
		{template: "marketer", wantAlias: "marketer", wantSessionName: "marketer"},
		{template: "demo/witness", wantAlias: "demo/witness", wantSessionName: "demo--witness"},
	}
	for _, tt := range tests {
		t.Run(tt.template, func(t *testing.T) {
			t.Setenv("GC_BEADS", "file")
			t.Setenv("GC_SESSION", "fake")

			cityDir := t.TempDir()
			t.Setenv("GC_CITY", cityDir)
			writeSingletonSessionCityTOML(t, cityDir)

			var stdout, stderr bytes.Buffer
			if code := cmdSessionNew([]string{tt.template}, "", "", "", true, false, 0, &stdout, &stderr); code != 0 {
				t.Fatalf("cmdSessionNew(%q) = %d, want 0; stderr=%s", tt.template, code, stderr.String())
			}

			all := sessionBeads(t, cityDir)
			if len(all) != 1 {
				t.Fatalf("session beads = %d, want 1", len(all))
			}
			meta := all[0].Metadata
			if got := meta["alias"]; got != tt.wantAlias {
				t.Errorf("alias = %q, want %q", got, tt.wantAlias)
			}
			if got := meta["session_name"]; got != tt.wantSessionName {
				t.Errorf("session_name = %q, want alias-derived %q", got, tt.wantSessionName)
			}
			if got := meta["session_name"]; strings.HasPrefix(got, "s-") {
				t.Errorf("session_name = %q, want no s-<beadID> fallback for a canonical singleton", got)
			}
			if got := meta["session_origin"]; got != "manual" {
				t.Errorf("session_origin = %q, want manual", got)
			}
		})
	}
}

func TestCmdSessionNew_CanonicalSingletonRefusesSecondSessionHoldingItsAlias(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_SESSION", "fake")

	cityDir := t.TempDir()
	t.Setenv("GC_CITY", cityDir)
	writeSingletonSessionCityTOML(t, cityDir)

	var stdout, stderr bytes.Buffer
	if code := cmdSessionNew([]string{"marketer"}, "", "", "", true, false, 0, &stdout, &stderr); code != 0 {
		t.Fatalf("first cmdSessionNew(marketer) = %d, want 0; stderr=%s", code, stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	if code := cmdSessionNew([]string{"marketer"}, "", "", "", true, false, 0, &stdout, &stderr); code == 0 {
		t.Fatalf("second cmdSessionNew(marketer) = 0, want refusal while the open singleton holds alias marketer")
	}
	if !strings.Contains(stderr.String(), "marketer") {
		t.Fatalf("stderr = %q, want it to name the contested alias", stderr.String())
	}
	if all := sessionBeads(t, cityDir); len(all) != 1 {
		t.Fatalf("session beads = %d, want 1 (no second singleton session)", len(all))
	}
}

func TestEnsureSessionForTemplate_CanonicalSingletonUsesQualifiedNameAlias(t *testing.T) {
	t.Setenv("GC_SESSION", "fake")

	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{{
			Name:              "marketer",
			StartCommand:      "true",
			MaxActiveSessions: intPtr(1),
		}},
	}
	store := beads.NewMemStore()

	sessionName, err := ensureSessionForTemplate(t.TempDir(), cfg, store, "marketer", io.Discard)
	if err != nil {
		t.Fatalf("ensureSessionForTemplate(marketer): %v", err)
	}
	if sessionName != "marketer" {
		t.Fatalf("session name = %q, want alias-derived marketer", sessionName)
	}

	all, err := store.ListByLabel(session.LabelSession, 0)
	if err != nil {
		t.Fatalf("ListByLabel: %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("session bead count = %d, want 1", len(all))
	}
	if got := all[0].Metadata["alias"]; got != "marketer" {
		t.Fatalf("alias = %q, want marketer", got)
	}
	if got := all[0].Metadata["session_name"]; got != "marketer" {
		t.Fatalf("session_name = %q, want marketer", got)
	}
}
