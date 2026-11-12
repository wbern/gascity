package sling

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"strings"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/molecule"
	"github.com/gastownhall/gascity/internal/sourceworkflow"
)

const legacyAttachmentStateKey = beadmeta.LegacyAttachmentStateMetadataKey

// withLegacyAttachment serializes discovery, materialization, linkage and the
// caller's routing step. Re-deliveries reuse an existing matching family;
// unassigned does not mean abandoned and never authorizes burning a live root.
func withLegacyAttachment(ctx context.Context, deps SlingDeps, sourceID, formulaName string, vars map[string]string, create func() (*molecule.Result, error), finish func(*molecule.Result) (SlingResult, error), routeTarget ...string) (SlingResult, error) {
	if deps.GraphStore != nil && strings.TrimSpace(deps.StoreRef) == "" {
		return SlingResult{}, fmt.Errorf("legacy source %s requires a source-store identity before shared-store materialization", sourceID)
	}
	// The fence is looked up through the wrapper's declared resolve target
	// (cmd/gc's policy store embeds the Store interface and so hides the
	// backing store's UpdateIfMatch). Its absence is refused below, after
	// discovery and before any mutation, so a store that cannot be READ is
	// reported as the store fault it is rather than as a missing capability.
	writer, hasRevisionWriter := beads.ConditionalWriterForTarget(deps.Store)
	patchWriter, hasPatchWriter := beads.MetadataPatchCASWriterFor(deps.Store)
	var result SlingResult
	err := sourceworkflow.WithLock(ctx, deps.CityPath, sourceWorkflowLockScope(deps), sourceID, func() error {
		source, err := beads.HandlesFor(deps.Store).Live.Get(sourceID)
		if err != nil {
			return fmt.Errorf("read legacy source %s: %w", sourceID, err)
		}
		if source.Status != "open" && source.Status != "in_progress" {
			return fmt.Errorf("legacy source %s is %s; refusing materialization", sourceID, source.Status)
		}
		// A legacy root lives in the store that minted it: the work store for a
		// v1 poured recipe, the graph store for a graph-class one (split-city
		// storage routing). Discovery reads both; every mutation of a root goes
		// to the store that holds it.
		scope := deps.attachmentScope()
		located, err := CollectAttachments(source, deps.Store, scope.stores()...)
		if err != nil && !errors.Is(err, beads.ErrNotFound) {
			// A store fault, not a conflict: the guard cannot tell whether a
			// live family is attached, so the API answers it 5xx.
			return fmt.Errorf("inspect legacy source %s: %w", sourceID, attachmentProbeFailure(sourceID, err, scope))
		}
		if err != nil {
			return fmt.Errorf("inspect legacy source %s: %w", sourceID, err)
		}
		// requireFence refuses an unfenced store before the first mutation
		// (a retire, a create, or the publication). Refusals that change
		// nothing - a live conflict, a reconciliation fault - are reported
		// as themselves, ahead of a missing capability.
		requireFence := func() error {
			if !hasRevisionWriter && !hasPatchWriter {
				return fmt.Errorf("legacy source %s requires conditional publication; refusing unfenced materialization", sourceID)
			}
			return nil
		}
		rootStoreOf := map[string]beads.Store{}
		var live []beads.Bead
		for _, loc := range located {
			root := loc.Bead
			rootStoreOf[root.ID] = loc.Store
			rootSourceRef := strings.TrimSpace(root.Metadata[beadmeta.SourceStoreRefMetadataKey])
			// If root has no source-store identity recorded, and we are in a shared graph store,
			// fallback to deps.StoreRef for roots residing in the same store.
			if rootSourceRef == "" && deps.StoreRef != "" {
				rootSourceRef = strings.TrimSpace(deps.StoreRef)
			}
			rootScope := deps
			rootScope.StoreRef = rootSourceRef
			if rootSourceRef != "" && sourceWorkflowLockScope(rootScope) != sourceWorkflowLockScope(deps) {
				if source.Metadata[beadmeta.MoleculeIDMetadataKey] == root.ID {
					return fmt.Errorf("legacy source %s points to foreign-store family %s (%s)", sourceID, root.ID, rootSourceRef)
				}
				continue
			}
			if root.Status == "closed" {
				if IsMoleculeAttachment(root) && !IsWorkflowAttachment(root) {
					family, err := molecule.ListSubtree(legacyRootMolecule(deps, rootStoreOf[root.ID]), root.ID)
					if err != nil {
						return fmt.Errorf("inspect closed legacy family %s: %w", root.ID, err)
					}
					for _, child := range family {
						if child.Status != "closed" {
							return fmt.Errorf("closed legacy root %s has live descendant %s; reconciliation required", root.ID, child.ID)
						}
					}
				}
				continue
			}
			if deps.GraphStore != nil && rootSourceRef == "" {
				return fmt.Errorf("legacy family %s has no source-store identity; reconciliation required", root.ID)
			}
			live = append(live, root)
		}
		var materialized *molecule.Result
		if len(live) > 0 {
			root := live[0]
			if IsWorkflowAttachment(root) {
				return &sourceworkflow.ConflictError{SourceBeadID: sourceID, WorkflowIDs: []string{root.ID}}
			}
			matches := len(live) == 1 && IsMoleculeAttachment(root) && !IsWorkflowAttachment(root) && root.Metadata[beadmeta.FormulaNameMetadataKey] == formulaName && root.Metadata[beadmeta.SourceBeadIDMetadataKey] == sourceID
			for key, value := range vars {
				matches = matches && root.Metadata["gc.var."+key] == value
			}
			isFailed := root.Metadata[beadmeta.MoleculeFailedMetadataKey] != ""
			isReady := root.Metadata[legacyAttachmentStateKey] == "ready"
			abandoned := isAbandonedPreparingRoot(root, live, source, deps, sourceID, formulaName)

			switch {
			case matches && !isFailed && isReady && !deps.Force:
				if err := requireFence(); err != nil {
					return err
				}
				materialized = &molecule.Result{RootID: root.ID}
			case isFailed || deps.Force || abandoned:
				if err := requireFence(); err != nil {
					return err
				}
				closeReason := "retired: superseded by rework"
				if abandoned && !isFailed && !deps.Force {
					closeReason = "retired: abandoned preparing attempt (sling died before ready)"
				}
				for _, r := range live {
					if _, err := molecule.CloseSubtreeWithMetadata(legacyRootMolecule(deps, rootStoreOf[r.ID]), r.ID, map[string]string{
						"close_reason": closeReason,
					}); err != nil {
						return fmt.Errorf("retire superseded legacy family %s: %w", r.ID, err)
					}
				}
				materialized, err = createAndStampReady(deps, scope, create)
				if err != nil {
					return err
				}
			default:
				if len(live) == 1 && root.Metadata[beadmeta.SourceBeadIDMetadataKey] == "" && root.Metadata["gc.var.issue"] == "" {
					return &MoleculeAttachedError{BeadID: sourceID, Label: "molecule", AttachmentID: root.ID}
				}
				// An explicit --on naming a different formula than the live
				// family was poured from is a refusal the caller can read (a
				// MoleculeAttachedError), not an opaque reconciliation fault; the
				// family is still never retired here.
				if len(live) == 1 && IsMoleculeAttachment(root) && !IsWorkflowAttachment(root) && root.Metadata[beadmeta.SourceBeadIDMetadataKey] == sourceID {
					if attached := strings.TrimSpace(root.Metadata[beadmeta.FormulaNameMetadataKey]); attached != "" && attached != formulaName {
						return &MoleculeAttachedError{BeadID: sourceID, Label: AttachmentLabel(root), AttachmentID: root.ID}
					}
				}
				return fmt.Errorf("legacy source %s has conflicting families (%s); reconciliation required", sourceID, root.ID)
			}
		} else {
			if err := requireFence(); err != nil {
				return err
			}
			materialized, err = createAndStampReady(deps, scope, create)
			if err != nil {
				return err
			}
		}
		// Re-read after materialization: external claim/close operations do not
		// participate in this launcher lock. Never overwrite changed custody.
		fresh, err := beads.HandlesFor(deps.Store).Live.Get(sourceID)
		if err == nil && (fresh.Status != source.Status || fresh.Assignee != source.Assignee || fresh.ClaimFence != source.ClaimFence || fresh.ParentID != source.ParentID || !maps.Equal(fresh.Metadata, source.Metadata)) {
			err = fmt.Errorf("legacy source %s changed during materialization", sourceID)
		}
		if err == nil {
			publication := map[string]string{beadmeta.MoleculeIDMetadataKey: materialized.RootID}
			if len(routeTarget) != 0 && routeTarget[0] != "" {
				publication[beadmeta.RoutedToMetadataKey] = routeTarget[0]
			}
			// The route and pointer become visible in the SAME fenced write.
			// A close/claim/pointer change after the live read invalidates its
			// revision. No router or shell callback may re-write the route later.
			if hasPatchWriter {
				var swapped bool
				swapped, err = patchWriter.CompareAndSetMetadataPatch(sourceID, fresh, publication)
				if err == nil && !swapped {
					err = fmt.Errorf("legacy source %s changed during conditional publication", sourceID)
				}
			} else {
				err = writer.UpdateIfMatch(sourceID, fresh.Revision, beads.UpdateOpts{Metadata: publication})
			}
		}
		if err != nil {
			return fmt.Errorf("publish legacy attachment for %s: %w; family %s retained for reconciliation", sourceID, err, materialized.RootID)
		}
		result, err = finish(materialized)
		// A failed route retains its discoverable family for the next retry.
		// Closing it here could race a successful-but-unacknowledged route.
		return err
	})
	return result, err
}

// createAndStampReady materializes the family through create and stamps it
// ready (and with the source-store identity) in the store that holds the new
// root: create mints a v1 poured recipe in the work store and a graph-class one
// in the graph store, so the root is located rather than assumed.
func createAndStampReady(deps SlingDeps, scope attachmentScope, create func() (*molecule.Result, error)) (*molecule.Result, error) {
	materialized, err := create()
	if err != nil {
		return nil, err
	}
	_, rootStore, err := getFromStores(materialized.RootID, scope.stores())
	if err == nil {
		err = rootStore.SetMetadata(materialized.RootID, legacyAttachmentStateKey, "ready")
	}
	if err != nil {
		return nil, fmt.Errorf("finish legacy family %s: %w; family retained for reconciliation", materialized.RootID, err)
	}
	if deps.StoreRef != "" {
		_ = rootStore.SetMetadata(materialized.RootID, beadmeta.SourceStoreRefMetadataKey, deps.StoreRef)
	}
	return materialized, nil
}

// isAbandonedPreparingRoot reports whether the single live root is a legacy
// family left in gc.legacy_attachment_state=preparing by a sling that died
// between create() and the ready stamp (gci-142rk9).
//
// Only InstantiateCompiledSlingFormula stamps "preparing", and only from the
// create() callback that withLegacyAttachment runs while holding
// sourceworkflow.WithLock for this source (an in-process mutex plus a
// cross-process flock that the kernel releases when the holder dies). The
// ready stamp happens before that lock is released. So when the caller
// observes such a root while holding the same lock, no in-flight sling can
// own it: it is provably a dead prior attempt and is safe to retire.
//
// The root's recorded source-store ref must be present (or both refs empty) so
// the creator is known to have used this same lock scope. Vars are
// deliberately NOT compared: whatever vars the dead attempt used, it was an
// attempt for this source under this lock and never published a pointer.
// Formula and source identity must still match, and the source must not
// already point at the root, to stay conservative.
func isAbandonedPreparingRoot(root beads.Bead, live []beads.Bead, source beads.Bead, deps SlingDeps, sourceID, formulaName string) bool {
	if len(live) != 1 || !IsMoleculeAttachment(root) || IsWorkflowAttachment(root) || root.Status == "closed" {
		return false
	}
	if root.Metadata[legacyAttachmentStateKey] != "preparing" {
		return false
	}
	if root.Metadata[beadmeta.FormulaNameMetadataKey] != formulaName || root.Metadata[beadmeta.SourceBeadIDMetadataKey] != sourceID {
		return false
	}
	recordedRef := strings.TrimSpace(root.Metadata[beadmeta.SourceStoreRefMetadataKey])
	if recordedRef == "" && strings.TrimSpace(deps.StoreRef) != "" {
		return false
	}
	return source.Metadata[beadmeta.MoleculeIDMetadataKey] != root.ID
}

// legacyRootMolecule is the molecule target for a legacy root held by store: the
// graph target when store is the graph store of a city that relocates the graph
// class (a graph store physically distinct from the work store), else the work
// target: where the classes share one store a legacy root is work class. The
// molecule helpers take a typed target (upstream T9); a legacy root lives in the
// store that minted it.
func legacyRootMolecule(deps SlingDeps, store beads.Store) beads.MoleculeTarget {
	if deps.GraphStore != nil && !beads.SameStore(deps.GraphStore, deps.Store) && beads.SameStore(store, deps.GraphStore) {
		return beads.GraphMolecule(beads.GraphStore{Store: store})
	}
	return beads.WorkMolecule(beads.WorkStore{Store: store})
}
