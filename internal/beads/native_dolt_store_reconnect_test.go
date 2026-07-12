package beads

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	beadslib "github.com/steveyegge/beads"
)

// transientThenHealthyStorage helpers build a NativeDoltStore whose initial
// handle fails every read with a transient connection error, plus a healthy
// replacement handed back by the overridden open. Together they exercise the
// read-path reconnect: attempt 1 fails transient -> reconnect swaps the dead
// handle for the healthy one -> attempt 2 succeeds.

func healthySearchStorage(issues ...*beadslib.Issue) *nativeDoltStorageSpy {
	return &nativeDoltStorageSpy{
		searchIssues: func(context.Context, string, beadslib.IssueFilter) ([]*beadslib.Issue, error) {
			return issues, nil
		},
	}
}

func TestNativeDoltStoreGetReconnectsAfterTransientConnError(t *testing.T) {
	healthy := healthySearchStorage(&beadslib.Issue{
		ID: "gc-1", Title: "recovered", Status: beadslib.StatusOpen, IssueType: beadslib.TypeTask, Priority: 2,
	})
	var reopens int32
	oldOpen := nativeDoltOpenBestAvailable
	t.Cleanup(func() { nativeDoltOpenBestAvailable = oldOpen })
	nativeDoltOpenBestAvailable = func(context.Context, string) (beadslib.Storage, error) {
		atomic.AddInt32(&reopens, 1)
		return healthy, nil
	}

	dead := &nativeDoltStorageSpy{
		searchIssues: func(context.Context, string, beadslib.IssueFilter) ([]*beadslib.Issue, error) {
			return nil, errors.New("begin read tx: invalid connection")
		},
	}
	store := newNativeDoltStoreForTest(dead)
	store.scopeRoot = t.TempDir() // enable reconnect

	got, err := store.Get("gc-1")
	if err != nil {
		t.Fatalf("Get after transient conn error: %v", err)
	}
	if got.ID != "gc-1" {
		t.Fatalf("Get.ID = %q, want gc-1", got.ID)
	}
	if n := atomic.LoadInt32(&reopens); n == 0 {
		t.Fatalf("expected a reconnect (nativeDoltOpenBestAvailable call); got %d", n)
	}
}

func TestNativeDoltStoreListReconnectsAfterTransientConnError(t *testing.T) {
	healthy := healthySearchStorage(&beadslib.Issue{
		ID: "gc-2", Title: "recovered list", Status: beadslib.StatusOpen, IssueType: beadslib.TypeTask, Priority: 2,
	})
	var reopens int32
	oldOpen := nativeDoltOpenBestAvailable
	t.Cleanup(func() { nativeDoltOpenBestAvailable = oldOpen })
	nativeDoltOpenBestAvailable = func(context.Context, string) (beadslib.Storage, error) {
		atomic.AddInt32(&reopens, 1)
		return healthy, nil
	}

	dead := &nativeDoltStorageSpy{
		searchIssues: func(context.Context, string, beadslib.IssueFilter) ([]*beadslib.Issue, error) {
			return nil, errors.New("[mysql] i/o timeout")
		},
	}
	store := newNativeDoltStoreForTest(dead)
	store.scopeRoot = t.TempDir()

	got, err := store.List(ListQuery{AllowScan: true, TierMode: TierBoth})
	if err != nil {
		t.Fatalf("List after transient conn error: %v", err)
	}
	if len(got) != 1 || got[0].ID != "gc-2" {
		t.Fatalf("List = %#v, want [gc-2]", got)
	}
	if n := atomic.LoadInt32(&reopens); n == 0 {
		t.Fatalf("expected a reconnect (nativeDoltOpenBestAvailable call); got %d", n)
	}
}

func TestNativeDoltStoreReadDoesNotRetryNonTransientError(t *testing.T) {
	var reopens int32
	oldOpen := nativeDoltOpenBestAvailable
	t.Cleanup(func() { nativeDoltOpenBestAvailable = oldOpen })
	nativeDoltOpenBestAvailable = func(context.Context, string) (beadslib.Storage, error) {
		atomic.AddInt32(&reopens, 1)
		return nil, errors.New("should not be called")
	}

	permanent := errors.New("syntax error near 'FROM'")
	storage := &nativeDoltStorageSpy{
		searchIssues: func(context.Context, string, beadslib.IssueFilter) ([]*beadslib.Issue, error) {
			return nil, permanent
		},
	}
	store := newNativeDoltStoreForTest(storage)
	store.scopeRoot = t.TempDir()

	if _, err := store.Get("gc-1"); err == nil || !errContains(err, "syntax error") {
		t.Fatalf("Get error = %v, want the non-transient syntax error", err)
	}
	if n := atomic.LoadInt32(&reopens); n != 0 {
		t.Fatalf("non-transient error must not reconnect; got %d reopens", n)
	}
}

func TestNativeDoltStoreReadWithoutScopeRootDoesNotReconnect(t *testing.T) {
	var reopens int32
	oldOpen := nativeDoltOpenBestAvailable
	t.Cleanup(func() { nativeDoltOpenBestAvailable = oldOpen })
	nativeDoltOpenBestAvailable = func(context.Context, string) (beadslib.Storage, error) {
		atomic.AddInt32(&reopens, 1)
		return nil, errors.New("should not be called")
	}

	dead := &nativeDoltStorageSpy{
		searchIssues: func(context.Context, string, beadslib.IssueFilter) ([]*beadslib.Issue, error) {
			return nil, errors.New("invalid connection")
		},
	}
	store := newNativeDoltStoreForTest(dead) // scopeRoot empty -> reconnect disabled

	if _, err := store.Get("gc-1"); err == nil || !errContains(err, "invalid connection") {
		t.Fatalf("Get error = %v, want the transient error returned as-is (fail fast)", err)
	}
	if n := atomic.LoadInt32(&reopens); n != 0 {
		t.Fatalf("a store without a captured scopeRoot must not reconnect; got %d reopens", n)
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
