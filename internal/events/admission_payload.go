package events

import (
	"encoding/json"
	"strings"
)

// AdmissionVetoPayload is the typed payload for admission.veto events.
// Emitted when an admission check or host pressure gate vetoes new session demand.
type AdmissionVetoPayload struct {
	Signal    string `json:"signal"`
	Value     string `json:"value"`
	Threshold string `json:"threshold"`
	Pool      string `json:"pool"`
	Reason    string `json:"reason,omitempty"`
}

// IsEventPayload marks AdmissionVetoPayload as an events.Payload variant.
func (AdmissionVetoPayload) IsEventPayload() {}

func init() {
	RegisterPayload(AdmissionVeto, AdmissionVetoPayload{})
}

// ParseAdmissionVetoReason parses common admission veto reason strings into signal, value, and threshold.
// Formats supported:
// - "<signal>=<val>_exceeds_<thresh>" (e.g. "memory_psi_avg10=1.50_exceeds_1.00")
// - "<signal>=<val>_below_<thresh>" (e.g. "swap_free_kb=1000000_below_2000000")
// - "<signal>=<val>"
// For arbitrary or unformatted strings, signal is set to the full reason string.
func ParseAdmissionVetoReason(reason string) (signal, value, threshold string) {
	reason = strings.TrimSpace(reason)
	if strings.Contains(reason, "_exceeds_") {
		parts := strings.SplitN(reason, "_exceeds_", 2)
		threshold = parts[1]
		if eqParts := strings.SplitN(parts[0], "=", 2); len(eqParts) == 2 {
			signal = eqParts[0]
			value = eqParts[1]
		} else {
			signal = parts[0]
		}
		return
	}
	if strings.Contains(reason, "_below_") {
		parts := strings.SplitN(reason, "_below_", 2)
		threshold = parts[1]
		if eqParts := strings.SplitN(parts[0], "=", 2); len(eqParts) == 2 {
			signal = eqParts[0]
			value = eqParts[1]
		} else {
			signal = parts[0]
		}
		return
	}
	if eqParts := strings.SplitN(reason, "=", 2); len(eqParts) == 2 {
		signal = eqParts[0]
		value = eqParts[1]
		return
	}
	signal = reason
	return
}

// NewAdmissionVetoPayload constructs an AdmissionVetoPayload with parsed reason fields.
func NewAdmissionVetoPayload(pool, reason string) AdmissionVetoPayload {
	sig, val, thresh := ParseAdmissionVetoReason(reason)
	return AdmissionVetoPayload{
		Signal:    sig,
		Value:     val,
		Threshold: thresh,
		Pool:      pool,
		Reason:    reason,
	}
}

// AdmissionVetoPayloadJSON builds the JSON wire form for attachment to
// events.Event.Payload when emitting AdmissionVeto events.
func AdmissionVetoPayloadJSON(p AdmissionVetoPayload) json.RawMessage {
	data, _ := json.Marshal(p)
	return json.RawMessage(data)
}
