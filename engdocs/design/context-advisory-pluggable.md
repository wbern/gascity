# Pluggable, config-driven context-pressure advisory

| Field | Value |
|---|---|
| Status | Draft — refined 2026-06-30 per William; for review |
| Date | 2026-06-29, refined 2026-06-30 |
| Author(s) | Architect (gas-city-wbern + crm/architect), William |
| Bead | `gcw-dnh` (fork) → gh-3810; stopgap `gcw-v21` **closed/landed** |
| Upstream issue | gastownhall/gascity#3810 (filed 2026-06-29) |
| Relates | upstream #3371 (shipped feature), #3604 (under-report bug), #903 (deferred PackV2 agent-defaults), #1465 (per-step recycle policy) |
| Supersedes | the env-var-only knobs + hardcoded strings in `cmd/gc/context_inject.go` |

Turn the context-pressure advisory from a hard-coded, env-only, human-gated
*warning* into a **programmable, mostly-autonomous handoff trigger**: an operator
declares — at **global (city)** and **per-target (per-agent)** scope — a
configurable **array of tiers**, each with its own threshold and
operator-authored message template. Zero-config reproduces today's behavior.

## North star (why this exists)

The shipped advisory exists to make agents **defer work and recycle early**, and
to **escalate the recycle decision to the human**. That conservatism is the
*annoyance* this work removes. The goal is the inverse: context-pressure handling
that is a **quiet, fluid, near-transparent part of normal work** — at the
operator's threshold the agent receives a message that points it at a **skill
that performs the handoff itself**, autonomously, **without asking** (except at a
genuinely critical step) and **without getting more conservative** beforehand.
Net effect: large contexts simply stop accumulating, as background hygiene.

**Crucially, that opinion is not what we upstream.** See the next section.

## Mechanism vs opinion — what ships where

This is the spine of the design.

- **MECHANISM (neutral, upstream-contributable).** The config surface itself:
  the tier array, per-target messages, thresholds, window override, the
  resolution/precedence order, validation, and back-compat. It carries **zero
  behavioral opinion** — "we added knobs; we changed no one's behavior."
  → lands on the feature branch → `develop` → upstream PR to `gastownhall/gascity`.
- **OPINION (operator config, stays local).** The transparent/autonomous-handoff
  posture — the 80% message that references a handoff skill, non-conservative
  wording, no-permission-ask — is **operator-authored config values**. It lives in
  the **DevOps pack** (`gas-city-infra/city-template/city.toml` + `agents/`),
  applied only at dogfood time. It is **never** in the feature branch, `develop`
  code, or the upstream PR.
- **Enable-by-default is a maintainer question**, raised *in the PR*, not decided
  here. The operator would accept the feature shipping **off by default** upstream.
  Zero-config preserves today's thresholds + the `gc handoff` wording from the
  closed `gcw-v21` stopgap.

## Problem

[`cmd/gc/context_inject.go`](../../cmd/gc/context_inject.go) injects one line of
context-pressure guidance into each `UserPromptSubmit` hook payload (folded with
the clock line at [`cmd/gc/cmd_nudge.go:406`](../../cmd/gc/cmd_nudge.go#L406)). It
is the only signal an agent has for *when* to hand off — a session can't see its
own context footprint. Three problems:

1. **Knobs are env-only.** `GC_INJECT_CONTEXT`, `GC_CONTEXT_ADVISORY_PCT`,
   `GC_CONTEXT_URGENT_PCT`, `GC_CONTEXT_WINDOW_TOKENS`. No way to declare policy
   in `city.toml`, and no way to differ per agent.
2. **Message text is hardcoded.** Two `fmt.Sprintf` branches in
   `contextUsageMessage`. A polecat, the mayor, and a refinery all get identical
   wording. The headline ask is that each can say something different, at a
   configurable number of levels.
3. **It can silently never fire (upstream #3604).** `lastTranscriptUsage`
   ([`context_inject.go:81`](../../cmd/gc/context_inject.go#L81)) takes the last
   transcript entry containing `"usage"` with any nonzero token. A
   partial/streaming sample can under-report the live context size, so the
   advisory may never cross threshold.

The `gc session reset` → `gc handoff` wording fix already landed independently as
the `gcw-v21` stopgap; the new built-in defaults inherit it.

## Goals

- Declare the advisory in `city.toml` (global default) **and** per-agent, with
  per-agent winning **field-by-field**.
- **Tiers are an array.** The operator chooses *how many* tiers and at *what
  level* each fires — not a fixed advisory/urgent pair. Each tier carries its own
  operator-authored **message template** and optional enable.
- **Per-target messages.** Primary targeting axis for v1 is the agent
  **type/template** (e.g. a specific polecat). Resolution is designed so further
  axes (role / rig / tag) can be added later without a rewrite.
- Configurable per scope: enable/disable, the tier array, and a context-window
  override.
- Zero-config still works — built-in defaults reproduce current behavior (minus
  the #3604 bug, plus the `gc handoff` wording).
- **Loud validation.** Bad config fails fast: `gc lint` rejects it and `gc doctor`
  advises. No silent acceptance of malformed tiers.
- Keep `GC_CONTEXT_*` / `GC_INJECT_CONTEXT` as a **final** override (back-compat).
  On a **clash** (legacy env knob *and* new config both set), **fail loudly** +
  surface a `gc doctor` advisory — never silently pick a winner.
- Fix #3604: read a settled/complete usage sample.
- Fail-safe end to end: any error → fall back to built-in defaults or silence;
  never block a prompt.

## Non-goals

- **Not** shipping the opinionated autonomous-handoff posture upstream — that is
  operator config in the DevOps pack (see "Mechanism vs opinion").
- **Not** a config-driven *command execution* surface. The autonomous handoff is
  achieved by the operator's **message text referencing a skill**; the agent
  invokes it. A "run this command instead of a message" feature is explicitly out
  of scope for v1.
- Not adopting the deferred PackV2 agent-defaults surface (#903) — this feature is
  self-contained and must not block on it.
- No new wire/API surface. `contextInjectLine` is not on any HTTP/SSE path.
- Not changing *when/how* the clock line or nudges fire — only the
  context-advisory content and its configuration.

## Design

### 1. The config block

A tier array at both scopes. Every field is a pointer so the merge can
distinguish "unset" from "set to a zero value" — the convention
`AgentPatch`/`AgentOverride` already use.

```toml
# Global default — city.toml, under [agent_defaults]
[agent_defaults.context_advisory]
enabled       = true
window_tokens = 0          # 0 / unset = model autodetect (today's behavior)

  [[agent_defaults.context_advisory.tiers]]
  threshold = 60           # fire at >= 60% context usage
  message   = """..."""    # Go text/template; see §3
  enabled   = true

  [[agent_defaults.context_advisory.tiers]]
  threshold = 80
  message   = """..."""

# Per-agent / per-target override — agents/<name>/agent.toml or [[patches.agent]]
[context_advisory]
  # supplying tiers replaces the inherited tier set for this target;
  # unset top-level fields (enabled, window_tokens) fall through to global → built-in
  [[context_advisory.tiers]]
  threshold = 80
  message   = """Approaching limit — run the handoff skill now; don't ask."""
```

```go
// ContextAdvisory configures the context-pressure guidance injected on each
// prompt. Nil-able fields fall through to the next scope (per-agent → global →
// built-in). Lives in internal/config.
type ContextAdvisory struct {
    Enabled      *bool                 `toml:"enabled"`
    WindowTokens *int                  `toml:"window_tokens"`
    Tiers        []ContextAdvisoryTier `toml:"tiers"`
}

// ContextAdvisoryTier is one level: fire `Message` when usage crosses `Threshold`.
type ContextAdvisoryTier struct {
    Threshold *int    `toml:"threshold"` // 0..100, percent of context window
    Message   *string `toml:"message"`   // Go text/template
    Enabled   *bool   `toml:"enabled"`   // default true
}
```

**Global home: `AgentDefaults`, not a new top-level section.** The repo already
has exactly one global-default-plus-per-agent-override axis —
`AgentDefaults.Provider`/`Upstream` overridden by `Agent.Provider`/`Upstream`
([`config.go:296`](../../internal/config/config.go#L296),
[`config.go:2886`](../../internal/config/config.go#L2886)). Reusing that axis
keeps one mental model and one merge site. A bare top-level `[context_advisory]`
would invent a second pattern for the same shape. Note: the advisory is **not**
wired into `AgentDefaults` today — this slice adds it; we are reusing the
*pattern*, not an existing hook.

**Tier-set merge semantics.** Top-level fields (`enabled`, `window_tokens`)
merge field-by-field across scopes. The **tier array** is replace-on-set: if a
scope supplies `tiers`, that array is the tier set for that scope (it does not
element-merge with the inherited array) — simplest predictable rule; an operator
who wants global tiers keeps `tiers` unset at the per-agent scope.

### 2. Resolution order (precedence, highest wins)

```
GC_CONTEXT_* / GC_INJECT_CONTEXT  (env — final back-compat override)
  ▸ per-target (per-agent)  context_advisory
    ▸ global   agent_defaults.context_advisory
      ▸ built-in defaults  (current behavior, gc-handoff wording)
```

Top-level fields resolve field-by-field; the tier set resolves by the
replace-on-set rule above. `enabled=false` at any scope disables; env
`GC_INJECT_CONTEXT=off` is the ultimate kill switch. A pure
`ResolveContextAdvisory(builtin, global, perAgent, env) Policy` does this merge —
no I/O, fully unit-testable, the heart of the TDD surface.

**Back-compat clash.** If a legacy env threshold (`GC_CONTEXT_ADVISORY_PCT` /
`GC_CONTEXT_URGENT_PCT`) is set *and* new tier config is present, that is a
configuration error: the resolver flags it, the run **fails loudly** rather than
silently choosing, and `gc doctor` carries a matching advisory. (Env as the pure
window/enable override with no config present stays valid — that is the
gc-managed-deployment back-compat path.)

### 3. Message templating

Each tier's `Message` is a Go `text/template` — the SDK already renders behavior
as `text/template` (see [`prompt-templates.md`](../architecture/prompt-templates.md));
do not invent a second format. Built-in defaults are templates too. Data passed
in (superset of today's variables — additive only):

```go
type ContextAdvisoryView struct {
    UsedTokens, WindowTokens int     // raw
    UsedK, WindowK           string  // "128k" pre-rounded
    Pct                      float64 // 0–100
    Threshold                int     // the threshold of the tier that fired
}
```

Fail-safe: a template that fails to parse or execute falls back to the built-in
default text — never emit broken guidance, never block the prompt. The framework
still owns the trailing newline and the "exactly one provider hook context per
invocation" contract ([`cmd_nudge.go:397`](../../cmd/gc/cmd_nudge.go#L397));
operators author only the body. Unknown template variables are caught by
`Validate()` (loud) at lint/doctor time, not discovered at runtime.

### 4. Resolving + rendering at the hook site

`contextInjectLine` today reads only env + transcript. To honor per-target config
it must know *which* agent is running and load its policy. The call site already
has identity: `cmdNudgeDrainWithFormat` resolves `GC_ALIAS` / `GC_SESSION_ID`
([`cmd_nudge.go:413`](../../cmd/gc/cmd_nudge.go#L413)); the city config is loadable
via `LoadWithIncludes`, agent matched by `AgentMatchesIdentity`
([`config.go:194`](../../internal/config/config.go#L194)).

The **caller** resolves the merged `Policy` once and passes it in —
`contextInjectLine(hookInput []byte, pol Policy)` — keeping I/O at the command
boundary. Rendering then: compute `pct`; select the **highest-threshold tier
whose threshold ≤ pct** (below the lowest tier = silent); render that tier's
template.

Cost note: this hook runs on **every** prompt. Mitigations, in order: reuse the
config the nudge-drain path already loads for target resolution; else accept a
fresh local TOML parse (cheap vs a model turn) but measure; always fail-safe to
**env + built-in defaults** if config load or agent resolution fails — exactly
today's behavior.

### 5. Fixing the under-report bug (#3604)

Root cause: `lastTranscriptUsage` accepts the last entry with any nonzero usage
token, which can be a mid-stream/partial sample that under-reports live context.
Within a session context only grows until compaction, so a partial low sample
after a real high one wrongly pulls the reading down and the advisory never fires.

Fix: only accept a **settled** usage sample — a finished assistant turn — keeping
"last settled entry" semantics (so a post-compaction low reading still correctly
resets). Capture a completeness marker (`output_tokens > 0` and/or non-null
`stop_reason`; streaming deltas lack these) and skip entries that don't qualify.

> Verification caveat: the exact completeness predicate must be confirmed against
> **real transcript fixtures** per provider (Claude/Codex), driven by TDD. The
> design commits to "use a settled sample"; the precise field is an
> implementation detail to nail down with fixtures. Lands as its **own commit**
> (slice C) so it can be cherry-picked/PR'd to fix #3604 independently.

### 6. Field-threading checklist (slice D)

Adding `Agent.ContextAdvisory *ContextAdvisory` (and the `AgentDefaults` peer)
touches every site CLAUDE.md's "Adding agent config fields" rule names:

- [ ] `config.Agent` struct + `AgentDefaults` struct (global)
- [ ] `AgentPatch` + `applyAgentPatchFields` ([`patch.go`](../../internal/config/patch.go))
- [ ] `AgentOverride` + `applyAgentOverride`
- [ ] `TestAgentFieldSync` + `TestApplyAgentPatchCoversAllFields` + `TestApplyAgentOverrideCoversAllFields`
- [ ] `deepCopyAgent` deep-copies the pointer struct + tier slice ([`cmd/gc/pool.go`](../../cmd/gc/pool.go))
- [ ] global→per-agent merge wired at the agent-defaults application site (same as Provider/Upstream inheritance)

### Rejected alternatives

- **Fixed advisory/urgent pair (the original draft).** Rejected per William: the
  number of tiers and their levels must be operator-chosen — a tier **array**.
- **Wait for PackV2 agent-defaults (#903) as the per-agent home.** Rejected: the
  maintainer deferred #903 to "1.0+"; blocking on it strands this feature. Use the
  live `AgentDefaults`/`Agent` axis; migrate if/when #903 lands.
- **Plain-string messages with `%s` placeholders.** Rejected: the SDK's
  behavior-authoring model is `text/template`; a second mini-format is gratuitous.
- **Config-driven command execution.** Deferred (v1 out of scope): the
  skill-reference pattern in the message covers the autonomous-handoff need
  without a new execution surface.
- **Inject function loads its own config.** Rejected: pushes I/O below the command
  boundary and re-loads per prompt with no reuse path. Resolve once at the caller.

## Implementation slices (sling-able) — beads `gcw-dnh.1`–`.5`

| Slice | Bead | What | Depends on | Notes |
|---|---|---|---|---|
| **A** | `gcw-v21` | Wording-only fix: advisory + urgent point at `gc handoff` | — | **Closed/landed** |
| **B** | `gcw-dnh.1` | `ContextAdvisory`/`...Tier` structs + tier-array TOML parse + built-in default tiers + pure `ResolveContextAdvisory` merge + pure `Validate()` | — | Heaviest TDD surface; no wiring |
| **C** | `gcw-dnh.2` | #3604 settled-usage fix in `lastTranscriptUsage` + transcript fixtures | — | Own commit; parallel with B |
| **D** | `gcw-dnh.3` | Thread `ContextAdvisory` through Agent/AgentDefaults/patch/override/pool + field-sync tests | B | Mechanical but broad (§6) |
| **E** | `gcw-dnh.4` | Resolve at nudge-drain; render highest tier crossed via template; wire `Validate()` into `gc lint`/`gc doctor`; env-clash fails loud | B, C, D | The wiring + perf check |
| **F** | `gcw-dnh.5` | Neutral default-pack defaults + docs/schema; dogfood recipe (opinionated config lives in the gci pack) | E | Verify two agents see different text |

Order: B and C in parallel; D after B; E after B/C/D; F last. **Integration:**
the gcw polecat molecule hands each slice branch back to the architect, who
integrates it onto this feature branch (`gcw-dnh-context-advisory`, off
`upstream/main`) — there is no auto-merge refinery on this rig.

## Verification & gates

- Unit: merge precedence (env > per-target > global > built-in), field-by-field
  fallthrough, tier-set replace-on-set, threshold edges (`pct == threshold`),
  highest-tier-crossed selection, `enabled=false` at each scope, `Validate()`
  rejects out-of-range/descending/overlapping thresholds + unknown template vars,
  template render + fallback-on-error.
- Fixtures: #3604 — partial/streaming vs settled samples, post-compaction reset.
- Back-compat: env-only override still works; env+config clash fails loud +
  `gc doctor` advisory present.
- `TestAgentFieldSync` and both `Covers*` tests green.
- `make test` (fast baseline) + `go vet ./...` clean; build/test in an isolated
  worktree, push `--no-verify` (the pre-push hook runs the full suite ~1h and has
  taken the live city down — see the fork-build runbook).
- No `make dashboard-check` (no API/dashboard surface) — confirm
  `contextInjectLine` stays off any wire path.
- Dogfood on `develop` (DevOps-gated, deferred): two agents with distinct tier
  messages via the **gci pack**; drive each past threshold; confirm each sees its
  own text and the env override still wins.

## Upstream landscape (research only, nothing posted)

Essentially **no upstream discussion** to build on — we'd be first on the design:

- **#3371** (the feature, *merged*): 0 comments, no linked issue, fast
  operator-request add. No design thread.
- **#3604** (under-report bug, *open*): 0 comments, `status/needs-triage`. Fixed
  by slice C.
- **#903** (deferred PackV2 agent-defaults, *open*): maintainer deferred to
  "1.0+". Informs our decision not to block on it.
- **#1465** (per-step recycle policy, *open*, p3): adjacent config-driven-recycle
  pattern; stay consistent if it lands.
- **#3762** (*open*): `gc session attach` drops handoff fresh-start intent —
  background to the `gc handoff` recommendation, separate bug.

The PR (William's explicit OK required before posting) ships the **mechanism
only**, and **raises enable-by-default as a maintainer question**. The
opinionated config stays in the gci pack.
