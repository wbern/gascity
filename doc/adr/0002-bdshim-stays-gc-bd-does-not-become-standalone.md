---
status: accepted
date: 2026-08-08
applies_to:
  - "cmd/bdshim/**/*.go"
  - "internal/bdshim/**/*.go"
  - "internal/bddispatch/**/*.go"
  - "cmd/gc/cmd_bd.go"
  - "cmd/gc/bd_*.go"
pre_filter:
  - "bdshim"
  - "bddispatch"
  - "bd_fastpath"
  - "LookPath"
---

# 2. bdshim stays; `gc bd` does not become standalone

**Ratified by William on 2026-08-08.** The decision was already taken in code,
in `b38447716`; what was missing was the record, not the choice. This ADR was
filed as `proposed` so it could be ratified or reversed explicitly, and it was
ratified — the epic `gcw-yr0o` had been blocked for weeks on a decision nobody
had written down, not on any technical obstacle.

## Context

Epic `gcw-yr0o` — "retire bdshim, `gc bd` becomes the only bd" — has been open
at P1 with seven children. Investigation on 2026-08-08 established that the
fleet has moved the opposite way, and that the epic's stated blockers are not
the real ones.

Measured, with controls, on `fork-b31dbd713`:

- **`gc bd` execs a `bd` from PATH.** A logging wrapper placed first on PATH
  saw every verb tried (`show`, `list`, `ready`, `dep tree`, `stats`,
  `--help`) exec'd verbatim. `cmd/gc/cmd_bd.go:373` is a bare
  `exec.LookPath("bd")`, and PATH is fronted by `.gc/shimbin/bd`.
  (A `bdshim.log` line-delta oracle was tried first and is **unsound** on a
  live fleet — background traffic contributes ~1 record/sec at busy times, so
  the delta is confounded. The wrapper is the sound oracle.)
- The dependency is on *a* `bd`, not on bdshim specifically:
  `cmd/gc/bd_shimbin.go:190-199` already falls back to real bd, and with
  `.gc/shimbin` stripped for one command `gc bd show` returned rc=0 and
  byte-identical output.
- **`b38447716` deleted `cmd/gc/bd_fastpath.go`**, the in-process path that
  would have made `gc` standalone. `git merge-base --is-ancestor b38447716
  develop` → rc=0 (control: inverse → rc=1); `ls cmd/gc/bd_fastpath.go` → rc=1
  (control: `cmd_bd.go` → rc=0).

Three arguments previously offered for retirement do not survive measurement:

- **Scale.** The benefit is ~2,380 prod lines, not 5,229: 2,766 of the 5,229
  are tests, `internal/bdflags` (850) survives, `pr_gate.go` relocates rather
  than goes, and `routelog.go` is a rewrite.
- **Speed.** On a quiet box with CPU timing, removing the shim from `gc bd` is
  free (0.460/0.260 vs 0.450/0.260 s), and bare `bd` through the shim costs a
  ~20 ms tax over real bd. Retirement is a small win, not a loss — an earlier
  "measured performance gain: negative" claim rested on wall-clock timings
  taken on a loaded box and is withdrawn.
- **Capability.** `bd ready` through the shim federates (563 beads across
  `crm`, `gci`, `gcw`, `gc2`, `els`) while real bd returns 100, own-rig only.
  This looked like a hard capability gate. It is not: `bdCommandEnv`
  (`cmd/gc/cmd_bd.go:146-170`) sets `GC_STORE_SCOPE`, and
  `pinnedNonCityStoreScope()` (`cmd/bdshim/main.go:114,:356`) forces
  passthrough for any non-city scope — so `gc bd --city <path> ready` returns
  563 across all five prefixes today. Federation is a **scope choice**, and it
  lives server-side in `internal/api/huma_handlers_beads.go:493-585`; the shim
  is only an HTTP client for it.

What remains is therefore not technical. It is that the direction was reversed
in code and never ratified.

## Decision

bdshim remains the `bd` on PATH. `gc bd` continues to reach `bd` by exec rather
than in-process, and `cmd/gc/bd_fastpath.go` is not reinstated.

Epic `gcw-yr0o`'s headline goal is closed. Its still-valuable children —
cheap controller reach (`.1`, rescoped), routed show/list field coverage
(`.3`), gc's own bd self-calls (`.5`, re-baselined), and per-candidate Dolt
store opens (`.6`) — are reparented under an efficiency-and-correctness epic
and pursued on their own merits. `.6` first: largest measured cost
(`resolve_scope` at 153.9/159.1/173.7 ms on ID-bearing reads), smallest blast
radius, no dependency on this ADR.

## Consequences

Anyone reinstating an in-process bd path in `cmd/gc` is reversing a recorded
decision and should supersede this ADR rather than land it quietly — which is
precisely what did not happen in `b38447716` and is why this ADR exists.

Retirement remains *possible*. If it is ever revisited, the real gate is
pointing `gc` at the federation endpoint `gc` already hosts, plus preserving
the shim's traffic telemetry (`gcw-yr0o.4`). Neither is hard; neither is done.

Keeping the shim keeps its defects. `gcw-cdgj` is open: `bd ready` advertises
563 beads and `bd show` then reports "no issue found" for the ~404 that are not
`gcw-`, in the exact wording `AGENTS.md` says means "you used the wrong tool" —
except here the same binary supplied the ID. That is a consequence of this
decision and is owned by it.

An earlier revision of this analysis recorded "capability REGRESSION" and
"MEASURED PERFORMANCE GAIN: NEGATIVE". Both are struck; see Context.
