package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/gastownhall/gascity/internal/fsys"
	gcgit "github.com/gastownhall/gascity/internal/git"
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
	if cityPath == "" || len(cfg.InstallAgentHooks) == 0 {
		return
	}
	providers := append([]string(nil), cfg.InstallAgentHooks...)
	cfg.ConvergeManagedHooks = func(workDir string) error {
		workDirs, err := managedHookWorkDirs(workDir, providers)
		if err != nil {
			return err
		}
		for _, hookWorkDir := range workDirs {
			if err := hooks.Install(fsys.OSFS{}, cityPath, hookWorkDir, providers); err != nil {
				return fmt.Errorf("installing managed hooks in %q: %w", hookWorkDir, err)
			}
		}
		return nil
	}
}

func managedHookWorkDirs(workDir string, providers []string) ([]string, error) {
	workDirs := []string{workDir}
	if !containsProvider(providers, "codex") {
		return workDirs, nil
	}

	gitMarker := filepath.Join(workDir, ".git")
	info, err := os.Stat(gitMarker)
	if os.IsNotExist(err) {
		return workDirs, nil
	}
	if err != nil {
		return nil, fmt.Errorf("checking git worktree marker %q: %w", gitMarker, err)
	}
	if info.IsDir() {
		return workDirs, nil
	}

	worktrees, err := gcgit.New(workDir).WorktreeList()
	if err != nil {
		return nil, fmt.Errorf("listing git worktrees for Codex hook convergence from %q: %w", workDir, err)
	}
	for _, worktree := range worktrees {
		marker := filepath.Join(worktree.Path, ".git")
		info, err := os.Stat(marker)
		if err != nil {
			return nil, fmt.Errorf("checking git worktree marker %q: %w", marker, err)
		}
		if !info.IsDir() {
			continue
		}
		if filepath.Clean(worktree.Path) != filepath.Clean(workDir) {
			workDirs = append(workDirs, worktree.Path)
		}
		return workDirs, nil
	}
	return nil, fmt.Errorf("finding primary git worktree for Codex hook convergence from %q: no worktree has a .git directory", workDir)
}

func containsProvider(providers []string, want string) bool {
	for _, provider := range providers {
		if provider == want {
			return true
		}
	}
	return false
}
