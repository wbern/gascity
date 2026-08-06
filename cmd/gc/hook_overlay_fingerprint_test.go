package main

import (
	"os"
	"path"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/overlay"
	"github.com/gastownhall/gascity/internal/runtime"
)

// hookOverlayFixture lays out a city with one pack overlay that owns
// per-provider/gemini/.gemini/settings.json plus a workdir copy, and returns
// (workDir, overlayDir, overlaySrc, workCopy).
func hookOverlayFixture(t *testing.T, workContent, overlayContent string) (string, string, string, string) {
	t.Helper()
	cityPath := t.TempDir()
	overlayDir := filepath.Join(cityPath, "packs", "core", "overlay")
	overlaySrc := filepath.Join(overlayDir, overlay.PerProviderDir, "gemini", ".gemini", "settings.json")
	workDir := filepath.Join(cityPath, "worker")
	workCopy := filepath.Join(workDir, ".gemini", "settings.json")
	for _, dir := range []string{filepath.Dir(overlaySrc), filepath.Dir(workCopy)} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("MkdirAll(%q): %v", dir, err)
		}
	}
	if overlayContent != "" {
		if err := os.WriteFile(overlaySrc, []byte(overlayContent), 0o644); err != nil {
			t.Fatalf("WriteFile(%q): %v", overlaySrc, err)
		}
	}
	if err := os.WriteFile(workCopy, []byte(workContent), 0o644); err != nil {
		t.Fatalf("WriteFile(%q): %v", workCopy, err)
	}
	return workDir, overlayDir, overlaySrc, workCopy
}

func geminiHookEntry(t *testing.T, entries []runtime.CopyEntry) runtime.CopyEntry {
	t.Helper()
	want := path.Join("worker", ".gemini", "settings.json")
	for _, e := range entries {
		if e.RelDst == want {
			return e
		}
	}
	t.Fatalf("no CopyEntry for %q in %#v", want, entries)
	return runtime.CopyEntry{}
}

// TestStageHookFiles_HashIsStableAcrossOverlayStaging pins the core invariant:
// the fingerprint must be computed over the overlay SOURCE that session staging
// will write, not over the workdir destination that staging rewrites. Rewriting
// the destination into its canonical/merged form is not config drift.
func TestStageHookFiles_HashIsStableAcrossOverlayStaging(t *testing.T) {
	nonCanonical := `{"hooks":{"SessionStart":[{"hooks":[{"type":"command","command":"gc prime --hook"}],"matcher":""}]}}` + "\n"
	canonical := "{\n  \"hooks\": {\n    \"SessionStart\": [\n      {\n        \"matcher\": \"\",\n        \"hooks\": [\n          {\n            \"type\": \"command\",\n            \"command\": \"gc prime --hook\"\n          }\n        ]\n      }\n    ]\n  }\n}\n"
	workDir, overlayDir, _, workCopy := hookOverlayFixture(t, nonCanonical, canonical)
	cityPath := filepath.Dir(workDir)
	providers := []string{"gemini"}
	dirs := []string{overlayDir}

	before := geminiHookEntry(t, stageHookFiles(nil, cityPath, workDir, providers, dirs))

	// Simulate what session staging does to the destination.
	if err := os.WriteFile(workCopy, []byte(canonical), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	after := geminiHookEntry(t, stageHookFiles(nil, cityPath, workDir, providers, dirs))

	if before.ContentHash != after.ContentHash {
		t.Fatalf("hook ContentHash moved when staging rewrote the destination: before=%q after=%q", before.ContentHash, after.ContentHash)
	}
	if before.ContentHash == "" {
		t.Fatal("hook ContentHash is empty: the entry lost content fingerprinting entirely")
	}
}

// TestStageHookFiles_OverlayEditMovesHash forbids the tempting "just make it
// path-only" fix. Overlay dir CONTENT is hashed nowhere else in the
// fingerprint, so this entry is the only detector of a genuine hook edit. If
// this test fails, the fleet has been blinded to hook changes.
func TestStageHookFiles_OverlayEditMovesHash(t *testing.T) {
	workDir, overlayDir, overlaySrc, _ := hookOverlayFixture(t, `{"hooks":{}}`, `{"hooks":{}}`)
	cityPath := filepath.Dir(workDir)
	providers := []string{"gemini"}
	dirs := []string{overlayDir}

	before := geminiHookEntry(t, stageHookFiles(nil, cityPath, workDir, providers, dirs))

	if err := os.WriteFile(overlaySrc, []byte(`{"hooks":{"SessionStart":[]}}`), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	after := geminiHookEntry(t, stageHookFiles(nil, cityPath, workDir, providers, dirs))

	if before.ContentHash == after.ContentHash {
		t.Fatalf("editing the hook overlay source did NOT move ContentHash (%q): genuine hook edits are undetected", before.ContentHash)
	}
}

// TestStageHookFiles_UnmanagedHookFileStillContentHashed covers the case with
// no overlay source: a user-authored hook file nothing overwrites must keep
// full content fingerprinting off the destination.
func TestStageHookFiles_UnmanagedHookFileStillContentHashed(t *testing.T) {
	workDir, overlayDir, _, workCopy := hookOverlayFixture(t, `{"hooks":{}}`, "")
	cityPath := filepath.Dir(workDir)
	providers := []string{"gemini"}
	dirs := []string{overlayDir}

	before := geminiHookEntry(t, stageHookFiles(nil, cityPath, workDir, providers, dirs))
	if before.ContentHash == "" {
		t.Fatal("unmanaged hook file lost content fingerprinting")
	}

	if err := os.WriteFile(workCopy, []byte(`{"hooks":{"SessionStart":[]}}`), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	after := geminiHookEntry(t, stageHookFiles(nil, cityPath, workDir, providers, dirs))
	if before.ContentHash == after.ContentHash {
		t.Fatal("editing an unmanaged hook file did not move ContentHash")
	}
}

// TestStageHookFiles_AllLayeredSourcesContribute forbids a first-match-wins
// implementation. With two overlay dirs and two provider slots, staging applies
// every matching layer, so editing ANY of them must move the hash — a hash that
// stops at the first source it finds silently ignores later layers that
// overwrite it.
func TestStageHookFiles_AllLayeredSourcesContribute(t *testing.T) {
	cityPath := t.TempDir()
	workDir := filepath.Join(cityPath, "worker")
	workCopy := filepath.Join(workDir, ".gemini", "settings.json")
	if err := os.MkdirAll(filepath.Dir(workCopy), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(workCopy, []byte(`{"hooks":{}}`), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	packA := filepath.Join(cityPath, "packs", "a", "overlay")
	packB := filepath.Join(cityPath, "packs", "b", "overlay")
	// Four distinct layers staging would apply, in order.
	sources := []string{
		filepath.Join(packA, ".gemini", "settings.json"),
		filepath.Join(packA, overlay.PerProviderDir, "gemini", ".gemini", "settings.json"),
		filepath.Join(packB, ".gemini", "settings.json"),
		filepath.Join(packB, overlay.PerProviderDir, "codex", ".gemini", "settings.json"),
	}
	for i, src := range sources {
		if err := os.MkdirAll(filepath.Dir(src), 0o755); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
		if err := os.WriteFile(src, []byte(`{"layer":`+string(rune('0'+i))+`}`), 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
	}
	providers := []string{"gemini", "codex"}
	dirs := []string{packA, packB}

	base := geminiHookEntry(t, stageHookFiles(nil, cityPath, workDir, providers, dirs)).ContentHash
	if base == "" {
		t.Fatal("empty ContentHash with sources present")
	}

	for i, src := range sources {
		original, err := os.ReadFile(src)
		if err != nil {
			t.Fatalf("ReadFile: %v", err)
		}
		if err := os.WriteFile(src, []byte(`{"edited":true}`), 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		got := geminiHookEntry(t, stageHookFiles(nil, cityPath, workDir, providers, dirs)).ContentHash
		if got == base {
			t.Errorf("editing layer %d (%s) did not move ContentHash: a first-match-wins hash ignores it", i, src)
		}
		if err := os.WriteFile(src, original, 0o644); err != nil {
			t.Fatalf("restore: %v", err)
		}
	}
}

// TestStageHookFiles_UnreadableSourceYieldsSentinel pins that an owning source
// which cannot be read yields the stable empty sentinel rather than falling
// back to hashing the destination. Falling back would flip the entry between
// two unrelated values on a transient error and cost two restarts.
func TestStageHookFiles_UnreadableSourceYieldsSentinel(t *testing.T) {
	workDir, overlayDir, overlaySrc, _ := hookOverlayFixture(t, `{"hooks":{}}`, `{"hooks":{}}`)
	cityPath := filepath.Dir(workDir)
	providers := []string{"gemini"}
	dirs := []string{overlayDir}

	if err := os.Chmod(overlaySrc, 0o000); err != nil {
		t.Skipf("cannot chmod source unreadable: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(overlaySrc, 0o644) })
	if data, err := os.ReadFile(overlaySrc); err == nil {
		t.Skipf("source still readable (running as root?): %d bytes", len(data))
	}

	got := geminiHookEntry(t, stageHookFiles(nil, cityPath, workDir, providers, dirs))
	if got.ContentHash != "" {
		t.Fatalf("unreadable owning source produced ContentHash %q, want the empty sentinel", got.ContentHash)
	}
}

// TestStageHookFiles_EmitsEntryForNotYetStagedOverlayFile pins gcw-u67z. A
// mergeable hook file whose provider slot is absent from install_agent_hooks is
// deliberately NOT created before fingerprinting (the pre-fingerprint
// materializer skips mergeable paths so hooks.Install can own them), but
// session start stages it with no such skip. If the CopyEntry is emitted only
// when the destination already exists, the entry goes absent -> present across
// staging and the CopyFiles field hash moves: config drift, and a restart.
//
// The entry must therefore be emitted whenever an overlay owns the path, even
// though the destination does not exist yet.
func TestStageHookFiles_EmitsEntryForNotYetStagedOverlayFile(t *testing.T) {
	cityPath := t.TempDir()
	overlayDir := filepath.Join(cityPath, "packs", "core", "overlay")
	overlaySrc := filepath.Join(overlayDir, overlay.PerProviderDir, "gemini", ".gemini", "settings.json")
	workDir := filepath.Join(cityPath, "worker")
	if err := os.MkdirAll(filepath.Dir(overlaySrc), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(overlaySrc, []byte(`{"hooks":{"SessionStart":[]}}`), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	providers := []string{"gemini"}
	dirs := []string{overlayDir}
	workCopy := filepath.Join(workDir, ".gemini", "settings.json")

	// Destination does NOT exist yet — this is the pre-staging state.
	if _, err := os.Stat(workCopy); !os.IsNotExist(err) {
		t.Fatalf("fixture broken: destination should not exist yet (%v)", err)
	}
	before := geminiHookEntry(t, stageHookFiles(nil, cityPath, workDir, providers, dirs))
	if before.ContentHash == "" {
		t.Fatal("entry for a not-yet-staged overlay-owned hook file has no ContentHash")
	}

	// Simulate session start staging the overlay into the workdir.
	if err := os.MkdirAll(filepath.Dir(workCopy), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(workCopy, []byte(`{"hooks":{"SessionStart":[]}}`), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	after := geminiHookEntry(t, stageHookFiles(nil, cityPath, workDir, providers, dirs))
	if before.ContentHash != after.ContentHash {
		t.Fatalf("ContentHash moved once staging created the file: before=%q after=%q", before.ContentHash, after.ContentHash)
	}
	if before.RelDst != after.RelDst || before.Probed != after.Probed || before.Src != after.Src {
		t.Fatalf("entry shape changed across staging:\n before=%#v\n after=%#v", before, after)
	}
}

// TestStageHookFiles_NoEntryWhenNeitherSideExists pins the negative: a hook
// path that neither the workdir nor any overlay owns must stay unstaged.
// Emitting an entry for it would fabricate a fingerprint input and, on
// container runtimes, a copy of a file that does not exist anywhere.
func TestStageHookFiles_NoEntryWhenNeitherSideExists(t *testing.T) {
	cityPath := t.TempDir()
	overlayDir := filepath.Join(cityPath, "packs", "core", "overlay")
	workDir := filepath.Join(cityPath, "worker")
	for _, d := range []string{overlayDir, workDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
	}
	got := stageHookFiles(nil, cityPath, workDir, []string{"gemini"}, []string{overlayDir})
	for _, e := range got {
		if e.RelDst == path.Join("worker", ".gemini", "settings.json") {
			t.Fatalf("staged a hook entry with no workdir file and no overlay source: %#v", e)
		}
	}
}

// TestStageHookFiles_UniversalOverlaySourceIsHashed covers a hook file shipped
// at the overlay root rather than in a per-provider slot.
func TestStageHookFiles_UniversalOverlaySourceIsHashed(t *testing.T) {
	cityPath := t.TempDir()
	overlayDir := filepath.Join(cityPath, "packs", "core", "overlay")
	universalSrc := filepath.Join(overlayDir, ".gemini", "settings.json")
	workDir := filepath.Join(cityPath, "worker")
	workCopy := filepath.Join(workDir, ".gemini", "settings.json")
	for _, dir := range []string{filepath.Dir(universalSrc), filepath.Dir(workCopy)} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
	}
	if err := os.WriteFile(universalSrc, []byte(`{"hooks":{}}`), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := os.WriteFile(workCopy, []byte(`{"stale":true}`), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	providers := []string{"gemini"}
	dirs := []string{overlayDir}

	before := geminiHookEntry(t, stageHookFiles(nil, cityPath, workDir, providers, dirs))

	// Destination churn must not move the hash when a universal source owns it.
	if err := os.WriteFile(workCopy, []byte(`{"hooks":{}}`), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	after := geminiHookEntry(t, stageHookFiles(nil, cityPath, workDir, providers, dirs))
	if before.ContentHash != after.ContentHash {
		t.Fatalf("universal overlay source not used for hashing: before=%q after=%q", before.ContentHash, after.ContentHash)
	}

	if err := os.WriteFile(universalSrc, []byte(`{"hooks":{"SessionStart":[]}}`), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	edited := geminiHookEntry(t, stageHookFiles(nil, cityPath, workDir, providers, dirs))
	if edited.ContentHash == before.ContentHash {
		t.Fatal("editing a universal overlay source did not move ContentHash")
	}
}
