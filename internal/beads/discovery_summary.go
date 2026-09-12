package beads

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
)

const (
	// DefaultDiscoverySummaryBudget is the maximum serialized size of a compact
	// discovery response.
	DefaultDiscoverySummaryBudget = 16 << 10
	// MaxDiscoverySummaryRows bounds discovery records independently from their
	// serialized size.
	MaxDiscoverySummaryRows = 100
	// DiscoverySummaryKind identifies the bounded discovery envelope.
	DiscoverySummaryKind = "gc.bead_summary"
)

// DiscoverySummaryEnvelope is the bounded discovery projection for list and
// ready reads. Full bead content remains available through an explicit show
// request.
type DiscoverySummaryEnvelope struct {
	SchemaVersion string             `json:"schema_version"`
	Kind          string             `json:"kind"`
	Verb          string             `json:"verb"`
	BudgetBytes   int                `json:"budget_bytes"`
	Total         int                `json:"total"`
	Omitted       int                `json:"omitted"`
	Beads         []DiscoverySummary `json:"beads"`
}

// DiscoverySummary contains only the fields needed to schedule or select a
// bead.
type DiscoverySummary struct {
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

var discoverySummaryRoutingMetadataKeys = []string{
	beadmeta.RunTargetMetadataKey,
	beadmeta.RoutedToMetadataKey,
	beadmeta.InstantiatingMetadataKey,
	beadmeta.RootBeadIDMetadataKey,
	beadmeta.SessionIDMetadataKey,
	beadmeta.SessionNameMetadataKey,
	beadmeta.StepIDMetadataKey,
	"target",
	"branch",
	beadmeta.BaseSHAMetadataKey,
	beadmeta.TaskWorktreeMetadataKey,
	"work_dir",
	beadmeta.WorkspaceOwnerMetadataKey,
}

var discoverySummaryDetailsOmitted = []string{
	"description",
	"notes",
	"acceptance_criteria",
	"design",
	"comments",
	"dependencies",
}

// NewDiscoverySummaryEnvelope projects beads into a bounded, typed discovery
// response. It omits complete entries rather than truncating their underlying
// evidence when adding them would exceed the budget.
func NewDiscoverySummaryEnvelope(verb string, input []Bead, budget int) DiscoverySummaryEnvelope {
	if budget <= 0 {
		budget = DefaultDiscoverySummaryBudget
	}
	envelope := DiscoverySummaryEnvelope{
		SchemaVersion: "1",
		Kind:          DiscoverySummaryKind,
		Verb:          verb,
		BudgetBytes:   budget,
		Total:         len(input),
		Omitted:       len(input),
		Beads:         make([]DiscoverySummary, 0, len(input)),
	}
	for _, bead := range input {
		if len(envelope.Beads) == MaxDiscoverySummaryRows {
			envelope.Omitted = len(input) - len(envelope.Beads)
			break
		}
		candidate := envelope
		candidate.Beads = append(append([]DiscoverySummary(nil), envelope.Beads...), discoverySummary(bead))
		candidate.Omitted = len(input) - len(candidate.Beads)
		payload, err := json.Marshal(candidate)
		if err != nil || len(payload) > budget-256 {
			continue
		}
		envelope = candidate
	}
	return envelope
}

func discoverySummary(bead Bead) DiscoverySummary {
	source, _ := json.Marshal(bead)
	title, titleOmitted := boundedDiscoverySummaryStringWithOmission(bead.Title)
	routingMetadata, routingOmitted := selectedDiscoveryRoutingMetadata(bead.Metadata)
	summary := DiscoverySummary{
		ID:                    bead.ID,
		Title:                 title,
		Status:                bead.Status,
		Type:                  bead.Type,
		Priority:              bead.Priority,
		CreatedAt:             bead.CreatedAt,
		Assignee:              bead.Assignee,
		Parent:                bead.ParentID,
		Labels:                boundedDiscoverySummaryStrings(bead.Labels),
		RoutingMetadata:       routingMetadata,
		SourceSerializedBytes: len(source),
		DetailsOmitted:        append([]string(nil), discoverySummaryDetailsOmitted...),
	}
	if titleOmitted {
		summary.FieldsOmitted = []string{"title"}
	}
	summary.FieldsOmitted = append(summary.FieldsOmitted, routingOmitted...)
	return summary
}

func selectedDiscoveryRoutingMetadata(metadata StringMap) (map[string]string, []string) {
	selected := make(map[string]string)
	omitted := make([]string, 0)
	for _, key := range discoverySummaryRoutingMetadataKeys {
		if value, ok := metadata[key]; ok {
			bounded, wasOmitted := boundedDiscoverySummaryStringWithOmission(value)
			selected[key] = bounded
			if wasOmitted {
				omitted = append(omitted, "routing_metadata."+key)
			}
		}
	}
	return selected, omitted
}

func boundedDiscoverySummaryStrings(values []string) []string {
	const maxLabels = 32
	if len(values) > maxLabels {
		values = values[:maxLabels]
	}
	result := make([]string, len(values))
	for i, value := range values {
		result[i] = boundedDiscoverySummaryString(value)
	}
	return result
}

func boundedDiscoverySummaryString(value string) string {
	result, _ := boundedDiscoverySummaryStringWithOmission(value)
	return result
}

func boundedDiscoverySummaryStringWithOmission(value string) (string, bool) {
	const maxBytes = 512
	if len(value) <= maxBytes {
		return value, false
	}
	return strings.ToValidUTF8(value[:maxBytes], ""), true
}
