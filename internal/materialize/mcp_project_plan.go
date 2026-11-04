package materialize

import (
	"bytes"
	"fmt"

	"github.com/gastownhall/gascity/internal/fsys"
)

// MCPPlanAction is the filesystem action a projection would take.
type MCPPlanAction string

// Projection plan actions describe the change without applying it.
const (
	MCPPlanNone      MCPPlanAction = "none"
	MCPPlanCreate    MCPPlanAction = "create"
	MCPPlanUpdate    MCPPlanAction = "update"
	MCPPlanUnchanged MCPPlanAction = "unchanged"
	MCPPlanRemove    MCPPlanAction = "remove"
)

// MCPProjectionPlan describes the read-only result of reconciling one target.
type MCPProjectionPlan struct {
	Provider    string
	Target      string
	Servers     int
	Action      MCPPlanAction
	Adopt       bool
	SnapshotDir string
	Refused     *MCPAdoptionRefusedError
}

// Plan reports what Apply would do without writing a target, marker, lock,
// snapshot, or .gitignore entry.
func (p MCPProjection) Plan(fs fsys.FS) (MCPProjectionPlan, error) {
	plan := MCPProjectionPlan{
		Provider: p.Provider,
		Target:   p.Target,
		Servers:  len(p.Servers),
		Action:   MCPPlanNone,
	}
	if err := p.validateProviderContract(); err != nil {
		return plan, err
	}
	managed := p.isManaged(fs)
	if len(p.Servers) == 0 && !managed {
		return plan, nil
	}
	refused, err := p.AdoptionRefusal(fs)
	if err != nil {
		return plan, err
	}
	if refused != nil {
		plan.Refused = refused
		return plan, nil
	}
	if err := ensureNotSymlink(fs, p); err != nil {
		return plan, err
	}
	current, err := readRegularFile(fs, p.Target)
	exists := err == nil
	if err != nil && !errorsIsNotExist(err) {
		// A managed Claude target is gc-owned whole; its current bytes do not
		// affect the replacement plan.
		if !managed || p.Provider != MCPProviderClaude {
			return plan, fmt.Errorf("reading %s: %w", p.Target, err)
		}
		exists = true
	}
	content, remove, err := p.plannedContent(current)
	if err != nil {
		return plan, err
	}
	switch {
	case remove:
		if exists || managed {
			plan.Action = MCPPlanRemove
		}
	case !exists:
		plan.Action = MCPPlanCreate
	case bytes.Equal(current, content):
		plan.Action = MCPPlanUnchanged
	default:
		plan.Action = MCPPlanUpdate
	}
	if exists && len(p.Servers) > 0 && !managed {
		plan.Adopt = true
		plan.SnapshotDir = p.adoptionSnapshotDir()
	}
	return plan, nil
}

func (p MCPProjection) plannedContent(current []byte) ([]byte, bool, error) {
	if p.Provider == MCPProviderClaude {
		if len(p.Servers) == 0 {
			return nil, true, nil
		}
		content, err := marshalJSONDoc(map[string]any{"mcpServers": p.claudeServersDoc()})
		return content, false, err
	}
	doc, err := parseMCPDocument(p.Provider, current)
	if err != nil {
		return nil, false, fmt.Errorf("parsing %s: %w", p.Target, err)
	}
	key, servers, err := p.serversDoc()
	if err != nil {
		return nil, false, err
	}
	if len(p.Servers) == 0 {
		delete(doc, key)
		if len(doc) == 0 {
			return nil, true, nil
		}
	} else {
		doc[key] = servers
	}
	if p.Provider == MCPProviderCodex {
		content, err := marshalTOMLDoc(doc)
		return content, false, err
	}
	content, err := marshalJSONDoc(doc)
	return content, false, err
}
