package sling

import "github.com/gastownhall/gascity/internal/beads"

// The one-store attachment probes below take a single raw store for both the source
// bead and its attachments. They are test fixtures: production callers reach the
// same checks through SlingDeps.attachmentScope() with typed class handles.

// singleStoreAttachmentScope is the scope of a caller that hands one store for
// both the source bead and its attachments.
func singleStoreAttachmentScope(store beads.Store) attachmentScope {
	return attachmentScope{work: beads.WorkStore{Store: store}, graph: beads.GraphStore{Store: store}}
}

// CheckNoMoleculeChildren returns an error if the bead already has an attached
// molecule or wisp child that is still open. This probe never retires families:
// an unassigned source is not proof that its descendants have no live custody.
func CheckNoMoleculeChildren(q BeadQuerier, beadID string, store beads.Store, result *SlingResult) error {
	return checkNoMoleculeChildren(q, beadID, singleStoreAttachmentScope(store), result, false)
}

// CheckBatchNoMoleculeChildren checks all open children for existing molecule
// attachments before any wisps are created.
func CheckBatchNoMoleculeChildren(q BeadChildQuerier, open []beads.Bead, store beads.Store, result *SlingResult) error {
	return checkBatchNoMoleculeChildren(q, open, singleStoreAttachmentScope(store), result, false)
}

// CheckNoMoleculeChildrenAllowLiveWorkflow is like CheckNoMoleculeChildren
// but permits an existing live workflow attachment (used on --force graph
// launches that will supersede the existing root under the source-workflow
// lock).
func CheckNoMoleculeChildrenAllowLiveWorkflow(q BeadQuerier, beadID string, store beads.Store, result *SlingResult) error {
	return checkNoMoleculeChildren(q, beadID, singleStoreAttachmentScope(store), result, true)
}

// CheckBatchNoMoleculeChildrenAllowLiveWorkflow is the batch variant of
// CheckNoMoleculeChildrenAllowLiveWorkflow.
func CheckBatchNoMoleculeChildrenAllowLiveWorkflow(q BeadChildQuerier, open []beads.Bead, store beads.Store, result *SlingResult) error {
	return checkBatchNoMoleculeChildren(q, open, singleStoreAttachmentScope(store), result, true)
}
