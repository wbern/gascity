package worker

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/usage"
)

func TestMessagePersistsExplicitBindingBeforeCompletion(t *testing.T) {
	ctx := context.Background()
	store := beads.NewMemStore()
	provider := runtime.NewFake()
	ledger := usage.NewInvocationLedger(filepath.Join(t.TempDir(), "invocations.jsonl"))
	headCalls := 0
	handle, err := NewSessionHandle(SessionHandleConfig{
		Manager:          sessionpkg.NewManagerWithOptions(store, provider),
		InvocationLedger: ledger,
		HeadObserver: func(context.Context, string) (usage.Head, error) {
			headCalls++
			return usage.Head{State: usage.HeadClean, SHA: "before"}, nil
		},
		Session: SessionSpec{
			Profile:  ProfileClaudeTmuxCLI,
			Template: "worker",
			Command:  "claude",
			WorkDir:  t.TempDir(),
			Provider: "claude",
		},
	})
	if err != nil {
		t.Fatalf("NewSessionHandle: %v", err)
	}
	if err := handle.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	_, err = handle.Message(ctx, MessageRequest{
		Text: "do the bounded work",
		Binding: &InvocationBinding{
			City:         "gc2",
			Rig:          "gas-city",
			WorkBeadID:   "work-original",
			Attempt:      "1",
			InvocationID: "turn-1",
		},
	})
	if err != nil {
		t.Fatalf("Message: %v", err)
	}
	if headCalls != 1 {
		t.Fatalf("pre-turn head observations = %d, want 1", headCalls)
	}

	completed, err := handle.CompleteInvocation(ctx, InvocationCompletion{
		InvocationID:      "turn-1",
		UpstreamRequestID: "provider-response-1",
		CompletionHead:    usage.Head{State: usage.HeadDirty, SHA: "after"},
	})
	if err != nil {
		t.Fatalf("CompleteInvocation: %v", err)
	}
	if completed.Attribution.WorkBeadID != "work-original" {
		t.Fatalf("WorkBeadID = %q, want original binding", completed.Attribution.WorkBeadID)
	}
	if completed.Attribution.StartHead.SHA != "before" || completed.Attribution.CompletionHead.SHA != "after" {
		t.Fatalf("heads = start=%+v completion=%+v", completed.Attribution.StartHead, completed.Attribution.CompletionHead)
	}
}

func TestObserveGitHeadReportsExplicitBoundaryStates(t *testing.T) {
	ctx := context.Background()
	missing, err := ObserveGitHead(ctx, filepath.Join(t.TempDir(), "missing"))
	if err != nil || missing != (usage.Head{State: usage.HeadMissing}) {
		t.Fatalf("missing head = %+v, %v", missing, err)
	}
	nonrepo, err := ObserveGitHead(ctx, t.TempDir())
	if err != nil || nonrepo != (usage.Head{State: usage.HeadNonRepo}) {
		t.Fatalf("nonrepo head = %+v, %v", nonrepo, err)
	}

	repo := t.TempDir()
	runBindingGit(t, repo, "init")
	unborn, err := ObserveGitHead(ctx, repo)
	if err != nil || unborn != (usage.Head{State: usage.HeadUnborn}) {
		t.Fatalf("unborn head = %+v, %v", unborn, err)
	}
	runBindingGit(t, repo, "config", "user.email", "test@example.com")
	runBindingGit(t, repo, "config", "user.name", "Test User")
	if err := os.WriteFile(filepath.Join(repo, "tracked.txt"), []byte("clean\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runBindingGit(t, repo, "add", "tracked.txt")
	runBindingGit(t, repo, "commit", "-m", "initial")
	clean, err := ObserveGitHead(ctx, repo)
	if err != nil || clean.State != usage.HeadClean || clean.SHA == "" {
		t.Fatalf("clean head = %+v, %v", clean, err)
	}
	if err := os.WriteFile(filepath.Join(repo, "tracked.txt"), []byte("dirty\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	dirty, err := ObserveGitHead(ctx, repo)
	if err != nil || dirty.State != usage.HeadDirty || dirty.SHA != clean.SHA {
		t.Fatalf("dirty head = %+v, %v (clean %+v)", dirty, err, clean)
	}
}

func runBindingGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	command := exec.Command("git", append([]string{"-C", dir}, args...)...)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, output)
	}
}
