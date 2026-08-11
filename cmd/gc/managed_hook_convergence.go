package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/gastownhall/gascity/internal/codexhooks"
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
	codex := cfg.ProviderName == "codex" || containsProvider(providers, "codex")
	if cfg.ProviderName == "codex" && !containsProvider(providers, "codex") {
		providers = append(providers, "codex")
	}
	if len(providers) == 0 {
		return
	}
	cfg.ConvergeManagedHooks = func(workDir string) error {
		if !codex {
			if err := hooks.Install(fsys.OSFS{}, cityPath, workDir, providers); err != nil {
				return fmt.Errorf("installing managed hooks in %q: %w", workDir, err)
			}
			return nil
		}
		return convergeCodexProjectHooks(cityPath, workDir, providers)
	}
}

func convergeCodexProjectHooks(cityPath, workDir string, providers []string) error {
	root, err := resolveCodexProjectRoot(workDir)
	if err != nil {
		return fmt.Errorf("resolving canonical Codex project root from %q: %w", workDir, err)
	}
	if strings.TrimSpace(root) == "" {
		root = workDir
	}
	root = filepath.Clean(root)
	workDir = filepath.Clean(workDir)

	for _, provider := range providers {
		if provider == "codex" {
			continue
		}
		if err := hooks.Install(fsys.OSFS{}, cityPath, workDir, []string{provider}); err != nil {
			return fmt.Errorf("installing %s hooks in %q: %w", provider, workDir, err)
		}
	}

	if err := verifyCodexNonOwnerHasNoManagedHooks(codexUserHooksPath(), "global user"); err != nil {
		return err
	}

	var configuredLayers [][]byte
	if root != workDir {
		layer, err := readOptionalCodexHooks(filepath.Join(workDir, ".codex", "hooks.json"))
		if err != nil {
			return fmt.Errorf("reading linked-worktree Codex hooks before canonical convergence: %w", err)
		}
		if layer != nil {
			configuredLayers = append(configuredLayers, layer)
		}
	}
	if _, err := hooks.ReconcileCodexHooks(fsys.OSFS{}, cityPath, root, configuredLayers); err != nil {
		return fmt.Errorf("converging canonical Codex project hooks in %q: %w", root, err)
	}
	if root != workDir {
		if err := stripCurrentCityManagedCodexHooks(filepath.Join(workDir, ".codex", "hooks.json"), cityPath); err != nil {
			return fmt.Errorf("cleaning linked-worktree Codex non-owner %q: %w", workDir, err)
		}
	}
	if err := verifyCodexOwner(root, cityPath); err != nil {
		return err
	}
	if err := verifyCodexNonOwnerHasNoManagedHooks(codexUserHooksPath(), "global user"); err != nil {
		return err
	}
	return nil
}

func readOptionalCodexHooks(path string) ([]byte, error) {
	data, _, err := readRegularCodexHookFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return data, nil
}

func stripCurrentCityManagedCodexHooks(path, cityPath string) error {
	return codexhooks.WithPathLock(fsys.OSFS{}, path, func() error {
		data, info, err := readRegularCodexHookFile(path)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		stripped, changed, err := hooks.RemoveManagedCodexHooksForCity(data, cityPath)
		if err != nil {
			return err
		}
		if !changed {
			return nil
		}
		if err := codexhooks.WriteFileAtomicNoFollow(fsys.OSFS{}, path, stripped, fsys.ComparableMode(info.Mode())); err != nil {
			return fmt.Errorf("writing stripped Codex hooks: %w", err)
		}
		return nil
	})
}

func verifyCodexOwner(root, cityPath string) error {
	path := filepath.Join(root, ".codex", "hooks.json")
	data, _, err := readRegularCodexHookFile(path)
	if err != nil {
		return fmt.Errorf("reading canonical Codex hook owner %s: %w", path, err)
	}
	if !hooks.CodexHooksAreConverged(data, cityPath) {
		return fmt.Errorf("canonical Codex hook owner %s is not exact-one and bound to city %q", path, cityPath)
	}
	return nil
}

func verifyCodexNonOwnerHasNoManagedHooks(path, label string) error {
	if strings.TrimSpace(path) == "" {
		return nil
	}
	data, _, err := readRegularCodexHookFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("auditing %s Codex hooks %s: %w", label, path, err)
	}
	audit, err := hooks.AuditCodexHooks(data)
	if err != nil {
		return fmt.Errorf("auditing %s Codex hooks %s: %w", label, path, err)
	}
	if len(audit.ManagedBehaviorCounts) > 0 {
		return fmt.Errorf("%s Codex hooks %s contain Gas City-managed behavior %s; remove the redundant managed handlers after verifying their owner, then retry", label, path, formatCodexHookCounts(audit.ManagedBehaviorCounts, []string{"mail", "nudge", "pre-compact", "session-start"}))
	}
	return nil
}

func containsProvider(providers []string, want string) bool {
	for _, provider := range providers {
		if provider == want {
			return true
		}
	}
	return false
}
