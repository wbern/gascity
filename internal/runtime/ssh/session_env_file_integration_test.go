//go:build integration

package ssh

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestStagingScriptFailsClosedOnAShortWrite(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
	envBody := "K='v'\nexport K\n"
	script, err := stagingScript(envBody, "")
	if err != nil {
		t.Fatal(err)
	}
	short := strings.Replace(script, fmt.Sprintf("-eq %d ]", len(envBody)), "-eq 999 ]", 1)
	if short == script {
		t.Fatal("test needs a byte-count check")
	}
	tmp := t.TempDir()
	cmd := exec.Command("sh")
	cmd.Stdin = strings.NewReader(short)
	cmd.Env = append(os.Environ(), "TMPDIR="+tmp)
	if out, err := cmd.Output(); err == nil || strings.HasPrefix(lastLine(string(out)), "/") {
		t.Fatalf("short write must fail without reporting a directory: output=%q err=%v", out, err)
	}
	entries, err := os.ReadDir(tmp)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("failed staging left files behind: %v", entries)
	}
}

func TestStagingScriptCreatesPrivateFilesAndSweepsStaleDirectories(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
	if _, err := exec.LookPath("find"); err != nil {
		t.Skip("no find")
	}
	tmp := t.TempDir()
	stale := filepath.Join(tmp, stagedDirPrefix+"orphan")
	if err := os.Mkdir(stale, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stale, envFileName), []byte("K='secret'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-2 * staleStagedDirMinutes * time.Minute)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatal(err)
	}

	envBody := shellEnvFile(map[string]string{"K": "a'b\"c$d#e\nf"})
	script, err := stagingScript(envBody, tmuxCommandLine([]string{"new-session", "-d", "-e", "K=v"})+"\n")
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sh")
	cmd.Stdin = strings.NewReader(script)
	cmd.Env = append(os.Environ(), "TMPDIR="+tmp)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("run staging script: %v", err)
	}
	dir := lastLine(string(out))
	if info, err := os.Stat(dir); err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("staged directory privacy = (%v, %v)", info, err)
	}
	for _, name := range []string{envFileName, tmuxFileName} {
		info, err := os.Stat(filepath.Join(dir, name))
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("%s privacy = (%v, %v)", name, info, err)
		}
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("stale staged directory survived: %v", err)
	}
}
