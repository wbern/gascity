//go:build !linux && !darwin

package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestUnsupportedWorktreeRetirementFailsClosed(t *testing.T) {
	if err := workerDirRetryPlatformSupported(); err == nil {
		t.Fatal("unsupported platform admitted retirement registry")
	}
	if cmd, err := archiveInventoryCommand(context.Background(), "must-not-start"); err == nil || cmd != nil {
		t.Fatal("unsupported platform supplied inventory command")
	}
	if workerDirRetryOwned(nil) || workerDirRetryLinkCount(nil) != 0 || sameWorkerDirRetryOwnerLinks(nil, nil) {
		t.Fatal("unsupported metadata was accepted")
	}
	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close() //nolint:errcheck // Read-only test root has no buffered writes.
	if f, err := openWorkerDirRetryPlatformFile(root, "record", os.O_WRONLY|os.O_CREATE, 0o600); err == nil || f != nil {
		t.Fatal("unsupported platform opened metadata")
	}
	if _, err := os.Stat(filepath.Join(dir, "record")); !os.IsNotExist(err) {
		t.Fatalf("unsupported operation changed metadata: %v", err)
	}
}
