package main

import (
	"encoding/json"
	"path/filepath"
	"strings"

	"github.com/gastownhall/gascity/internal/runtime/t3bridge"
)

func applyT3BridgeRuntimeConfig(tp TemplateParams, env map[string]string) {
	if !templateParamsUseT3Bridge(tp) {
		return
	}
	alias := strings.TrimSpace(tp.Alias)
	if alias == "" {
		alias = strings.TrimSpace(env["GC_AGENT"])
	}
	if alias == "" {
		alias = strings.TrimSpace(tp.InstanceName)
	}
	if alias == "" {
		alias = strings.TrimSpace(tp.TemplateName)
	}
	if alias != "" && strings.TrimSpace(env["GC_ALIAS"]) == "" {
		env["GC_ALIAS"] = alias
	}
	if strings.TrimSpace(env["GC_PROVIDER"]) == "" && tp.ResolvedProvider != nil {
		env["GC_PROVIDER"] = tp.ResolvedProvider.Name
	}
}

func buildT3BridgeStartupEnvelope(tp TemplateParams, startupPrompt string) json.RawMessage {
	if !templateParamsUseT3Bridge(tp) {
		return nil
	}
	provider := ""
	if tp.ResolvedProvider != nil {
		provider = tp.ResolvedProvider.Name
	}
	if provider == "" {
		provider = tp.Env["GC_PROVIDER"]
	}
	// gcEnv is the env the t3bridge runtime projects onto the codex process.
	// It MUST carry the same store-connection env the tmux/claude path gives its
	// sessions (template_resolve.go Step 8), or a codex session's bd cannot reach
	// the managed Dolt server the controller/claude use: bd falls back to a
	// different/empty store, `bd ready ...routed_to...` returns nothing, and
	// `gc hook --claim` = no_work -> pool spawn-loop (gci-x8zo). Identity + shim
	// vars alone are necessary-but-insufficient (the shim still needs GC_DOLT_PORT
	// + backend creds to connect). Forward the canonical Dolt/backend keys plus
	// the scope/actor/session keys instead of a hand-maintained allowlist that
	// keeps losing vars. GC_DOLT_PORT is rebuilt per managed-server restart
	// (bd_env.go), so reading tp.Env at envelope-build time picks up the live port.
	gcEnv := map[string]any{
		"GC_AGENT":        tp.Env["GC_AGENT"],
		"GC_PROVIDER":     provider,
		"GC_TEMPLATE":     tp.Env["GC_TEMPLATE"],
		"GC_CITY_PATH":    tp.Env["GC_CITY_PATH"],
		"GC_RIG":          tp.Env["GC_RIG"],
		"GC_SESSION_NAME": tp.Env["GC_SESSION_NAME"],
		// bd-shim resolution vars: without ZDOTDIR/GC_BD_REAL the codex tool shell
		// resolves the wrong bd; GC_BIN/GC_BEADS keep bead ops on the shimmed gc.
		"GC_BD_REAL": tp.Env["GC_BD_REAL"],
		"ZDOTDIR":    tp.Env["ZDOTDIR"],
		"GC_BIN":     tp.Env["GC_BIN"],
		"GC_BEADS":   tp.Env["GC_BEADS"],
	}
	// Store-connection env (the load-bearing gap for gci-x8zo): the managed Dolt
	// server coordinates + the bead scope/actor/session identity.
	storeEnvKeys := append([]string{}, projectedDoltEnvKeys...)
	storeEnvKeys = append(storeEnvKeys,
		canonicalDoltHostEnv, canonicalDoltPortEnv,
		"GC_BEADS_SCOPE_ROOT", "BEADS_DIR", "BEADS_ACTOR",
		"GC_SESSION_ID", "GC_BEADS_BACKEND", "BEADS_BACKEND", "GC_CITY",
	)
	for _, k := range storeEnvKeys {
		if v := strings.TrimSpace(tp.Env[k]); v != "" {
			gcEnv[k] = v
		}
	}
	envelope := map[string]any{
		"version": 1,
		"gc": map[string]any{
			"cityPath":    tp.Env["GC_CITY_PATH"],
			"cityName":    filepath.Base(tp.Env["GC_CITY_PATH"]),
			"rigName":     tp.RigName,
			"rigPath":     tp.RigRoot,
			"agent":       tp.TemplateName,
			"template":    tp.TemplateName,
			"sessionName": tp.SessionName,
		},
		"runtime": map[string]any{
			"provider":         provider,
			"model":            t3BridgeStartupEnvelopeModel(provider, tp),
			"sessionTransport": tp.EffectiveSessionProvider,
			"runtimeMode":      "full-access",
			"interactionMode":  "default",
			"workDir":          tp.WorkDir,
			"command":          tp.Command,
		},
		"startup": map[string]any{
			"promptTemplate": tp.TemplateName,
			"startupPrompt":  startupPrompt,
			"initialNudge":   tp.Hints.Nudge,
		},
		"context": map[string]any{
			"gcEnv": gcEnv,
		},
		"resume": map[string]any{
			"policy":                 "match-or-recreate",
			"allowThreadReuse":       t3bridge.AllowThreadReuse(t3bridge.AgentKindNamed, tp.WakeMode),
			"requiredThreadProvider": provider,
			"requiredThreadModel":    t3BridgeStartupEnvelopeModel(provider, tp),
		},
	}
	data, err := json.Marshal(envelope)
	if err != nil {
		return nil
	}
	return json.RawMessage(data)
}

func templateParamsUseT3Bridge(tp TemplateParams) bool {
	sessionProvider := strings.TrimSpace(tp.EffectiveSessionProvider)
	if sessionProvider == "" {
		sessionProvider = strings.TrimSpace(tp.SessionOverride)
	}
	if sessionProvider == "t3bridge" {
		return true
	}
	if strings.HasPrefix(sessionProvider, "exec:") {
		return isLegacyT3BridgeExecScript(strings.TrimPrefix(sessionProvider, "exec:"))
	}
	return false
}

func effectiveSessionProvider(sessionOverride, citySessionProvider string) string {
	if strings.TrimSpace(sessionOverride) != "" {
		return strings.TrimSpace(sessionOverride)
	}
	return strings.TrimSpace(citySessionProvider)
}

func t3BridgeStartupEnvelopeModel(provider string, tp TemplateParams) string {
	if strings.Contains(tp.Command, "--model ") {
		parts := strings.Fields(tp.Command)
		for i := 0; i < len(parts)-1; i++ {
			if parts[i] == "--model" {
				return parts[i+1]
			}
		}
	}
	if v := tp.Env["GC_MODEL"]; v != "" {
		return v
	}
	if provider == "codex" {
		return "gpt-5-codex"
	}
	return "claude-opus-4-6"
}
