package main

import (
	"io"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/orders"
)

// TestOrderExecEnvFrontsShimbinWhenInstalled pins that an exec order's PATH is
// fronted with the bd-shim bin dir when the shim is installed, so its `bd`
// routes through the warm controller (symmetric with managed sessions), and is
// NOT fronted under bd_shim=off (the supervisor removes the shim). gcw-p6al.
func TestOrderExecEnvFrontsShimbinWhenInstalled(t *testing.T) {
	cityPath := t.TempDir()
	realBdDir := t.TempDir()
	writeFakeBd(t, realBdDir)
	t.Setenv("PATH", realBdDir)

	// auto install -> shim present -> exec-order PATH is fronted with the shim dir.
	if err := ensureCityBdShimbin(cityPath, config.BdShimModeAuto, io.Discard); err != nil {
		t.Fatalf("install shim: %v", err)
	}
	env, err := orderExecEnvWithError(cityPath, &config.City{}, execStoreTarget{ScopeKind: "city", ScopeRoot: cityPath}, orders.Order{}, nil)
	if err != nil {
		t.Fatalf("orderExecEnvWithError: %v", err)
	}
	shimDir := cityBdShimbinDir(cityPath)
	if !envPathFrontedWith(env, shimDir) {
		t.Fatalf("exec-order PATH not fronted with shim dir %q; env PATH=%q", shimDir, envValue(env, "PATH"))
	}

	// off -> shim removed -> exec-order PATH is NOT fronted with the shim dir.
	if err := ensureCityBdShimbin(cityPath, config.BdShimModeOff, io.Discard); err != nil {
		t.Fatalf("switch off: %v", err)
	}
	env, err = orderExecEnvWithError(cityPath, &config.City{}, execStoreTarget{ScopeKind: "city", ScopeRoot: cityPath}, orders.Order{}, nil)
	if err != nil {
		t.Fatalf("orderExecEnvWithError (off): %v", err)
	}
	if envPathFrontedWith(env, shimDir) {
		t.Fatalf("exec-order PATH fronted with shim dir under bd_shim=off; env PATH=%q", envValue(env, "PATH"))
	}
}

func envValue(env []string, key string) string {
	for _, kv := range env {
		if strings.HasPrefix(kv, key+"=") {
			return strings.TrimPrefix(kv, key+"=")
		}
	}
	return ""
}

func envPathFrontedWith(env []string, dir string) bool {
	path := envValue(env, "PATH")
	if path == "" {
		return false
	}
	return strings.Split(path, ":")[0] == dir || strings.Contains(path, dir+":") || filepath.Dir(path) == dir
}
