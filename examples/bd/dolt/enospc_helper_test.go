//go:build unix

package dolt_test

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// enospcImplementations returns the shell sources that must behave identically:
// the production helper (assets/scripts/dolt-enospc.sh) and the inline fallback
// copy inside gc-beads-bd.sh that focused harnesses use when the helper is not
// next to the script.
func enospcImplementations(t *testing.T) map[string]string {
	t.Helper()
	root := repoRoot(t)
	helper := filepath.Join(root, "..", "assets", "scripts", "dolt-enospc.sh")
	if _, err := os.Stat(helper); err != nil {
		t.Fatalf("production helper missing: %v", err)
	}
	src, err := os.ReadFile(filepath.Join(root, "..", "assets", "scripts", "gc-beads-bd.sh"))
	if err != nil {
		t.Fatalf("read gc-beads-bd.sh: %v", err)
	}
	text := string(src)
	const startMarker = "    # Some focused shell harnesses execute gc-beads-bd's prelude"
	const endMarker = "\nfi\n\n# op_recover stops the dolt server"
	i := strings.Index(text, startMarker)
	j := strings.Index(text, endMarker)
	if i < 0 || j < i {
		t.Fatalf("could not locate the inline fallback detector in gc-beads-bd.sh")
	}
	fallback := filepath.Join(t.TempDir(), "fallback-enospc.sh")
	if err := os.WriteFile(fallback, []byte("#!/bin/sh\n"+text[i:j]+"\n"), 0o644); err != nil {
		t.Fatalf("write fallback copy: %v", err)
	}
	return map[string]string{"helper": helper, "inline-fallback": fallback}
}

// fakeDF writes a df shim that reports availKB (1K blocks) as the free space, or
// fails when availKB is negative.
func fakeDF(t *testing.T, availKB int64) string {
	t.Helper()
	dir := t.TempDir()
	body := "#!/bin/sh\nexit 1\n"
	if availKB >= 0 {
		body = fmt.Sprintf("#!/bin/sh\nprintf 'Filesystem 1024-blocks Used Available Capacity Mounted on\\nfake 99999999 1 %d 1%% /\\n'\n", availKB)
	}
	if err := os.WriteFile(filepath.Join(dir, "df"), []byte(body), 0o755); err != nil {
		t.Fatalf("write fake df: %v", err)
	}
	return dir
}

func runEnospcDetector(t *testing.T, source, logContent string, availKB int64, extraEnv ...string) (int, string) {
	t.Helper()
	logPath := filepath.Join(t.TempDir(), "dolt.log")
	if err := os.WriteFile(logPath, []byte(logContent), 0o644); err != nil {
		t.Fatalf("write log: %v", err)
	}
	cmd := exec.Command("sh", "-c", `. "$1"; recovery_should_skip_due_to_enospc; echo "rc=$?"`, "sh", source)
	cmd.Env = append(filteredEnv("PATH"),
		"PATH="+fakeDF(t, availKB)+string(os.PathListSeparator)+os.Getenv("PATH"),
		"LOG_FILE="+logPath,
	)
	cmd.Env = append(cmd.Env, extraEnv...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("detector script failed: %v\n%s", err, out)
	}
	text := string(out)
	var rc int
	if _, err := fmt.Sscanf(text[strings.LastIndex(text, "rc="):], "rc=%d", &rc); err != nil {
		t.Fatalf("no rc in output %q", text)
	}
	return rc, text
}

const (
	enospcLine = "time=\"2026-11-21T17:03:18+01:00\" level=warning msg=\"write: no space left on device\"\n"
	gibKB      = int64(1 << 20) // 1 GiB expressed in the 1K blocks df -Pk reports
)

// TestEnospcDetectorRequiresTheDiskToStillBeShort pins the fix for the 2026-11-22
// outage: ENOSPC lines in a quiet server's log tail must not block recovery (and
// push operators into `gc dolt restart --force`) once free space is back.
func TestEnospcDetectorRequiresTheDiskToStillBeShort(t *testing.T) {
	filler := strings.Repeat("level=info msg=\"query ok\"\n", 1200)
	cases := []struct {
		name     string
		log      string
		availKB  int64
		env      []string
		wantSkip bool // rc 0 = skip recovery
		wantNote string
	}{
		{"no ENOSPC evidence, disk low", "level=info msg=ok\n", 100, nil, false, ""},
		{"evidence and disk still short", enospcLine, 100 * 1024, nil, true, ""},
		{"evidence but disk recovered (stale)", enospcLine, 10 * gibKB, nil, false, "stale"},
		{"evidence and free space cannot be measured", enospcLine, -1, nil, true, ""},
		{"threshold override above free space keeps the refusal", enospcLine, 3 * gibKB,
			[]string{"GC_DOLT_ENOSPC_CLEAR_FREE_BYTES=4294967296"}, true, ""},
		{"threshold override below free space clears it", enospcLine, 3 * gibKB,
			[]string{"GC_DOLT_ENOSPC_CLEAR_FREE_BYTES=1073741824"}, false, "stale"},
		{"junk threshold falls back to 2 GiB", enospcLine, 3 * gibKB,
			[]string{"GC_DOLT_ENOSPC_CLEAR_FREE_BYTES=abc"}, false, "stale"},
		{"junk threshold still refuses below 2 GiB", enospcLine, gibKB,
			[]string{"GC_DOLT_ENOSPC_CLEAR_FREE_BYTES=abc"}, true, ""},
		{"evidence older than the 1000-line tail is ignored", enospcLine + filler, 100, nil, false, ""},
	}
	for impl, source := range enospcImplementations(t) {
		for _, tc := range cases {
			t.Run(impl+"/"+tc.name, func(t *testing.T) {
				rc, out := runEnospcDetector(t, source, tc.log, tc.availKB, tc.env...)
				if gotSkip := rc == 0; gotSkip != tc.wantSkip {
					t.Fatalf("skip=%v, want %v\n%s", gotSkip, tc.wantSkip, out)
				}
				if tc.wantNote != "" && !strings.Contains(out, tc.wantNote) {
					t.Fatalf("expected a %q note explaining why recovery is not blocked:\n%s", tc.wantNote, out)
				}
				if tc.wantNote == "" && strings.Contains(out, "stale") {
					t.Fatalf("unexpected stale note:\n%s", out)
				}
			})
		}
	}
}
