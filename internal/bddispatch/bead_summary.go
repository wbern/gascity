package bddispatch

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
)

const (
	// DefaultBeadSummaryBudget is the maximum serialized size of the compact
	// discovery response used by list and ready callers.
	DefaultBeadSummaryBudget = beads.DefaultDiscoverySummaryBudget
	// MaxBeadSummaryRows bounds the number of discovery records independently
	// from their serialized size.
	MaxBeadSummaryRows = beads.MaxDiscoverySummaryRows
	// BeadSummaryKind is the Kind discriminator of BeadSummaryEnvelope. It names
	// a wire envelope, not a bead-metadata key. Readers must match against this
	// constant rather than respell the literal, so writer and reader cannot
	// drift apart into a silently unrecognized envelope.
	BeadSummaryKind = beads.DiscoverySummaryKind
)

// BeadSummaryEnvelope is retained as the shim's compatibility name for the
// lower-layer discovery projection shared with the API.
type BeadSummaryEnvelope = beads.DiscoverySummaryEnvelope

// BeadSummary is retained as the shim's compatibility name for one discovery
// record.
type BeadSummary = beads.DiscoverySummary

// BeadShowSummary is the bounded show projection. It preserves bd show's
// top-level array contract while carrying the scalar fields machine consumers
// need to locate and manage a task workspace.
type BeadShowSummary struct {
	ID                    string            `json:"id"`
	Status                string            `json:"status"`
	CreatedAt             time.Time         `json:"created_at,omitempty"`
	Assignee              string            `json:"assignee"`
	Metadata              map[string]string `json:"metadata,omitempty"`
	SourceSerializedBytes int               `json:"source_serialized_bytes"`
	DetailsOmitted        []string          `json:"details_omitted"`
	FieldsOmitted         []string          `json:"fields_omitted,omitempty"`
}

var summaryRoutingMetadataKeys = []string{
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
	return beads.NewDiscoverySummaryEnvelope(verb, input, budget)
}

// WriteBeadSummaryOutput decodes a successful raw bd discovery response and
// emits the bounded discovery envelope used by controller-routed reads.
func WriteBeadSummaryOutput(verb string, payload []byte, stdout, stderr io.Writer) int {
	var beadsOut []beads.Bead
	if err := json.Unmarshal(payload, &beadsOut); err != nil {
		fmt.Fprintf(stderr, "bdshim: decoding %s JSON for summary: %v\n", verb, err) //nolint:errcheck // best-effort stderr
		return 1
	}
	return WriteManagedJSON(context.Background(), "managed_bd_summary", verb, NewBeadSummaryEnvelope(verb, beadsOut, DefaultBeadSummaryBudget), stdout, stderr)
}

// NewBeadShowSummaries projects show results without changing JSON's top-level
// array type. An absent metadata field is absent from Metadata; a withheld one
// is named in FieldsOmitted.
func NewBeadShowSummaries(input []beads.Bead) []BeadShowSummary {
	result := make([]BeadShowSummary, 0, len(input))
	for _, bead := range input {
		source, _ := json.Marshal(bead)
		metadata, omitted := selectedShowMetadata(bead.Metadata)
		result = append(result, BeadShowSummary{
			ID:                    bead.ID,
			Status:                bead.Status,
			CreatedAt:             bead.CreatedAt,
			Assignee:              bead.Assignee,
			Metadata:              metadata,
			SourceSerializedBytes: len(source),
			DetailsOmitted:        append([]string(nil), summaryDetailsOmitted...),
			FieldsOmitted:         omitted,
		})
	}
	return result
}

func selectedShowMetadata(metadata beads.StringMap) (map[string]string, []string) {
	selected := make(map[string]string)
	omitted := make([]string, 0)
	for _, key := range summaryRoutingMetadataKeys {
		value, ok := metadata[key]
		if !ok {
			continue
		}
		if _, wasOmitted := boundedSummaryStringWithOmission(value); wasOmitted {
			omitted = append(omitted, "metadata."+key)
			continue
		}
		selected[key] = value
	}
	return selected, omitted
}

func boundedSummaryStringWithOmission(value string) (string, bool) {
	const maxBytes = 512
	if len(value) <= maxBytes {
		return value, false
	}
	return strings.ToValidUTF8(value[:maxBytes], ""), true
}
