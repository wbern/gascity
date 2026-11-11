package nudgequeue

import "github.com/gastownhall/gascity/internal/beads"

// StateFileQueue is the two verbs of the city's state.json nudge queue that
// packages below cmd/gc write through: the deferred-submit append and the
// wait-nudge withdrawal. cmd/gc's file queue delegates these two verbs here, so
// each has exactly one implementation.
type StateFileQueue struct {
	// CityPath is the city whose queue this is.
	CityPath string
	// Shadows is the nudges-class store holding the queue's shadow beads; nil
	// means the withdrawal removes queue entries and touches no shadow.
	Shadows beads.Store
}

// EnqueueDeferred appends item to the pending bucket. It runs no maintenance,
// writes no shadow and does not check for a duplicate id: a deferred submit
// mints a fresh id for every message.
//
// Fork behavior: an item that carries a Reference supersedes every still-pending
// item of the same session, continuation epoch, source and reference; the older
// ones move to the dead bucket with a "superseded" reason (the replace-key
// contract of session submit).
func (q StateFileQueue) EnqueueDeferred(item Item) error {
	return WithStateFor(q.CityPath, "enqueue_deferred", func(state *State) error {
		if item.Reference != nil {
			now := item.CreatedAt
			remaining := state.Pending[:0]
			for _, pending := range state.Pending {
				if pending.SessionID == item.SessionID && pending.ContinuationEpoch == item.ContinuationEpoch && pending.Source == item.Source && pending.Reference != nil && pending.Reference.Kind == item.Reference.Kind && pending.Reference.ID == item.Reference.ID {
					pending.DeadAt = now
					pending.LastError = "superseded by newer deferred session submit"
					state.Dead = append(state.Dead, pending)
					continue
				}
				remaining = append(remaining, pending)
			}
			state.Pending = remaining
		}
		state.Pending = append(state.Pending, item)
		SortState(state)
		return nil
	})
}

// WithdrawQueuedWaitNudges withdraws the pending and in-flight wait nudges
// ids (WithdrawWaitNudges).
func (q StateFileQueue) WithdrawQueuedWaitNudges(ids []string) error {
	return WithdrawWaitNudges(q.Shadows, q.CityPath, ids)
}
