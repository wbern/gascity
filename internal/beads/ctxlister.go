package beads

import "context"

// CtxLister is an optional Store capability for context-cancellable list
// reads. Stores backed by an external service should implement it so callers
// can release the underlying read when their deadline expires instead of
// abandoning a goroutine that remains blocked in List.
//
// Implementations keep List as a context.Background shim for compatibility
// with the Store interface.
type CtxLister interface {
	ListCtx(ctx context.Context, query ListQuery) ([]Bead, error)
}
