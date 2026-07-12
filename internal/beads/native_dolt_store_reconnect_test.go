package beads

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	beadslib "github.com/steveyegge/beads"
)

// These tests exercise the native read-path reconnect: a read against the
// initial (dead) handle fails with a transient connection error, the injected
// reopen hook hands back a fresh (healthy) handle, and the retry succeeds. The
// reopen hook stands in for the store factory's real hook, which re-resolves the
// current managed Dolt port and re-opens against the live server.

func healthySearchStorage(issues ...*beadslib.Issue) *nativeDoltStorageSpy {
	return &nativeDoltStorageSpy{
		searchIssues: func(context.Context, string, beadslib.IssueFilter) ([]*beadslib.Issue, error) {
			return issues, nil
		},
	}
}

func deadSearchStorage(err error) *nativeDoltStorageSpy {
	return &nativeDoltStorageSpy{
		searchIssues: func(context.Context, string, beadslib.IssueFilter) ([]*beadslib.Issue, error) {
			return nil, err
		},
	}
}

// storeWithReopen builds a test NativeDoltStore starting on dead and swapping to
// fresh via the reopen hook; reopens counts hook invocations.
func storeWithReopen(dead beadslib.Storage, fresh beadslib.Storage, reopens *int32) *NativeDoltStore {
	store := newNativeDoltStoreForTest(dead)
	store.reopen = func(context.Context) (beadslib.Storage, error) {
		atomic.AddInt32(reopens, 1)
		return fresh, nil
	}
	return store
}

func TestNativeDoltStoreGetReconnectsAfterTransientConnError(t *testing.T) {
	healthy := healthySearchStorage(&beadslib.Issue{
		ID: "gc-1", Title: "recovered", Status: beadslib.StatusOpen, IssueType: beadslib.TypeTask, Priority: 2,
	})
	var reopens int32
	store := storeWithReopen(deadSearchStorage(errors.New("begin read tx: dial tcp 127.0.0.1:58216: i/o timeout")), healthy, &reopens)

	got, err := store.Get("gc-1")
	if err != nil {
		t.Fatalf("Get after transient conn error: %v", err)
	}
	if got.ID != "gc-1" {
		t.Fatalf("Get.ID = %q, want gc-1", got.ID)
	}
	if n := atomic.LoadInt32(&reopens); n == 0 {
		t.Fatalf("expected the reopen hook to fire; got %d", n)
	}
}

func TestNativeDoltStoreListReconnectsAfterTransientConnError(t *testing.T) {
	healthy := healthySearchStorage(&beadslib.Issue{
		ID: "gc-2", Title: "recovered list", Status: beadslib.StatusOpen, IssueType: beadslib.TypeTask, Priority: 2,
	})
	var reopens int32
	store := storeWithReopen(deadSearchStorage(errors.New("[mysql] i/o timeout")), healthy, &reopens)

	got, err := store.List(ListQuery{AllowScan: true, TierMode: TierBoth})
	if err != nil {
		t.Fatalf("List after transient conn error: %v", err)
	}
	if len(got) != 1 || got[0].ID != "gc-2" {
		t.Fatalf("List = %#v, want [gc-2]", got)
	}
	if n := atomic.LoadInt32(&reopens); n == 0 {
		t.Fatalf("expected the reopen hook to fire; got %d", n)
	}
}

func TestNativeDoltStoreReadDoesNotRetryNonTransientError(t *testing.T) {
	var reopens int32
	store := storeWithReopen(deadSearchStorage(errors.New("syntax error near 'FROM'")), healthySearchStorage(), &reopens)

	if _, err := store.Get("gc-1"); err == nil || !errContains(err, "syntax error") {
		t.Fatalf("Get error = %v, want the non-transient syntax error", err)
	}
	if n := atomic.LoadInt32(&reopens); n != 0 {
		t.Fatalf("non-transient error must not reconnect; got %d reopens", n)
	}
}

func TestNativeDoltStoreReadWithoutReopenHookDoesNotReconnect(t *testing.T) {
	// No reopen hook injected -> reconnect disabled, transient error returns as-is.
	store := newNativeDoltStoreForTest(deadSearchStorage(errors.New("invalid connection")))

	if _, err := store.Get("gc-1"); err == nil || !errContains(err, "invalid connection") {
		t.Fatalf("Get error = %v, want the transient error returned as-is (fail fast)", err)
	}
}

func TestNativeDoltStoreReconnectReopenErrorIsTerminalWhenNonTransient(t *testing.T) {
	store := newNativeDoltStoreForTest(deadSearchStorage(errors.New("invalid connection")))
	store.reopen = func(context.Context) (beadslib.Storage, error) {
		return nil, errors.New("permission denied resolving managed dolt port")
	}
	_, err := store.Get("gc-1")
	if err == nil || !errContains(err, "reconnect after transient read error") {
		t.Fatalf("Get error = %v, want a wrapped reconnect failure", err)
	}
	if !errContains(err, "permission denied") {
		t.Fatalf("Get error = %v, want the reopen cause preserved", err)
	}
}

func TestIsNativeDoltTransientReadError(t *testing.T) {
	transient := []string{
		"begin read tx: invalid connection",
		"[mysql] i/o timeout",
		"dial tcp 127.0.0.1:3307: connect: connection refused",
		"write: broken pipe",
		"unexpected EOF",
		"use of closed network connection",
		"bad connection",
		"read: connection reset by peer",
	}
	for _, msg := range transient {
		if !isNativeDoltTransientReadError(errors.New(msg)) {
			t.Errorf("isNativeDoltTransientReadError(%q) = false, want true", msg)
		}
	}
	permanent := []string{
		"issue gc-1 not found",
		"syntax error",
		"no rows in result set",
	}
	for _, msg := range permanent {
		if isNativeDoltTransientReadError(errors.New(msg)) {
			t.Errorf("isNativeDoltTransientReadError(%q) = true, want false", msg)
		}
	}
	if isNativeDoltTransientReadError(nil) {
		t.Errorf("isNativeDoltTransientReadError(nil) = true, want false")
	}
}

func errContains(err error, sub string) bool {
	return err != nil && strings.Contains(err.Error(), sub)
}
