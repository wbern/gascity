# `gc bd` controller-read experiment — HISTORICAL

**Status: RETIRED. The controls below no longer exist.** The experiment ran
from 2026-07-19 to 2026-08-02 and is concluded. `internal/bdexperiment` and
`cmd/bdexperiment-report` are deleted, and the `gc bd` fastpath that consumed
them was removed by `dd786179e` ("retire the in-process bd fastpath and route
every `gc bd` through the shim").

**Do not set `GC_BD_EXPERIMENT_ARMS`, `GC_BD_EXPERIMENT_GENERATION`,
`GC_BD_EXPERIMENT_FORCE_ARM`, `GC_BD_EXPERIMENT_SHAPE_OVERRIDES` or
`GC_BD_EXPERIMENT_LOG`.** No `gc` build after `dd786179e` reads any of them.
They are inert, not a rollback lever: on a current binary every `gc bd` verb
goes to the managed shim unconditionally, and there is no second arm to weight.
A long-lived session that still exports them (see `gcw-iph4`) is carrying dead
configuration, not a live override.

This page is kept because it records what the arm controls meant, which the
observations in `.gc/bd-experiment.jsonl` cannot be read without.

## What it measured, and the window it did not cover

The experiment compared only five controller-read shapes that already passed
the trusted managed-`bdshim` early-path gate:

- `show <id> --json`
- JSON `list` without metadata predicates
- the ephemeral JSON `query` shape
- `mol current <id>`
- `mol progress <id>`

`ready` (later added), all writes and claims, explicit `--city`/`--rig`,
ambient Dolt overrides, metadata-filtered lists, and every passthrough/refuse
shape bypassed the experiment and retained their existing paths.

Each observation carried two timings, `main_ms` and `dispatcher_ms`, both taken
**inside an already-running `gc` process**. Neither included process spawn, Go
runtime init, or paging in a 127 MB binary. The original runbook said so
explicitly — "its timings are not subprocess wall time" — and that limit is why
the arm log could not settle the question the epic ultimately asked. On the
shapes where the two arms differ at all, the in-process gap is 10–18 ms, while
the wall-clock gap the same change produces is measured in hundreds of
milliseconds and points the other way once `gc`'s own startup is counted
(`dd786179e`: `gc bd query --json ephemeral=true` 59–118 ms → 526–714 ms).
Wall-clock evidence lives in `bd-shim-retirement-findings.md`; this harness
never produced any.

## Final results

Produced with `bdexperiment-report` against `~/gc2/.gc/bd-experiment.jsonl`
immediately before deleting the tool: 1,208 observations, arms shim 667 /
direct 541, split per build because the report rejects mixed-build artifacts.
`p50`/`p95` are `main_ms`; see the window caveat above.

| build | shape | arm | n | success | p50 | p95 |
| --- | --- | --- | ---: | ---: | ---: | ---: |
| `fork-ed97e7965` | `query_ephemeral` | direct | 33 | 100% | 3 | 6 |
| `fork-ed97e7965` | `query_ephemeral` | shim | 33 | 100% | 13 | 22 |
| `fork-ed97e7965` | `ready_json` | direct | 112 | 100% | 239 | 292 |
| `fork-ed97e7965` | `ready_json` | shim | 76 | 100% | 257 | 303 |
| `fork-e1636a6c8` | `show_json` | direct | 38 | 100% | 7 | 96 |
| `fork-e1636a6c8` | `show_json` | shim | 503 | 100% | 27 | 197 |
| `fork-e1636a6c8` | `list_json` | direct | 2 | 100% | 36 | 36 |
| `fork-e1636a6c8` | `list_json` | shim | 31 | 100% | 123 | 482 |
| `fork-c45bd6783` | `query_ephemeral` | direct | 14 | 100% | 19 | 162 |
| `fork-c45bd6783` | `query_ephemeral` | shim | 18 | 100% | 61 | 164 |
| `dev` | `query_ephemeral` | direct | 59 | 100% | 9 | 29 |
| `dev` | `ready_json` | direct | 280 | 100% | 241 | 501 |
| `dev` | `ready_json` | shim | 5 | 100% | 301 | 309 |

Two conclusions the log does support:

1. **Error parity is clean.** Success rate is 1.000 on every arm, every shape,
   every build — 1,208 observations, zero non-zero exits. The in-process arm
   never produced a correctness or error regression, so the rollback gate the
   ladder below was built to catch never fired.
2. **The in-process saving is real but small and confined.** It is largest on
   `query_ephemeral`, whose controller work is negligible, and it shrinks to
   ~7% on `ready_json`, the shape that actually dominates agent traffic.

## What the retired controls meant

The process-global environment controls were intentionally fail-closed:

```sh
GC_BD_EXPERIMENT_ARMS='shim=100,direct=0,legacy=0'
GC_BD_EXPERIMENT_GENERATION=1
```

Weights had to name `shim`, `direct` and `legacy`, total 100, and keep legacy
at or below 10; invalid settings selected `shim=100`. `GC_BD_EXPERIMENT_FORCE_ARM`
was a temporary exact-arm diagnostic override and
`GC_BD_EXPERIMENT_SHAPE_OVERRIDES=show_json=direct` pinned a known shape.
`GC_BD_EXPERIMENT_GENERATION` was, at first, only stamped onto each record; a
late fix (`d0e8c6b5d`) made it authoritative so that an inherited weighting
declaring a generation older than the binary's own was superseded rather than
honoured. That fix is deleted with the package — on a current binary there is
no weighting left to supersede.

The rollout ladder was `100/0/0`, then `95/5/0`, then `45/45/10`, advancing only
after test and live-read gates were green, comparing arms within one shape and
build. The live fleet reached rung two: sessions exporting
`shim=95,direct=5,legacy=0` were at a legitimately-set stage, not a corrupted
one — they simply outlived the experiment.

## Recovering the harness

The selector, the observation writer and the report tool are in git history:

```sh
git log --diff-filter=D --oneline -- internal/bdexperiment
git show <sha>^:internal/bdexperiment/summary.go
```

The raw observations are outside the repo, at `.gc/bd-experiment.jsonl` in the
city (`~/gc2/.gc/bd-experiment.jsonl` on the machine that ran it), and are not
affected by this deletion. Note the retirement lesson before restoring it: the
randomized per-call assignment was the right *method* (see
`bd-shim-retirement-findings.md` §3), but it must be instrumented at
subprocess wall time, not inside `gc`'s own process, or it measures the wrong
window again.
