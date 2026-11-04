package main

import (
	"fmt"
	"time"

	"github.com/gastownhall/gascity/internal/nudgequeue"
)

// errNudgeShadowRetiring refuses reuse while a payer may still terminalize the
// old item's shadow. The producer can retry after that payer finishes.
var errNudgeShadowRetiring = fmt.Errorf("an earlier nudge's terminal write to its shadow is in flight: %w", nudgequeue.ErrQueueBusy)

// dropRetiredDebtsOnShadow prevents an old terminal payment from closing the
// shadow a new enqueue reuses. A leased payment cannot be recalled, so reuse
// fails without changing state, even after lease expiry: a synchronous store
// call may still be running. Expiry permits another payer to finish the debt,
// while reuse waits for confirmation. An unpaid, unleased debt on the same shadow
// is superseded by the new item's eventual terminal payment.
func dropRetiredDebtsOnShadow(state *nudgeQueueState, item queuedNudge, _ time.Time) error {
	if item.BeadID == "" {
		return nil
	}
	matches := func(retired queuedNudge) bool { return retired.ID == item.ID && retired.BeadID == item.BeadID }
	for _, retired := range state.Retired {
		if matches(retired) && retired.Shadow != nil && retired.Shadow.Leased && !retired.Shadow.Paid {
			return fmt.Errorf("queueing nudge %q on shadow %s: %w", item.ID, item.BeadID, errNudgeShadowRetiring)
		}
	}
	kept := state.Retired[:0]
	for _, retired := range state.Retired {
		if !matches(retired) {
			kept = append(kept, retired)
		}
	}
	state.Retired = kept
	return nil
}
