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

function world(over: { transcripts?: Record<string, any[]>; detail?: Record<string, any>; pending?: Record<string, any>; onSleep?: (t: number) => void } = {}) {
  const clock = { t: T0, now() { return clock.t; }, async sleep(ms: number) { clock.t += ms; over.onSleep?.(clock.t); } };
  const calls: { method: string; path: string; body?: any; headers: Record<string, string> }[] = [];
  const transcripts = over.transcripts ?? {};
  const fetchImpl = (async (url: string, init: RequestInit) => {
    const u = new URL(url), p = u.pathname, method = String(init?.method);
    calls.push({ method, path: p, body: init?.body ? JSON.parse(String(init.body)) : undefined, headers: Object.fromEntries(Object.entries((init?.headers ?? {}) as Record<string, string>).map(([k, v]) => [k.toLowerCase(), v])) });
    if (method === "POST") return new Response(JSON.stringify({ request_id: "req-1", status: "accepted" }), { status: 202 });
    if (p.endsWith("/sessions")) return new Response(JSON.stringify({ total: SESS.length, items: SESS }), { status: 200 });
    let m = /\/session\/([^/]+)\/transcript$/.exec(p);
    if (m) return new Response(JSON.stringify({ turns: transcripts[m[1]] ?? [] }), { status: 200 });
    m = /\/session\/([^/]+)\/pending$/.exec(p);
    if (m) return new Response(JSON.stringify({ supported: true, pending: over.pending?.[m[1]] ?? null }), { status: 200 });
    m = /\/session\/([^/]+)$/.exec(p);
    if (m) return new Response(JSON.stringify({ ...(SESS.find((s) => s.id === m![1]) ?? {}), ...(over.detail?.[m[1]] ?? {}) }), { status: 200 });
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
  it("the deny list no longer blocks the owner's own session but still blocks other people's, even by partial name", async () => {
    const w = world(); const { gate } = w.mk();
    expect((await gate.submit(W, "claude-william", "hi")).structured).toMatchObject({ sent: true });
    expect((await gate.submit(W, "zachris", "hi")).structured).toMatchObject({ sent: false, reason: "denied_session" });   // resolves to zachris-dev, then denied
    expect((await gate.submit(W, "zachris-dev", "hi")).structured).toMatchObject({ sent: false, reason: "denied_session" });
    expect(w.posts().length).toBe(1);
  });
  it("a session with a person attached is refused, except the owner's configured default session", async () => {
    const w = world(); const { gate } = w.mk();
    expect((await gate.submit(W, "attached-one", "hi")).structured).toMatchObject({ reason: "session_attached" });
    expect((await gate.submit(W, undefined, "hi")).structured).toMatchObject({ sent: true });   // claude-william is attached in the fixture: it IS the target
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
  it("returns the first complete reply after our own send (not an older one)", async () => {
    const w = world({ transcripts: { "gc2-wisp-www": [asst(T0 - 60_000, "An OLD reply from before the send.")] }, onSleep: (t) => { if (t >= T0 + 3000) w.transcripts["gc2-wisp-www"] = [asst(T0 - 60_000, "An OLD reply from before the send."), asst(T0 + 2500, "Done. All 160 tests pass and I pushed the branch.")]; } });
    const { gate, d } = w.mk();
    await gate.submit(W, undefined, "run the tests");
    const r = await w.tool(d, W, "wait_for_reply").handler({});
    expect(r.structured).toMatchObject({ complete: true });
    expect(r.text).toContain("replied");
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
    expect(r.structured).toMatchObject({ complete: true });
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
    expect(r.structured).toMatchObject({ complete: false, reply: "" });
    expect(r.text).toMatch(/still working/);
    expect((r.structured as any).waited_ms).toBeLessThanOrEqual(15_000);
    expect(w.calls.length - before).toBeLessThanOrEqual(Math.ceil(15_000 / POLL_MS) + 3);
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
    const w = world({ onSleep: (t) => { n++; w.transcripts["gc2-wisp-www"] = [asst(t - 100, "Working on it " + "step ".repeat(n))]; } });
    const { gate, d } = w.mk();
    await gate.submit(W, undefined, "go");
    const r = await w.tool(d, W, "wait_for_reply").handler({ max_seconds: 6 });
    expect(r.structured).toMatchObject({ complete: false });
    expect(r.text).toMatch(/still working\. So far/);
    expect((r.structured as any).reply).toContain("Working on it");
  });
  it("an explicit marker from the send acknowledgement is honoured", async () => {
    const w = world({ transcripts: { "gc2-wisp-www": [asst(T0 + 500, "Reply after the marker.")] } });
    const { d } = w.mk();
    const r = await w.tool(d, W, "wait_for_reply").handler({ since: `m${T0}` });
    expect(r.structured).toMatchObject({ complete: true });
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
    expect((first.structured as any).reply.length).toBe(long.length);              // the full text is there
    let offset = (first.structured as any).next_offset as number, pieces = [first.text], guard = 0;
    for (;;) {
      const more = await w.tool(d, R, "read_more").handler({ offset });
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

describe("who may READ a reply: default session, peek allowlist, or a gc:write holder where writes are permitted", () => {
  it("with a peek allowlist: the default session and listed sessions are readable, others are not for a read-only token", async () => {
    const w = world({ transcripts: { "gc2-wisp-www": [asst(T0, "mine")], "gc2-wisp-s1": [asst(T0, "steve's")], "gc2-wisp-m1": [asst(T0, "mayor's")] } });
    const { d } = w.mk({}, { peekAllow: ["mayor"] });
    expect((await w.tool(d, R, "latest_reply").handler({})).text).toContain("mine");                       // the default session
    expect((await w.tool(d, R, "latest_reply").handler({ session: "mayor" })).text).toContain("mayor's");  // on the allowlist
    expect((await w.tool(d, R, "latest_reply").handler({ session: "steve" })).text).toMatch(/not on my read list/);
  });
  it("a gc:write token may read what it may write to (steve), but never another person's session", async () => {
    const w = world({ transcripts: { "gc2-wisp-s1": [asst(T0, "steve's")], "gc2-wisp-zz": [asst(T0, "zachris private")] } });
    const { d } = w.mk({}, { peekAllow: ["mayor"] });
    expect((await w.tool(d, W, "latest_reply").handler({ session: "steve" })).text).toContain("steve's");
    expect((await w.tool(d, W, "latest_reply").handler({ session: "zachris-dev" })).text).toMatch(/protected list/);
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
