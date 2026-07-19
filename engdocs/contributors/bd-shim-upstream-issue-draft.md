# Upstream discussion issue

> Posted: **gastownhall/gascity#4441**
> (https://github.com/gastownhall/gascity/issues/4441). Numbers are from live
> gc2, 2026-07-19. Source text below.

---

**Title:** Proposal: route hot `bd` verbs to the warm controller via a tiny thin client (8× faster point lookups)

## Context

Gas City deliberately has agents reach subsystems through their CLIs rather than
registered tools or MCP — per the project's own principle, *"No MCP/tool
registration — if a tool has a CLI, the agent uses it."* The beads work ledger
follows that model: agents drive work by calling `bd`, and order/gate condition
scripts shell out to it too. A healthy consequence is that `bd`'s per-invocation
cost is a first-class performance concern by design — every agent action and
gate evaluation pays it.

Each `bd` invocation is a fresh, standalone process. For the cache-servable read
verbs it does that work from cold — even though a warm `gc` controller is already
running and holds the answer in memory. Plain `bd` simply doesn't use it. That is
the opportunity: route the hot verbs to the already-warm controller instead of
recomputing them per call.

## What we built

`bdshim` — a small, pure-Go, `bd`-CLI-compatible front end that:

- **Routes** the cache-servable verbs (`show`, `list`, `query`, `update
  --claim`) through the already-warm controller HTTP API (the same endpoints, so
  output is byte-identical), and
- **Passes through** everything else by exec'ing the real `bd` (`GC_BD_REAL`)
  with the caller's env/cwd.
- Is **opt-in / default-off** (a `[session] bd_shim = auto|on|off` toggle) and
  fronted on the agent's PATH.
- **Passthrough is always byte-identical** to raw `bd`; routing is a pure latency
  optimization, never a correctness requirement. Controller-down → routed reads
  fail loud (rc=1) rather than silently returning a wrong/empty local answer.

## Measured wins (live gc2, warm controller, n=25/verb)

`bdshim` vs the real `bd`:

| verb              | wall (bdshim / bd) | CPU (bdshim / bd) | result                                              |
| ----------------- | ------------------ | ----------------- | --------------------------------------------------- |
| `show <id>`       | **14.5 / 117 ms**  | **7.2 / 102 ms**  | 🟢 **routed — 8.1× faster wall, ~14× less CPU**     |
| `list --status …` | 126 / 120 ms       | 117 / 109 ms      | ⚪ passthrough — identical to real `bd`             |
| `ready`           | 463 / 123 ms       | 9.4 / 108 ms      | 🟡 routed but slower — federated round-trip (separate lever) |

`show <id>` is the point-lookup verb agents run most, so the 8.1× is the win that
matters in practice. Binary footprint: bdshim **7.7 MB** vs the real `bd` **187
MB**.

## The lever is routing, not binary size

The speedup comes from *avoiding the work* — answering from the warm controller —
not from a smaller binary. Binary size does not drive startup: the real `bd` is
187 MB yet boots in ~99 ms; Go's startup cost is dominated by package `init()`
breadth, not size. So bdshim being small is a footprint/hygiene benefit (disk,
per-process RSS across many concurrent workers, faster cold first-load), not the
source of the latency win — chase the warm-path routing, not the megabytes.

## How the binary got small (independently useful to upstream)

Two steps took the thin client from 18 MB → 7.7 MB, both of which are clean and
arguably valuable on their own:

1. **A bead-scoped client leaf** so the thin client stops importing the huma
   *server* package (a client importing the server is a layering inversion).
2. **Splitting the OTLP exporter out of `internal/telemetry`** into a sub-package
   that only telemetry-*exporting* binaries import. The OTLP/HTTP exporters pull
   ~160 `google.golang.org/grpc` packages (~10 MB linked); the `Record*` API
   itself is grpc-free. After the split, any record-only binary drops grpc. **This
   one is a self-contained refactor we'd be happy to send as a standalone PR.**

Floor check: a trivial Go program doing one `https.Get` is already ~5.5 MB
(runtime + `crypto/tls` + FIPS module + `net/http`); bdshim's entire client adds
only ~1.9 MB on top. We're at the practical floor.

## What we tried and moved away from (concise)

- **Symlinking `bd` → `gc`** — an earlier attempt to give the CLI controller
  access by making `bd` *be* gc. It cold-started the 122 MB gc on every call —
  2–6× *slower* than plain `bd` on reads — so any routing benefit was swamped by
  the boot. Removed; the thin client gets the controller routing without the gc
  boot.
- **Fat thin-client (18 MB)** — the first thin client still transitively imported
  the server and the OTLP/grpc exporter. Slimmed via the two steps above.
- **Remote (WAN) Dolt store** → cut over to a single local gc-managed `dolt
  sql-server`. The remote machinery was built for a churny remote endpoint and
  became cold overhead once local; the connection benefit is *concentration*, not
  latency. (Separate from bdshim, but part of the same "measure, then cut" pass.)
- **Rejected routing scope** — telemetry showed some verbs get zero benefit from
  routing (e.g. 751/878 live `dep-list` calls are `--direction=up` with no
  win), so we route only the verbs the data justified.
- **Size levers we rejected:** *TinyGo* (its size win is WASM/baremetal-specific;
  reflection-heavy JSON / generated clients panic at runtime); *UPX* (decompresses
  the whole image on every `exec` and defeats page-cache sharing — a startup +
  RSS regression, exactly wrong for a per-call binary).

## Questions for maintainers

1. Is there appetite for this upstream? Every multi-agent deployment pays the
   same per-call CLI cost, and the mechanism is opt-in and composable.
2. Preferred shape: a separate `cmd/bdshim` binary (as we have it), or a `gc`
   subcommand built as a size-constrained artifact?
3. We'd like to start with the **telemetry OTLP-exporter split** as a small
   standalone PR regardless — any objection to that separation of concerns?

## Code & references

- Fork: `github.com/wbern/gascity`, branch `develop`.
- Origin: `542270c9d` (tiny bd thin-client). Fat-shim removal: `87ba2cd42`.
  Client leaf: `00da26053`. Telemetry exporter split: `cc9960577`. Build flags:
  `05f6f5820`.
- Design doc: `engdocs/contributors/bd-shim-thin-client.md`.
