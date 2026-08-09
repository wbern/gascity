package main

import (
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/hooks"
	"github.com/gastownhall/gascity/internal/runtime"
)

// configureManagedHookConvergence makes managed hooks the final writer after
// runtime overlay staging. This keeps provider task worktrees hook-enabled
// while normalizing managed entries to the city's canonical form.
func configureManagedHookConvergence(cfg *runtime.Config, cityPath string) {
	if cfg == nil || cityPath == "" || len(cfg.InstallAgentHooks) == 0 {
		return
	}
	providers := append([]string(nil), cfg.InstallAgentHooks...)
	cfg.ConvergeManagedHooks = func(workDir string) error {
		return hooks.Install(fsys.OSFS{}, cityPath, workDir, providers)
	}
}
