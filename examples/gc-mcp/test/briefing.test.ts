import { describe, it, expect } from "vitest";
import { Client } from "@modelcontextprotocol/sdk/client/index.js";
import { InMemoryTransport } from "@modelcontextprotocol/sdk/inMemory.js";
import { GcClient, GcError } from "../src/gc-client.js";
import { createMcpServer } from "../src/server.js";
import { buildTools, type Deps } from "../src/tools.js";
import { INSTRUCTIONS } from "../src/instructions.js";
import { BRIEFING_MAX } from "../src/briefing.js";
import { WRITE_SCOPE, WriteGate } from "../src/writes.js";

const T0 = 1_800_000_000_000;
const iso = (ms: number) => new Date(ms).toISOString();
const R = { client: "ChatGPT (owner client)", scopes: ["gc:read"] };

interface Fix { status?: any; sessions?: any[]; pending?: any[]; gates?: any[]; beads?: Record<string, string>; events?: any[]; detail?: Record<string, any>; fail?: string[]; seen?: { path: string; query: Record<string, string> }[] }
const S = (id: string, over: any = {}) => ({ id, state: "active", running: true, last_active: iso(T0 - 60_000), ...over });
function city(f: Fix = {}) {
  const seen: { path: string; query: Record<string, string> }[] = f.seen ?? [];
  const fetchImpl = (async (url: string, init: RequestInit) => {
    const u = new URL(url), p = u.pathname.replace(/^\/v0\/city\/[^/]+/, ""), q = Object.fromEntries(u.searchParams);
    seen.push({ path: p, query: q });
    const key = p === "/status" ? "status" : p === "/sessions" ? "sessions" : p === "/pending" ? "pending" : p === "/beads" ? "gates" : p === "/events" ? "events" : p.startsWith("/bead/") ? "beads" : p.startsWith("/session/") ? "detail" : "other";
    if (f.fail?.includes(key)) return new Response(JSON.stringify({ detail: "down" }), { status: 503 });
    if (key === "status") return new Response(JSON.stringify(f.status ?? { name: "gc3", agents: { running: 8, total: 10, quarantined: 0 }, work: { in_progress: 3, ready: 5 }, mail: { unread: 3 } }), { status: 200 });
    if (key === "sessions") return new Response(JSON.stringify({ total: (f.sessions ?? []).length, items: f.sessions ?? [] }), { status: 200 });
    if (key === "pending") return new Response(JSON.stringify({ items: f.pending ?? [], total: (f.pending ?? []).length }), { status: 200 });
    if (key === "gates") return new Response(JSON.stringify({ items: f.gates ?? [], total: (f.gates ?? []).length }), { status: 200 });
    if (key === "events") return new Response(JSON.stringify({ items: f.events ?? [], total: (f.events ?? []).length }), { status: 200 });
    if (key === "beads") { const id = decodeURIComponent(p.slice("/bead/".length)); return f.beads?.[id] ? new Response(JSON.stringify({ id, title: f.beads[id], status: "in_progress" }), { status: 200 }) : new Response("{}", { status: 404 }); }
    if (key === "detail") { const m = /^\/session\/([^/]+)(\/pending)?$/.exec(p); if (m?.[2]) return new Response(JSON.stringify({ pending: null, supported: true }), { status: 200 }); const s = (f.sessions ?? []).find((x) => x.id === m?.[1]); return new Response(JSON.stringify({ ...(s ?? {}), ...(f.detail?.[m?.[1] ?? ""] ?? {}) }), { status: 200 }); }
    return new Response("{}", { status: 200 });
  }) as unknown as typeof fetch;
  const clock = { t: T0, now() { return clock.t; }, async sleep(ms: number) { clock.t += ms; } };
  const gc = new GcClient({ baseUrl: "http://127.0.0.1:1", city: "gc3", fetchImpl });
  const deps: Deps = { gc, mailAgent: "human", now: () => new Date(clock.t), clock, briefings: new Map(), defaultSession: "claude-william" };
  const tool = (name: string, who = R) => buildTools(deps, who).find((t) => t.name === name)!;
  return { deps, clock, seen, tool, brief: (who = R) => tool("briefing", who).handler({}) };
}
const FIXTURE: Fix = {
  sessions: [
    S("gc2-wisp-s1", { template: "crm/steve", activity: "working", active_bead: "gcw-a111", last_active: iso(T0 - 10_000) }),
    S("gc2-wisp-d1", { template: "gas-city-infra/devops", alias: "devops", activity: "working", active_bead: "gci-b222", last_active: iso(T0 - 20_000) }),
    S("gc2-wisp-m1", { template: "mayor", alias: "mayor", activity: "working", active_bead: "gcw-c333", last_active: iso(T0 - 90_000) }),
    S("gc2-wisp-w1", { template: "claude-william", alias: "claude-william", activity: "idle" }),
    S("gc2-wisp-z1", { template: "zachris-dev", alias: "zachris-dev", activity: "idle" }),
    S("gc2-wisp-q1", { template: "crm/bob", activity: "idle" }),                                  // waiting for input (listed in /pending)
    S("gc2-wisp-x1", { template: "gas-city-wbern/architect", state: "stranded", activity: "idle" }),   // stuck
    S("gc2-wisp-sl", { template: "sleeper", state: "asleep", running: false }),
  ],
  pending: [{ kind: "question", request_id: "q-1", session_id: "gc2-wisp-q1" }],
  gates: [
    { id: "gcw-g001", title: "Pick the database for gcw-ti563", await_type: "human", status: "open", issue_type: "gate" },
    { id: "gcw-g002", title: "Wait for CI on PR 12", await_type: "gh:run", status: "open", issue_type: "gate" },
    { id: "gcw-g003", title: "Approve the production deploy", await_type: "human", status: "open", issue_type: "gate" },
  ],
  beads: { "gcw-a111": "Fix the login redirect (gcw-a111)", "gci-b222": "Renew the certificate for mcp.gc3", "gcw-c333": "Write the quarterly plan" },
  status: { name: "gc3", agents: { running: 7, total: 9, quarantined: 1 }, work: { in_progress: 3, ready: 5 }, mail: { unread: 3 } },
  events: [{ type: "bead.closed" }, { type: "bead.closed" }, { type: "bead.created" }, { type: "session.woke" }, { type: "mail.sent" }],
};
const sentences = (t: string) => t.split(/(?<=[.!?])\s+/).filter(Boolean);
const ID_LIKE = /\b[a-z0-9]+-wisp-[a-z0-9]+\b|\b(?:gc[a-z0-9]{0,3}|[a-z]{2,4})-(?=[a-z0-9]*\d)[a-z0-9]{4,12}\b|[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}/i;

describe("briefing: how are my agents doing, in one call", () => {
  it("counts working, idle and waiting; names the busiest by what they work on; says what needs the owner FIRST", async () => {
    const c = city(FIXTURE);
    const r = await c.brief();
    expect(r.structured).toMatchObject({ counts: { working: 3, idle: 2, waiting: 1, stuck: 1, not_running: 1 } });
    expect(r.text.startsWith("Needs you:")).toBe(true);
    expect(r.text).toMatch(/3 working, 2 idle/);
    expect(r.text).toMatch(/1 waiting/);
    expect(r.text).toContain("3 unread mail");
    // busiest: most recently active first, by bead TITLE, names spoken naturally
    expect(r.text).toMatch(/crm steve.*login redirect/);
    expect(r.text).toMatch(/devops.*certificate/);
    expect(r.text.indexOf("login redirect")).toBeLessThan(r.text.indexOf("certificate"));
    expect((r.structured as any).busiest.map((b: any) => b.name)).toEqual(["crm steve", "devops", "mayor"]);
  });
  it("attention items are ordered: decisions for the owner, then agents waiting for input, then failed or stuck work", async () => {
    const r = await city(FIXTURE).brief();
    const att = (r.structured as any).attention as { kind: string; count: number }[];
    expect(att.map((a) => a.kind)).toEqual(["decision", "waiting_for_input", "stuck"]);
    expect(att[0].count).toBe(2);                                  // only the HUMAN gates; the CI gate is not the owner's
    expect(r.text).toMatch(/Needs you: 2 decisions/);
    expect(r.text.indexOf("decision")).toBeLessThan(r.text.indexOf("waiting for input"));
    expect(r.text).toMatch(/Pick the database/);
    expect(r.text).not.toMatch(/Wait for CI/);
  });
  it("when nothing needs the owner it says so, plainly", async () => {
    const c = city({ ...FIXTURE, pending: [], gates: [], sessions: FIXTURE.sessions!.filter((s) => s.state !== "stranded"), status: { ...FIXTURE.status, agents: { running: 7, total: 9, quarantined: 0 } } });
    const r = await c.brief();
    expect(r.text.startsWith("Nothing needs you")).toBe(true);
    expect((r.structured as any).attention).toEqual([]);
  });
  it("is 3 to 5 short spoken sentences within the length cap, even for a huge city", async () => {
    const r = await city(FIXTURE).brief();
    expect(BRIEFING_MAX).toBe(400);
    expect(r.text.length).toBeLessThanOrEqual(400);
    expect(sentences(r.text).length).toBeGreaterThanOrEqual(3);
    expect(sentences(r.text).length).toBeLessThanOrEqual(5);
    const many = Array.from({ length: 60 }, (_, i) => S(`gc2-wisp-b${i}`, { template: `rig-with-a-long-name/agent-number-${i}`, activity: "working", active_bead: `gcw-z${i}00`, last_active: iso(T0 - i * 1000) }));
    const beads = Object.fromEntries(many.map((m, i) => [m.active_bead, `A very long title about task number ${i} that goes on and on and on and on and on and on and on`]));
    const gates = Array.from({ length: 30 }, (_, i) => ({ id: `gcw-h${i}00`, title: `Decision number ${i} with an unreasonably long description of what is being asked of you today`, await_type: "human", status: "open" }));
    const big = await city({ ...FIXTURE, sessions: many, beads, gates, pending: many.slice(0, 10).map((m) => ({ kind: "question", request_id: "x", session_id: m.id })) }).brief();
    expect(big.text.length).toBeLessThanOrEqual(400);
    expect(sentences(big.text).length).toBeLessThanOrEqual(5);
    expect(big.text.startsWith("Needs you:")).toBe(true);           // attention survives truncation
  });
  it("never reads out session ids, bead ids or uuids, and speaks names naturally", async () => {
    const c = city({ ...FIXTURE, sessions: [...FIXTURE.sessions!, S("gc2-wisp-n1", { activity: "working", active_bead: "gcw-d444" })], beads: { ...FIXTURE.beads, "gcw-d444": "Do 3f2a9c1e-1b2c-4d5e-8f90-a1b2c3d4e5f6 and gcw-ti563 things" } });
    const r = await c.brief();
    expect(r.text).not.toMatch(ID_LIKE);
    expect(r.text).not.toMatch(/gc2-wisp|gcw-|gci-|\//);
    expect(r.text).toContain("crm steve");
    expect(r.text).not.toContain("crm/steve");
    expect(r.text).toMatch(/an unnamed agent|unnamed/);              // a session with no name is not announced by its id
  });
  it("what changed since the last briefing: first call looks back 12 hours, later calls look back to the previous briefing, per client", async () => {
    const c = city(FIXTURE);
    await c.brief();
    expect(c.seen.find((s) => s.path === "/events")!.query.since).toBe("12h");
    c.seen.length = 0;
    c.clock.t += 2 * 3_600_000;
    const second = await c.brief();
    expect(c.seen.find((s) => s.path === "/events")!.query.since).toBe("120m");
    expect(second.text).toMatch(/Since your last briefing 2 hours ago/);
    expect(second.text).toMatch(/2 beads closed/);
    c.seen.length = 0;
    await c.brief({ client: "Claude (owner client)", scopes: ["gc:read"] });      // another client has its own memory
    expect(c.seen.find((s) => s.path === "/events")!.query.since).toBe("12h");
    expect((second.structured as any).since_ms).toBe(2 * 3_600_000);
  });
  it("an unavailable source is named in one clause and the rest is still said; nothing is invented", async () => {
    const r = await city({ ...FIXTURE, fail: ["gates"] }).brief();
    expect(r.text).toMatch(/couldn't read the decisions/);
    expect((r.structured as any).unavailable).toEqual(["decisions"]);
    expect(r.text).toMatch(/3 working/);                                   // sessions still counted
    expect(r.text).not.toMatch(/Pick the database/);                         // and no decision is made up
    const two = await city({ ...FIXTURE, fail: ["sessions", "events"] }).brief();
    expect((two.structured as any).unavailable.sort()).toEqual(["recent changes", "sessions"]);
    expect(two.text).toMatch(/couldn't read the sessions and the recent changes/);
    expect(two.text).toContain("3 unread mail");                           // status still answers
    expect(two.text.length).toBeLessThanOrEqual(400);
    const none = await city({ fail: ["status", "sessions", "pending", "gates", "events"] }).brief();
    expect(none.text.length).toBeLessThanOrEqual(400);
    expect(none.text).toMatch(/couldn't read/);
    expect((none.structured as any).unavailable.length).toBe(5);
  });
  it("is a read tool with no required arguments, and a read-only token may call it", () => {
    const t = city(FIXTURE).tool("briefing");
    expect((t.annotations as any).readOnlyHint).toBe(true);
    for (const shape of Object.values(t.inputShape)) expect((shape as any).safeParse(undefined).success).toBe(true);
  });
});

describe("what_is_working_on: 'what is steve working on?'", () => {
  it("resolves a spoken name or the default session and answers in 1-2 sentences with the bead TITLE", async () => {
    const c = city(FIXTURE);
    const r = await c.tool("what_is_working_on").handler({ session: "steve" });
    expect(r.text).toMatch(/^crm steve is working on Fix the login redirect/);
    expect(r.text).not.toMatch(ID_LIKE);
    expect(r.text.length).toBeLessThanOrEqual(280);
    expect(sentences(r.text).length).toBeLessThanOrEqual(2);
    expect(r.structured).toMatchObject({ found: true, state: "working" });
    const idle = await c.tool("what_is_working_on").handler({});                  // default session: claude-william, idle
    expect(idle.text).toMatch(/^claude william is idle/);
    expect(idle.text.length).toBeLessThanOrEqual(280);
  });
  it("asks when a name is ambiguous or unknown and says what a sleeping or waiting agent is doing", async () => {
    const c = city({ ...FIXTURE, sessions: [...FIXTURE.sessions!, S("gc2-wisp-s2", { template: "crm/stella", activity: "working" })] });
    expect((await c.tool("what_is_working_on").handler({ session: "crm" })).text).toMatch(/which one\?/);
    expect((await c.tool("what_is_working_on").handler({ session: "nobody" })).text).toMatch(/No session matches/);
    expect((await c.tool("what_is_working_on").handler({ session: "sleeper" })).text).toMatch(/not running/);
    const w = await c.tool("what_is_working_on").handler({ session: "bob" });
    expect(w.text).toMatch(/waiting for input|waiting for you/);
  });
});

describe("the server tells a spoken assistant how to behave (initialize instructions)", () => {
  it("instructions travel in the initialize result and say what a voice assistant needs", async () => {
    const server = createMcpServer(city(FIXTURE).deps, undefined, R);
    const [a, b] = InMemoryTransport.createLinkedPair();
    const client = new Client({ name: "t", version: "1" });
    await Promise.all([server.connect(b), client.connect(a)]);
    const text = client.getInstructions() ?? "";
    expect(text).toBe(INSTRUCTIONS);
    for (const phrase of [/briefing/, /how are (my )?(things|agents)/i, /short.*sentences/i, /never read.*(ids|paths)/i, /want more/i, /read_more/, /needs? the owner|needs? you/i, /first/i,
      /send_to_session/, /wait_for_reply/, /one short sentence/i, /untrusted/i, /not instructions/i]) expect(text, String(phrase)).toMatch(phrase);
    expect(text.length).toBeLessThan(1800);
    await client.close();
  });
});

describe("tool titles and descriptions are plain spoken intent", () => {
  const all = () => buildTools({ ...city(FIXTURE).deps, writes: new WriteGate(new GcClient({ baseUrl: "http://127.0.0.1:1", city: "gc3", fetchImpl: (async () => new Response("{}")) as unknown as typeof fetch }), { enabled: true }, () => undefined) }, { client: "c", scopes: ["gc:read", WRITE_SCOPE] });
  it("the key tools carry the titles a person would say", () => {
    const titles = Object.fromEntries(all().map((t) => [t.name, t.title]));
    expect(titles.briefing).toBe("Ask how the agents are doing");
    expect(titles.send_to_session).toBe("Say something to an agent");
    expect(titles.what_is_working_on).toBe("What is an agent working on");
    expect(titles.wait_for_reply).toMatch(/answer|reply/i);
    expect(titles.respond_to_session).toMatch(/waiting/i);
  });
  it("every tool has a title and a short description that says WHEN to use it; no jargon like 'operation' or 'endpoint'", () => {
    const tools = all();
    expect(tools.length).toBeGreaterThanOrEqual(13);
    for (const t of tools) {
      expect(t.title.length, t.name).toBeGreaterThan(5);
      expect(t.description.length, t.name).toBeGreaterThan(40);
      expect(t.description.length, t.name).toBeLessThanOrEqual(420);
      expect(t.description, t.name).toMatch(/\b(use|when|ask|say|answer|tell|check|find|look|read|wait|send)\b/i);
      expect(t.description, t.name).not.toMatch(/\b(endpoint|operation|api|json|payload|supervisor)\b/i);
    }
    const by = Object.fromEntries(tools.map((t) => [t.name, t.description]));
    expect(by.briefing).toMatch(/how are (my )?agents doing/i);
    expect(by.briefing).toMatch(/first/i);
    expect(by.send_to_session).toMatch(/wait_for_reply/);
    expect(by.what_is_working_on).toMatch(/what is .* working on/i);
    expect(by.latest_reply).toMatch(/last said|newest/i);
  });
});
