package usage

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
)

func TestTerminalOutboxReplaysFailedDeliveryWithoutDuplicatingCommittedOutcome(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "terminal-outbox.jsonl")
	outcome, err := NewTerminalOutcome("gc2", "gas-city", "work-1", "1", "shipped", 123)
	if err != nil {
		t.Fatal(err)
	}

	outbox := NewTerminalOutbox(path)
	if err := outbox.Enqueue(ctx, outcome); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if err := outbox.Enqueue(ctx, outcome); err != nil {
		t.Fatalf("idempotent Enqueue: %v", err)
	}

	attempts := 0
	err = outbox.Deliver(ctx, func(_ context.Context, got TerminalOutcome) error {
		attempts++
		if got.Key != outcome.Key {
			t.Fatalf("outcome key = %q, want %q", got.Key, outcome.Key)
		}
		return errors.New("transient sink failure")
	})
	if err == nil {
		t.Fatal("Deliver succeeded after sink failure")
	}
	if attempts != 1 {
		t.Fatalf("failed delivery attempts = %d, want 1", attempts)
	}

	// A new process must reconstruct the pending record and replay the same
	// idempotency key. The receiver owns external dedupe if it accepted a record
	// before the outbox could persist its acknowledgement.
	restarted := NewTerminalOutbox(path)
	if err := restarted.Deliver(ctx, func(_ context.Context, got TerminalOutcome) error {
		attempts++
		if got != outcome {
			t.Fatalf("replayed outcome = %+v, want %+v", got, outcome)
		}
		return nil
	}); err != nil {
		t.Fatalf("replay Deliver: %v", err)
	}
	if attempts != 2 {
		t.Fatalf("delivery attempts = %d, want failed + replay", attempts)
	}
	if err := restarted.Deliver(ctx, func(context.Context, TerminalOutcome) error {
		t.Fatal("delivered outcome was replayed")
		return nil
	}); err != nil {
		t.Fatalf("second Deliver: %v", err)
	}
}

func TestTerminalOutcomeKeyUsesAttemptSoReopenIsDistinct(t *testing.T) {
	first, err := NewTerminalOutcome("gc2", "gas-city", "work-1", "1", "shipped", 1)
	if err != nil {
		t.Fatal(err)
	}
	reopened, err := NewTerminalOutcome("gc2", "gas-city", "work-1", "2", "shipped", 2)
	if err != nil {
		t.Fatal(err)
	}
	if first.Key == reopened.Key {
		t.Fatalf("reopened attempt reused terminal key %q", first.Key)
	}
}

func TestTerminalOutboxRejectsConflictingOutcomeForSameAttempt(t *testing.T) {
	ctx := context.Background()
	outbox := NewTerminalOutbox(filepath.Join(t.TempDir(), "terminal-outbox.jsonl"))
	shipped, err := NewTerminalOutcome("gc2", "gas-city", "work-1", "1", "shipped", 1)
	if err != nil {
		t.Fatal(err)
	}
	blocked, err := NewTerminalOutcome("gc2", "gas-city", "work-1", "1", "blocked", 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := outbox.Enqueue(ctx, shipped); err != nil {
		t.Fatalf("Enqueue(shipped): %v", err)
	}
	shippedRetry, err := NewTerminalOutcome("gc2", "gas-city", "work-1", "1", "shipped", 2)
	if err != nil {
		t.Fatal(err)
	}
	if err := outbox.Enqueue(ctx, shippedRetry); err != nil {
		t.Fatalf("idempotent Enqueue(shipped retry): %v", err)
	}
	if err := outbox.Enqueue(ctx, blocked); err == nil {
		t.Fatal("conflicting terminal outcome for one attempt was accepted")
	}
}

func TestTerminalOutcomeRejectsUnboundedOutcomeValues(t *testing.T) {
	if _, err := NewTerminalOutcome("gc2", "gas-city", "work-1", "1", "secret-looking-freeform-outcome", 1); err == nil {
		t.Fatal("freeform terminal outcome was accepted")
	}
}
