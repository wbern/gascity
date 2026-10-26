package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestArchiveManagedTaskPath(t *testing.T) {
	root := t.TempDir()
	for _, tc := range []struct {
		path string
		want bool
	}{
		{"tasks/gcw-abc", true},
		{"tasks/gcw-abc-legacy", true},
		{"gcw-abc", true},
		{"crew/gcw-abc", false},
		{"polecats/gcw-abc", false},
		{"tasks/prefix-gcw-abc", false},
		{"tasks/gcw-abcd", false},
		{"tasks/gcw-abc/worktree", false},
		{"gcw-abc/nested", false},
	} {
		if got := archiveManagedTaskPath(root, filepath.Join(root, tc.path), "gcw-abc"); got != tc.want {
			t.Errorf("%s = %v", tc.path, got)
		}
	}
}

func TestArchiveRequestStrictBoundedJSON(t *testing.T) {
	dir := t.TempDir()
	for _, tc := range []struct {
		name, data string
		valid      bool
	}{
		{"object", `{"schema_version":"1"}`, true},
		{"unknown", `{"schema_version":"1","force":true}`, false},
		{"trailing", `{"schema_version":"1"} {}`, false},
		{"malformed", `{`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(dir, tc.name)
			if err := os.WriteFile(path, []byte(tc.data), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := readWorktreeArchiveRequest(path)
			if (err == nil) != tc.valid {
				t.Fatalf("error=%v", err)
			}
		})
	}
	large := filepath.Join(dir, "large")
	if err := os.WriteFile(large, make([]byte, 65537), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readWorktreeArchiveRequest(large); err == nil {
		t.Fatal("oversized request accepted")
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(filepath.Join(dir, "object"), link); err != nil {
		t.Fatal(err)
	}
	if _, err := readWorktreeArchiveRequest(link); err == nil {
		t.Fatal("symlink request accepted")
	}
}

func TestArchiveIgnoredInventoryProtectsNestedRepositoryAndSource(t *testing.T) {
	for _, kind := range []string{"case_git", "bare"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			nested := filepath.Join(dir, "node_modules", "private")
			if err := os.MkdirAll(nested, 0o700); err != nil {
				t.Fatal(err)
			}
			if kind == "case_git" {
				if err := os.WriteFile(filepath.Join(nested, ".GIT"), []byte("gitdir: somewhere"), 0o600); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := os.Mkdir(filepath.Join(nested, "objects"), 0o700); err != nil {
					t.Fatal(err)
				}
				for _, name := range []string{"HEAD", "config"} {
					if err := os.WriteFile(filepath.Join(nested, name), []byte("data"), 0o600); err != nil {
						t.Fatal(err)
					}
				}
			}
			if _, err := archiveIgnoredInventory(dir, []string{"node_modules/"}, builtinDisposableIgnored); err == nil {
				t.Fatal("nested repository admitted")
			}
			if got := nonDisposableIgnored(dir, []string{"node_modules/"}, nil); len(got) == 0 {
				t.Fatal("ordinary reaper admitted nested repository")
			}
		})
	}
	for _, path := range []string{"config/custom.py", "src/config.json", "scratch/notes.md", "../notes.md", "config/.git/HEAD"} {
		if archiveArtifactPath(path) {
			t.Errorf("source or unsafe path admitted: %s", path)
		}
	}
}

func TestArchiveSubjectRejectsForeignCommonDirectory(t *testing.T) {
	city, rig := initReapRig(t)
	wt := addClosedWorktree(t, rig, city, "tasks", "ga-identity01")
	otherCity, otherRig := initReapRig(t)
	other := addClosedWorktree(t, otherRig, otherCity, "tasks", "ga-foreign01")
	foreignPointer, err := os.ReadFile(filepath.Join(other, ".git"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt, ".git"), foreignPointer, 0o600); err != nil {
		t.Fatal(err)
	}
	wt, err = normalizeReapBorrowPath(wt)
	if err != nil {
		t.Fatal(err)
	}
	cfg := reapTestConfig(rig)
	_, err = archiveSubjectIdentity(cfg, worktreeArchiveRequest{Rig: reapTestRigName}, wt)
	if err == nil || !strings.Contains(err.Error(), "another repository") {
		t.Fatalf("foreign registered pointer accepted or wrong guard: %v", err)
	}
	// Even a linked worktree cannot be retired when it is itself the bound rig.
	cfg.Rigs[0].Path = wt
	_, err = archiveSubjectIdentity(cfg, worktreeArchiveRequest{Rig: reapTestRigName}, wt)
	if err == nil || !strings.Contains(err.Error(), "configured rig") {
		t.Fatalf("configured linked rig accepted: %v", err)
	}
}

func TestArchivePrivateDirectoryRejectsAlias(t *testing.T) {
	dir, err := normalizeReapBorrowPath(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dir, "target")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(dir, "artifacts")
	if err := os.Symlink(target, alias); err != nil {
		t.Fatal(err)
	}
	if err := archivePrivateDirectory(alias); err == nil {
		t.Fatal("archive alias accepted")
	}
	if entries, err := os.ReadDir(target); err != nil || len(entries) != 0 {
		t.Fatalf("alias target changed: entries=%v err=%v", entries, err)
	}
}

func TestWorktreeArchiveCapabilitiesJSON(t *testing.T) {
	stdout, stderr, code := runWorktreeJSONCommand(t, "worktree", "archive-reap", "--capabilities", "--json")
	if code != 0 {
		t.Fatalf("code=%d stdout=%s stderr=%s", code, stdout, stderr)
	}
	validateJSONResultSchema(t, []string{"worktree", "archive-reap"}, []byte(stdout))
	var capability worktreeArchiveCapabilities
	if err := json.Unmarshal([]byte(stdout), &capability); err != nil {
		t.Fatal(err)
	}
	if !capability.OK || capability.Operation != "worktree.archive-reap" || !capability.PlanNoMutation || capability.RequestSchemaVersion != "1" || capability.ReportSchemaVersion != "1" {
		t.Fatalf("capability=%+v", capability)
	}
}
