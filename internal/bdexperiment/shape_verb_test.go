package bdexperiment

import "testing"

// TestEveryKnownShapeHasAnAcceptedVerb is the drift guard for the observation
// log's verb allowlist.
//
// validRecord filtered on a literal verb list that was never updated when
// ShapeReadyJSON was approved for in-process service. Every `ready` observation
// was therefore discarded silently — Append's return is dropped at the call site
// — so the telemetry meant to prove the in-process ready path safe recorded
// nothing at all for it. Measured: bdshim.log showed routed `ready` calls while
// bd-experiment.jsonl held zero ready records.
//
// A shape approved for the fastpath whose verb the log rejects is unobservable,
// which is worse than unimplemented: it looks like clean data.
func TestEveryKnownShapeHasAnAcceptedVerb(t *testing.T) {
	for _, tc := range []struct {
		shape Shape
		verb  string
	}{
		{ShapeShowJSON, "show"},
		{ShapeListJSON, "list"},
		{ShapeQueryEphemeral, "query"},
		{ShapeReadyJSON, "ready"},
		{ShapeMolCurrent, "mol"},
		{ShapeMolProgress, "mol"},
	} {
		t.Run(string(tc.shape), func(t *testing.T) {
			if !knownShape(tc.shape) {
				t.Fatalf("knownShape(%s) = false", tc.shape)
			}
			rec := Record{
				Schema: SchemaVersion, Build: "b", Shape: tc.shape, Arm: ArmDirect,
				Verb: tc.verb, Disposition: "controller", ConfigGeneration: "1",
			}
			if !validRecord(rec) {
				t.Errorf("validRecord rejects verb %q for approved shape %s; its observations are silently discarded", tc.verb, tc.shape)
			}
		})
	}
}
