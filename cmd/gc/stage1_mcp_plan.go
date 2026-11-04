package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/materialize"
)

func planStage1MCPProjection(cityPath string, cfg *config.City, lookPath config.LookPathFunc, stderr io.Writer) ([]string, error) {
	targets, err := buildStage1MCPTargets(cityPath, cfg, lookPath, stderr)
	if err != nil {
		return nil, err
	}
	fs := fsys.OSFS{}
	var lines []string
	desired := make(map[string]bool, len(targets))
	gitignore := make(map[string][]string)
	for _, target := range targets {
		plan, err := target.Projection.Plan(fs)
		if err != nil {
			return nil, fmt.Errorf("planning %s: %w", target.Projection.Target, err)
		}
		if line := describeMCPPlan(plan); line != "" {
			lines = append(lines, line)
		}
		if plan.Refused != nil {
			continue
		}
		desired[managedKey(target.Projection.Provider, target.Root)] = true
		if len(target.Projection.Servers) > 0 {
			gitignore[target.Root] = append(gitignore[target.Root], mcpGitignoreEntry(target.Projection))
		}
	}
	orphans, err := planOrphanStage1MCPTargets(cityPath, cfg, desired)
	if err != nil {
		return nil, err
	}
	lines = append(lines, orphans...)
	for _, root := range sortedMapKeys(gitignore) {
		missing, err := missingMCPGitignoreEntries(fs, root, gitignore[root])
		if err != nil {
			return nil, err
		}
		if len(missing) == 0 {
			continue
		}
		path := filepath.Join(root, ".gitignore")
		verb := "update"
		if _, err := fs.Stat(path); err != nil {
			verb = "create"
		}
		lines = append(lines, fmt.Sprintf("%s %s: add %s", verb, path, strings.Join(missing, ", ")))
	}
	return lines, nil
}

func planOrphanStage1MCPTargets(cityPath string, cfg *config.City, desired map[string]bool) ([]string, error) {
	if cfg == nil {
		return nil, nil
	}
	var lines []string
	for _, root := range collectStage1ScopeRoots(cityPath, cfg) {
		entries, err := fsys.OSFS{}.ReadDir(filepath.Join(root, ".gc", "mcp-managed"))
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, fmt.Errorf("scanning %s/.gc/mcp-managed: %w", root, err)
		}
		for _, entry := range entries {
			if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
				continue
			}
			provider := strings.TrimSuffix(entry.Name(), ".json")
			if !supportsMCPProviderKind(provider) || desired[managedKey(provider, root)] {
				continue
			}
			projection, err := materialize.BuildMCPProjection(provider, root, nil)
			if err != nil {
				return nil, fmt.Errorf("building orphan MCP projection for %s at %s: %w", provider, root, err)
			}
			plan, err := projection.Plan(fsys.OSFS{})
			if err != nil {
				return nil, fmt.Errorf("planning orphan MCP cleanup for %s at %s: %w", provider, root, err)
			}
			if line := describeMCPPlan(plan); line != "" {
				lines = append(lines, line+" (no agent claims it)")
			}
		}
	}
	return lines, nil
}

func describeMCPPlan(plan materialize.MCPProjectionPlan) string {
	servers := fmt.Sprintf("%d MCP server", plan.Servers)
	if plan.Servers != 1 {
		servers += "s"
	}
	switch {
	case plan.Refused != nil:
		return fmt.Sprintf("leave %s (%s) as it is: %s; %s", plan.Target, plan.Provider, plan.Refused.Reason(), plan.Refused.Remedy())
	case plan.Adopt && plan.Provider == materialize.MCPProviderClaude:
		return fmt.Sprintf("adopt %s (%s): save its current content under %s, then replace it with %s", plan.Target, plan.Provider, plan.SnapshotDir, servers)
	case plan.Adopt:
		return fmt.Sprintf("adopt %s (%s): save its current content under %s, then replace its MCP servers with %s", plan.Target, plan.Provider, plan.SnapshotDir, servers)
	case plan.Action == materialize.MCPPlanCreate:
		return fmt.Sprintf("create %s (%s) with %s", plan.Target, plan.Provider, servers)
	case plan.Action == materialize.MCPPlanUpdate && plan.Servers == 0:
		return fmt.Sprintf("update %s (%s): remove gc's MCP servers", plan.Target, plan.Provider)
	case plan.Action == materialize.MCPPlanUpdate:
		return fmt.Sprintf("update %s (%s) to %s", plan.Target, plan.Provider, servers)
	case plan.Action == materialize.MCPPlanRemove:
		return fmt.Sprintf("remove %s (%s)", plan.Target, plan.Provider)
	default:
		return ""
	}
}
