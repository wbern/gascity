# Upstream discussion issue

> Posted: **gastownhall/gascity#4441**
> (https://github.com/gastownhall/gascity/issues/4441). Numbers are from live
> gc2, 2026-07-19. Source text below.

---

**Title:** Proposal: a tiny `bd` thin-client shim to kill per-call CLI cold-start (measured 8–19× on hot verbs)

## Context

Gas City deliberately has agents reach subsystems through their CLIs rather than
registered tools or MCP — per the project's own principle, *"No MCP/tool
registration — if a tool has a CLI, the agent uses it."* The beads work ledger
follows that model: agents drive work by calling `bd`, and order/gate condition
scripts shell out to it too. A healthy consequence is that `bd`'s per-invocation
cost is a first-class performance concern by design — every agent action and
gate evaluation pays it.

In our fork, `bd` resolves (via PATH) to the `gc` binary, so each of those calls
cold-starts gc. Across many agents and gates that per-command startup adds up,
and it surfaces as general sluggishness.

## The finding that shaped the fix: binary size is *not* the boot cost

Boot floor + end-to-end, warm cache, live gc2 (the genesis measurement that
started this work):

| `bd` front end            | binary size | boot floor | show   | list   | ready  |
| ------------------------- | ----------- | ---------- | ------ | ------ | ------ |
| gc-as-bd (fat shim)       | 122 MB      | 228 ms     | 230 ms | 265 ms | 650 ms |
| raw `bd.real`             | 187 MB      | 99 ms      | 99 ms  | 119 ms | 110 ms |
| **bdshim (this proposal)**| **7.7 MB**  | **~10 ms** | **12 ms** | 40 ms | 367 ms |

Key point: `bd.real` is *larger* (187 MB) than `gc` (122 MB) yet boots ~2.3×
faster. **The driver is package-level `init()` breadth, not binary size** — `gc`
wires the whole orchestration graph at startup; `bd` only wires the ledger.
(Building `gc` with `CGO_ENABLED=0` halved its *size* and did nothing to the boot
floor.) So the fix is a program with a *tiny init surface*, not merely a smaller
binary.

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

## Measured wins (post-slim, live gc2, n=25/verb)

- **`bd show <id>` — the dominant point-lookup verb — is 8.1× faster: 14.5 ms vs
  117 ms, and ~14× less CPU.**
- Boot floor 228 ms → ~10 ms.
- Binary: **122 MB → 7.7 MB** for the thing on every agent's PATH.
- Honest caveats: `ready` is *slower* via the shim (federated controller
  round-trip loses to a local scan on a loaded controller — a separate lever);
  `list` is passthrough (neutral).

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

- **gc-as-bd fat shim** — an earlier attempt kept warm-cache routing but paid
  gc's ~228 ms cold-start per call, making it 2–6× *slower* than plain `bd` on
  real reads. Removed; bdshim keeps the routing, drops the boot.
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

1. Is there appetite for this upstream? The cold-start tax hits any multi-agent
   deployment, and the mechanism is opt-in and composable.
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
