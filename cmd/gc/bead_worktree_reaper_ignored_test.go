package main

import (
	"bytes"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
)

func writeFileAt(t *testing.T, root, rel, content string) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestNonDisposableIgnored(t *testing.T) {
	cases := []struct {
		name    string
		setup   func(t *testing.T, root string)
		roots   []string
		extra   []string
		wantND  []string // expected non-disposable paths; nil means all disposable
		wantAny bool     // only assert that something is non-disposable
	}{
		{
			name:  "node_modules anywhere is disposable",
			setup: func(t *testing.T, r string) { writeFileAt(t, r, "packages/ui/node_modules/x/index.js", "x") },
			roots: []string{"packages/ui/node_modules/"},
		},
		{
			name:  "pycache is disposable",
			setup: func(t *testing.T, r string) { writeFileAt(t, r, "scripts/__pycache__/a.pyc", "x") },
			roots: []string{"scripts/__pycache__/"},
		},
		{
			name:  "beads redirect only is disposable",
			setup: func(t *testing.T, r string) { writeFileAt(t, r, ".beads/redirect", "../x") },
			roots: []string{".beads/"},
		},
		{
			name: "beads with a database is kept",
			setup: func(t *testing.T, r string) {
				writeFileAt(t, r, ".beads/redirect", "../x")
				writeFileAt(t, r, ".beads/issues.jsonl", "{}")
			},
			roots:  []string{".beads/"},
			wantND: []string{".beads/issues.jsonl"},
		},
		{
			name: "gc runtime files are disposable",
			setup: func(t *testing.T, r string) {
				writeFileAt(t, r, ".gc/settings.json", "{}")
				writeFileAt(t, r, ".gc/mcp-managed/claude.json", "{}")
				writeFileAt(t, r, ".gc/mcp-locks/a.lock", "")
				writeFileAt(t, r, ".codex/.hooks.json.gascity.lock", "")
			},
			roots: []string{".gc/", ".codex/"},
		},
		{
			name: "gc-merged provider config is kept",
			setup: func(t *testing.T, r string) {
				writeFileAt(t, r, ".codex/hooks.json", "{}")
				writeFileAt(t, r, ".codex/config.toml", "")
				writeFileAt(t, r, ".mcp.json", "{}")
			},
			roots:  []string{".codex/", ".mcp.json"},
			wantND: []string{".codex/config.toml", ".mcp.json"},
		},
		{
			name: "nested worktree hidden under gc dir is kept",
			setup: func(t *testing.T, r string) {
				writeFileAt(t, r, ".gc/settings.json", "{}")
				writeFileAt(t, r, ".gc/worktrees/rig/polecats/a/.git", "gitdir: /elsewhere")
				writeFileAt(t, r, ".gc/worktrees/rig/polecats/a/notes.md", "wip")
			},
			roots:   []string{".gc/"},
			wantAny: true,
		},
		{
			name: "nested repository inside node_modules is kept",
			setup: func(t *testing.T, r string) {
				writeFileAt(t, r, "node_modules/dep/.git/HEAD", "ref: refs/heads/main")
			},
			roots:  []string{"node_modules/"},
			wantND: []string{"node_modules/dep/.git"},
		},
		{
			name:   "regular env file is kept",
			setup:  func(t *testing.T, r string) { writeFileAt(t, r, ".env.local", "SECRET=1") },
			roots:  []string{".env.local"},
			wantND: []string{".env.local"},
		},
		{
			name: "symlinked env file is disposable",
			setup: func(t *testing.T, r string) {
				target := filepath.Join(t.TempDir(), "env")
				writeFileAt(t, filepath.Dir(target), "env", "SECRET=1")
				if err := os.Symlink(target, filepath.Join(r, ".env.local")); err != nil {
					t.Skipf("symlinks unsupported: %v", err)
				}
			},
			roots: []string{".env.local"},
		},
		{
			name: "symlink to a directory is disposable and never followed",
			setup: func(t *testing.T, r string) {
				target := t.TempDir()
				writeFileAt(t, target, "precious.txt", "keep")
				writeFileAt(t, target, "repo/.git/HEAD", "x")
				if err := os.MkdirAll(filepath.Join(r, ".agents", "skills"), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, filepath.Join(r, ".agents", "skills", "basics.x")); err != nil {
					t.Skipf("symlinks unsupported: %v", err)
				}
				writeFileAt(t, r, ".agents/skills/.gc-skill-ownership.json", "{}")
			},
			roots: []string{".agents/"},
		},
		{
			name: "collapsed dir hiding a non-matching file is kept",
			setup: func(t *testing.T, r string) {
				writeFileAt(t, r, ".runtime/handoff.md", "notes")
			},
			roots:  []string{".runtime/"},
			wantND: []string{".runtime/handoff.md"},
		},
		{
			name:   "project dirs need operator config",
			setup:  func(t *testing.T, r string) { writeFileAt(t, r, "report/index.html", "x") },
			roots:  []string{"report/"},
			wantND: []string{"report/index.html"},
		},
		{
			name:  "operator pattern makes a project dir disposable",
			setup: func(t *testing.T, r string) { writeFileAt(t, r, "report/index.html", "x") },
			roots: []string{"report/"},
			extra: []string{"report/"},
		},
		{
			name:   "anchored operator pattern does not match deeper paths",
			setup:  func(t *testing.T, r string) { writeFileAt(t, r, "src/report/index.html", "x") },
			roots:  []string{"src/report/"},
			extra:  []string{"report/"},
			wantND: []string{"src/report/index.html"},
		},
		{
			name:   "directory-only pattern does not match a file",
			setup:  func(t *testing.T, r string) { writeFileAt(t, r, "node_modules", "file, not dir") },
			roots:  []string{"node_modules"},
			wantND: []string{"node_modules"},
		},
		{
			name:   "missing root fails closed",
			setup:  func(*testing.T, string) {},
			roots:  []string{"gone/"},
			wantND: []string{"gone"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			tc.setup(t, root)
			got := nonDisposableIgnored(root, tc.roots, tc.extra)
			switch {
			case tc.wantAny:
				if len(got) == 0 {
					t.Fatalf("nonDisposableIgnored = none, want the tree kept")
				}
			case tc.wantND == nil:
				if len(got) != 0 {
					t.Fatalf("nonDisposableIgnored = %q, want all disposable", got)
				}
			default:
				if strings.Join(got, ",") != strings.Join(tc.wantND, ",") {
					t.Fatalf("nonDisposableIgnored = %q, want %q", got, tc.wantND)
				}
			}
		})
	}
}

func TestNonDisposableIgnored_UnreadableDirFailsClosed(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	root := t.TempDir()
	writeFileAt(t, root, "node_modules/x/index.js", "x")
	locked := filepath.Join(root, "node_modules", "x")
	if err := os.Chmod(locked, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })

	if got := nonDisposableIgnored(root, []string{"node_modules/"}, nil); len(got) == 0 {
		t.Fatal("nonDisposableIgnored = none for an unreadable subtree, want it kept")
	}
}

// commitGitignore commits a .gitignore to the rig and pushes it so worktrees
// branched afterwards start clean.
func commitGitignore(t *testing.T, rigRoot, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(rigRoot, ".gitignore"), []byte(content), 0o644); err != nil {
		t.Fatalf("write .gitignore: %v", err)
	}
	mustGit(t, rigRoot, "add", ".gitignore")
	mustGit(t, rigRoot, "-c", "commit.gpgsign=false", "commit", "-m", "ignore")
	mustGit(t, rigRoot, "push", "origin", "main")
}

// TestReapClosedBeadWorktrees_ReapsDisposableIgnoredOnlyWorktree is the
// regression test for the reaper holding every worktree forever: gc itself
// writes ignored files (.beads/redirect, .gc/settings.json) into each task
// worktree and installs leave node_modules/, so treating any ignored file as
// unsaved work protected 27 of 38 live closed-bead worktrees indefinitely.
func TestReapClosedBeadWorktrees_ReapsDisposableIgnoredOnlyWorktree(t *testing.T) {
	cityPath, rigRoot := initReapRig(t)
	commitGitignore(t, rigRoot, ".beads/\n.gc/\nnode_modules/\n.env.local\n")
	wt := addClosedWorktree(t, rigRoot, cityPath, "builder", "ga-disp01")
	writeFileAt(t, wt, ".beads/redirect", "../../.beads")
	writeFileAt(t, wt, ".gc/settings.json", "{}")
	writeFileAt(t, wt, "node_modules/dep/index.js", "x")
	envTarget := filepath.Join(rigRoot, ".env.local")
	writeFileAt(t, rigRoot, ".env.local", "SECRET=1")
	if err := os.Symlink(envTarget, filepath.Join(wt, ".env.local")); err != nil {
		t.Skipf("symlinks unsupported: %v", err)
	}
	store := beads.NewMemStoreFrom(1, []beads.Bead{{ID: "ga-disp01", Status: "closed"}}, nil)
	injectLiveness(t, liveWorktreeState{scanned: true})

	var stderr bytes.Buffer
	report := reapClosedBeadWorktrees(cityPath, reapTestConfig(rigRoot), map[string]beads.Store{"mrig": store}, nil, false, events.Discard, nil, &stderr)

	if len(report.Reaped) != 1 {
		t.Fatalf("Reaped = %+v, want the disposable-ignored worktree reaped\nProtected: %+v\nstderr:\n%s",
			report.Reaped, report.Protected, stderr.String())
	}
	if _, err := os.Stat(wt); !os.IsNotExist(err) {
		t.Errorf("worktree %s still on disk after reap (stat err = %v)", wt, err)
	}
	if b, err := os.ReadFile(envTarget); err != nil || string(b) != "SECRET=1" {
		t.Errorf("symlink target damaged by reap: %q, %v", b, err)
	}
}

// TestReapClosedBeadWorktrees_OperatorDisposablePatterns proves the daemon
// config extends the built-in disposable set.
func TestReapClosedBeadWorktrees_OperatorDisposablePatterns(t *testing.T) {
	cityPath, rigRoot := initReapRig(t)
	commitGitignore(t, rigRoot, "report/\n")
	wt := addClosedWorktree(t, rigRoot, cityPath, "builder", "ga-disp02")
	writeFileAt(t, wt, "report/index.html", "x")
	store := beads.NewMemStoreFrom(1, []beads.Bead{{ID: "ga-disp02", Status: "closed"}}, nil)
	injectLiveness(t, liveWorktreeState{scanned: true})

	cfg := reapTestConfig(rigRoot)
	var stderr bytes.Buffer
	report := reapClosedBeadWorktrees(cityPath, cfg, map[string]beads.Store{"mrig": store}, nil, true, events.Discard, nil, &stderr)
	if len(report.Protected) != 1 || !strings.Contains(report.Protected[0].Reason, "report/index.html") {
		t.Fatalf("Protected = %+v, want report/ named as non-disposable without operator config", report.Protected)
	}

	cfg.Daemon.AutoReapDisposableIgnored = []string{"report/"}
	report = reapClosedBeadWorktrees(cityPath, cfg, map[string]beads.Store{"mrig": store}, nil, false, events.Discard, nil, &stderr)
	if len(report.Reaped) != 1 {
		t.Fatalf("Reaped = %+v, want reaped once report/ is configured disposable\nProtected: %+v", report.Reaped, report.Protected)
	}
}

// TestNonDisposableIgnored_KeptTreeSkipsDeepWalk pins the cost bound: the
// reaper classifies every candidate on every controller tick, so a tree that
// is already kept by an unmatched file must not walk its node_modules.
func TestNonDisposableIgnored_KeptTreeSkipsDeepWalk(t *testing.T) {
	root := t.TempDir()
	writeFileAt(t, root, "node_modules/x/index.js", "x")
	writeFileAt(t, root, "report/index.html", "x")
	walks := 0
	prev := walkMatchedDirsFn
	walkMatchedDirsFn = func(dir string, fn fs.WalkDirFunc) error {
		walks++
		return prev(dir, fn)
	}
	t.Cleanup(func() { walkMatchedDirsFn = prev })

	got := nonDisposableIgnored(root, []string{"node_modules/", "report/"}, nil)
	if len(got) != 1 || got[0] != "report/index.html" {
		t.Fatalf("nonDisposableIgnored = %q, want [report/index.html]", got)
	}
	if walks != 0 {
		t.Errorf("deep walks = %d, want 0 for a tree already kept by the shallow pass", walks)
	}

	if got := nonDisposableIgnored(root, []string{"node_modules/"}, nil); len(got) != 0 {
		t.Fatalf("nonDisposableIgnored = %q, want none", got)
	}
	if walks != 1 {
		t.Errorf("deep walks = %d, want 1 once the tree is otherwise disposable", walks)
	}
}

// TestReapClosedBeadWorktrees_DirectorySymlinkTargetSurvives proves end to end
// that reaping a worktree holding an ignored symlink to a directory unlinks
// the link and leaves the target, including a repository inside it, intact.
func TestReapClosedBeadWorktrees_DirectorySymlinkTargetSurvives(t *testing.T) {
	cityPath, rigRoot := initReapRig(t)
	commitGitignore(t, rigRoot, "shared\n")
	wt := addClosedWorktree(t, rigRoot, cityPath, "builder", "ga-disp03")
	target := t.TempDir()
	writeFileAt(t, target, "precious.txt", "keep")
	writeFileAt(t, target, "repo/.git/HEAD", "ref: refs/heads/main")
	if err := os.Symlink(target, filepath.Join(wt, "shared")); err != nil {
		t.Skipf("symlinks unsupported: %v", err)
	}
	store := beads.NewMemStoreFrom(1, []beads.Bead{{ID: "ga-disp03", Status: "closed"}}, nil)
	injectLiveness(t, liveWorktreeState{scanned: true})

	var stderr bytes.Buffer
	report := reapClosedBeadWorktrees(cityPath, reapTestConfig(rigRoot), map[string]beads.Store{"mrig": store}, nil, false, events.Discard, nil, &stderr)

	if len(report.Reaped) != 1 {
		t.Fatalf("Reaped = %+v, want the worktree reaped\nProtected: %+v\nstderr:\n%s", report.Reaped, report.Protected, stderr.String())
	}
	if b, err := os.ReadFile(filepath.Join(target, "precious.txt")); err != nil || string(b) != "keep" {
		t.Errorf("symlink target file damaged by reap: %q, %v", b, err)
	}
	if _, err := os.Stat(filepath.Join(target, "repo", ".git", "HEAD")); err != nil {
		t.Errorf("repository inside symlink target damaged by reap: %v", err)
	}
}
