//go:build integration

package core

import (
	"context"
	"os/exec"
	"testing"
	"time"
)

// TestEscalationReceipts owns the script/filesystem/process composition risk.
// The Python suite uses an injected clock and a strict, local mail executable.
func TestEscalationReceipts(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "python3", "escalate_behavior_test.py")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("escalation receipt suite: %v\n%s", err, output)
	}
}
