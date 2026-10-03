package beads

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	beadslib "github.com/steveyegge/beads"
)

// ErrConditionalHumanGateResolveUnsupported reports that a store cannot check
// a human-gate contract and close it in the same authoritative write boundary.
var ErrConditionalHumanGateResolveUnsupported = errors.New("conditional human-gate resolve unsupported")

// HumanGateContract is the immutable conversation and dependency target a
// caller inspected before asking to resolve a human gate.
type HumanGateContract struct {
	GateID      string
	TargetID    string
	Fingerprint string
}

// HumanGateContractFrom captures the contract a caller must present when it
// resolves gate. The fingerprint covers every persisted conversation field so
// additions to the human-gate payload cannot silently escape the guard.
func HumanGateContractFrom(gate Bead, targetID string) HumanGateContract {
	return HumanGateContract{
		GateID:      gate.ID,
		TargetID:    targetID,
		Fingerprint: humanGateFingerprint(gate),
	}
}

// HumanGateStaleReason explains which checked fact changed before resolution.
type HumanGateStaleReason string

// Human-gate stale reasons identify the authoritative fact that changed.
const (
	HumanGateStaleIdentityChanged HumanGateStaleReason = "identity_changed"
	HumanGateStaleContractChanged HumanGateStaleReason = "contract_changed"
	HumanGateStaleTargetChanged   HumanGateStaleReason = "target_changed"
)

// StaleHumanGateError reports that a conditional resolution made no mutation
// because the authoritative gate was no longer the contract the caller read.
type StaleHumanGateError struct {
	GateID string
	Reason HumanGateStaleReason
}

// Error describes the stale human-gate contract.
func (e *StaleHumanGateError) Error() string {
	if e == nil {
		return "<nil>"
	}
	return fmt.Sprintf("human gate %s is stale: %s", e.GateID, e.Reason)
}

// ConditionalHumanGateResolver is an optional store capability that resolves a
// human gate only when its inspected contract remains authoritative.
type ConditionalHumanGateResolver interface {
	ResolveHumanGateIfCurrent(HumanGateContract) error
}

// ResolveHumanGateIfCurrent resolves contract through the store's authoritative
// conditional-resolve capability. It never falls back to a read-then-close
// sequence because that sequence reintroduces the race this operation owns.
func ResolveHumanGateIfCurrent(store Store, contract HumanGateContract) error {
	resolver, ok := store.(ConditionalHumanGateResolver)
	if !ok {
		return ErrConditionalHumanGateResolveUnsupported
	}
	return resolver.ResolveHumanGateIfCurrent(contract)
}

// ResolveHumanGateIfCurrent verifies and closes a human gate while MemStore's
// mutex holds the gate and its dependency edge at one authoritative boundary.
func (m *MemStore) ResolveHumanGateIfCurrent(contract HumanGateContract) error {
	if strings.TrimSpace(contract.GateID) == "" || strings.TrimSpace(contract.TargetID) == "" || contract.Fingerprint == "" {
		return fmt.Errorf("conditional human-gate resolve: gate ID, target ID, and fingerprint are required")
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	i := m.indexOfLocked(contract.GateID)
	if i < 0 {
		return fmt.Errorf("resolving human gate %q: %w", contract.GateID, ErrNotFound)
	}
	gate := m.beads[i]
	if gate.Status != "open" || gate.Type != "gate" || gate.AwaitType != "human" {
		return &StaleHumanGateError{GateID: contract.GateID, Reason: HumanGateStaleIdentityChanged}
	}
	if humanGateFingerprint(gate) != contract.Fingerprint {
		return &StaleHumanGateError{GateID: contract.GateID, Reason: HumanGateStaleContractChanged}
	}
	if !humanGateBlocksTargetLocked(contract.GateID, contract.TargetID, m.deps) {
		return &StaleHumanGateError{GateID: contract.GateID, Reason: HumanGateStaleTargetChanged}
	}
	m.beads[i].Status = "closed"
	m.beads[i].UpdatedAt = time.Now()
	m.beads[i].Revision++
	return nil
}

// ResolveHumanGateIfCurrent delegates the authoritative check to the cache's
// backing store, then evicts the gate and target so subsequent reads cannot
// serve the pre-resolution snapshot.
func (c *CachingStore) ResolveHumanGateIfCurrent(contract HumanGateContract) error {
	resolver, ok := c.backing.(ConditionalHumanGateResolver)
	if !ok {
		return ErrConditionalHumanGateResolveUnsupported
	}
	if err := resolver.ResolveHumanGateIfCurrent(contract); err != nil {
		c.applyConditionalWriteFailure(contract.GateID, err)
		return err
	}
	c.evictForConditionalWrite(contract.GateID)
	c.evictForConditionalWrite(contract.TargetID)
	return nil
}

func humanGateBlocksTargetLocked(gateID, targetID string, deps []Dep) bool {
	for _, dep := range deps {
		if dep.IssueID == targetID && dep.DependsOnID == gateID && dep.Type == "blocks" {
			return true
		}
	}
	return false
}

// ResolveHumanGateIfCurrent verifies the gate, its conversation, and the
// target's blocking edge inside one native Dolt transaction before closing it.
func (s *NativeDoltStore) ResolveHumanGateIfCurrent(contract HumanGateContract) error {
	if strings.TrimSpace(contract.GateID) == "" || strings.TrimSpace(contract.TargetID) == "" || contract.Fingerprint == "" {
		return fmt.Errorf("conditional human-gate resolve: gate ID, target ID, and fingerprint are required")
	}
	return s.withOpRetry(func(ctx context.Context, storage beadslib.Storage, _ int) error {
		return storage.RunInTransaction(ctx, fmt.Sprintf("gc: conditionally resolve human gate %s", contract.GateID), func(tx beadslib.Transaction) error {
			issue, err := tx.GetIssue(ctx, contract.GateID)
			if err != nil {
				return nativeStoreError(contract.GateID, err)
			}
			if issue == nil {
				return fmt.Errorf("resolving human gate %q: %w", contract.GateID, ErrNotFound)
			}
			gate, err := beadFromNativeIssue(issue)
			if err != nil {
				return err
			}
			if gate.Status != "open" || gate.Type != "gate" || gate.AwaitType != "human" {
				return &StaleHumanGateError{GateID: contract.GateID, Reason: HumanGateStaleIdentityChanged}
			}
			if humanGateFingerprint(gate) != contract.Fingerprint {
				return &StaleHumanGateError{GateID: contract.GateID, Reason: HumanGateStaleContractChanged}
			}
			if _, err := tx.GetIssue(ctx, contract.TargetID); err != nil {
				return nativeStoreError(contract.TargetID, err)
			}
			deps, err := tx.GetDependencyRecords(ctx, contract.TargetID)
			if err != nil {
				return nativeStoreError(contract.TargetID, err)
			}
			if !nativeHumanGateBlocksTarget(contract.GateID, contract.TargetID, deps) {
				return &StaleHumanGateError{GateID: contract.GateID, Reason: HumanGateStaleTargetChanged}
			}
			if err := tx.CloseIssue(ctx, contract.GateID, nativeCloseReasonFromIssue(issue), s.actor, ""); err != nil {
				return nativeStoreError(contract.GateID, err)
			}
			return nil
		})
	})
}

func nativeHumanGateBlocksTarget(gateID, targetID string, deps []*beadslib.Dependency) bool {
	for _, dep := range deps {
		if dep != nil && dep.IssueID == targetID && dep.DependsOnID == gateID && dep.Type == beadslib.DepBlocks {
			return true
		}
	}
	return false
}

func humanGateFingerprint(gate Bead) string {
	payload := struct {
		Title       string    `json:"title"`
		Description string    `json:"description"`
		Metadata    StringMap `json:"metadata"`
	}{
		Title:       gate.Title,
		Description: gate.Description,
		Metadata:    gate.Metadata,
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		// StringMap is JSON-serializable. Keep a deterministic fallback so this
		// guard remains fail-closed if that invariant ever changes.
		encoded = []byte(fmt.Sprintf("%q\x00%q", gate.Title, gate.Description))
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}
