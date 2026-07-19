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

`show <id>` is the point-lookup verb agents run most, so the 8.1× is the win
that matters in practice. To be clear, the lever is *routing* — answering from
the already-warm controller — not a smaller binary: Go startup is dominated by
package `init` breadth, not binary size (the real `bd` is far larger than gc yet
boots faster). bdshim is also small and dependency-light, a modest footprint
bonus, but that is a side effect, not the proposal.

**Why we route only some verbs.** The shim routes only what the controller can
serve quickly from its warm in-memory store — point lookups like `show`. Other
endpoints are *intentionally* live and uncached upstream, for freshness:
`GET /beads/ready` federates across every store (#3817, Julian Knutsen), and the
open/live work it returns is deliberately served fresh rather than from the
response cache — the same freshness lever used for `/status` (#3186, Jay German) —
so blocking callers see the event they waited for. There is no warm-cache
shortcut to exploit there, so routing `ready` only adds a round-trip (the 🟡 row
above). Verbs like that are best left as passthrough — a deliberate upstream
design we respect, not a gap to close.

## What we tried and moved away from (concise)

- **Symlinking `bd` → `gc`** — an earlier attempt to give the CLI controller
  access by making `bd` *be* gc. But every call then paid gc's own startup: gc
  wires the whole orchestration graph at `init` (the same init-breadth effect as
  above — not its size), so it boots slowly (~230 ms) — 2–6× *slower* than plain
  `bd` on reads. The routing benefit was swamped by the boot. Removed; the thin
  client reaches the controller over HTTP without booting gc at all.
- **Remote (WAN) Dolt store** → cut over to a single local gc-managed `dolt
  sql-server`. The remote machinery was built for a churny remote endpoint and
  became cold overhead once local; the connection benefit is *concentration*, not
  latency. (Separate from bdshim, but part of the same "measure, then cut" pass.)
- **Rejected routing scope** — telemetry showed some verbs get zero benefit from
  routing (e.g. 751/878 live `dep-list` calls are `--direction=up` with no
  win), so we route only the verbs the data justified.

## Questions for maintainers

1. Is there appetite for this upstream? Every multi-agent deployment pays the
   same per-call CLI cost, and the mechanism is opt-in and composable.
2. Preferred shape: a separate `cmd/bdshim` binary (as we have it), or a `gc`
   subcommand?

## Code & references

- Fork: [`wbern/gascity` @ `develop`](https://github.com/wbern/gascity/tree/develop)
- Origin — tiny bd thin client: [`542270c9d`](https://github.com/wbern/gascity/commit/542270c9d)
- `bd`→`gc` removal: [`87ba2cd42`](https://github.com/wbern/gascity/commit/87ba2cd42)
- Design doc: [`bd-shim-thin-client.md`](https://github.com/wbern/gascity/blob/develop/engdocs/contributors/bd-shim-thin-client.md)

---

## Follow-up comment — posted 2026-07-19

Posted at
[gascity#4441 (comment-5016421700)](https://github.com/gastownhall/gascity/issues/4441#issuecomment-5016421700).
Reframes the pitch after re-verifying the adjacent upstream work. Key moves:

- **Lever = read-result recompute, not connection/process warmth.** Neither a
  `bd serve` daemon nor a connection pool avoids recomputing the query; routing
  a point-lookup to the warm controller skips the query *and* the process boot.
- **The win survives the gc-layer conn-churn fixes.** Benchmarked on gc fork
  `cc9960577` (upstream base `gastownhall/gascity@d5cb9125f`, 2026-07-16) with
  bd v1.1.0 — i.e. *with* #1692/#1706/#1722/#2152/#2153 and bd's post-#3674
  auto-commit fix already in. The 8.1× on `show` is the residual those don't
  touch.
- **vs beads#3760** (`bd serve`): withdrawn by its author after the connection
  storm collapsed at the gc layer; we do **not** re-propose it and do **not**
  pitch on idle Dolt CPU.
- **vs beads#4303** (db-proxy pooling): complementary — it warms the
  *connection*; we skip the *query recompute*. #4303's own `bd daemon`
  rejection is about the write/transaction path, which is why we route only
  read point-lookups.
- Transport → **Unix domain socket**; contract → **identical `--json`** (not
  byte-identical human output); `update --claim` flagged as the one
  write-through boundary still being validated; version handshake noted.
- Appended a **"related work / what this is *not*"** section (#3760, #4303,
  gascity#1978, #3248/#3946, #2987/#3892) to rule out overlaps.

Still an opt-in / default-off design-discussion RFC, not a PR ask (upstream is
under a new-feature freeze).
