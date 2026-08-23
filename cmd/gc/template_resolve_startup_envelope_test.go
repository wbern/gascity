package main

import (
	"encoding/json"
	"testing"
)

func TestBuildT3BridgeStartupEnvelope_UsesTemplateForGroupingAgent(t *testing.T) {
	tp := TemplateParams{
		TemplateName:             "t3code/polecat",
		InstanceName:             "t3code/polecat-1",
		SessionName:              "t3code--polecat-1",
		EffectiveSessionProvider: "t3bridge",
		WorkDir:                  "/data/projects/gc/.gc/worktrees/t3code/polecat/furiosa",
		Command:                  "codex",
		Env: map[string]string{
			"GC_CITY_PATH":    "/data/projects/gc",
			"GC_PROVIDER":     "codex",
			"GC_AGENT":        "t3code/polecat-1",
			"GC_TEMPLATE":     "t3code/polecat",
			"GC_SESSION_NAME": "t3code--polecat-1",
		},
	}

	raw := buildT3BridgeStartupEnvelope(tp, "prime")
	var envelope map[string]any
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatalf("unmarshal envelope: %v", err)
	}

	gc, ok := envelope["gc"].(map[string]any)
	if !ok {
		t.Fatalf("gc section missing: %#v", envelope["gc"])
	}
	if got := gc["agent"]; got != "t3code/polecat" {
		t.Fatalf("gc.agent = %#v, want t3code/polecat", got)
	}
	if got := gc["template"]; got != "t3code/polecat" {
		t.Fatalf("gc.template = %#v, want t3code/polecat", got)
	}
	if got := gc["sessionName"]; got != "t3code--polecat-1" {
		t.Fatalf("gc.sessionName = %#v, want t3code--polecat-1", got)
	}
}

func TestBuildT3BridgeStartupEnvelope_ForwardsBdShimEnvSoCodexRoutesThroughController(t *testing.T) {
	// Regression (gcw-b8yk / codex no_work spawn-loop): codex/t3bridge sessions
	// must receive the bd-shim env block (GC_BD_REAL, ZDOTDIR, GC_BIN, GC_BEADS)
	// so their tool shells resolve the gc-as-bd shim, not the real bd. Without
	// ZDOTDIR the codex zsh tool shell sources the user's ~/.zshrc (which
	// re-prepends ~/go/bin), resolves the real bd, reads a store view that misses
	// controller-fresh/federated routed work, and `gc hook --claim` returns
	// no_work -> pool spawn-loop. Claude sessions (tmux) already receive these.
	tp := TemplateParams{
		TemplateName:             "gas-city-infra/codex-polecat",
		InstanceName:             "gas-city-infra/codex-polecat-1",
		SessionName:              "codex-polecat-gc2-nhr6q",
		EffectiveSessionProvider: "t3bridge",
		WorkDir:                  "/tmp/wt",
		Command:                  "codex",
		Env: map[string]string{
			"GC_CITY_PATH":    "/tmp/city",
			"GC_PROVIDER":     "codex",
			"GC_AGENT":        "gas-city-infra/codex-polecat-1",
			"GC_TEMPLATE":     "gas-city-infra/codex-polecat",
			"GC_SESSION_NAME": "codex-polecat-gc2-nhr6q",
			"GC_BD_REAL":      "/home/u/go/bin/bd",
			"ZDOTDIR":         "/tmp/city/.gc/shimzdotdir",
			"GC_BIN":          "/tmp/city/.gc/shimbin/gc",
			"GC_BEADS":        "bd",
			"PATH":            "/tmp/city/.gc/shimbin:/usr/local/bin:/usr/bin",
			// store-connection env: the load-bearing gap — without these the
			// codex session's bd cannot reach the managed Dolt server -> no_work.
			"GC_DOLT_PORT":           "49813",
			canonicalDoltPortEnv:     "49813",
			"GC_DOLT_USER":           "root",
			"BEADS_DOLT_SERVER_PORT": "49813",
			"GC_BEADS_SCOPE_ROOT":    "/tmp/city",
			"BEADS_DIR":              "/tmp/city/rigs/crm/.beads",
			"BEADS_ACTOR":            "gas-city-infra/codex-polecat-1",
			"GC_SESSION_ID":          "gc2-nhr6q",
		},
	}

	raw := buildT3BridgeStartupEnvelope(tp, "prime")
	var envelope map[string]any
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatalf("unmarshal envelope: %v", err)
	}
	ctx, ok := envelope["context"].(map[string]any)
	if !ok {
		t.Fatalf("context section missing: %#v", envelope["context"])
	}
	gcEnv, ok := ctx["gcEnv"].(map[string]any)
	if !ok {
		t.Fatalf("context.gcEnv missing: %#v", ctx["gcEnv"])
	}
	// shim vars + the store-connection vars must all reach codex, or its bd
	// resolves the wrong binary AND/OR cannot reach the managed Dolt store.
	for _, k := range []string{
		"GC_BD_REAL", "ZDOTDIR", "GC_BIN", "GC_BEADS", "PATH",
		"GC_DOLT_PORT", canonicalDoltPortEnv, "GC_DOLT_USER", "BEADS_DOLT_SERVER_PORT",
		"GC_BEADS_SCOPE_ROOT", "BEADS_DIR", "BEADS_ACTOR", "GC_SESSION_ID",
	} {
		if got, _ := gcEnv[k].(string); got != tp.Env[k] {
			t.Fatalf("gcEnv[%q] = %#v, want %q (codex must receive the store/shim env or gc hook --claim -> no_work)", k, gcEnv[k], tp.Env[k])
		}
	}
}

func TestBuildT3BridgeStartupEnvelope_NamedSessionPublishesTemplatePatchIdentity(t *testing.T) {
	tp := TemplateParams{
		TemplateName:             "crew",
		InstanceName:             "t3code/gastown.crew",
		Alias:                    "t3code/gastown.crew",
		SessionName:              "t3code--gastown__crew",
		EffectiveSessionProvider: "t3bridge",
		WorkDir:                  "/data/projects/gc/.gc/worktrees/t3code/crew/gastown.crew",
		Command:                  "codex",
		Env: map[string]string{
			"GC_CITY_PATH":    "/data/projects/gc",
			"GC_PROVIDER":     "codex",
			"GC_AGENT":        "t3code/gastown.crew",
			"GC_TEMPLATE":     "crew",
			"GC_SESSION_NAME": "t3code--gastown__crew",
		},
	}

	raw := buildT3BridgeStartupEnvelope(tp, "prime")
	var envelope map[string]any
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatalf("unmarshal envelope: %v", err)
	}

	gc, ok := envelope["gc"].(map[string]any)
	if !ok {
		t.Fatalf("gc section missing: %#v", envelope["gc"])
	}
	if got := gc["template"]; got != "crew" {
		t.Fatalf("gc.template = %#v, want crew", got)
	}
}

func TestBuildT3BridgeStartupEnvelope_ThreadReuseFollowsWakeMode(t *testing.T) {
	for _, tc := range []struct {
		name      string
		wakeMode  string
		wantReuse bool
	}{
		{name: "fresh", wakeMode: "fresh", wantReuse: false},
		{name: "resume", wakeMode: "resume", wantReuse: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tp := TemplateParams{
				TemplateName:             "worker",
				SessionName:              "worker-1",
				EffectiveSessionProvider: "t3bridge",
				WorkDir:                  "/tmp/work",
				Command:                  "codex",
				WakeMode:                 tc.wakeMode,
				Env: map[string]string{
					"GC_CITY_PATH": "/tmp/city",
					"GC_PROVIDER":  "codex",
				},
			}

			var envelope struct {
				Resume struct {
					AllowThreadReuse bool `json:"allowThreadReuse"`
				} `json:"resume"`
			}
			if err := json.Unmarshal(buildT3BridgeStartupEnvelope(tp, "prime"), &envelope); err != nil {
				t.Fatalf("unmarshal envelope: %v", err)
			}
			if envelope.Resume.AllowThreadReuse != tc.wantReuse {
				t.Fatalf("allowThreadReuse = %v, want %v", envelope.Resume.AllowThreadReuse, tc.wantReuse)
			}
		})
	}
}

func TestTemplateParamsToConfigCarriesManagedCodexSessionFlagsForT3Bridge(t *testing.T) {
	tp := TemplateParams{
		CityPath:                 "/city with spaces",
		EffectiveSessionProvider: "t3bridge",
		Env: map[string]string{
			"GC_CITY_PATH": "/city with spaces",
			"GC_PROVIDER":  "codex",
		},
	}
	if err := resolveT3BridgeSessionFlags(&tp); err != nil {
		t.Fatalf("resolveT3BridgeSessionFlags: %v", err)
	}

	flags := templateParamsToConfig(tp).CodexSessionFlags
	if flags == nil {
		t.Fatal("runtime config missing managed Codex session flags")
	}
	if err := flags.Validate(); err != nil {
		t.Fatalf("validate session flags payload: %v", err)
	}
	if !flags.Config.FeaturesHooks || !flags.Config.BypassHookTrust {
		t.Fatalf("hook activation flags = features:%v trust:%v, want both true", flags.Config.FeaturesHooks, flags.Config.BypassHookTrust)
	}
	if len(flags.Config.SessionStart) != 1 || len(flags.Config.PreCompact) != 1 || len(flags.Config.UserPromptSubmit) != 1 {
		t.Fatalf("managed hook cardinality = SessionStart:%d PreCompact:%d UserPromptSubmit:%d, want 1/1/1",
			len(flags.Config.SessionStart), len(flags.Config.PreCompact), len(flags.Config.UserPromptSubmit))
	}
}

func TestResolveT3BridgeSessionFlagsAbsentOutsideT3Codex(t *testing.T) {
	for _, tc := range []struct {
		name            string
		sessionProvider string
		provider        string
	}{
		{name: "non-T3 Codex", sessionProvider: "tmux", provider: "codex"},
		{name: "T3 non-Codex", sessionProvider: "t3bridge", provider: "claudeAgent"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tp := TemplateParams{
				CityPath:                 "/city",
				EffectiveSessionProvider: tc.sessionProvider,
				Env:                      map[string]string{"GC_PROVIDER": tc.provider},
			}
			if err := resolveT3BridgeSessionFlags(&tp); err != nil {
				t.Fatalf("resolveT3BridgeSessionFlags: %v", err)
			}
			if tp.CodexSessionFlags != nil {
				t.Fatalf("CodexSessionFlags = %#v, want nil", tp.CodexSessionFlags)
			}
		})
	}
}
