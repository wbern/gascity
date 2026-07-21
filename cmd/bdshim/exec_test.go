package main

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestExecRealBdFailsLoudlyWhenChildCannotStart(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX executable permission semantics")
	}
	path := filepath.Join(t.TempDir(), "not-executable")
	if err := os.WriteFile(path, []byte("not executable\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(realBdEnvVar, path)

	var stderr bytes.Buffer
	code := execRealBd([]string{"update", "gcw-1"}, "", nil, strings.NewReader(""), &bytes.Buffer{}, &stderr)
	if code == 0 {
		t.Fatalf("execRealBd() = 0 when child cannot start; stderr=%q", stderr.String())
	}
	if strings.TrimSpace(stderr.String()) == "" {
		t.Fatal("execRealBd() wrote no error when child cannot start")
	}
}
