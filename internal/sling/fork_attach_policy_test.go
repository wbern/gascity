package sling

import "testing"

// skipUpstreamAttachBurnPolicy skips the tests that pin upstream's attach-guard
// burn policy (T7 PROVISIONAL, for William). Upstream retires an unassigned
// source's stale attachment when nothing holds a step, and refuses a same-formula
// re-sling on an assigned source. The fork keeps its own design: attachment
// probes never retire a family (an unassigned source is not proof its
// descendants have no live custody, a3ba79d2a), and withLegacyAttachment reuses
// a matching ready family so a re-delivery is idempotent. The multi-store
// discovery those tests also exercise is adopted and covered by the tests that
// still run.
func skipUpstreamAttachBurnPolicy(t *testing.T) {
	t.Helper()
	t.Skip("pins upstream's attach-guard burn policy; the fork keeps no-burn probes and idempotent legacy re-delivery (T7 PROVISIONAL)")
}
