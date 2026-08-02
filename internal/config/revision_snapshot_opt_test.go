package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/fsys"
)

// The revision snapshot content-hashes every pack directory at load time so a
// later Revision() call can compare against the config as it was loaded. That
// costs real work on every load: profiled on `gc bd list --json --limit 5`,
// PackContentHashRecursive under captureRevisionSnapshot accounted for 16% of
// the process's CPU samples.
//
// Only long-running processes call Revision(). A one-shot CLI command loads
// config, uses it, and exits — and cmd/gc's loadCityConfig does not even return
// the Provenance, so nothing it loads can observe the snapshot at all.
// SkipRevisionSnapshot lets those callers decline the prefetch.
//
// Every read of the snapshot already falls back to reading from disk
// (writeRevisionDirHash → PackContentHashRecursive, revisionSnapshotFile →
// fs.ReadFile, revisionConventionDirs → existingConventionDiscoveryDirsFS), so
// declining it must change the revision VALUE for nobody. That is what these
// tests pin.

// TestSkipRevisionSnapshotPreservesRevisionValue is the load-bearing assertion:
// a config loaded without the snapshot must produce the same revision as one
// loaded with it. If it does not, the fallback path is not equivalent and the
// option is unsafe at any speed.
func TestSkipRevisionSnapshotPreservesRevisionValue(t *testing.T) {
	cityRoot := writeRevisionSnapshotTestCity(t)
	tomlPath := filepath.Join(cityRoot, "city.toml")

	eagerCfg, eagerProv, err := LoadWithIncludesOptions(fsys.OSFS{}, tomlPath, LoadOptions{})
	if err != nil {
		t.Fatalf("load with snapshot: %v", err)
	}
	lazyCfg, lazyProv, err := LoadWithIncludesOptions(fsys.OSFS{}, tomlPath, LoadOptions{SkipRevisionSnapshot: true})
	if err != nil {
		t.Fatalf("load without snapshot: %v", err)
	}

	if eagerProv.revisionSnapshot == nil {
		t.Fatal("default load captured no snapshot; the test no longer compares two different states")
	}
	if lazyProv.revisionSnapshot != nil {
		t.Fatal("SkipRevisionSnapshot still captured a snapshot")
	}

	eager := Revision(fsys.OSFS{}, eagerProv, eagerCfg, cityRoot)
	lazy := Revision(fsys.OSFS{}, lazyProv, lazyCfg, cityRoot)
	if eager != lazy {
		t.Fatalf("revision differs without the snapshot:\n  with    %s\n  without %s", eager, lazy)
	}
	if eager == "" {
		t.Fatal("revision is empty; the fixture is not exercising Revision")
	}
}

// TestSkipRevisionSnapshotStillDetectsPackContentChange pins that declining the
// prefetch does not blind revision comparison: a pack file edit must still
// change the revision, which is the property the reconciler depends on
// (gastownhall/gascity#779).
func TestSkipRevisionSnapshotStillDetectsPackContentChange(t *testing.T) {
	cityRoot := writeRevisionSnapshotTestCity(t)
	tomlPath := filepath.Join(cityRoot, "city.toml")

	cfg, prov, err := LoadWithIncludesOptions(fsys.OSFS{}, tomlPath, LoadOptions{SkipRevisionSnapshot: true})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	before := Revision(fsys.OSFS{}, prov, cfg, cityRoot)

	packFile := filepath.Join(cityRoot, "packs", "local", "prompts", "worker.md")
	if err := os.WriteFile(packFile, []byte("edited prompt body\n"), 0o644); err != nil {
		t.Fatalf("edit pack file: %v", err)
	}

	cfg2, prov2, err := LoadWithIncludesOptions(fsys.OSFS{}, tomlPath, LoadOptions{SkipRevisionSnapshot: true})
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	after := Revision(fsys.OSFS{}, prov2, cfg2, cityRoot)

	if before == after {
		t.Fatal("pack content edit did not change the revision; reconciler change detection would be blind")
	}
}

// TestDefaultLoadStillCapturesSnapshot pins that the option is opt-IN: every
// existing caller keeps today's load-time-faithful behavior untouched.
func TestDefaultLoadStillCapturesSnapshot(t *testing.T) {
	cityRoot := writeRevisionSnapshotTestCity(t)
	_, prov, err := LoadWithIncludes(fsys.OSFS{}, filepath.Join(cityRoot, "city.toml"))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if prov.revisionSnapshot == nil {
		t.Fatal("LoadWithIncludes captured no revision snapshot; the default changed")
	}
}

// writeRevisionSnapshotTestCity builds a minimal city with a local pack dir so
// the revision covers pack directory content, not just city.toml.
func writeRevisionSnapshotTestCity(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	packDir := filepath.Join(root, "packs", "local")
	if err := os.MkdirAll(filepath.Join(packDir, "prompts"), 0o755); err != nil {
		t.Fatalf("mkdir pack: %v", err)
	}
	writeRevisionTestFile(t, filepath.Join(packDir, "pack.toml"), `
[pack]
name = "local"
schema = 1
`)
	writeRevisionTestFile(t, filepath.Join(packDir, "prompts", "worker.md"), "worker prompt body\n")
	writeRevisionTestFile(t, filepath.Join(root, "city.toml"), `
[workspace]
name = "revision-snapshot-test"
includes = ["packs/local"]
`)
	return root
}

func writeRevisionTestFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
