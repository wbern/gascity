# Pluggable, config-driven context-pressure advisory

| Field | Value |
|---|---|
| Status | Draft — for William's review |
| Date | 2026-06-29 |
| Author(s) | Architect (gas-city-wbern), William |
| Bead | `gcw-dnh` (fork) — stopgap `gcw-v21` blocks it |
| Upstream issue | gastownhall/gascity#3810 (filed 2026-06-29) |
| Relates | upstream #3371 (shipped feature), #3604 (under-report bug), #903 (deferred PackV2 agent-defaults), #1465 (per-step recycle policy) |
| Supersedes | the env-var-only knobs in `cmd/gc/context_inject.go` |

Make the context-pressure advisory a first-class Gas City feature: declared in
config at **global (city)** and **per-agent** scope, with operator-authored
**message text per tier**, sane zero-config defaults, and the known
under-report bug fixed. Today it is "too Gas Town, not enough Gas City" —
env-var-only knobs and a hardcoded string.

## Problem

[`cmd/gc/context_inject.go`](../../cmd/gc/context_inject.go) injects one line of
context-pressure guidance into each `UserPromptSubmit` hook payload (folded
with the clock line at
[`cmd/gc/cmd_nudge.go:406`](../../cmd/gc/cmd_nudge.go#L406)). It is the only
signal an agent has for *when* to trigger a handoff — a session can't see its
own context footprint. Three problems:

1. **Knobs are env-only.** `GC_INJECT_CONTEXT`, `GC_CONTEXT_ADVISORY_PCT`,
   `GC_CONTEXT_URGENT_PCT`, `GC_CONTEXT_WINDOW_TOKENS`
   ([`context_inject.go:39-41`](../../cmd/gc/context_inject.go#L39)). No way to
   declare policy in `city.toml`, and no way to differ per agent.
2. **Message text is hardcoded.** Two `fmt.Sprintf` branches
   ([`context_inject.go:179-185`](../../cmd/gc/context_inject.go#L179)). A
   polecat, the mayor, and a refinery all get the identical wording. The
   headline ask is that each can say something different per tier.
3. **It can silently never fire (upstream #3604).** `lastTranscriptUsage`
   ([`context_inject.go:81`](../../cmd/gc/context_inject.go#L81)) takes the last
   transcript entry containing `"usage"` with any nonzero token. A
   partial/streaming sample can under-report the live context size, so the
   advisory may never cross threshold.

Wording is also wrong (the `gcw-v21` stopgap): the urgent tier tells agents to
`gc session reset` themselves, which carries no continuation note and isn't
attended-safe. `gc handoff` is the correct command. That fix is folded into the
defaults here but lands independently first.

## Goals

- Declare the advisory in `city.toml` (global default) **and** per-agent, with
  per-agent winning field-by-field.
- Operator-authored **message text per tier** (advisory / urgent) at both
  scopes.
- Configurable per scope: enable/disable, `advisory_pct`, `urgent_pct`,
  context-window override, and the two message templates.
- Zero-config still works — built-in defaults reproduce current behavior minus
  the bugs.
- Keep `GC_CONTEXT_*` / `GC_INJECT_CONTEXT` as a **final** override (back-compat
  for gc-managed deployments that pin the window).
- Fix #3604: read a settled/complete usage sample.
- Fail-safe preserved end to end: any error → fall back to built-in defaults or
  silence; never block a prompt.

## Non-goals

- Not adopting the deferred PackV2 agent-defaults surface (#903) — see the
  decision below; this feature is self-contained and must not block on it.
- No new wire/API surface. This is a CLI hook + config feature;
  `contextInjectLine` is not on any HTTP/SSE path.
- Not changing *when/how* the clock line or nudges fire — only the
  context-advisory content and its configuration.

## Design

### 1. The config block

A single shared struct, used at both scopes. Every field is a pointer so the
merge can distinguish "unset" from "set to a zero value" — the same convention
`AgentPatch`/`AgentOverride` already use.

```toml
# Global default — city.toml, under [agent_defaults]
[agent_defaults.context_advisory]
enabled       = true
advisory_pct  = 60
urgent_pct    = 80
window_tokens = 0          # 0 / unset = model autodetect (today's behavior)
advisory_message = """..."""   # Go text/template; see §3
urgent_message   = """..."""

# Per-agent override — agents/<name>/agent.toml (PackV2) or [[patches.agent]]
[context_advisory]
urgent_message = """Mayor: stop planning, write your handoff bead, run `gc handoff`."""
# unset fields fall through to the global default, then to built-ins
```

```go
// ContextAdvisory configures the context-pressure guidance injected on each
// prompt. Nil-able fields fall through to the next scope (per-agent → global
// → built-in). Lives in internal/config.
type ContextAdvisory struct {
    Enabled         *bool   `toml:"enabled"`
    AdvisoryPct     *int    `toml:"advisory_pct"`
    UrgentPct       *int    `toml:"urgent_pct"`
    WindowTokens    *int    `toml:"window_tokens"`
    AdvisoryMessage *string `toml:"advisory_message"`
    UrgentMessage   *string `toml:"urgent_message"`
}
```

**Global home: `AgentDefaults`, not a new top-level section.** The repo already
has exactly one global-default-plus-per-agent-override axis —
`AgentDefaults.Provider`/`Upstream` overridden by `Agent.Provider`/`Upstream`
([`config.go:296`](../../internal/config/config.go#L296),
[`config.go:3049`](../../internal/config/config.go#L3049)). Reusing that axis
keeps one mental model and one merge site. A bare top-level `[context_advisory]`
would invent a second pattern for the same shape.

### 2. Resolution order (precedence, highest wins)

```
GC_CONTEXT_* / GC_INJECT_CONTEXT  (env — final back-compat override)
  ▸ per-agent  context_advisory
    ▸ global   agent_defaults.context_advisory
      ▸ built-in defaults  (current behavior, gc-handoff wording)
```

Field-by-field, not whole-struct: a per-agent block that sets only
`urgent_message` inherits global/built-in thresholds. `enabled=false` at any
scope disables; env `GC_INJECT_CONTEXT=off` is the ultimate kill switch.

A pure `ResolveContextAdvisory(builtin, global, perAgent, env) Policy` function
does this merge — no I/O, fully unit-testable, the heart of the TDD surface.

### 3. Message templating

The message becomes a Go `text/template` (consistent with prompt-templates —
the SDK already renders behavior as `text/template` in Markdown,
[`prompt-templates.md`](../architecture/prompt-templates.md)). Built-in defaults
are templates too. Data passed in:

```go
type ContextAdvisoryView struct {
    UsedTokens, WindowTokens int     // raw
    UsedK, WindowK           string  // "128k" pre-rounded
    Pct                      float64 // 0–100
    Tier                     string  // "advisory" | "urgent"
}
```

Fail-safe: a template that fails to parse or execute falls back to the built-in
default text for that tier — we never emit broken guidance and never block the
prompt. The framework still owns the trailing newline and the
"exactly one provider hook context per invocation" contract
([`cmd_nudge.go:397`](../../cmd/gc/cmd_nudge.go#L397)); operators author only the
body.

### 4. Resolving config at the hook site

This is the real plumbing change. `contextInjectLine` today reads only env +
transcript. To honor per-agent config it must know *which* agent is running and
load its policy. The call site already has identity:
`cmdNudgeDrainWithFormat` resolves `GC_ALIAS` / `GC_SESSION_ID`
([`cmd_nudge.go:413-419`](../../cmd/gc/cmd_nudge.go#L413)), and the city config
is loadable via `LoadWithIncludes`, with the agent matched by
`AgentMatchesIdentity` ([`config.go:194`](../../internal/config/config.go#L194)).

Plan: the **caller** resolves the merged `Policy` once and passes it in —
`contextInjectLine(hookInput []byte, pol Policy)` — rather than
`contextInjectLine` reaching for config itself. This keeps the I/O at the
command boundary and the inject function pure-ish (still reads the transcript).

Cost note: this hook runs on **every** prompt. Loading + composing the full city
config per prompt is the one performance risk. Mitigations, in order of
preference:
- Resolve the policy in the nudge-drain path that *already* touches config for
  target resolution, and reuse it (confirm during impl whether
  `resolveNudgeTarget` already loads it).
- If a fresh load is unavoidable, accept it (TOML parse of the local city is
  cheap relative to a model turn) but measure.
- Fail-safe: if config load fails or the agent can't be resolved, fall back to
  **env + built-in defaults** — i.e. exactly today's behavior. Zero-config and
  broken-config paths both keep working.

### 5. Fixing the under-report bug (#3604)

Root cause: `lastTranscriptUsage` accepts the last entry with any nonzero usage
token, which can be a mid-stream/partial sample that under-reports the live
context size. Within a session the context only grows until compaction, so a
partial low sample after a real high one wrongly pulls the reading down and the
advisory never fires.

Fix: only accept a **settled** usage sample — a finished assistant turn — and
keep "last settled entry" semantics (so a post-compaction low reading still
correctly resets). Concretely, extend the parsed entry to capture a
completeness marker (`output_tokens > 0` and/or a non-null `stop_reason`;
streaming deltas lack these) and skip entries that don't qualify.

> Verification caveat: the exact completeness predicate must be confirmed
> against **real transcript fixtures** for each provider (Claude/Codex), driven
> by TDD. The design commits to "use a settled sample"; the precise field is an
> implementation detail to nail down with fixtures, not an assumption to ship
> blind.

### 6. Field-threading checklist

Adding `Agent.ContextAdvisory *ContextAdvisory` touches every site CLAUDE.md's
"Adding agent config fields" rule names (confirmed by exploration):

- [ ] `config.Agent` struct ([`config.go:2995`](../../internal/config/config.go#L2995))
- [ ] `AgentDefaults` struct (global) ([`config.go:2886`](../../internal/config/config.go#L2886))
- [ ] `AgentPatch` + `applyAgentPatchFields` ([`patch.go:23`](../../internal/config/patch.go#L23), [`patch.go:430`](../../internal/config/patch.go#L430))
- [ ] `AgentOverride` + `applyAgentOverride` ([`config.go:645`](../../internal/config/config.go#L645), [`pack.go:2789`](../../internal/config/pack.go#L2789))
- [ ] `TestAgentFieldSync` + `TestApplyAgentPatchCoversAllFields` + `TestApplyAgentOverrideCoversAllFields` ([`field_sync_test.go`](../../internal/config/field_sync_test.go))
- [ ] `deepCopyAgent` deep-copies the pointer struct ([`cmd/gc/pool.go:248`](../../cmd/gc/pool.go#L248))
- [ ] global→per-agent merge wired in agent-defaults application (same site as Provider/Upstream inheritance)

### Rejected alternatives

- **Wait for PackV2 agent-defaults (#903) as the per-agent home.** Rejected: the
  maintainer explicitly deferred #903 to "1.0+" to avoid risk near 1.0
  (issue #903 comment). Blocking on it strands this feature. We use the existing
  `AgentDefaults`/`Agent` axis, which is live today; if/when #903 lands, the
  block migrates with everything else.
- **Plain-string messages with `%s` placeholders.** Rejected: the SDK's whole
  behavior-authoring model is `text/template`; a second mini-format is
  gratuitous. Templates also give operators named fields and conditionals.
- **Inject function loads its own config.** Rejected: pushes I/O below the
  command boundary and re-loads per prompt with no reuse path. Resolve once at
  the caller.

## Implementation slices (sling-able)

| Slice | What | Depends on | Notes |
|---|---|---|---|
| **A** `gcw-v21` | Wording-only fix: advisory + urgent point at `gc handoff` | — | Already fully spec'd; land independently now |
| **B** | `ContextAdvisory` struct + TOML parse + built-in default templates + pure `ResolveContextAdvisory` merge | — | Heaviest TDD surface; no wiring |
| **C** | #3604 settled-usage fix in `lastTranscriptUsage` + transcript fixtures | — | Independently valuable; parallel with B |
| **D** | Thread `Agent.ContextAdvisory` through struct/patch/override/pool + field-sync tests | B | Mechanical but broad (checklist §6) |
| **E** | Resolve policy at nudge-drain call site; pass into `contextInjectLine`; render via templates | B, C, D | The wiring + perf check |
| **F** | Default-pack defaults + docs; dogfood distinct per-agent messages on develop | E | Verify two agents see different text |

Suggested order: A anytime; B and C in parallel; D after B; E after B/C/D; F last.

## Verification & gates

- Unit: merge precedence (env > per-agent > global > built-in), field-by-field
  fallthrough, threshold edges (`pct == advisory`, `== urgent`), `enabled=false`
  at each scope, template render + fallback-on-error.
- Fixtures: #3604 — partial/streaming vs settled samples, and a post-compaction
  reset reads low.
- `TestAgentFieldSync` and the two `Covers*` tests green.
- `make test` (fast baseline) + `go vet ./...` clean; `.githooks/pre-commit`
  active.
- No `make dashboard-check` needed (no API/dashboard surface) — confirm
  `contextInjectLine` stays off any wire path.
- Dogfood on develop: two agents with distinct `urgent_message`; drive each past
  threshold; confirm each sees its own text and env override still wins.

## Upstream landscape (for William — research only, nothing posted)

There is essentially **no upstream discussion** to build on — we'd be first on
the design:

- **#3371** (the feature, *merged*): **0 comments**, no linked issue, labeled a
  fast operator-request add. No design thread exists.
- **#3604** (under-report bug, *open*): **0 comments**, `status/needs-triage`.
  This design fixes it (slice C).
- **#903** (deferred PackV2 agent-defaults, *open*): **1 comment** — maintainer
  deferred it to "1.0+" ("instead of increasing risk by adding these so close to
  1.0"). Directly informs our decision **not** to block on it.
- **#1465** (per-step recycle policy `process|conversation|none`, *open*, p3):
  **0 comments**. Adjacent config-driven-recycle pattern worth staying
  consistent with if it ever lands.
- **#3762** (*open*): `gc session attach` drops handoff fresh-start intent —
  handoff-correctness, relevant background to the `gc handoff` recommendation
  but a separate bug.
- **#3799**, **#81** (*both closed*): handoff/session-reset fresh-start
  mechanics; closed, no live thread.

If you later want it, I can draft an upstream issue ("make the context advisory
pluggable + config-driven") and a PR description against `gastownhall/gascity` —
held locally until you say post.
