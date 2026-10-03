package beads

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	beadslib "github.com/steveyegge/beads"
)

// serializationConflictErr is the verbatim error Dolt returns when a write
// transaction loses a serialization race. Reproduced from a CI failure of
// TestHumaBinary_SessionMessageAsync, where a concurrent supervisor write made
// a session suspend fail permanently and surface as HTTP 500.
const serializationConflictErr = "sql commit (regular): Error 1213 (40001): serialization failure: " +
	"this transaction conflicts with a committed transaction from another client, try restarting transaction."

// conflictThenOKStorage fails the first failures transactions with a
// serialization conflict, then succeeds, counting every attempt.
func conflictThenOKStorage(failures int32, attempts *int32) *nativeDoltStorageSpy {
	return &nativeDoltStorageSpy{
		runInTransaction: func(context.Context, string, func(beadslib.Transaction) error) error {
			if atomic.AddInt32(attempts, 1) <= failures {
				return errors.New(serializationConflictErr)
			}
			return nil
		},
	}
}

func TestNativeDoltStoreUpdateRetriesSerializationConflict(t *testing.T) {
	var attempts int32
	store := newNativeDoltStoreForTest(conflictThenOKStorage(1, &attempts))

	if err := store.Update("gc-1", UpdateOpts{Metadata: map[string]string{"state": "suspended"}}); err != nil {
		t.Fatalf("Update after one serialization conflict: got %v, want nil", err)
	}
	if got := atomic.LoadInt32(&attempts); got != 2 {
		t.Fatalf("transaction attempts = %d, want 2 (one conflict, one retry)", got)
	}
}

func TestNativeDoltStoreUpdateStopsAtAttemptLimit(t *testing.T) {
	var attempts int32
	store := newNativeDoltStoreForTest(conflictThenOKStorage(int32(nativeWriteAttempts), &attempts))

	err := store.Update("gc-1", UpdateOpts{Metadata: map[string]string{"state": "suspended"}})
	if err == nil {
		t.Fatal("Update with unrelenting conflicts: got nil, want the serialization error")
	}
	if !isNativeDoltSerializationConflict(err) {
		t.Fatalf("returned error lost its serialization-conflict identity: %v", err)
	}
	if got := atomic.LoadInt32(&attempts); got != int32(nativeWriteAttempts) {
		t.Fatalf("transaction attempts = %d, want %d", got, nativeWriteAttempts)
	}
}

func TestNativeDoltStoreUpdateDoesNotRetryNonConflictErrors(t *testing.T) {
	var attempts int32
	// A constraint violation is a genuine fault: retrying it only multiplies
	// the write load and hides the real error behind a slower failure.
	store := newNativeDoltStoreForTest(&nativeDoltStorageSpy{
		runInTransaction: func(context.Context, string, func(beadslib.Transaction) error) error {
			atomic.AddInt32(&attempts, 1)
			return errors.New("Error 1062 (23000): duplicate entry")
		},
	})

	err := store.Update("gc-1", UpdateOpts{Metadata: map[string]string{"state": "suspended"}})
	if err == nil || !strings.Contains(err.Error(), "duplicate entry") {
		t.Fatalf("Update error = %v, want the duplicate-entry error", err)
	}
	if got := atomic.LoadInt32(&attempts); got != 1 {
		t.Fatalf("transaction attempts = %d, want 1 (non-conflict errors must not retry)", got)
	}
}

// errNativeSerializationConflict is the wire shape Dolt returns to a
// transaction that loses a serialization race. isNativeDoltSerializationConflict
// matches it, and its own doc records that such a transaction is known NOT to
// have committed — which is what makes a blind retry safe here.
var errNativeSerializationConflict = errors.New(
	"Error 1213 (40001): this transaction conflicts with a committed transaction")

// conflictThenSucceedStorage fails the first n RunInTransaction calls with a
// serialization conflict and succeeds afterwards, counting every attempt.
func conflictThenSucceedStorage(conflicts int32) (*nativeDoltStorageSpy, *int32) {
	var attempts int32
	spy := &nativeDoltStorageSpy{
		runInTransaction: func(_ context.Context, _ string, fn func(beadslib.Transaction) error) error {
			if atomic.AddInt32(&attempts, 1) <= conflicts {
				return errNativeSerializationConflict
			}
			return fn(nil)
		},
	}
	return spy, &attempts
}

// A concurrent writer losing a serialization race is normal on this fleet: the
// supervisor, the reconciler and operator requests all write during city
// startup. The losing transaction never committed, so the update must be
// retried and the caller must see success.
//
// This asserts the DEFAULT store — writeRetryEnabled is off (opt-in, see
// config.DoltConfig.WriteRetryEnabled) and our live city does not set it. A
// conflict retry that only runs inside the config-gated reconnect loop would
// be inert in production, so the retry must not depend on that gate.
func TestNativeDoltStoreUpdateRetriesSerializationConflictWithWriteRetryDisabled(t *testing.T) {
	spy, attempts := conflictThenSucceedStorage(1)
	store := newNativeDoltStoreForTest(spy)
	if store.writeRetryEnabled {
		t.Fatal("test store has writeRetryEnabled set; this must assert the production default (off)")
	}

	if err := store.Update("gc-1", UpdateOpts{}); err != nil {
		t.Fatalf("Update() = %v, want the lost serialization race retried and committed", err)
	}
	if got := atomic.LoadInt32(attempts); got != 2 {
		t.Fatalf("RunInTransaction attempts = %d, want 2 (one conflict, one success)", got)
	}
}

// The retry is bounded: a conflict on every attempt must surface the error
// rather than spin forever.
func TestNativeDoltStoreUpdateSerializationConflictIsBounded(t *testing.T) {
	spy, attempts := conflictThenSucceedStorage(1 << 30)
	store := newNativeDoltStoreForTest(spy)

	err := store.Update("gc-1", UpdateOpts{})
	if err == nil {
		t.Fatal("Update() = nil, want the exhausted conflict surfaced")
	}
	if !isNativeDoltSerializationConflict(err) {
		t.Fatalf("Update() = %v, want a serialization-conflict error", err)
	}
	if got := atomic.LoadInt32(attempts); got != nativeWriteAttempts {
		t.Fatalf("RunInTransaction attempts = %d, want the bound %d", got, nativeWriteAttempts)
	}
}

// With the reconnect loop ENABLED the conflict arm must stay distinct from the
// transient-read arm: a serialization conflict means the connection is healthy
// and only the transaction lost, so it is re-run WITHOUT a reconnect. Folding
// the two predicates together would churn the managed connection on every
// contended write.
func TestNativeDoltStoreUpdateConflictRetryDoesNotReconnect(t *testing.T) {
	spy, attempts := conflictThenSucceedStorage(1)
	store := newNativeDoltStoreForTest(spy)
	store.writeRetryEnabled = true
	var reopens int32
	store.reopen = func(context.Context) (beadslib.Storage, error) {
		atomic.AddInt32(&reopens, 1)
		return spy, nil
	}

	if err := store.Update("gc-1", UpdateOpts{}); err != nil {
		t.Fatalf("Update() = %v, want the conflict retried to success", err)
	}
	if got := atomic.LoadInt32(attempts); got != 2 {
		t.Fatalf("RunInTransaction attempts = %d, want 2", got)
	}
	if got := atomic.LoadInt32(&reopens); got != 0 {
		t.Fatalf("reopen called %d times, want 0 (a conflict needs no reconnect)", got)
	}
}

// A non-conflict write error must stay fail-fast. Retrying an ambiguous
// connection failure is exactly what the write path deliberately does not do:
// it cannot tell whether the write landed before the connection died.
func TestNativeDoltStoreUpdateDoesNotRetryAmbiguousError(t *testing.T) {
	var attempts int32
	spy := &nativeDoltStorageSpy{
		runInTransaction: func(context.Context, string, func(beadslib.Transaction) error) error {
			atomic.AddInt32(&attempts, 1)
			return errors.New("commit: connection reset by peer")
		},
	}
	store := newNativeDoltStoreForTest(spy)

	err := store.Update("gc-1", UpdateOpts{})
	if err == nil {
		t.Fatal("Update() = nil, want the ambiguous write error surfaced")
	}
	if !strings.Contains(err.Error(), "connection reset by peer") {
		t.Fatalf("Update() = %v, want the original error", err)
	}
	if got := atomic.LoadInt32(&attempts); got != 1 {
		t.Fatalf("RunInTransaction attempts = %d, want exactly 1 (no blind replay)", got)
	}
}
