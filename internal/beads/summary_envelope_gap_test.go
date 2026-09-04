package beads

import (
	"errors"
	"testing"
)

// realSummaryEnvelope is the shape internal/bddispatch emits for
// `bd list --json --summary-json`, captured live from the gas-city-wbern rig.
const realSummaryEnvelope = `{
  "schema_version": "1",
  "kind": "gc.bead_summary",
  "verb": "list",
  "budget_bytes": 16384,
  "total": 50,
  "omitted": 14,
  "beads": [
    {
      "id": "gc2-wisp-kw9csq",
      "title": "order:order-tracking-sweep",
      "status": "open",
      "type": "task",
      "created_at": "2026-09-03T20:32:07Z",
      "labels": ["order-run"],
      "source_serialized_bytes": 1042,
      "details_omitted": ["description", "notes"]
    }
  ]
}`

// TestSummaryEnvelopeIsDetectableByMachineReaders states the contract that
// must hold before summary projection can become the firewall's default
// over-budget action.
//
// A summary envelope is a PROJECTION: every row is present but each row's
// details are withheld. A machine reader must be able to recognize that and
// re-read, exactly as it can today for a gc.output_firewall manifest. Today it
// cannot: OutputFirewallTruncation only knows "gc.output_firewall", so the
// envelope reaches parseIssuesTolerant and dies as a generic malformed-JSON
// error ("JSON object missing issues field") that carries no hint about what
// happened or how to recover.
//
// That generic error is the danger: mergeListTierResults downgrades an
// ephemeral-tier error to a PartialResultError, and PrimeActive swallows
// partials — so it would be absorbed silently and the control-ready cache
// would prime without the wisps tier. Same truncation-to-absence failure as
// gcw-qap3.16, reached through a different error string.
func TestSummaryEnvelopeIsDetectableByMachineReaders(t *testing.T) {
	t.Skip("PENDING FIX (gcw-qap3): documents a known-open gap, not a regression. " +
		"Unskip together with the change that teaches OutputFirewallTruncation (or a " +
		"sibling discriminator) to recognize kind=gc.bead_summary. Verified failing " +
		"2026-09-03: OutputFirewallTruncation returns nil for the envelope below.")

	payload := []byte(realSummaryEnvelope)

	err := OutputFirewallTruncation(payload)
	if err == nil {
		t.Fatal("summary envelope is NOT detected as a bounded read; a machine reader " +
			"cannot distinguish it from a genuine full result. Promoting summary " +
			"projection to the default over-budget action requires a typed " +
			"discriminator for kind=gc.bead_summary here.")
	}
	if !errors.Is(err, ErrOutputTruncated) {
		t.Fatalf("summary envelope detected but not as a truncation-class error: %v", err)
	}
}

// TestSummaryEnvelopeFailsParsingLoudly records the CURRENT behavior so the
// fallback path is not mistaken for silent absence. The parser does reject the
// envelope rather than returning an empty-but-successful result — the failure
// is loud at this layer. It only becomes silent one layer up, where
// PrimeActive swallows partial results.
func TestSummaryEnvelopeFailsParsingLoudly(t *testing.T) {
	issues, err := parseIssuesTolerant(extractJSON([]byte(realSummaryEnvelope)))
	if err == nil {
		t.Fatalf("expected a parse error for a summary envelope, got %d issues and nil error "+
			"— that would be silent absence", len(issues))
	}
	if len(issues) != 0 {
		t.Fatalf("expected zero issues from a summary envelope, got %d", len(issues))
	}
}
