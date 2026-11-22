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
	i := strings.Index(text, startMarker)
	if i < 0 {
		t.Fatalf("could not locate the inline fallback detector in gc-beads-bd.sh")
	}
	// The first column-0 "fi" after the marker closes the if/else around the fallback.
	// Anything that follows (the fsync-stall helper, op_recover) must not be captured.
	k := strings.Index(text[i:], "\nfi\n")
	if k < 0 {
		t.Fatalf("could not find the end of the inline fallback detector")
	}
	j := i + k
	if strings.Contains(text[i:j], "fsync_stall") {
		t.Fatalf("fallback extraction swallowed unrelated code")
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
	return fakeDFDevice(t, "fake", availKB)
}

// fakeDFDevice is fakeDF with a chosen device name (which may contain spaces) and a
// mount point containing a space, to prove the parser anchors on the Capacity column.
func fakeDFDevice(t *testing.T, device string, availKB int64) string {
	t.Helper()
	dir := t.TempDir()
	body := "#!/bin/sh\nexit 1\n"
	if availKB >= 0 {
		// A heredoc, not printf: printf would swallow the "%" of the Capacity column,
		// and real df -Pk output must be reproduced exactly for the parser under test.
		body = fmt.Sprintf("#!/bin/sh\ncat <<'EOF'\nFilesystem 1024-blocks Used Available Capacity Mounted on\n%s 99999999 1 %d 1%% /mnt/a b\nEOF\n", device, availKB)
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

// TestEnospcDetectorParsesDfWithSpacesInNames: a device or mount name containing
// spaces must not shift the Available column into Used.
func TestEnospcDetectorParsesDfWithSpacesInNames(t *testing.T) {
	for impl, source := range enospcImplementations(t) {
		t.Run(impl, func(t *testing.T) {
			logPath := filepath.Join(t.TempDir(), "dolt.log")
			if err := os.WriteFile(logPath, []byte(enospcLine), 0o644); err != nil {
				t.Fatal(err)
			}
			run := func(availKB int64) string {
				cmd := exec.Command("sh", "-c", `. "$1"; recovery_should_skip_due_to_enospc; echo "rc=$?"`, "sh", source)
				cmd.Env = append(filteredEnv("PATH"),
					"PATH="+fakeDFDevice(t, "my weird dev", availKB)+string(os.PathListSeparator)+os.Getenv("PATH"),
					"LOG_FILE="+logPath)
				out, err := cmd.CombinedOutput()
				if err != nil {
					t.Fatalf("detector failed: %v\n%s", err, out)
				}
				return string(out)
			}
			if out := run(10 * gibKB); !strings.Contains(out, "rc=1") || !strings.Contains(out, "stale") {
				t.Fatalf("10 GiB free on a spaced device name must read as recovered:\n%s", out)
			}
			if out := run(100 * 1024); !strings.Contains(out, "rc=0") {
				t.Fatalf("100 MiB free on a spaced device name must keep the refusal:\n%s", out)
			}
		})
	}
}
