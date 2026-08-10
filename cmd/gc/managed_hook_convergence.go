package main

import (
	"fmt"

	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/hooks"
	"github.com/gastownhall/gascity/internal/runtime"
)

// configureManagedHookConvergence makes managed hooks the final writer after
// runtime overlay staging. This keeps provider task worktrees hook-enabled
// while normalizing managed entries to the city's canonical form.
func configureManagedHookConvergence(cfg *runtime.Config, cityPath string) {
	if cfg == nil {
		return
	}
	cfg.ConvergeManagedHooks = nil
	if cityPath == "" {
		return
	}
	providers := append([]string(nil), cfg.InstallAgentHooks...)
	if cfg.ProviderName == "codex" && !containsProvider(providers, "codex") {
		providers = append(providers, "codex")
	}
	if len(providers) == 0 {
		return
	}
	cfg.ConvergeManagedHooks = func(workDir string) error {
		if err := hooks.Install(fsys.OSFS{}, cityPath, workDir, providers); err != nil {
			return fmt.Errorf("installing managed hooks in %q: %w", workDir, err)
		}
		return nil
	}
}

func containsProvider(providers []string, want string) bool {
	for _, provider := range providers {
		if provider == want {
			return true
		}
	}
	return false
}
