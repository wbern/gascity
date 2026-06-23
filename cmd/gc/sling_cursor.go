package main

import (
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
)

// selectDefaultSlingTarget resolves the target for a targetless gc sling.
// DefaultSlingTargets (plural) takes precedence over DefaultSlingTarget; among
// plural targets the rig's DefaultSlingStrategy decides selection ("random"
// default, or "round_robin" using a durable per-rig cursor).
func selectDefaultSlingTarget(rig config.Rig, cityPath string) (string, error) {
	if len(rig.DefaultSlingTargets) > 0 {
		for _, t := range rig.DefaultSlingTargets {
			if strings.TrimSpace(t) == "" {
				return "", fmt.Errorf("rig %q has an empty entry in default_sling_targets", rig.Name)
			}
		}
		switch strings.TrimSpace(rig.DefaultSlingStrategy) {
		case "", "random":
			return rig.DefaultSlingTargets[rand.Intn(len(rig.DefaultSlingTargets))], nil //nolint:gosec // load-balancing, not security-critical
		case "round_robin":
			idx, err := advanceSlingCursor(cityPath, rig.Name, len(rig.DefaultSlingTargets))
			if err != nil {
				return "", err
			}
			return rig.DefaultSlingTargets[idx], nil
		default:
			return "", fmt.Errorf("rig %q has invalid default_sling_strategy %q (want \"random\" or \"round_robin\")", rig.Name, rig.DefaultSlingStrategy)
		}
	}
	if rig.DefaultSlingTarget != "" {
		return rig.DefaultSlingTarget, nil
	}
	return "", fmt.Errorf("rig %q has no default_sling_target or default_sling_targets", rig.Name)
}

// advanceSlingCursor returns the index to use for this round-robin dispatch and
// advances the durable per-rig cursor stored under .gc/runtime/sling. The
// stored value is a monotonically increasing counter; the index is counter % n,
// so adding or removing targets never strands the cursor. Persistence is a
// best-effort atomic write without a lock: concurrent targetless slings to the
// same rig are rare, and the worst case under a race is a single repeated or
// skipped target, not corruption — acceptable for naive load-balancing.
func advanceSlingCursor(cityPath, rigName string, n int) (int, error) {
	if n <= 0 {
		return 0, fmt.Errorf("rig %q has no default_sling_targets to select", rigName)
	}
	dir := filepath.Join(citylayout.RuntimeDataDir(cityPath), "sling")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return 0, fmt.Errorf("creating sling cursor dir: %w", err)
	}
	path := filepath.Join(dir, rigName+".cursor")

	cur := 0
	if data, err := os.ReadFile(path); err == nil {
		if v, perr := strconv.Atoi(strings.TrimSpace(string(data))); perr == nil && v >= 0 {
			cur = v
		}
	}
	idx := cur % n
	if err := fsys.WriteFileAtomic(fsys.OSFS{}, path, []byte(strconv.Itoa(cur+1)+"\n"), 0o644); err != nil {
		return 0, fmt.Errorf("persisting sling cursor: %w", err)
	}
	return idx, nil
}
