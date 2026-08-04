package main

import (
	"bytes"
	"io"
	"reflect"
	"strings"
	"testing"
)

func TestRunRoutesPRGateCreateThroughGCGuard(t *testing.T) {
	original := executeGCBD
	t.Cleanup(func() { executeGCBD = original })

	var got []string
	executeGCBD = func(args []string, _ io.Reader, _, _ io.Writer) int {
		got = append([]string(nil), args...)
		return 23
	}

	var stderr bytes.Buffer
	args := []string{"gate", "create", "--blocks", "gcw-task", "--type", "gh:pr", "--await-id", "42"}
	if code := run(args, strings.NewReader(""), &bytes.Buffer{}, &stderr); code != 23 {
		t.Fatalf("run() = %d, want delegated exit 23; stderr=%q", code, stderr.String())
	}
	if !reflect.DeepEqual(got, args) {
		t.Fatalf("guard args = %#v, want %#v", got, args)
	}
}

func TestRunPinnedPRGatePassesThroughWithoutReenteringGC(t *testing.T) {
	t.Setenv(storeScopeEnv, "rig")
	dir := t.TempDir()
	t.Setenv(realBdEnvVar, fakeBd(t, dir, dir+"/calls.txt", 0))

	original := executeGCBD
	t.Cleanup(func() { executeGCBD = original })
	executeGCBD = func([]string, io.Reader, io.Writer, io.Writer) int {
		t.Fatal("pinned gc child reentered gc")
		return 1
	}

	var stderr bytes.Buffer
	code := run(
		[]string{"gate", "create", "--blocks", "gcw-task", "--type", "gh:pr", "--await-id", "42"},
		strings.NewReader(""),
		&bytes.Buffer{},
		&stderr,
	)
	if code != 0 {
		t.Fatalf("run() = %d, want real-bd passthrough success; stderr=%q", code, stderr.String())
	}
}

func TestRunRefusesMalformedPRGateBeforeExecution(t *testing.T) {
	original := executeGCBD
	t.Cleanup(func() { executeGCBD = original })

	called := false
	executeGCBD = func([]string, io.Reader, io.Writer, io.Writer) int {
		called = true
		return 0
	}

	var stderr bytes.Buffer
	code := run(
		[]string{"gate", "create", "--blocks", "gcw-task", "--type", "gh:pr"},
		strings.NewReader(""),
		&bytes.Buffer{},
		&stderr,
	)
	if code == 0 {
		t.Fatalf("run() accepted malformed gh:pr gate; stderr=%q", stderr.String())
	}
	if called {
		t.Fatal("run() executed malformed gh:pr gate")
	}
	if !strings.Contains(stderr.String(), "--await-id") {
		t.Fatalf("stderr = %q, want missing --await-id explanation", stderr.String())
	}
}
