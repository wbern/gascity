# bdshim — prior-art & pitfalls deep research

**Date:** 2026-07-19 · **Author:** gas-city-wbern/architect (research pass) ·
**Subject:** Adversarial "cover our bases" review of upstream proposal
[gastownhall/gascity#4441](https://github.com/gastownhall/gascity/issues/4441)
— "route hot `bd` verbs to the warm controller via a tiny thin client."

> Method: five parallel research streams (comparators, beads, gascity, forks,
> best-practices/counterarguments) via web + `gh` search, followed by direct
> adversarial verification of the highest-impact claims against primary
> sources (issue/PR bodies quoted verbatim). Confidence and thin evidence are
> flagged inline. All URLs are primary where possible (issue/PR threads,
> official docs, CVE advisories, RFCs).

---

> **Status corrections (2026-07-19, verified against primary sources):**
> Two claims below were re-verified live and corrected.
> 1. **beads#3760** ("bd serve" daemon) — the issue is still OPEN but was
>    **WITHDRAWN by its own author**: re-measured against gc 1.1.1, the
>    connection storm collapsed **41→0.6 conn/sec at the gc layer**, idle Dolt
>    CPU 475%→313%, and the residual was reattributed to auto-commit no-op
>    churn (**beads#3674**, merged 2026-06-17), not connection setup. Upstream
>    is also under a **new-feature freeze**. Read every "#3760 is a live
>    competing proposal" line below through this lens.
> 2. **beads#4303** (db-proxy pooling) — **OPEN + Draft, `mergedAt=null`; NOT
>    in beads `main` or any release.** Where the text says "already in main",
>    read "unmerged". Our v1.1.0 baseline is therefore honest/current.
>
> **Net effect on the #4441 pitch:** do NOT lead with idle Dolt CPU (#3760's
> author retracted that framing). Lead with **read-result recompute latency** —
> the one lever neither #3760 nor #4303 touches (both only warm the
> connection/process; they still recompute every query).

## Executive summary

The proposal is sound in spirit and well-measured, but the research surfaces
**three things that materially change the upstream pitch**:

1. **We are not first, and the ecosystem has already picked a different
   horse.** `bd` itself *had* a Unix-socket daemon and deliberately **removed
   it** (~19,663 lines); there is a live, prototyped upstream proposal for a
   `bd serve` daemon
   ([beads#3760](https://github.com/gastownhall/beads/issues/3760)) and an
   open connection-pooling proxy PR
   ([beads#4303](https://github.com/gastownhall/beads/pull/4303)) whose author
   states plainly that *"a `bd daemon` alternative was rejected."* On the gc
   side, [gascity#1978](https://github.com/gastownhall/gascity/issues/1978)
   already enumerated "long-lived per-agent bd daemon" and "in-process bead
   client" as fix directions. **#4441 must position itself relative to this
   prior art or it will read as a re-proposal of a rejected shape.**

2. **The community diagnoses the cost differently than we do.** Our doc frames
   the win as eliminating gc-binary `init()` breadth / cold-start. The beads
   maintainers and reporters frame it as **per-connection Dolt setup cost**
   (view resolution redone on every fresh MySQL connection) — a *server-side*
   tax, not a *client-side* boot tax. Both are real; the framings lead to
   different "correct" fixes (a client shim vs. a connection pool vs. a
   daemon). We should acknowledge the connection-setup framing explicitly.

3. **Two citation-accuracy bugs in our own issue draft** that a maintainer
   will catch (details in Q3). The "blocking callers bypass the cache" lever is
   from **#3186 (Jay German)**, not #3208/#3217 (Stephanie Jarmak); and #3817
   proves *federation completeness*, not a "deliberately uncached" policy.

Beyond those, the strongest *design* objection is transport/security: every
comparator that exposed a **localhost TCP/HTTP** port had a security incident
or gap, and there is a fresh CVE class (DNS-rebinding against localhost dev
servers) directly on point. The strongest *architectural* objection is that a
`bd serve` mode inside beads is a cleaner single-source-of-truth than a
gc-routing shim that must reproduce `bd`'s output byte-for-byte.

---

## Q1 — Prior art for "thin client → warm daemon" (comparators)

Cross-cutting lessons, each grounded in a specific tool. Full per-tool detail
follows the synthesis.

**Transport: UDS beats localhost TCP.** The best-secured tools use a **Unix
domain socket** with filesystem permissions as the auth model — Docker (default
socket, `root:docker 0660`), Watchman (enforced `0700`, refuses to run if the
dir is group/other-writable —
[watchman#711](https://github.com/facebook/watchman/issues/711)), git
`fsmonitor--daemon` (per-worktree UDS / named pipe on Windows —
[docs](https://git-scm.com/docs/git-fsmonitor--daemon)), Emacs (default UDS).
Every tool that exposed a **TCP port** had a security problem: Nailgun binds
`0.0.0.0` by default → unauthenticated RCE
([background](https://www.martiansoftware.com/nailgun/background.html)); Gradle
≤5.6 advertised all interfaces and a worker connected to an *unrelated local
process* (fixed to `127.0.0.1` in 6.0 —
[gradle#11426](https://github.com/gradle/gradle/issues/11426)); gopls' opt-in
TCP daemon mode ships with **no documented auth**
([gopls/daemon](https://go.dev/gopls/daemon)).

**Auth/discovery: write a token+PID+port to a per-user dir, atomically.**
Bazel writes a random request cookie + response cookie + command port to the
server dir ("rudimentary mutual authentication" —
[client-server](https://bazel.build/run/client-server)); Emacs TCP mode
requires a **64-char random key** the client reads from a server file
([TCP Emacs server](https://www.gnu.org/software/emacs/manual/html_node/emacs/TCP-Emacs-server.html));
Gradle publishes an auth token in its registry. **Pitfall:** Bazel's cookie/port
files are written non-atomically and not cleaned on crash → "bad cookie"
errors ([bazel#12190](https://github.com/bazelbuild/bazel/issues/12190));
Gradle has a port-recycling race yielding "Unexpected authentication token"
([gradle#12530](https://github.com/gradle/gradle/issues/12530)). Write
discovery files temp+rename (which this project already mandates) and embed the
daemon PID.

**Version skew → kill/restart the daemon; never interoperate across versions.**
This is the single most universal pitfall. adb is the model UX: *"adb server
version (41) doesn't match this client (39); killing…"* — explicit handshake,
client-driven restart
([scrcpy#3273](https://github.com/Genymobile/scrcpy/issues/3273)). Bazel kills
and restarts the server on version **or startup-flag** mismatch
([client-server](https://bazel.build/run/client-server),
[bazel#9068](https://github.com/bazelbuild/bazel/issues/9068)); Gradle never
reuses an "incompatible daemon" (different Gradle *or* JVM)
([gradle#30153](https://github.com/gradle/gradle/issues/30153)); Buck2 requires
`buck2 kill` on upgrade
([steveklabnik](https://steveklabnik.com/writing/updating-buck/)). The
anti-pattern is Nailgun, which silently runs stale classes forever until
manually restarted
([narkive thread](https://jvm-languages.narkive.com/ez7ogMcC/using-nailgun-to-defeat-jvm-startup-woes)).
gopls hashes the build ID into the socket path so different builds get
different daemons ([gopls/daemon](https://go.dev/gopls/daemon)).

**Fallback: degrade to a correct full computation, never to wrong/stale data.**
The two best freshness designs both hand the client an escape hatch that means
"you may have missed events — do a full resync": git fsmonitor's token
**encodes the daemon PID**, so a token from a dead/foreign daemon triggers a
**trivial (full-scan) response** — correct, just slower
([GitHub blog](https://github.blog/engineering/infrastructure/improve-git-monorepo-performance-with-a-file-system-monitor/));
Watchman returns **`is_fresh_instance`** meaning "discard cached state and
full-resync"
([troubleshooting](https://facebook.github.io/watchman/docs/troubleshooting)).
Contrast sccache, whose default is to **fail the build** if the server is
unreachable unless you set `SCCACHE_IGNORE_SERVER_IO_ERROR=1`
([mozilla/sccache](https://github.com/mozilla/sccache)) — a footgun.

**Freshness tokens should be opaque/monotonic, not wall-clock.** Watchman uses
an abstract clock id (`c:1234:2342`, ticks per change —
[clockspec](https://facebook.github.io/watchman/docs/clockspec.html)); git
fsmonitor uses an opaque token; both drain pending OS events with **cookie
files** before answering. Avoids timestamp races.

**Bound lifetime; assume the daemon leaks.** Idle timeouts cluster: Bazel 3h,
Gradle 3h, mvnd 3h, sccache 10min, gopls 1min. Warm long-lived processes leak —
Nailgun/JVM Metaspace OOM, Gradle's health monitor restarts on *slow* leaks but
*fast* leaks OOM before self-expiry and some daemons fail to die
([gradle#24026](https://github.com/gradle/gradle/issues/24026)). "Daemon
sprawl" is real (report of 100 Gradle daemons exhausting memory —
[discuss.gradle](https://discuss.gradle.org/t/tons-of-gradle-daemons-exhausting-memory/20579)).

**Don't over-index on LSP.** gopls/rust-analyzer are **per-editor-session
subprocesses over stdio**, client-owned — the *opposite* of a shared warm
daemon on PATH. The transferable lesson is only the incremental-sync contract
(versioned state), not the architecture
([LSP spec](https://microsoft.github.io/language-server-protocol/specifications/lsp/3.17/specification/)).

*Per-tool protocol/security summary (condensed):* Nailgun = TCP:2113, insecure
by design, no auto-spawn. Bazel = gRPC, cookie auth, auto-spawn, 3h idle. Buck2
= gRPC (UDS-vs-TCP undocumented — thin), isolation-dir keyed, auto-spawn,
Restarter. adb = localhost TCP:5037, auto-spawn, version-handshake kill.
Docker = UDS default (root-equivalent access; TCP:2375 = remote root). sccache
= localhost TCP:4226 or `SCCACHE_SERVER_UDS`, auto-spawn, no local auth.
Emacs = UDS default / TCP with 64-char key, `--alternate-editor=''` auto-spawns.
mvnd/Gradle = loopback TCP + registry token, auto-spawn, exact-version reuse.

---

## Q2 — Does beads itself have / propose a daemon, cache, or latency work?

**Yes — extensively, and this is the most decision-relevant finding.** (Repo
note: `github.com/steveyegge/beads` redirects to the canonical
`gastownhall/beads`.)

**Backend model.** Beads is **Dolt-powered** (version-controlled MySQL-compatible
DB), migrated off SQLite. Two modes: **embedded** (in-process Dolt, default)
and **server** (`bd init --server` → external `dolt sql-server`). Every `bd`
invocation is a fresh process opening a fresh connection pool that dies at
exit — *zero* cross-invocation reuse
([README](https://github.com/steveyegge/beads)).

**`bd` HAD a daemon and removed it.** `CHANGELOG.md`: *"Removed daemon/RPC
subsystem — internal daemon, RPC layer, and `internal/rpc/` package deleted
(~19,663 lines). All commands use direct embedded database access."* and
*"the bd daemon has been fully removed; bd is now purely CLI-driven."*
([CHANGELOG](https://github.com/gastownhall/beads/blob/main/CHANGELOG.md)).
So the exact "thin CLI → warm daemon over Unix socket with direct-DB fallback"
shape existed and was dropped — **strong prior art and a cautionary datapoint.**

**Live proposal for a `bd serve` daemon —
[beads#3760](https://github.com/gastownhall/beads/issues/3760)** (DarranShepherd,
**OPEN**, verified verbatim). Opt-in long-running `bd` on a Unix socket;
invocations forward via `BEADS_DAEMON_SOCKET`, **falling through to direct
connection on stale socket** (same fallback philosophy as bdshim). Motivated by
managed Dolt pegging **400–550% CPU at idle** in a 4-agent city; prototype cut
connection rate **41→0.4 conn/sec** and Dolt CPU **~475%→30–80%**. Its "Ask" is
the exact debate #4441 sits inside: *"Is the maintainer position that bd should
remain strictly process-per-invocation and the rate problem belongs at the gc
layer?"* Its alternatives table explicitly rejects *"stick gc supervisor's bd
calls behind a SQL-server shared connection"* because it *"would require gc to
embed bd as a library."*

**A full `bdd` daemon implementation series — built, then closed UNMERGED:**
[beads#3972](https://github.com/gastownhall/beads/pull/3972),
[#3973](https://github.com/gastownhall/beads/pull/3973),
[#3974](https://github.com/gastownhall/beads/pull/3974),
[#3975](https://github.com/gastownhall/beads/pull/3975),
[#3977](https://github.com/gastownhall/beads/pull/3977),
[#3978](https://github.com/gastownhall/beads/pull/3978),
[#3984](https://github.com/gastownhall/beads/pull/3984) — all `state=closed
merged=false`. The standalone daemon-RPC approach was built out and **not
accepted**.

**The surviving direction is a connection-pooling DB proxy —
[beads#4303](https://github.com/gastownhall/beads/pull/4303)** (cstar, Draft).
A MySQL-protocol-aware forwarder that borrows an already-authenticated backend
from a pool and returns it via `COM_RESET_CONNECTION`. Real CLI: 7.02→1.02 new
Dolt connections/invocation. **Verified verbatim quote:** *"A `bd daemon`
alternative was rejected (routing the whole domain layer over IPC, or
reinventing the wire protocol over a socket; transactions still force
per-invocation session pinning → no gain)."* **Important nuance for our pitch:**
that rejection reasoning is about the **write/transaction path** (session
pinning), which does **not** apply to routing *read* point-lookups like
`show` — a defensible distinction for #4441. **Status (verified 2026-07-19):
#4303 is OPEN + Draft, `mergedAt=null` — NOT in any bd release.** We run bd
v1.1.0 (latest release, 2026-07-04), which contains no connection-pooling
proxy, so our measured baseline is honest against shipping `bd`; re-measure
only *if/when* #4303 merges. (Some transparent-forwarder db-proxy scaffolding
predates this PR in `cmd/bd/`; the session-pooling behavior that would shrink
per-call connection cost is what #4303 adds and it is unmerged.)

**Perf issues diagnosing cold-start** (frame the cost as connection setup, not
Go init):
[beads#4102](https://github.com/gastownhall/beads/issues/4102) traces 5–10s
commands to cold `sql.DB` pool per process (~200–500ms TLS+auth) + 10–15 serial
roundtrips, and lists *"long-lived connection proxy / bd daemon … keeps the
MySQL connection warm across CLI invocations, similar to `ssh-agent` or `git
credential-helper`"* as a fix;
[#4128](https://github.com/gastownhall/beads/issues/4128) (write path re-imports
JSONL per call → OOM);
[#4282](https://github.com/gastownhall/beads/issues/4282) (orphaned embedded
`dolt sql-server` daemons — the real long-lived process today is Dolt itself).
Incremental fixes: [#4382](https://github.com/gastownhall/beads/pull/4382)
(MERGED, pool conn retirement), [#3710](https://github.com/gastownhall/beads/pull/3710)
(12s stale-`.local_version` slow path).

**MCP mode exists but is NOT warm-process routing.** `beads-mcp` (separate PyPI
package) is a *stateless* protocol adapter that shells out to `bd` per call and
is explicitly *higher* latency/token cost than the CLI
([pypi/beads-mcp](https://pypi.org/project/beads-mcp/)). Not relevant as a
warm router.

*Gap:* GitHub search API rate-limited the sweep; a dedicated *query-result
cache* proposal (distinct from connection pooling) was **not found** — "not
found," not "confirmed absent." The community framing is consistently "keep the
connection warm," not "cache results."

---

## Q3 — gascity issues/PRs on bd latency / caching / daemon / freshness

**Closest architectural precedent (must-cite):
[gascity#1978](https://github.com/gastownhall/gascity/issues/1978)** (vbtcl,
OPEN, verified): *"gc agents shell out to bd per-write — ~25k short-lived dolt
connections in a 90-second window."* Its three fix directions are our design
space: **(1)** *"Long-lived per-agent bd daemon — agents connect to a Unix
socket; the daemon multiplexes onto a small pool of dolt connections,"* **(2)**
write batching/coalescing, **(3)** *"in-process bead client — link bd as a
library."* This is the issue most likely to be seen as **prior art for /
overlapping with #4441.** Differentiate: #1978 targets the *write storm*;
#4441 targets *read point-lookups routed to an already-warm controller*.
Follow-ups show the maintainer bias toward **in-process cache fixes over a new
bd front-end**: [#2152](https://github.com/gastownhall/gascity/issues/2152)
(Phase 1, merged, "No bd-binary or protocol changes") and
[#2153](https://github.com/gastownhall/gascity/issues/2153) (Phase 2, "Still no
daemon, no protocol change").

**Quantification of the flood:**
[#2463](https://github.com/gastownhall/gascity/issues/2463) (idle city runs 7+
bd subprocesses/sec ≈ 463 dolt queries/s; verb mix `list` 38.7%, `query`
27.1%, **`show` 15.6%**, `update` 8.7%) — confirms `show` (our headline routed
verb) is a top-3 caller.
[#4246](https://github.com/gastownhall/gascity/issues/4246) (~27 bd forks/s;
mitigation idea = "share one read cache across supervisor and pollers").
[#1028](https://github.com/gastownhall/gascity/issues/1028) (`--follow` fans 8
bd subprocess/s/rig, "each a fresh Go process opening a new TCP+MySQL
handshake").

**The warm in-process store already exists** —
[#3248](https://github.com/gastownhall/gascity/issues/3248) (native store
silently disabled → per-op CLI fallback; self-describes as an instance of the
#2463/#1978 flood) and
[#3946](https://github.com/gastownhall/gascity/issues/3946) (version-skew
disables NativeDoltStore → slow CLI fallback). **A maintainer will ask why a
CLI shim is needed instead of keeping the native store enabled.** Pre-empt:
the shim serves *agents that invoke `bd` as a subprocess*, which don't share
the controller's in-process store.

**Freshness evidence supporting "route only `show`, keep `ready`/`list` live":**
[#2987](https://github.com/gastownhall/gascity/issues/2987) (`bd list`/`ready`
serve stale cache lagging minutes under write load, while `bd show <id>` stays
live — direct support for our verb selection) and
[#3892](https://github.com/gastownhall/gascity/issues/3892) which states the
real rationale: *"raw `bd` writes publish no city event, so the re-poll is the
only way to notice those transitions."*

**⚠ Two citation-accuracy corrections to our issue draft (verified):**

1. The **"blocking callers bypass the cache so the body reflects the awaited
   event"** lever is grounded in
   **[#3186](https://github.com/gastownhall/gascity/issues/3186)** (Jay German /
   `jsgerman-oss`, CLOSED), whose body says verbatim: *"Callers that require
   strict freshness keep the existing blocking path."* It is **not** stated in
   #3208/#3217. Our draft attributes it to "#3208/#3217, Stephanie Jarmak" —
   **wrong issue and wrong author.** #3208 (Jarmak, OPEN) + its fix PR #3217
   (MERGED) are about *ordering/pagination + latency + a 2s time-bucket cache*,
   not the blocking-bypass lever.
2. **[#3817](https://github.com/gastownhall/gascity/pull/3817)** (Julian
   Knutsen, MERGED) proves `/beads/ready` **federates the city store** (was
   invisible over HTTP) — a *completeness* fix. It does **not** assert
   `/beads/ready` is "deliberately live/uncached." Soften our citation to the
   federation point; source the "ready stays live" rationale from #2987/#3892
   instead.

*Fork:* `wbern/gascity` issue tracker is empty; #4441 lives on **upstream**.

---

## Q4 — Forks doing anything similar

**Clean negative among forks; strong parallels outside them.** gascity has 329
forks, beads 1,708 — essentially all `stars=0`, stale/undiverged (the only
starred gascity fork, `keithballinger/gascity`, is 0 ahead / 365 behind). A
GitHub-wide `gh search code` for `bdproxy`/`bdshim`/`bddispatch` returns only
wbern + upstream + unrelated projects. **No other gascity/beads fork implements
a thin bd client, bd daemon, warm routing, or response cache** — wbern's bdshim
is unique among forks.

**Strongest independent parallel: `Liquescent-Development/gascamp`**
([repo](https://github.com/Liquescent-Development/gascamp), verified: Rust,
`isFork:false`) — a from-scratch Rust reimplementation of Gas City with a full
`campd` daemon (`crates/camp/src/daemon/`) **and** a shim subsystem
(`crates/camp/src/cmd/shim/{bd,hook,mail,prime,project,runtime,install}.rs` —
confirmed present) whose `bd` shim pokes the daemon (`poke_best_effort`), plus
`crates/camp/tests/perf_daemon.rs` (idle-CPU/RSS/dispatch-latency bounds).
Caveat: its bd shim is a *compatibility translation layer* and its perf work
targets idle-CPU/dispatch-latency, not per-call CLI cold-start — parallels the
*daemon+shim architecture* but not our specific read-routing idea.

**General prior art in AI-agent frameworks** (the concept is not novel):
[Claude Agent SDK issue #33 "Daemon Mode for Hot Process Reuse"](https://github.com/anthropics/claude-agent-sdk-typescript/issues/33)
(documents CLI cold-start too slow for <3s latency; process pooling / spawn
interception all failed → daemon is the only fix — the best external "why" for
our approach); [tobias-walle/agency](https://github.com/tobias-walle/agency)
(`agency daemon start|stop`, slim client over a Unix socket);
[awslabs/cli-agent-orchestrator](https://github.com/awslabs/cli-agent-orchestrator)
(persistent `cao-server` keeps agents warm, "eliminates inter-turn latency").

Upstream gascity also ships a warm-DB precedent:
`examples/bd/assets/scripts/gc-beads-bd.sh` lifecycle-manages a warm per-city
Dolt SQL server (one warm Dolt server per town, per Yegge's
[DoltHub "A Day in Gas Town"](https://www.dolthub.com/blog/2026-01-15-a-day-in-gas-town/)).

---

## Q5 — Best practices & known pitfalls

**Cache freshness vs correctness.** A cache between reader and source creates
two sources of truth; the specific hazard is a **read-your-writes violation**
(agent `claim`s then `show`s and the controller projection hasn't caught up).
The literature says "must immediately see own changes" cases should **not** be
served from a lagging read-through cache — they need write-through or
synchronous invalidation
([systemsarchitect.io](https://www.systemsarchitect.io/blog/read-replicas-read-through-caching-vs-write-through-caching-strategies)).
**RFC 9111** gives the vocabulary: a cache *"MUST NOT generate a stale response
if prohibited by … `no-cache`/`must-revalidate`"* and `must-revalidate`
requires revalidation-or-`504`, never silent stale
([RFC 9111 §4.2.4/§5.2.2.2](https://www.rfc-editor.org/rfc/rfc9111.html)).
Production triad: delete-on-write + TTL backstop + event invalidation. **Note
`claim` is a *mutation*, not a read — routing it "through a read cache path" is
a category error; it must be write-through or not routed.** (Our design does
treat claim specially with liveness probe + bd.real fallback; keep it that
way.)

**UDS vs localhost HTTP — the strongest objection to the spec as written.** A
`127.0.0.1` TCP port is **not** a security boundary: reachable by any
process/UID on the host, and — crucially — from a **browser tab via DNS
rebinding**. Fresh, on-point CVEs:
- **CVE-2025-49596** — Anthropic **MCP Inspector**, CVSS 9.4, RCE via missing
  auth + DNS rebinding on a localhost service accepting any origin
  ([Oligo](https://www.oligo.security/blog/critical-rce-vulnerability-in-anthropic-mcp-inspector-cve-2025-49596)).
- **CVE-2025-66414 / -66416** — official **MCP TypeScript/Python SDKs** shipped
  with DNS-rebinding protection **disabled by default** for localhost HTTP;
  fixed TS 1.24.0 / Py 1.23.0
  ([GHSA-w48q-cv73-mx4w](https://github.com/modelcontextprotocol/typescript-sdk/security/advisories/GHSA-w48q-cv73-mx4w)).
- **CVE-2025-53034** — Playwright MCP, no `Origin` validation on a localhost
  server.
- **Docker TCP 2375** — the canonical cautionary tale: unauthenticated HTTP
  control surface = remote root; mass-scanned
  ([Rapid7](https://www.rapid7.com/db/modules/exploit/linux/http/docker_daemon_tcp/)).

Mitigation if HTTP-over-TCP is retained: **strict `Host`/`Origin` allowlist +
a locally-generated token + loopback bind** — i.e., re-implementing exactly the
protections those CVEs were filed for. **Prefer a Unix domain socket** (kernel
enforces caller identity via file permissions / `SO_PEERCRED` for free —
[thinkaboutit](https://thinkaboutit.tech/posts/2025-05-25-why-uds-is-good/)).
Our current design ("no token — controller ignores Authorization on localhost")
is precisely the pattern these CVEs punish.

**Fallback / graceful degradation.** Distinguish two cases: **controller down →
exec real `bd`** is safe *and* correct (falling back to the authoritative
source) — the only risk is silent "degraded for days," so **count/log every
fallback and alert on degraded, not just down**. **Controller *up* but serving
a different scope/filter/default than `bd`** is the dangerous case — a
plausible wrong answer ("that is data corruption," the "fallback trap" —
[braingrid](https://www.braingrid.ai/blog/the-fallback-trap)). Rule: **fail
loud internally, degrade gracefully externally**, and the fallback path must be
*simpler/more reliable* than what it replaces (exec-ing the real binary
qualifies; a reimplemented query path does not). Our "loud-fail rc=1 rather
than silent wrong/empty passthrough" matches best practice.

**Version skew + the "byte-identical output" trap.** Bazel/Gradle/adb all
**restart the daemon on version mismatch** rather than interoperate. A
persistent controller + separately-versioned `bd`/shim **will** drift → needs a
version handshake + restart/refuse-on-mismatch. Separately, **committing to
byte-identical *human* output is a real maintenance liability**: human CLI
output is explicitly not a stable contract — Git formalized plumbing vs
porcelain precisely because "the interface to Porcelain commands is subject to
change"
([Git internals](https://git-scm.com/book/en/v2/Git-Internals-Plumbing-and-Porcelain)).
Every `bd` release that tweaks a column/color/relative-timestamp silently
breaks the shim with no compiler/test to catch it. **Route only a stable
machine format (`--json`/porcelain), never byte-identical human output.**
(Our doc's "byte-identical output" is currently framed as a feature; reframe it
as *"identical `--json` output"* and note the human-format risk.)

**PATH-fronting a shim.** Proven pattern (pyenv/rbenv/asdf shims dir at front
of PATH; ccache/sccache `argv[0]` masquerade via **symlinks, not hard links**
— [ccache manual](https://ccache.dev/manual/latest.html)) but fragile:
- Shim silently drops out of PATH → system binary wins (the #1 pyenv failure —
  [mungingdata](https://www.mungingdata.com/python/how-pyenv-works-shims/)).
- Shim execs a **stale hardcoded path** to the real binary after an upgrade
  ([pyenv#816](https://github.com/pyenv/pyenv/issues/816)) — resolve the real
  `bd` robustly, don't hardcode.
- **Infinite self-recursion** when a same-named shim can't find the *next*
  binary and re-execs itself
  ([brew#8773](https://github.com/Homebrew/brew/issues/8773)) — **first-class
  risk for a shim named `bd` ahead of `bd`.** Canonicalize paths before
  stripping the shim dir; guard against exec-ing itself. (Our `GC_BD_REAL`
  explicit-real-path approach sidesteps most of this — good.)
- **Windows `PATHEXT`** minefield: resolvers that ignore `.CMD`/`.BAT` order,
  CWD-first hijack ([ss64 PATH](https://ss64.com/nt/path.html)).
- Ship a diagnostic (ccache/pyenv publish `which`; git has `GIT_TRACE`). Our
  `gc doctor` routing check + `bdshim.log` cover this.

**Observability.** Envoy is the reference: **always-on access log per request
enriched with the routing decision**, explicit logging **when nothing matched /
fell back**, and **fallback-rate as a first-class metric** (a system silently
serving fallbacks is a degraded state that "deserves a ticket even if no user
is erroring" —
[designgurus](https://designgurus.substack.com/p/when-to-fail-fast-vs-degrade-gracefully)).
Our JSONL log (verb/disposition/exit/dur_ms) is on the right track; add a
fallback/cache-miss **metric** and a controller-version field.

---

## Q6 — Arguments AGAINST our approach / better alternatives

A skeptical maintainer will raise, strongest first:

1. **"Put `bd serve` in beads, not a gc-routing shim."** This is the strongest
   objection and it is **already a live upstream proposal**
   ([beads#3760](https://github.com/gastownhall/beads/issues/3760)). A serve
   mode inside `bd` is a **single source of truth**: it eliminates the
   byte-identical-output duplication and version-skew contract entirely (the
   server *is* `bd`), offers a real machine protocol instead of scraping human
   output, and makes read-your-writes the tool's own consistency problem to
   solve authoritatively. If we can influence beads, upstreaming a serve mode
   is architecturally cleaner than a gc-side shim that duplicates `bd`'s output.

2. **"The community already rejected the daemon shape and chose connection
   pooling."** [beads#4303](https://github.com/gastownhall/beads/pull/4303):
   *"a `bd daemon` alternative was rejected."* **Rebuttal we can make:** that
   rejection was reasoned about the **write/transaction path** (session pinning
   defeats reuse) — it does **not** apply to routing *read* point-lookups
   (`show`) to a warm in-memory projection. But we must say this explicitly, or
   #4441 reads as a re-litigation of settled ground.

3. **"gc already has an in-process native store — fix that instead of adding a
   CLI shim"** ([#3248](https://github.com/gastownhall/gascity/issues/3248),
   [#3946](https://github.com/gastownhall/gascity/issues/3946)). Rebuttal: the
   native store serves the *controller's own* reads; it does nothing for the
   *agent subprocesses* that shell out to `bd` — which is exactly what the shim
   fronts. Make this explicit.

4. **"Premature optimization — is 100ms actually the bottleneck for LLM agents
   whose model latency is seconds?"** Evidence cuts against reflexive
   optimization: agent latency is often dominated by model inference, not tool
   round-trips (Text-to-JQL study: "latency overhead is dominated by LLM
   inference … rather than API round-trips," ~1.3 tool calls/query —
   [arxiv](https://arxiv.org/pdf/2604.09470)). **Rebuttal we can make (and must
   quantify):** the cost here is not per-turn agent latency but **aggregate
   Dolt CPU** — beads#3760 measured **~475% idle Dolt CPU** and gascity#1978
   measured **25k connections/90s**; #2463 shows `show` is 15.6% of an
   *idle* city's 463 queries/s. The win is fleet-wide load reduction, not
   shaving a single agent's turn. Lead with that framing, not the 8.1×
   single-call number (which invites the "premature optimization" dismissal).

5. **"Byte-identical human output is a standing maintenance liability"** (Q5,
   Git plumbing/porcelain). Reframe to `--json`.

6. **"Complexity/maintenance surface vs the win."** Summed: a cache-consistency
   obligation, a localhost-IPC security obligation (live CVE class), a
   fallback-scoping decision, a version-handshake + output-contract, a fragile
   same-named PATH shim, plus observability — each a place a *quiet wrong
   answer* can hide from an agent that trusts `bd`. Opt-in/default-off contains
   the blast radius, which is our best mitigation and should be foregrounded.

**Alternatives inventory:** (a) `bd serve` in beads (#3760 — cleanest); (b)
connection-pooling proxy (#4303 — the surviving upstream direction, already in
`main`); (c) in-process/library-linked bd client (#1978 direction 3 — no IPC at
all, but ABI coupling); (d) write batching/coalescing (#1978 direction 2);
(e) event-bus cache convergence (#2153 — the direction maintainers actually
preferred over #1978's daemon).

---

## Things we may have missed

1. **beads#3760 was WITHDRAWN by its own author (verified 2026-07-19; issue
   still OPEN but the author retracted the proposal).** Re-measured against
   gc 1.1.1, the connection storm collapsed **41→0.6 conn/sec at the gc layer**;
   idle Dolt CPU 475%→313%; the residual was reattributed to auto-commit no-op
   churn (beads#3674, now CLOSED/merged 2026-06-17), **not** connection setup.
   A maintainer also noted upstream is under a **new-feature freeze**.
   Consequence for #4441: do **not** lead with aggregate idle Dolt CPU (#3760's
   author retracted exactly that framing). Position bdshim as *orthogonal* —
   our lever is **read-result recompute latency**, which neither #3760 nor
   #4303 addresses (both only warm the connection/process; they still recompute
   every query). This *removes* the "re-proposing a rejected shape" risk but
   *voids* the idle-CPU pitch.
2. **The connection-pooling proxy (#4303) is OPEN + Draft, `mergedAt=null` —
   NOT in beads `main`/any release** (the earlier "already merged" claim was
   wrong; verified 2026-07-19). Our v1.1.0 baseline is therefore honest and
   current; #4303 is **complementary** (it warms the *connection*; bdshim skips
   the *query recompute + process* for routed reads). Re-measure only *if/when*
   #4303 merges.
3. **The cost framing gap.** Upstream diagnoses the tax as **per-connection
   Dolt setup** (view resolution), not gc `init()` breadth. Our proposal should
   acknowledge both, because the "route to warm controller" fix and the
   "warm the Dolt connection" fix solve *different* halves.
4. **gascity#1978 is near-duplicative prior art** (daemon + in-process client
   already enumerated) and its follow-ups (#2152/#2153) reveal a **maintainer
   preference for in-process cache convergence over a new bd front-end** — the
   political headwind our proposal faces.
5. **Two citation bugs in our issue draft** (blocking-path lever = #3186 Jay
   German, not #3208/#3217 Jarmak; #3817 = federation completeness, not
   "uncached policy"). Fix before the thread gains traction.
6. **`claim` is a mutation routed on the read path** — verify our special-case
   (liveness probe + bd.real atomic fallback) is truly write-through-correct
   under a concurrent writer, and consider *not* routing it at all.
7. **gascamp** proves the daemon+shim architecture is independently arrived-at
   (Rust reimplementation) — useful "this shape is natural" evidence.

## Risks / counterarguments a maintainer will raise (ranked)

1. *"Why a gc shim when `bd serve` (#3760) is cleaner and already proposed?"*
2. *"The daemon shape was rejected (#4303) — what's different here?"* (answer:
   read-only routing, no transaction pinning.)
3. *"localhost HTTP with no token is a CVE waiting to happen — use a UDS."*
4. *"Byte-identical human output will break on every bd release — route JSON."*
5. *"Fix the disabled native store (#3248/#3946) instead."*
6. *"Prove bd overhead matters vs model latency"* (answer with the CPU/rate
   numbers, not the 8.1× single call).
7. *"Version skew between shim and controller"* (need a handshake +
   restart/refuse policy).
8. *"Maintainer preference is in-process cache convergence (#2153), not a new
   bd front-end."*

## Concrete recommendations for the upstream pitch

1. **Lead with aggregate load, not the 8.1× single call.** Cite idle Dolt CPU
   (#3760: ~475%) and connection rate (#1978: 25k/90s; #2463: `show` = 15.6%
   of 463 q/s) — that reframes the win as fleet cost reduction and defuses
   "premature optimization."
2. **Explicitly position against beads#3760 and #4303.** State the distinction:
   we route **read point-lookups** to an **already-warm controller** (no new
   long-lived process to babysit, no transaction session-pinning problem that
   sank the daemon), complementary to the connection-pool proxy — not a
   competing daemon.
3. **Switch the contract from "byte-identical output" to "identical `--json`
   output"** and say so — removes the human-format-drift liability (Git
   plumbing/porcelain precedent).
4. **Move the endpoint to a Unix domain socket** (or, if HTTP is retained, add
   `Host`/`Origin` validation + a per-city token). Cite the MCP DNS-rebinding
   CVE class and Docker 2375 as why "localhost = safe" is false.
5. **Add a shim↔controller version handshake** with restart/refuse-on-mismatch
   (adb/Bazel/Gradle discipline).
6. **Keep — and foreground — the loud-fail-not-silent-passthrough behavior**
   and the exec-real-`bd` fallback; add a fallback-rate metric.
7. **Fix the two citation bugs** (#3186 attribution; #3817 framing) before the
   thread gains attention.
8. **Reconsider routing `claim` at all** — it is a mutation; make the
   write-through correctness argument explicit or leave it passthrough.
9. **Reference the independent parallels** (gascamp `campd`+shim; Claude Agent
   SDK #33; AWS CAO) as evidence the warm-routing shape is a natural,
   recurring solution — not a fork-local hack.

---

*Evidence confidence: beads #3760/#4303/#1978/#3186/#3817 quotes and gascamp
file layout were verified directly against primary sources during this pass.
Comparator security/version-skew claims rest on official docs + CVE advisories.
Thin spots flagged inline: Buck2 transport medium (UDS vs TCP) undocumented;
mvnd socket auth undocumented; no dedicated beads query-result-cache proposal
found (search-API rate-limited — "not found," not "confirmed absent").*
