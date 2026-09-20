package worker

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/gastownhall/gascity/internal/usage"
)

// InvocationBinding is the complete work identity a producer supplies before
// asking a provider to execute a turn. A nil binding means explicitly unbound;
// the worker never substitutes a current session assignment, name, workdir, or
// time-based guess.
type InvocationBinding struct {
	City         string `json:"city"`
	Rig          string `json:"rig"`
	WorkBeadID   string `json:"work_bead_id"`
	Attempt      string `json:"attempt"`
	InvocationID string `json:"invocation_id"`
}

// InvocationCompletion is an exact terminal observation from a provider or
// transcript adapter. CompletionHead must be captured by that adapter at the
// provider completion boundary; this worker API deliberately does not inspect
// current session assignment while recovering it later.
type InvocationCompletion struct {
	InvocationID      string     `json:"invocation_id"`
	UpstreamRequestID string     `json:"upstream_request_id"`
	CompletionHead    usage.Head `json:"completion_head"`
}

// HeadObserver captures the repository state at a provider turn boundary.
type HeadObserver func(context.Context, string) (usage.Head, error)

// ObserveGitHead captures one explicit git boundary state for workDir. Missing,
// non-repository, and unborn repositories are valid states rather than empty
// strings; failures to inspect an existing repository are returned so a bound
// provider turn cannot silently claim a false head.
func ObserveGitHead(ctx context.Context, workDir string) (usage.Head, error) {
	workDir = strings.TrimSpace(workDir)
	if workDir == "" {
		return usage.Head{State: usage.HeadMissing}, nil
	}
	info, err := os.Stat(workDir)
	if errors.Is(err, os.ErrNotExist) {
		return usage.Head{State: usage.HeadMissing}, nil
	}
	if err != nil {
		return usage.Head{}, fmt.Errorf("stat work directory: %w", err)
	}
	if !info.IsDir() {
		return usage.Head{}, fmt.Errorf("work directory %q is not a directory", workDir)
	}
	inside, err := gitHeadOutput(ctx, workDir, "rev-parse", "--is-inside-work-tree")
	if err != nil {
		if isNotGitRepository(err) {
			return usage.Head{State: usage.HeadNonRepo}, nil
		}
		return usage.Head{}, fmt.Errorf("inspect repository: %w", err)
	}
	if strings.TrimSpace(inside) != "true" {
		return usage.Head{State: usage.HeadNonRepo}, nil
	}
	head, err := gitHeadOutput(ctx, workDir, "rev-parse", "--verify", "HEAD")
	if err != nil {
		if exitErr := new(exec.ExitError); errors.As(err, &exitErr) {
			return usage.Head{State: usage.HeadUnborn}, nil
		}
		return usage.Head{}, fmt.Errorf("read repository head: %w", err)
	}
	head = strings.TrimSpace(head)
	if head == "" {
		return usage.Head{}, errors.New("repository head is empty")
	}
	status, err := gitHeadOutput(ctx, workDir, "status", "--porcelain=v1", "--untracked-files=normal")
	if err != nil {
		return usage.Head{}, fmt.Errorf("read repository status: %w", err)
	}
	if strings.TrimSpace(status) != "" {
		return usage.Head{State: usage.HeadDirty, SHA: head}, nil
	}
	return usage.Head{State: usage.HeadClean, SHA: head}, nil
}

func gitHeadOutput(ctx context.Context, workDir string, args ...string) (string, error) {
	command := exec.CommandContext(ctx, "git", append([]string{"-C", workDir}, args...)...)
	output, err := command.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(output)))
	}
	return string(output), nil
}

func isNotGitRepository(err error) bool {
	return strings.Contains(strings.ToLower(err.Error()), "not a git repository")
}

func (h *SessionHandle) bindInvocation(ctx context.Context, sessionID string, binding *InvocationBinding) error {
	if binding == nil {
		return nil
	}
	if h.invocationLedger == nil {
		return errors.New("explicit invocation binding requires an invocation ledger")
	}
	head, err := h.headObserver(ctx, h.session.WorkDir)
	if err != nil {
		return fmt.Errorf("capture pre-turn head: %w", err)
	}
	attribution, err := usage.NewBoundInvocationAttribution(
		binding.City,
		binding.Rig,
		binding.WorkBeadID,
		binding.Attempt,
		binding.InvocationID,
		head,
	)
	if err != nil {
		return err
	}
	if err := h.invocationLedger.Bind(ctx, usage.InvocationRecord{
		SessionID:   sessionID,
		Attribution: attribution,
	}); err != nil {
		return fmt.Errorf("persist pre-turn invocation binding: %w", err)
	}
	return nil
}

// CompleteInvocation durably records an exact provider terminal observation.
// It requires the original invocation id and a provider-captured completion
// head, so delayed recovery cannot attribute a turn through a session's current
// assignment or a fuzzy transcript match.
func (h *SessionHandle) CompleteInvocation(ctx context.Context, completion InvocationCompletion) (usage.InvocationRecord, error) {
	if h.invocationLedger == nil {
		return usage.InvocationRecord{}, errors.New("invocation completion requires an invocation ledger")
	}
	sessionID := h.currentSessionID()
	if sessionID == "" {
		return usage.InvocationRecord{}, errors.New("invocation completion requires a session id")
	}
	record, err := h.invocationLedger.Complete(ctx, sessionID, completion.InvocationID, completion.UpstreamRequestID, completion.CompletionHead)
	if err != nil {
		return usage.InvocationRecord{}, fmt.Errorf("persist invocation completion: %w", err)
	}
	return record, nil
}
