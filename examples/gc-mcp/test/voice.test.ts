import { describe, it, expect } from "vitest";
import { GcClient } from "../src/gc-client.js";
import { buildTools, type Deps } from "../src/tools.js";
import { disambiguation, normalize, resolveIn, type SessionRef } from "../src/sessions.js";
import { POLL_MS, SPOKEN_MAX, chunkAt } from "../src/voice.js";
import { SendMarkers, WRITE_SCOPE, WriteGate, type WriteConfig } from "../src/writes.js";

const T0 = 1_800_000_000_000;
const SESS: SessionRef[] = [
  { id: "gc2-wisp-www", alias: "claude-william", template: "claude-william", state: "active", running: true, attached: true },
  { id: "gc2-wisp-s1", template: "crm/steve", agent_name: "crm/steve", state: "active", running: true },
  { id: "gc2-wisp-b1", template: "crm/bob", agent_name: "crm/bob", state: "active", running: true },
  { id: "gc2-wisp-m1", alias: "mayor", template: "mayor", state: "active", running: true },
  { id: "gc2-wisp-m2", template: "gas-city-wbern/mayor-helper", state: "active", running: true },
  { id: "gc2-wisp-zz", alias: "zachris-dev", template: "zachris-dev", state: "active", running: true },
  { id: "gc2-wisp-at", alias: "attached-one", template: "att", state: "active", running: true, attached: true },
  { id: "gc2-wisp-sl", alias: "sleeper", template: "sleep", state: "asleep", running: false },
];

function world(over: { sessions?: SessionRef[]; transcripts?: Record<string, any[]>; detail?: Record<string, any>; pending?: Record<string, any>; onSleep?: (t: number) => void } = {}) {
  const clock = { t: T0, now() { return clock.t; }, async sleep(ms: number) { clock.t += ms; over.onSleep?.(clock.t); } };
  const calls: { method: string; path: string; query?: string; body?: any; headers: Record<string, string> }[] = [];
  const transcripts = over.transcripts ?? {};
  const sessions = over.sessions ?? SESS;
  const fetchImpl = (async (url: string, init: RequestInit) => {
    const u = new URL(url), p = u.pathname, method = String(init?.method);
    calls.push({ method, path: p, query: u.search, body: init?.body ? JSON.parse(String(init.body)) : undefined, headers: Object.fromEntries(Object.entries((init?.headers ?? {}) as Record<string, string>).map(([k, v]) => [k.toLowerCase(), v])) });
    if (method === "POST") return new Response(JSON.stringify({ request_id: "req-1", status: "accepted" }), { status: 202 });
    if (p.endsWith("/sessions")) return new Response(JSON.stringify({ total: sessions.length, items: sessions }), { status: 200 });
    let m = /\/session\/([^/]+)\/transcript$/.exec(p);
    if (m) return new Response(JSON.stringify({ turns: transcripts[m[1]] ?? [] }), { status: 200 });
    m = /\/session\/([^/]+)\/pending$/.exec(p);
    if (m) return new Response(JSON.stringify({ supported: true, pending: over.pending?.[m[1]] ?? null }), { status: 200 });
    m = /\/session\/([^/]+)$/.exec(p);
    if (m) return new Response(JSON.stringify({ ...(sessions.find((s) => s.id === m![1]) ?? {}), last_active: iso(clock.t), ...(over.detail?.[m[1]] ?? {}) }), { status: 200 });   // a live agent: last_active moves
    return new Response("{}", { status: 200 });
  }) as unknown as typeof fetch;
  const gc = new GcClient({ baseUrl: "http://127.0.0.1:1", city: "gc2", fetchImpl });
  const events: any[] = [];
  const markers = new SendMarkers();
  const mk = (cfg: Partial<WriteConfig> = {}, deps: Partial<Deps> = {}) => {
    const gate = new WriteGate(gc, { enabled: true, defaultSession: "claude-william", ...cfg }, (e) => events.push(e), () => clock.t, markers);
    const d: Deps = { gc, mailAgent: "human", now: () => new Date(clock.t), writes: gate, markers, clock, defaultSession: "claude-william", ...deps };
    return { gate, d };
  };
  const tool = (d: Deps, who: { client: string; scopes: string[] }, name: string) => buildTools(d, who).find((t) => t.name === name)!;
  return { clock, calls, posts: () => calls.filter((c) => c.method === "POST"), events, markers, mk, tool, transcripts };
}
const W = { client: "ChatGPT (owner client)", scopes: ["gc:read", WRITE_SCOPE] };
const R = { client: "ChatGPT (owner client)", scopes: ["gc:read"] };
const iso = (ms: number) => new Date(ms).toISOString();
const asst = (ms: number, text: string) => ({ role: "assistant", text, timestamp: iso(ms) });

describe("name resolution: a spoken name becomes ONE live session or a question, never a guess", () => {
  it("normalises speech: case, 'slash', separators, 'the', trailing 'session'", () => {
    expect(normalize("Crm slash Steve")).toBe("crm-steve");
    expect(normalize("crm/steve")).toBe("crm-steve");
    expect(normalize("the mayor")).toBe("mayor");
    expect(normalize("Claude William session")).toBe("claude-william");
    expect(normalize("gc2_wisp.aaa")).toBe("gc2-wisp-aaa");
  });
  it("exact first: 'mayor' is the mayor even though another session contains 'mayor'", () => {
    expect(resolveIn(SESS, "mayor")).toMatchObject({ kind: "one", hit: { id: "gc2-wisp-m1" } });
    expect(resolveIn(SESS, "the mayor")).toMatchObject({ kind: "one", hit: { id: "gc2-wisp-m1" } });
    expect(resolveIn(SESS, "GC2-WISP-S1")).toMatchObject({ kind: "one", hit: { id: "gc2-wisp-s1" } });
  });
  it("a unique prefix, suffix or contains match resolves: 'steve' is crm/steve, 'william' is claude-william, 'helper' the mayor's helper", () => {
    expect(resolveIn(SESS, "steve")).toMatchObject({ kind: "one", hit: { id: "gc2-wisp-s1" } });                 // suffix of crm-steve
    expect(resolveIn(SESS, "crm slash steve")).toMatchObject({ kind: "one", hit: { id: "gc2-wisp-s1" } });       // spoken form, exact
    expect(resolveIn(SESS, "claude")).toMatchObject({ kind: "one", hit: { id: "gc2-wisp-www" } });               // prefix
    expect(resolveIn(SESS, "helper")).toMatchObject({ kind: "one", hit: { id: "gc2-wisp-m2" } });                // contains
  });
  it("ambiguous: more than one candidate at the deciding tier returns all of them", () => {
    const r = resolveIn(SESS, "crm");
    expect(r.kind).toBe("many");
    expect((r as any).hits.map((h: SessionRef) => h.id).sort()).toEqual(["gc2-wisp-b1", "gc2-wisp-s1"]);
    expect(disambiguation((r as any).hits)).toMatch(/^I found crm slash (steve|bob) and crm slash (steve|bob), which one\?$/);
  });
  it("none, too short and unsafe names never match", () => {
    expect(resolveIn(SESS, "nobody")).toMatchObject({ kind: "none" });
    expect(resolveIn(SESS, "st")).toMatchObject({ kind: "none" });          // 1-2 letter fragments are exact-only
    expect(resolveIn(SESS, "a;b")).toMatchObject({ kind: "bad" });
    expect(resolveIn(SESS, "")).toMatchObject({ kind: "bad" });
  });
  it("disambiguation is short and caps long lists", () => {
    const many = Array.from({ length: 7 }, (_, i) => ({ id: `x${i}`, template: `rig/agent${i}` }));
    const s = disambiguation(many);
    expect(s).toContain("and 3 more");
    expect(s.length).toBeLessThan(SPOKEN_MAX);
  });
});

describe("send_to_session: default session, resolution and no send on doubt", () => {
  it("omitting session sends to the configured default session", async () => {
    const w = world(); const { gate } = w.mk();
    const r = await gate.submit(W, undefined, "run the tests");
    expect(r.structured).toMatchObject({ sent: true, session: "gc2-wisp-www" });
    expect(w.posts()[0].path).toBe("/v0/city/gc2/session/gc2-wisp-www/submit");
  });
  it("no default and no session: asks which one and sends nothing", async () => {
    const w = world(); const { gate } = w.mk({ defaultSession: undefined });
    const r = await gate.submit(W, undefined, "hi");
    expect(r.structured).toMatchObject({ sent: false, reason: "no_session_given" });
    expect(r.text).toMatch(/Which session/);
    expect(w.posts().length).toBe(0);
  });
  it("a unique spoken name sends to that session's canonical id", async () => {
    const w = world(); const { gate } = w.mk();
    const r = await gate.submit(W, "steve", "please look at PR 12");
    expect(r.structured).toMatchObject({ sent: true, session: "gc2-wisp-s1" });
    expect(w.posts().map((p) => p.path)).toEqual(["/v0/city/gc2/session/gc2-wisp-s1/submit"]);
    expect(r.text).toContain("crm slash steve");
  });
  it("an ambiguous name asks and does NOT send; a missing one says so and does NOT send", async () => {
    const w = world(); const { gate } = w.mk();
    const amb = await gate.submit(W, "crm", "hello");
    expect(amb.structured).toMatchObject({ sent: false, reason: "ambiguous_session" });
    expect(amb.text).toMatch(/I found crm slash (steve|bob) and crm slash (steve|bob), which one\?/);
    expect((amb.structured as any).candidates.length).toBe(2);
    const none = await gate.submit(W, "nobody", "hello");
    expect(none.structured).toMatchObject({ sent: false, reason: "no_such_session" });
    expect(w.posts().length).toBe(0);
    expect(w.events.map((e) => e.result)).toEqual(["ambiguous_session", "no_such_session"]);
  });
  it("nothing is denied by default (every session is the owner's); an operator's deny list still blocks, even by partial name", async () => {
    const w = world(); const { gate } = w.mk();
    for (const name of ["claude-william", "zachris", "zachris-dev"]) expect((await gate.submit(W, name, `hi ${name}`)).structured, name).toMatchObject({ sent: true });
    expect(w.posts().length).toBe(3);
    const w2 = world(); const { gate: g2 } = w2.mk({ deny: ["zachris-*"] });
    expect((await g2.submit(W, "claude-william", "hi")).structured).toMatchObject({ sent: true });
    expect((await g2.submit(W, "zachris", "hi")).structured).toMatchObject({ sent: false, reason: "denied_session" });   // resolves to zachris-dev, then denied
    expect(w2.posts().length).toBe(1);
  });
  it("a session a person is attached to is written like any other by default; the optional setting refuses it, for every session alike", async () => {
    const w = world(); const { gate } = w.mk();
    expect((await gate.submit(W, "attached-one", "hi")).structured).toMatchObject({ sent: true });
    expect((await gate.submit(W, undefined, "hi")).structured).toMatchObject({ sent: true });
    const w2 = world(); const { gate: g2 } = w2.mk({ allowAttached: false });
    expect((await g2.submit(W, "attached-one", "hi")).structured).toMatchObject({ reason: "session_attached" });
    expect((await g2.submit(W, undefined, "hi")).structured).toMatchObject({ reason: "session_attached" });   // no special case for the default session (claude-william is attached in the fixture)
  });
  it("the acknowledgement is speakable (<= 280 chars) and carries a marker for wait_for_reply", async () => {
    const w = world(); const { gate } = w.mk();
    const r = await gate.submit(W, "steve", "hi");
    expect(r.text.length).toBeLessThanOrEqual(SPOKEN_MAX);
    expect((r.structured as any).marker).toBe(`m${T0}`);
    expect(w.markers.get(W.client, "gc2-wisp-s1")).toBe(T0);
  });
});

describe("idempotent resend: a voice client that retries does not send twice", () => {
  it("concurrent identical sends share one backend POST and handle", async () => {
    const w = world(); const { gate } = w.mk();
    const [a, b] = await Promise.all([gate.submit(W, "steve", "one request"), gate.submit(W, "steve", "one request")]);
    expect(w.posts()).toHaveLength(1);
    expect(a.structured.handle).toMatch(/^h/);
    expect(b.structured.handle).toBe(a.structured.handle);
  });
  it("preserves multiple outstanding boundaries and refuses a different OAuth principal", async () => {
    const w = world(); const { gate, d } = w.mk();
    const owner = { ...W, principal: "oauth-owner" };
    const first = await gate.submit(owner, "steve", "first request");
    w.clock.t += 2000;
    const second = await gate.submit(owner, "steve", "second request");
    expect(first.structured.handle).not.toBe(second.structured.handle);
    w.transcripts["gc2-wisp-s1"] = [asst(T0 + 1000, "Output between sends.")];
    const tool = buildTools(d, owner).find(t => t.name === "wait_for_reply")!;
    const a = await tool.handler({ handle: first.structured.handle, max_seconds: 1 });
    const b = await tool.handler({ handle: second.structured.handle, max_seconds: 1 });
    expect(a.structured).toMatchObject({ reply_available: true, since: `m${T0}`, correlation: "temporal_only", complete: false });
    expect(b.structured).toMatchObject({ reply_available: false, since: `m${T0 + 2000}` });
    const thief = buildTools(d, { ...owner, principal: "oauth-other" }).find(t => t.name === "wait_for_reply")!;
    const denied = await thief.handler({ handle: first.structured.handle, max_seconds: 1 });
    expect(denied.structured).toMatchObject({ found: false, reason: "unknown_handle" });
    expect(w.posts()).toHaveLength(2);
  });

  it("an identical send within 60 s from the same client returns the first acknowledgement, with ONE supervisor call", async () => {
    const w = world(); const { gate } = w.mk();
    const a = await gate.submit(W, "steve", "please run the build");
    w.clock.t += 20_000;
    const b = await gate.submit(W, "steve", "please run the build");
    expect(w.posts().length).toBe(1);
    expect(b.text).toBe(a.text);
    expect(b.structured).toMatchObject({ sent: true, replayed: true, marker: (a.structured as any).marker, idempotency_key: (a.structured as any).idempotency_key });
    expect(w.events.map((e) => e.result)).toEqual(["sent", "replayed"]);
  });
  it("a different text, intent, session or client sends again; so does the same send after the window", async () => {
    const w = world(); const { gate } = w.mk();
    await gate.submit(W, "steve", "run it");
    await gate.submit(W, "steve", "run it again");                                       // other text
    await gate.submit(W, "steve", "run it", "follow_up");                                // other intent
    await gate.submit(W, "bob", "run it");                                               // other session
    await gate.submit({ ...W, client: "Claude (owner client)" }, "steve", "run it");     // other client
    expect(w.posts().length).toBe(5);
    w.clock.t += 61_000;
    await gate.submit(W, "steve", "run it");
    expect(w.posts().length).toBe(6);
  });
  it("every real send carries its own idempotency key to the supervisor; a replay spends no rate limit", async () => {
    const w = world(); const { gate } = w.mk({ perMinute: 2 });
    await gate.submit(W, "steve", "a"); await gate.submit(W, "steve", "a"); await gate.submit(W, "steve", "a");
    const keys = w.posts().map((p) => p.headers["idempotency-key"]);
    expect(keys.length).toBe(1);
    expect(keys[0]).toMatch(/^[0-9a-f-]{36}$/);
    expect((await gate.submit(W, "steve", "b")).structured).toMatchObject({ sent: true });   // the replays above did not use up the 2 per minute
  });
});

describe("session_state: working | idle | waiting for input", () => {
  const state = async (w: ReturnType<typeof world>, session?: string) => (await w.tool(w.mk().d, R, "session_state").handler({ session })).structured as any;
  it("reports each state from the supervisor's own fields and speaks one short sentence", async () => {
    const w = world({ detail: { "gc2-wisp-s1": { turn: { state: "running" } }, "gc2-wisp-b1": { program_status: { state: "idle" } }, "gc2-wisp-m1": { program_status: { state: "blocked", kind: "permission" } } },
      pending: { "gc2-wisp-www": { request_id: "q1", kind: "question", prompt: "Which branch should I use?", options: ["main", "dev"] } } });
    expect((await state(w, "steve")).state).toBe("working");
    expect((await state(w, "bob")).state).toBe("idle");
    expect((await state(w, "mayor")).state).toBe("waiting for input");
    const waiting = await w.tool(w.mk().d, R, "session_state").handler({});          // default session claude-william has a pending question
    expect(waiting.structured).toMatchObject({ state: "waiting for input", waiting_for: { request_id: "q1", prompt: "Which branch should I use?" } });
    expect(waiting.text).toMatch(/waiting for input\. It asks: Which branch/);
    expect(waiting.text.length).toBeLessThanOrEqual(SPOKEN_MAX);
  });
  it("a sleeping session is 'not running'", async () => {
    const w = world();
    expect((await state(w, "sleeper")).state).toBe("not running");
  });
});

describe("wait_for_reply: bounded, since the send, never unbounded", () => {
  it("waits past an acknowledged interim candidate; explicit cursors make lost-response retries repeatable", async () => {
    let publish = false;
    const w = world({ transcripts: { "gc2-wisp-www": [asst(T0 + 500, "I'll check.")] }, onSleep: t => {
      if (publish) w.transcripts["gc2-wisp-www"].push(asst(t, "Tests now pass; here is the result."));
    } });
    const { gate, d } = w.mk();
    const ack = await gate.submit(W, undefined, "check tests");
    const tool = w.tool(d, W, "wait_for_reply");
    const args = { handle: ack.structured.handle, max_seconds: 3 };
    const interim = await tool.handler(args);
    const repeatedInitial = await tool.handler(args); // simulate loss of the first response
    expect(repeatedInitial.structured.reply_id).toBe(interim.structured.reply_id);
    const nextArgs = { ...args, after_reply_id: interim.structured.reply_id };
    const unchanged = await tool.handler(nextArgs);
    expect(unchanged.structured).toMatchObject({ reply_available: false, page: "", waited_ms: 3000, after_reply_id: interim.structured.reply_id });
    expect(unchanged.text).not.toContain("I'll check");
    publish = true;
    const next = await tool.handler(nextArgs);
    expect(next.structured.page).toContain("Tests now pass");
    expect(next.structured.reply_id).not.toBe(interim.structured.reply_id);
    expect(next.structured).toMatchObject({ complete: false, completion: "not_proven" });
    expect(Number(next.structured.waited_ms)).toBeGreaterThanOrEqual(POLL_MS);
    w.transcripts["gc2-wisp-www"].push(asst(w.clock.t + 100, "A later candidate must not replace a lost response."));
    const lostNextRetry = await tool.handler(nextArgs);
    expect(lostNextRetry.structured.reply_id).toBe(next.structured.reply_id);
    expect(w.posts()).toHaveLength(1);
  });
  it("a two-minute budget waits without narrating the same candidate, and reports missing cursor continuity", async () => {
    const w = world({ transcripts: { "gc2-wisp-www": [asst(T0 + 500, "I'll check.")] } });
    const { gate, d } = w.mk();
    const ack = await gate.submit(W, undefined, "check tests");
    const tool = w.tool(d, W, "wait_for_reply");
    const first = await tool.handler({ handle: ack.structured.handle });
    const started = w.clock.t;
    for (let i = 0; i < 8; i++) {
      const next = await tool.handler({ handle: ack.structured.handle, after_reply_id: first.structured.reply_id, max_seconds: 15 });
      expect(next.structured).toMatchObject({ page: "", reply_available: false, waited_ms: 15000 });
      expect(next.text).not.toContain("I'll check");
    }
    expect(w.clock.t - started).toBe(120000);
    w.transcripts["gc2-wisp-www"] = [asst(w.clock.t, "The prior segment has disappeared.")];
    const gap = await tool.handler({ handle: ack.structured.handle, after_reply_id: first.structured.reply_id, max_seconds: 1 });
    expect(gap.structured).toMatchObject({ reason: "reply_cursor_unavailable", reply_available: false, page: "", complete: false });
    expect(gap.text).toContain("Continuity is unknown");
    expect(w.posts()).toHaveLength(1);
  });
  it("a handle-only retry never silently substitutes output when its first candidate was compacted away", async () => {
    const w = world({ transcripts: { "gc2-wisp-www": [asst(T0 + 500, "Initial candidate.")] } });
    const { gate, d } = w.mk();
    const ack = await gate.submit(W, undefined, "check tests");
    const tool = w.tool(d, W, "wait_for_reply"), args = { handle: ack.structured.handle, max_seconds: 1 };
    const first = await tool.handler(args);
    expect(first.structured.page).toBe("Initial candidate.");
    w.transcripts["gc2-wisp-www"] = [asst(T0 + 1000, "Different segment candidate.")];
    const retry = await tool.handler(args);
    expect(retry.structured).toMatchObject({ reply_available: false, reason: "reply_candidate_unavailable", page: "", complete: false });
    expect(retry.text).not.toContain("Different segment candidate");
    expect(w.posts()).toHaveLength(1);
  });
  it("initial handle retries return the first candidate even if later output has arrived", async () => {
    const w = world({ transcripts: { "gc2-wisp-www": [asst(T0 + 500, "First candidate.")] } });
    const { gate, d } = w.mk();
    const ack = await gate.submit(W, undefined, "check tests");
    const tool = w.tool(d, W, "wait_for_reply"), args = { handle: ack.structured.handle };
    const first = await tool.handler(args);
    w.transcripts["gc2-wisp-www"].push(asst(T0 + 1000, "Later candidate."));
    const retry = await tool.handler(args);
    expect(retry.structured.reply_id).toBe(first.structured.reply_id);
    const advanced = await tool.handler({ ...args, after_reply_id: first.structured.reply_id });
    expect(advanced.structured.page).toBe("Later candidate.");
  });
  it("an unchanged one-second wait consumes its wait budget rather than returning in a hot loop", async () => {
    const w = world({ transcripts: { "gc2-wisp-www": [asst(T0 + 500, "I'll check.")] } });
    const { d } = w.mk();
    const tool = w.tool(d, W, "wait_for_reply");
    const first = await tool.handler({ since: `m${T0}`, max_seconds: 1 });
    const next = await tool.handler({ since: `m${T0}`, after_reply_id: first.structured.reply_id, max_seconds: 1 });
    expect(next.structured).toMatchObject({ reply_available: false, waited_ms: 1000, page: "" });
  });

  it("refuses a disappeared handle target even if another alias exactly or partially matches its old ID", async () => {
    for (const alias of ["gc2-wisp-s1", "gc2-wisp-s1-replacement"]) {
      const sessions = [{ ...SESS[1] }];
      const w = world({ sessions, transcripts: { replacement: [asst(T0 + 500, "Other agent private output.")] } });
      const { gate, d } = w.mk();
      const ack = await gate.submit(W, "steve", "one request");
      sessions.splice(0, 1, { id: "replacement", alias, running: true, state: "active" });
      const r = await w.tool(d, W, "wait_for_reply").handler({ handle: ack.structured.handle, max_seconds: 1 });
      expect(r.structured).toMatchObject({ found: false, reason: "handle_target_unavailable" });
      expect(w.calls.filter(c => c.path.endsWith("/transcript"))).toHaveLength(0);
      expect(r.text).not.toContain("private output");
    }
  });

  it("never proves completion from an unchanged interim reply and active session", async () => {
    const w = world({ transcripts: { "gc2-wisp-www": [asst(T0 + 500, "I will investigate and report back.")] } });
    const { d } = w.mk();
    const r = await w.tool(d, W, "wait_for_reply").handler({ since: `m${T0}`, max_seconds: 2 });
    expect(r.structured).toMatchObject({ complete: false, completion: "not_proven", reply_available: true });
    expect(r.text).not.toMatch(/completed|finished/);
  });

  it("returns the first candidate reply after our own send (not an older one)", async () => {
    const w = world({ transcripts: { "gc2-wisp-www": [asst(T0 - 60_000, "An OLD reply from before the send.")] }, onSleep: (t) => { if (t >= T0 + 3000) w.transcripts["gc2-wisp-www"] = [asst(T0 - 60_000, "An OLD reply from before the send."), asst(T0 + 2500, "Done. All 160 tests pass and I pushed the branch.")]; } });
    const { gate, d } = w.mk();
    await gate.submit(W, undefined, "run the tests");
    const r = await w.tool(d, W, "wait_for_reply").handler({});
    expect(r.structured).toMatchObject({ complete: false, completion: "not_proven", reply_available: true });
    expect(r.text).toContain("completion unverified");
    expect(r.text).toContain("All 160 tests pass");
    expect(r.text).not.toContain("OLD reply");
    expect((r.structured as any).waited_ms).toBeLessThanOrEqual(15_000);
    expect(r.text.length).toBeLessThanOrEqual(SPOKEN_MAX);
  });
  it("a reply that arrived BEFORE the wait call is still found, because the send left a marker (not 'from now')", async () => {
    const w = world(); const { gate, d } = w.mk();
    await gate.submit(W, undefined, "run the tests");            // sent at T0
    w.clock.t += 8_000;                                          // the voice client took 8 s to call wait_for_reply
    w.transcripts["gc2-wisp-www"] = [asst(T0 + 2_000, "Finished while you were asking: all green.")];
    const r = await w.tool(d, W, "wait_for_reply").handler({});
    expect(r.structured).toMatchObject({ complete: false, completion: "not_proven", reply_available: true });
    expect(r.text).toContain("all green");
    // another client has no marker for this session, so for it the same reply is older than "now" and is not returned
    const other = await w.tool(d, { client: "Claude (owner client)", scopes: ["gc:read"] }, "wait_for_reply").handler({ max_seconds: 2 });
    expect(other.structured).toMatchObject({ complete: false });
  });
  it("with no reply it returns 'still working' within the bound (default 15 s) and polls a bounded number of times", async () => {
    const w = world(); const { gate, d } = w.mk();
    await gate.submit(W, undefined, "do something long");
    const before = w.calls.length;
    const r = await w.tool(d, W, "wait_for_reply").handler({});
    expect(r.structured).toMatchObject({ complete: false, page: "" });
    expect((r.structured as any).fetches).toBeLessThanOrEqual(3);                        // the transcript is read at most 3 times per wait
    expect(r.text).toMatch(/No new reply/);
    expect((r.structured as any).waited_ms).toBeLessThanOrEqual(15_000);
    const during = w.calls.slice(before);
    expect(during.filter((c) => c.path.endsWith("/transcript")).length).toBeLessThanOrEqual(3);          // big reads: at most 3
    expect(during.filter((c) => c.path.endsWith("/transcript")).every((c) => /[?&]tail=1(&|$)/.test(c.query ?? ""))).toBe(true);        // `tail` is compaction SEGMENTS in the API: always the last one, never 3 or 8 "messages"
    expect(during.length).toBeLessThanOrEqual(Math.ceil(15_000 / POLL_MS) + 3 + 3);                       // cheap session reads every tick, plus the lookup and the 3 transcripts
  });
  it("max_seconds is clamped to 20 whatever the caller asks, and the tool schema refuses more", async () => {
    const w = world(); const { gate, d } = w.mk();
    await gate.submit(W, undefined, "x");
    const t = w.tool(d, W, "wait_for_reply");
    const r = await t.handler({ max_seconds: 9999 });
    expect((r.structured as any).waited_ms).toBeLessThanOrEqual(20_000);
    expect(Object.keys(t.inputShape)).toContain("max_seconds");
    expect(() => (t.inputShape.max_seconds as any).parse(21)).toThrow();
    expect(() => (t.inputShape.max_seconds as any).parse(20)).not.toThrow();
    const short = await t.handler({ max_seconds: 3 });
    expect((short.structured as any).waited_ms).toBeLessThanOrEqual(3_000);
  });
  it("a reply still growing at the deadline is returned as partial: still working, so far", async () => {
    let n = 0;
    const w = world({ detail: { "gc2-wisp-www": { activity: "working" } }, onSleep: (t) => { n++; w.transcripts["gc2-wisp-www"] = [asst(t - 100, "Working on it " + "step ".repeat(n))]; } });
    const { gate, d } = w.mk();
    await gate.submit(W, undefined, "go");
    const r = await w.tool(d, W, "wait_for_reply").handler({ max_seconds: 6 });
    expect(r.structured).toMatchObject({ complete: false });
    expect(r.text).toMatch(/untrusted/);
    expect((r.structured as any).page).toContain("Working on it");
  });
  it("an explicit marker from the send acknowledgement is honoured", async () => {
    const w = world({ transcripts: { "gc2-wisp-www": [asst(T0 + 500, "Reply after the marker.")] } });
    const { d } = w.mk();
    const r = await w.tool(d, W, "wait_for_reply").handler({ since: `m${T0}` });
    expect(r.structured).toMatchObject({ complete: false, completion: "not_proven", reply_available: true });
    const later = await w.tool(d, W, "wait_for_reply").handler({ since: `m${T0 + 10_000}`, max_seconds: 2 });
    expect(later.structured).toMatchObject({ complete: false });
  });
});

describe("latest_reply and read_more: long replies in speakable pieces", () => {
  const long = Array.from({ length: 40 }, (_, i) => `Sentence number ${i + 1} explains one more detail of the change.`).join(" ");
  it("latest_reply speaks the first piece (<= 280 chars) with the full text in structuredContent; read_more continues to the end", async () => {
    const w = world({ transcripts: { "gc2-wisp-www": [asst(T0, long)] } });
    const { d } = w.mk();
    const first = await w.tool(d, R, "latest_reply").handler({});
    expect(first.text.length).toBeLessThanOrEqual(SPOKEN_MAX);
    expect(first.text).toContain("untrusted");
    expect(first.structured).toMatchObject({ found: true, done: false, offset: 0, untrusted: true });
    const fs0 = first.structured as any;
    expect(fs0.reply).toBeUndefined();                                             // the whole reply is NOT re-sent on every call: only the page that was spoken
    expect(fs0.page.length).toBeLessThanOrEqual(SPOKEN_MAX);
    expect(fs0.total_chars).toBe(long.length);
    expect(fs0.reply_id).toMatch(/^[0-9a-f]{12}$/);
    let offset = (first.structured as any).next_offset as number, pieces = [first.text], guard = 0;
    for (;;) {
      const more = await w.tool(d, R, "read_more").handler({ offset, reply_id: fs0.reply_id });
      expect((more.structured as any).reply).toBeUndefined();
      expect(more.text.length).toBeLessThanOrEqual(SPOKEN_MAX);
      if (/That was the end/.test(more.text)) break;
      pieces.push(more.text);
      offset = (more.structured as any).next_offset;
      if ((more.structured as any).done) break;
      if (++guard > 40) throw new Error("read_more never finished");
    }
    const spoken = pieces.join(" ");
    for (const i of [1, 12, 25, 40]) expect(spoken).toContain(`Sentence number ${i} `);
  });
  it("chunkAt breaks at sentence or word boundaries and never loses characters", () => {
    const text = long;
    let off = 0, out = "";
    for (let i = 0; i < 100 && off < text.length; i++) { const c = chunkAt(text, off, 200); out += (out ? " " : "") + c.chunk; off = c.next; if (c.done) break; }
    expect(out.replace(/\s+/g, " ")).toBe(text);
    expect(chunkAt("short one", 0, 200)).toMatchObject({ chunk: "short one", done: true });
  });
  it("redacts secrets and drops links in a reply before anyone hears it", async () => {
    const w = world({ transcripts: { "gc2-wisp-www": [asst(T0, "Use ghp_A1b2C3d4E5f6G7h8I9j0K1l2M3n4 and see https://evil.example/x for details.")] } });
    const r = await w.tool(w.mk().d, R, "latest_reply").handler({});
    expect(r.text).not.toContain("ghp_");
    expect(r.text).not.toContain("evil.example");
    expect(JSON.stringify(r.structured)).not.toContain("ghp_");
  });
  it("a session that has said nothing yet is spoken as such", async () => {
    const w = world();
    expect((await w.tool(w.mk().d, R, "latest_reply").handler({})).text).toMatch(/has not said anything yet/);
  });
});

describe("who may READ a reply: gc:read reads any session; an optional peek list restricts", () => {
  it("by default a read-only token reads any session, the default session needs no special rule", async () => {
    const w = world({ transcripts: { "gc2-wisp-www": [asst(T0, "mine")], "gc2-wisp-s1": [asst(T0, "steve's")], "gc2-wisp-zz": [asst(T0, "zachris private")] } });
    const { d } = w.mk();
    for (const [session, text] of [[undefined, "mine"], ["steve", "steve's"], ["zachris-dev", "zachris private"]] as const) {
      expect((await w.tool(d, R, "latest_reply").handler({ session })).text, String(session)).toContain(text);
    }
  });
  it("with an OPTIONAL peek list: listed sessions are readable, others get the clear reason, for every token", async () => {
    const w = world({ transcripts: { "gc2-wisp-www": [asst(T0, "mine")], "gc2-wisp-m1": [asst(T0, "mayor's")] } });
    const { d } = w.mk({}, { peekAllow: ["mayor"] });
    expect((await w.tool(d, R, "latest_reply").handler({ session: "mayor" })).text).toContain("mayor's");
    for (const who of [R, W]) expect((await w.tool(d, who, "latest_reply").handler({ session: "steve" })).structured).toMatchObject({ allowed: false, reason: "not_in_allow_list" });
    expect((await w.tool(d, R, "latest_reply").handler({})).structured).toMatchObject({ reason: "not_in_allow_list" });   // the default session is just a session: it only fills in an omitted name
  });
  it("ambiguity and unknown names are spoken as a question and read nothing", async () => {
    const w = world(); const { d } = w.mk();
    const amb = await w.tool(d, R, "latest_reply").handler({ session: "crm" });
    expect(amb.text).toMatch(/which one\?/);
    expect(w.calls.some((c) => c.path.endsWith("/transcript"))).toBe(false);
  });
});

describe("every voice-loop tool: read-only, optional session, speakable", () => {
  it("annotations, optional session, and a spoken text within 280 chars for every outcome", async () => {
    const w = world({ transcripts: { "gc2-wisp-www": [asst(T0, "x".repeat(2000))] } });
    const { d } = w.mk();
    for (const name of ["session_state", "wait_for_reply", "latest_reply", "read_more"]) {
      const t = w.tool(d, R, name);
      expect((t.annotations as any).readOnlyHint).toBe(true);
      expect((t.inputShape.session as any).safeParse(undefined).success).toBe(true);
      const r = await t.handler(name === "read_more" ? { offset: 0 } : name === "wait_for_reply" ? { max_seconds: 2 } : {});
      expect(r.text.length, name).toBeLessThanOrEqual(SPOKEN_MAX);
    }
  });
});
