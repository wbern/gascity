# Fork consolidation plan — perf, bloat, upstream alignment

**Date:** 2026-07-18 · **Author:** gas-city-wbern/architect · **Driver:** William
("drive it all home; align closer to upstream; remove bloat we don't want to
maintain; keep what's useful, don't be too harsh")

The city degraded slowly since the fork began — felt even with zero polecats.
Root-caused to **two** costs, plus an accumulated **upstream-divergence tax**.
This plan is the synthesis; each lane has a bead.

## Root cause of the felt slowness (two things, both measured)

1. **Every `bd`/`gc`/order call cold-starts a 235 MB binary** (~1.1 CPU-sec, ~8 B
   instructions to boot), then does a tiny op and exits. At ~30 calls/min that's
   ~0.5–1 core burned continuously with no polecats.
2. **~282 order firings/hr** on tight 2–5 m timers, each running a script that
   cold-starts `gc` several times — the dominant source of the call volume.

The remote→local Dolt cutover is the hinge: it made Dolt connections cheap
(~60 ms loopback), which **quietly inverted the bd-shim's value** — the shim
solved a *remote*-Dolt problem that no longer exists, while adding a full gc
cold-start per call (a *double* cold-start on passthrough).

## The decisive lever (verified): `CGO_ENABLED=0` → 235 MB → 118 MB

**Bead `gcw-aa9r` (P1).** The 235 MB is **not** embedded assets (all `//go:embed`
< 3 MB) — it's the embedded Dolt/MySQL engine + cloud SDKs pulled through
`steveyegge/beads` under `//go:build cgo`. Verified: `CGO_ENABLED=0 go build
./cmd/gc` compiles cleanly → **118 MB (–50 %)**; `internal/{beads,dispatch,
session}` tests pass under CGO=0. CGO=0 keeps Dolt **server-mode** (what the
fleet actually uses via the loopback sql-server) and drops only the in-process
embedded engine (dead weight; the native reader is behind the off-by-default
`gascity_native_beads` tag). **~Halves cold-start on every invocation — shim or
not, order or not.** This single change does the heavy lifting.

> **The one decision for William:** is the fork's "DoltLite-backed beads store"
> direction **server-mode** (→ CGO=0 is aligned, keep a separate cgo/native
> target for future embedded work) or **in-process embedded** (→ CGO stays)?
> The live fleet is server-mode today, so CGO=0 matches reality.

## Lane 2 — cut call volume (order thinning)

**Bead `gcw-zgyu` (P1, → devops).** Right-size tight cadences: **283 → ~189
firings/hr (~33 % fewer)**. Highest-value single edit: `gc2-status-truth-watchdog`
3 m→10 m (its script cold-starts `gc` ~33×/tick). Correction from the audit:
remote-Dolt retirement mooted only `dolt-remotes-patrol` (already masked); the
other Dolt watchdogs guard the *local* sql-server — keep. Safety/SLA orders
(routed-work-backstop, reaper, claim-health, disk-space) untouched. Config-only,
reversible, live↔template 1:1.

## Lane 3 — bd-shim keep-vs-drop (measure, don't guess)

**Bead `gcw-wev0` (P2).** On local Dolt the shim is plausibly net-negative
(passthrough pays a wasted 235 MB cold-start then execs bd.real anyway). But
CGO=0 halves that, changing the math. Sequence: **slim first → measure with the
now-live disposition telemetry → decide.** Dropping is expensive (11 commits;
warm-claim woven into `internal/beads/*` + 9 API endpoints + generated openapi),
so it only pays if measurement says net-negative *even slimmed*. Likely outcome:
keep a slimmed shim for genuinely-routed hot verbs, stop shimming
passthrough-dominant ones (`context` 50 % is already cache-targeted by `gcw-40jw`).

## Lane 4 — upstream alignment (shrink the merge tax)

**Bead `gcw-dt3x` (P2).** develop is 128 ahead / 48 behind; 121 fork-unique
commits (doubled, driven by bd-shim + warm-claim). Nothing patch-dedupes, so the
mergedown is a real 3-way merge. Sequence: drop the self-cancelling revert pair
(`d793c61a1`+`ba967e3a2`) and ~8 RETIRE-NOW-OBSOLETE conflict-prone patches →
merge `upstream/main` (42 already-merged PRs take upstream) → chase the
KEEP-PENDING upstream PRs (#4137, #3279, #3954) → batch CONTRIBUTE-UPSTREAM
cleanups. Keep T3 bridge, DoltLite/beads resilience, deps/build.

## Recommended execution order

1. **Now / zero-risk:** apply `gcw-zgyu` order thinning (devops) — immediate
   ~33 % relief. Already merged: PR #37 durability (pre new city).
2. **Decide + land `gcw-aa9r` (CGO=0)** — the ~50 % cold-start cut. Needs
   William's server-vs-embedded call, then full-suite CGO=0 validation + a
   devops-owned validated bounce. **Biggest lever.**
3. **Measure `gcw-wev0`** (shim net cost) once slimmed — then keep/trim/drop.
4. **`gcw-dt3x` mergedown** on a cleanup branch — drop obsolete, merge upstream,
   validate openapi + suite, coordinate the bounce.

## What we keep (not too harsh)

T3 bridge, DoltLite/beads resilience + the bd-context cache (`gcw-40jw`), the
disposition telemetry (`gcw-j8oq.1` — now the instrument that makes lane 3
measurable), deps/build. The bd-shim is not deleted reflexively — it's measured;
CGO=0 may rescue its economics. The point isn't to undo the fork; it's to stop
paying for a premise (remote Dolt) that's gone and for weight (embedded engine,
tight timers, obsolete patches) we don't use.
