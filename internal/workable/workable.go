// Package workable provides declarative workability checks and containment
// for routed work beads.
package workable

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
)

// WorkKind defines declarative workability rules for a kind of work.
type WorkKind = config.WorkKind

// Result holds the decision of a workability check.
type Result struct {
	Workable bool
	Reason   string
}

// Context supplies environment and configuration resolution functions.
type Context struct {
	AgentExists   func(target string) bool
	IsHeadCurrent func(prNumber, head string) (current bool, newerVerdictPublished bool)
	WorkKinds     map[string]WorkKind
}

// Check evaluates whether a bead is workable according to declared predicates
// and core invariants.
func Check(b beads.Bead, ctx *Context) Result {
	if b.Metadata[beadmeta.WorkabilityExemptMetadataKey] == "true" {
		return Result{Workable: true}
	}

	// Excluded by construction: non-open, deferred, or dependency-blocked beads.
	if b.Status != "" && b.Status != "open" {
		return Result{Workable: true}
	}
	if b.DeferUntil != nil && b.DeferUntil.After(time.Now()) {
		return Result{Workable: true}
	}
	if len(b.Dependencies) > 0 {
		return Result{Workable: true}
	}

	routeTarget := strings.TrimSpace(b.Metadata[beadmeta.RoutedToMetadataKey])
	if routeTarget == "" {
		return Result{Workable: true}
	}

	// (5) Human-routed beads: pending a person is not impossible; do not fire.
	if routeTarget == "human" {
		return Result{Workable: true}
	}

	// (4) Gate routed to agent pool: structurally unreachable by readiness.
	if b.Type == "gate" {
		return Result{
			Workable: false,
			Reason:   "structurally unreachable: gates cannot be claimed by agent pools",
		}
	}

	// (2) Circuit open check: malformed custody.
	if b.Metadata["review_custody_circuit_open"] == "true" {
		reason := b.Metadata["review_custody_circuit_reason"]
		if reason == "" {
			reason = "malformed custody"
		}
		return Result{
			Workable: false,
			Reason:   fmt.Sprintf("circuit open: %s", reason),
		}
	}

	// (3) Unresolvable route target: must resolve to a configured agent.
	if ctx != nil && ctx.AgentExists != nil {
		if !ctx.AgentExists(routeTarget) {
			return Result{
				Workable: false,
				Reason:   fmt.Sprintf("unresolvable route target: %s is not a configured agent", routeTarget),
			}
		}
	}

	// Match against declared WorkKinds in pack/city configuration.
	if ctx != nil && len(ctx.WorkKinds) > 0 {
		for kindName, wk := range ctx.WorkKinds {
			if matchesWorkKind(b, kindName, wk) {
				// RequireMetadata check
				for _, reqKey := range wk.RequireMetadata {
					if strings.TrimSpace(b.Metadata[reqKey]) == "" {
						return Result{
							Workable: false,
							Reason:   fmt.Sprintf("missing required metadata %q for work kind %q", reqKey, kindName),
						}
					}
				}

				// FreshnessBinding check
				if wk.FreshnessBinding == "head_matches_pr" {
					pr := strings.TrimSpace(b.Metadata["pr_number"])
					head := strings.TrimSpace(b.Metadata["head"])
					if b.Assignee == "" && pr != "" && head != "" && ctx.IsHeadCurrent != nil {
						current, newerVerdict := ctx.IsHeadCurrent(pr, head)
						if !current && newerVerdict {
							return Result{
								Workable: false,
								Reason:   fmt.Sprintf("superseded head: PR #%s head moved from %s and newer head already has a verdict", pr, head),
							}
						}
					}
				}
			}
		}
	}

	return Result{Workable: true}
}

func matchesWorkKind(b beads.Bead, kindName string, wk WorkKind) bool {
	if strings.EqualFold(b.Metadata[beadmeta.KindMetadataKey], kindName) {
		return true
	}
	if strings.EqualFold(b.Type, kindName) {
		return true
	}
	if len(wk.MatchMetadata) > 0 {
		for _, m := range wk.MatchMetadata {
			if _, ok := b.Metadata[m]; ok {
				return true
			}
		}
	}
	return false
}

// StoreWriter captures bead store write operations needed by containment.
type StoreWriter interface {
	SetMetadataBatch(id string, kvs map[string]string) error
	Update(id string, opts beads.UpdateOpts) error
}

// EventRecorder captures event emission.
type EventRecorder interface {
	Record(event events.Event)
}

// ParkBead contains an unworkable bead by clearing its route, stamping reason & timestamp,
// setting a short defer_until, and emitting bead.unworkable and bead.parked events.
// It NEVER closes and NEVER deletes the bead.
func ParkBead(_ context.Context, bead beads.Bead, reason string, deferDuration time.Duration, store StoreWriter, rec EventRecorder) error {
	now := time.Now().UTC()
	deferUntil := now.Add(deferDuration)

	// 1. Clear gc.routed_to, stamp gc.unworkable_reason and gc.unworkable_at
	patch := map[string]string{
		beadmeta.RoutedToMetadataKey:         "",
		beadmeta.UnworkableReasonMetadataKey: reason,
		beadmeta.UnworkableAtMetadataKey:     now.Format(time.RFC3339),
	}

	if err := store.SetMetadataBatch(bead.ID, patch); err != nil {
		return fmt.Errorf("stamping unworkable metadata on %s: %w", bead.ID, err)
	}

	// 2. Set defer_until without closing or deleting
	if err := store.Update(bead.ID, beads.UpdateOpts{
		DeferUntil: &deferUntil,
	}); err != nil {
		return fmt.Errorf("deferring unworkable bead %s: %w", bead.ID, err)
	}

	// 3. Emit bead.unworkable and bead.parked events
	if rec != nil {
		routeTarget := bead.Metadata[beadmeta.RoutedToMetadataKey]
		rawUnworkable, _ := json.Marshal(events.BeadUnworkablePayload{
			BeadID: bead.ID,
			Reason: reason,
			Target: routeTarget,
		})
		rec.Record(events.Event{
			Type:    events.BeadUnworkable,
			Subject: bead.ID,
			Ts:      now,
			Payload: rawUnworkable,
		})

		rawParked, _ := json.Marshal(events.BeadParkedPayload{
			BeadID:     bead.ID,
			Reason:     reason,
			DeferUntil: deferUntil.Format(time.RFC3339),
		})
		rec.Record(events.Event{
			Type:    events.BeadParked,
			Subject: bead.ID,
			Ts:      now,
			Payload: rawParked,
		})
	}

	return nil
}
