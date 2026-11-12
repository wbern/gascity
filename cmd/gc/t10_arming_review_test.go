package main

import (
	"github.com/gastownhall/gascity/internal/config"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
	"testing"
)

// provider_kind is the builtin family only for a custom provider built on a
// builtin ancestor, and empty for a builtin provider. The start-time projection
// patch must derive it the way creation and the metadata refresh do, or the
// key is rewritten back and forth on every start (T10 arming-review).
func TestResolvedProviderProjectionPatchDerivesProviderKindLikeTheRefresh(t *testing.T) {
	builtin := TemplateParams{ResolvedProvider: &config.ResolvedProvider{Name: "claude", BuiltinAncestor: "claude"}}
	custom := TemplateParams{ResolvedProvider: &config.ResolvedProvider{Name: "mine", BuiltinAncestor: "claude"}}

	if patch := resolvedProviderProjectionPatch(sessionpkg.Info{Provider: "claude"}, builtin); hasKey(patch, "provider_kind") {
		t.Fatalf("builtin provider with no provider_kind: patch wrote provider_kind=%q, want no write", patch["provider_kind"])
	}
	patch := resolvedProviderProjectionPatch(sessionpkg.Info{Provider: "claude", ProviderKind: "claude"}, builtin)
	if v, set := patch["provider_kind"]; !set || v != "" {
		t.Fatalf("builtin provider with a stale provider_kind: patch = %q (set=%v), want it cleared to empty like the refresh", v, set)
	}
	if patch := resolvedProviderProjectionPatch(sessionpkg.Info{Provider: "mine"}, custom); patch["provider_kind"] != "claude" {
		t.Fatalf("custom provider on a builtin ancestor: provider_kind = %q, want claude", patch["provider_kind"])
	}
}

func hasKey(m sessionpkg.MetadataPatch, key string) bool {
	_, ok := m[key]
	return ok
}
