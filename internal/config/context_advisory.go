package config

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"text/template"
)

// advPtr returns a pointer to v. Used to build the built-in advisory defaults,
// whose fields are nil-able so scope merge can distinguish "unset" from "set".
func advPtr[T any](v T) *T { return &v }

// ContextAdvisoryTier is one level of the context-pressure advisory: when a
// session's context usage reaches Threshold (percent of the context window),
// Message is rendered and injected into the prompt. Pointer fields let scope
// merge distinguish "unset" (inherit) from a deliberately-set value.
type ContextAdvisoryTier struct {
	Threshold *int    `toml:"threshold"`
	Message   *string `toml:"message"`
	Enabled   *bool   `toml:"enabled"`
}

// ContextAdvisory is the neutral, config-driven context-pressure advisory. It is
// declared globally under [agent_defaults.context_advisory] and overridable
// per-agent under [context_advisory]; nil-able fields fall through to the next
// scope (per-agent -> global -> built-in). This type carries only the mechanism
// (a configurable array of tiers: threshold, message, enable). Any opinionated
// handoff posture is operator config, never baked in here.
type ContextAdvisory struct {
	Enabled      *bool                 `toml:"enabled"`
	WindowTokens *int                  `toml:"window_tokens"`
	Tiers        []ContextAdvisoryTier `toml:"tiers"`
}

// ResolvedTier is a fully-resolved advisory tier (no nil fields).
type ResolvedTier struct {
	Threshold int
	Message   string
}

// ContextAdvisoryPolicy is the effective advisory after scope merge: the enable
// flag, the context-window override (0 = autodetect), and the enabled tiers
// sorted ascending by threshold.
type ContextAdvisoryPolicy struct {
	Enabled      bool
	WindowTokens int
	Tiers        []ResolvedTier
}

// Built-in default tier messages. These reproduce the historical two-tier
// guidance, updated to recommend `gc handoff` (attended-safe, carries a
// continuation note) instead of `gc session reset`. Rendered as text/template
// against ContextAdvisoryView.
const (
	builtinAdvisoryMessage = "Context usage: {{.UsedK}}/{{.WindowK}} (~{{printf \"%.0f\" .Pct}}%). Approaching the recycle zone. Steer toward a clean seam: finish in-flight work, don't open new long-horizon tasks, and keep durable notes/work-items current so a handoff is cheap. Plan to run `gc handoff` before this climbs into the urgent band — a fresh session from durable notes outperforms riding lossy compaction."
	builtinUrgentMessage   = "Context usage: {{.UsedK}}/{{.WindowK}} (~{{printf \"%.0f\" .Pct}}%) — HIGH. Recycle now: reach a clean seam, then run `gc handoff` to resume fresh from durable notes, work-item updates, and memory. Repeated compaction degrades awareness — a clean handoff beats running to compaction. Do this at a seam; do NOT abandon work mid-step. (If an operator told you to stay up, hold at a clean seam instead.)"
)

// DefaultContextAdvisory returns the built-in advisory config: enabled, window
// autodetect, advisory@60 and urgent@80 tiers with gc-handoff wording. Used as
// the base scope so zero-config reproduces (close to) historical behavior.
func DefaultContextAdvisory() ContextAdvisory {
	return ContextAdvisory{
		Enabled:      advPtr(true),
		WindowTokens: advPtr(0),
		Tiers: []ContextAdvisoryTier{
			{Threshold: advPtr(60), Message: advPtr(builtinAdvisoryMessage), Enabled: advPtr(true)},
			{Threshold: advPtr(80), Message: advPtr(builtinUrgentMessage), Enabled: advPtr(true)},
		},
	}
}

// ContextAdvisoryView is the data passed to a tier's message template. New
// fields are additive only — existing fields are a stable contract.
type ContextAdvisoryView struct {
	UsedTokens   int
	WindowTokens int
	UsedK        string  // pre-rounded, e.g. "128k"
	WindowK      string  // pre-rounded, e.g. "200k"
	Pct          float64 // 0..100
	Threshold    int     // threshold of the tier that fired
}

// ResolveContextAdvisory merges scopes in increasing precedence order (e.g.
// built-in, global, per-agent, env), field-by-field, and returns the effective
// policy. Top-level fields (Enabled, WindowTokens) take the highest-precedence
// non-nil value. The tier set is replace-on-set: the highest-precedence scope
// that supplies a non-nil Tiers slice wins entirely (tiers do not element-merge).
// Disabled or incomplete tiers are dropped and the result is sorted ascending by
// threshold. nil scopes are ignored. This function is pure (no I/O).
func ResolveContextAdvisory(scopes ...*ContextAdvisory) ContextAdvisoryPolicy {
	pol := ContextAdvisoryPolicy{}
	var tiers []ContextAdvisoryTier
	for _, s := range scopes {
		if s == nil {
			continue
		}
		if s.Enabled != nil {
			pol.Enabled = *s.Enabled
		}
		if s.WindowTokens != nil {
			pol.WindowTokens = *s.WindowTokens
		}
		if s.Tiers != nil {
			tiers = s.Tiers
		}
	}
	for _, t := range tiers {
		if t.Threshold == nil || t.Message == nil {
			continue // incomplete tier; Validate reports these loudly at config time
		}
		if t.Enabled != nil && !*t.Enabled {
			continue
		}
		pol.Tiers = append(pol.Tiers, ResolvedTier{Threshold: *t.Threshold, Message: *t.Message})
	}
	sort.SliceStable(pol.Tiers, func(i, j int) bool { return pol.Tiers[i].Threshold < pol.Tiers[j].Threshold })
	return pol
}

// Validate checks a single ContextAdvisory scope for configuration errors that
// must fail loud at `gc lint` / `gc doctor` time. It validates only the fields
// this scope sets (nil fields are inherited and validated in their own scope):
// thresholds in 0..100, strictly ascending (no duplicates/overlap), a non-empty
// message, and a message template that parses and renders against the known
// variable set. Returns nil when the scope is valid.
func (c *ContextAdvisory) Validate() error {
	if c == nil {
		return nil
	}
	var prev *int
	for i, t := range c.Tiers {
		if t.Threshold == nil {
			return fmt.Errorf("context_advisory tier %d: threshold is required", i)
		}
		if *t.Threshold < 0 || *t.Threshold > 100 {
			return fmt.Errorf("context_advisory tier %d: threshold %d out of range (0-100)", i, *t.Threshold)
		}
		if prev != nil && *t.Threshold <= *prev {
			return fmt.Errorf("context_advisory tier %d: threshold %d must be greater than the previous tier's %d (tiers must ascend, no duplicates)", i, *t.Threshold, *prev)
		}
		prev = t.Threshold
		if t.Message == nil || strings.TrimSpace(*t.Message) == "" {
			return fmt.Errorf("context_advisory tier %d (threshold %d): message is required", i, *t.Threshold)
		}
		tmpl, err := template.New("tier").Option("missingkey=error").Parse(*t.Message)
		if err != nil {
			return fmt.Errorf("context_advisory tier %d (threshold %d): message template parse error: %w", i, *t.Threshold, err)
		}
		if err := tmpl.Execute(io.Discard, ContextAdvisoryView{}); err != nil {
			return fmt.Errorf("context_advisory tier %d (threshold %d): message template references an unknown variable or fails to render: %w", i, *t.Threshold, err)
		}
	}
	return nil
}

// SelectTier returns the highest-threshold tier whose threshold is <= pct, and
// true; or a zero tier and false when the policy is disabled, has no tiers, or
// pct is below the lowest tier. Tiers must be sorted ascending (as
// ResolveContextAdvisory returns them).
func (p ContextAdvisoryPolicy) SelectTier(pct float64) (ResolvedTier, bool) {
	if !p.Enabled {
		return ResolvedTier{}, false
	}
	sel, found := ResolvedTier{}, false
	for _, t := range p.Tiers {
		if pct >= float64(t.Threshold) {
			sel, found = t, true
			continue
		}
		break
	}
	return sel, found
}

// RenderTier renders a resolved tier's message template against view. On any
// parse or execute error it falls back to the raw (un-templated) message, so a
// prompt is never blocked by a bad template at runtime (Validate catches these
// loudly at config time). This function is pure.
func RenderTier(t ResolvedTier, view ContextAdvisoryView) string {
	tmpl, err := template.New("tier").Parse(t.Message)
	if err != nil {
		return t.Message
	}
	var b strings.Builder
	if err := tmpl.Execute(&b, view); err != nil {
		return t.Message
	}
	return b.String()
}
