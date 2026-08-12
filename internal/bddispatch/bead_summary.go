package bddispatch

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

const (
	// DefaultBeadSummaryBudget is the maximum serialized size of the compact
	// discovery response used by list and ready callers.
	DefaultBeadSummaryBudget = 16 << 10
	// MaxBeadSummaryRows bounds the number of discovery records independently
	// from their serialized size.
	MaxBeadSummaryRows = 100
)

// BeadSummaryEnvelope is the bounded discovery projection for list and ready.
// Full bead content remains available through an explicit show request.
type BeadSummaryEnvelope struct {
	SchemaVersion string        `json:"schema_version"`
	Kind          string        `json:"kind"`
	Verb          string        `json:"verb"`
	BudgetBytes   int           `json:"budget_bytes"`
	Total         int           `json:"total"`
	Omitted       int           `json:"omitted"`
	Beads         []BeadSummary `json:"beads"`
}

// BeadSummary contains only the fields needed to schedule or select a bead.
type BeadSummary struct {
	ID                    string            `json:"id"`
	Title                 string            `json:"title,omitempty"`
	Status                string            `json:"status"`
	Type                  string            `json:"type,omitempty"`
	Priority              *int              `json:"priority,omitempty"`
	CreatedAt             time.Time         `json:"created_at,omitempty"`
	Assignee              string            `json:"assignee,omitempty"`
	Parent                string            `json:"parent,omitempty"`
	Labels                []string          `json:"labels,omitempty"`
	RoutingMetadata       map[string]string `json:"routing_metadata,omitempty"`
	SourceSerializedBytes int               `json:"source_serialized_bytes"`
	DetailsOmitted        []string          `json:"details_omitted"`
	FieldsOmitted         []string          `json:"fields_omitted,omitempty"`
}

var summaryRoutingMetadataKeys = []string{
	"gc.routed_to",
	"gc.root_bead_id",
	"gc.session_id",
	"gc.session_name",
	"gc.step_id",
}

var summaryDetailsOmitted = []string{
	"description",
	"notes",
	"acceptance_criteria",
	"design",
	"comments",
	"dependencies",
}

// NewBeadSummaryEnvelope projects beads into a bounded, typed discovery
// response. It omits complete entries rather than truncating their underlying
// evidence when adding them would exceed budget.
func NewBeadSummaryEnvelope(verb string, input []beads.Bead, budget int) BeadSummaryEnvelope {
	if budget <= 0 {
		budget = DefaultBeadSummaryBudget
	}
	envelope := BeadSummaryEnvelope{
		SchemaVersion: "1",
		Kind:          "gc.bead_summary",
		Verb:          verb,
		BudgetBytes:   budget,
		Total:         len(input),
		Beads:         make([]BeadSummary, 0, len(input)),
	}
	for _, bead := range input {
		if len(envelope.Beads) == MaxBeadSummaryRows {
			envelope.Omitted = len(input) - len(envelope.Beads)
			break
		}
		candidate := envelope
		candidate.Beads = append(append([]BeadSummary(nil), envelope.Beads...), beadSummary(bead))
		candidate.Omitted = len(input) - len(candidate.Beads)
		payload, err := json.Marshal(candidate)
		if err != nil || len(payload) > budget-256 {
			envelope.Omitted++
			continue
		}
		envelope = candidate
	}
	return envelope
}

func beadSummary(bead beads.Bead) BeadSummary {
	source, _ := json.Marshal(bead)
	title, titleOmitted := boundedSummaryStringWithOmission(bead.Title)
	routingMetadata, routingOmitted := selectedRoutingMetadata(bead.Metadata)
	summary := BeadSummary{
		ID:                    bead.ID,
		Title:                 title,
		Status:                bead.Status,
		Type:                  bead.Type,
		Priority:              bead.Priority,
		CreatedAt:             bead.CreatedAt,
		Assignee:              bead.Assignee,
		Parent:                bead.ParentID,
		Labels:                boundedSummaryStrings(bead.Labels),
		RoutingMetadata:       routingMetadata,
		SourceSerializedBytes: len(source),
		DetailsOmitted:        append([]string(nil), summaryDetailsOmitted...),
	}
	if titleOmitted {
		summary.FieldsOmitted = []string{"title"}
	}
	summary.FieldsOmitted = append(summary.FieldsOmitted, routingOmitted...)
	return summary
}

func selectedRoutingMetadata(metadata beads.StringMap) (map[string]string, []string) {
	selected := make(map[string]string)
	omitted := make([]string, 0)
	for _, key := range summaryRoutingMetadataKeys {
		if value, ok := metadata[key]; ok {
			bounded, wasOmitted := boundedSummaryStringWithOmission(value)
			selected[key] = bounded
			if wasOmitted {
				omitted = append(omitted, "routing_metadata."+key)
			}
		}
	}
	return selected, omitted
}

func boundedSummaryStrings(values []string) []string {
	const maxLabels = 32
	if len(values) > maxLabels {
		values = values[:maxLabels]
	}
	result := make([]string, len(values))
	for i, value := range values {
		result[i] = boundedSummaryString(value)
	}
	return result
}

func boundedSummaryString(value string) string {
	result, _ := boundedSummaryStringWithOmission(value)
	return result
}

func boundedSummaryStringWithOmission(value string) (string, bool) {
	const maxBytes = 512
	if len(value) <= maxBytes {
		return value, false
	}
	return strings.ToValidUTF8(value[:maxBytes], ""), true
}
