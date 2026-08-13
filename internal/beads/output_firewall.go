package beads

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
)

// ErrOutputTruncated reports that a bd read returned a managed output-firewall
// manifest in place of its payload. It is deliberately distinct from
// ErrNotFound: a truncated read says nothing about whether the bead exists, and
// treating the two alike is what let a firewalled hook read present the wrong
// assignment and strand a bead it should never have claimed (gcw-qap3.16).
var ErrOutputTruncated = errors.New("bd read truncated by the managed output firewall")

// outputFirewallKind is the manifest discriminator emitted by
// internal/outputfirewall when a read exceeds its byte budget.
const outputFirewallKind = "gc.output_firewall"

// outputFirewallManifest mirrors the fields internal/outputfirewall publishes.
// Only the discriminator is required; the rest is evidence for the error text.
type outputFirewallManifest struct {
	Kind            string `json:"kind"`
	Reason          string `json:"reason"`
	CommandClass    string `json:"command_class"`
	BudgetBytes     int    `json:"budget_bytes"`
	SerializedBytes int    `json:"serialized_bytes"`
	SHA256          string `json:"sha256"`
	Spill           struct {
		Mode string `json:"mode"`
		Path string `json:"path"`
	} `json:"spill"`
}

// OutputFirewallTruncation returns a descriptive ErrOutputTruncated when
// payload is a managed output-firewall manifest rather than the read's real
// result, and nil for every genuine payload.
func OutputFirewallTruncation(payload []byte) error {
	trimmed := bytes.TrimSpace(payload)
	if len(trimmed) == 0 || trimmed[0] != '{' || !bytes.Contains(trimmed, []byte(outputFirewallKind)) {
		return nil
	}
	var manifest outputFirewallManifest
	if err := json.Unmarshal(trimmed, &manifest); err != nil || manifest.Kind != outputFirewallKind {
		return nil
	}
	detail := fmt.Sprintf("reason=%s", manifest.Reason)
	if manifest.CommandClass != "" {
		detail += fmt.Sprintf(" command_class=%s", manifest.CommandClass)
	}
	if manifest.BudgetBytes > 0 {
		detail += fmt.Sprintf(" budget_bytes=%d serialized_bytes=%d", manifest.BudgetBytes, manifest.SerializedBytes)
	}
	if manifest.SHA256 != "" {
		detail += fmt.Sprintf(" sha256=%s", manifest.SHA256)
	}
	if manifest.Spill.Path != "" {
		detail += fmt.Sprintf(" spill=%s", manifest.Spill.Path)
	}
	return fmt.Errorf("%w (%s); the payload was withheld, not absent — re-read with --allow-unbounded or raise output_firewall.byte_budget", ErrOutputTruncated, detail)
}
