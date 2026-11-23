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

interface Fix { status?: any; sessions?: any[]; pending?: any[]; gates?: any[]; beads?: Record<string, string>; events?: any[]; detail?: Record<string, any>; mail?: { items: any[]; total?: number }; inprogress?: any[]; fail?: string[]; seen?: { path: string; query: Record<string, string> }[] }
const S = (id: string, over: any = {}) => ({ id, state: "active", running: true, last_active: iso(T0 - 60_000), ...over });
function city(f: Fix = {}) {
  const seen: { path: string; query: Record<string, string> }[] = f.seen ?? [];
  const fetchImpl = (async (url: string, init: RequestInit) => {
    const u = new URL(url), p = u.pathname.replace(/^\/v0\/city\/[^/]+/, ""), q = Object.fromEntries(u.searchParams);
    seen.push({ path: p, query: q });
    const key = p === "/status" ? "status" : p === "/sessions" ? "sessions" : p === "/pending" ? "pending" : p === "/beads" ? (q.type === "gate" ? "gates" : "inprogress") : p === "/events" ? "events" : p === "/mail" ? "mail" : p.startsWith("/bead/") ? "beads" : p.startsWith("/session/") ? "detail" : "other";
    if (f.fail?.includes(key)) return new Response(JSON.stringify({ detail: "down" }), { status: 503 });
    if (key === "status") return new Response(JSON.stringify(f.status ?? { name: "gc3", agents: { running: 8, total: 10, quarantined: 0 }, work: { in_progress: 3, ready: 5 }, mail: { unread: 3 } }), { status: 200 });
    if (key === "sessions") return new Response(JSON.stringify({ total: (f.sessions ?? []).length, items: f.sessions ?? [] }), { status: 200 });
    if (key === "pending") return new Response(JSON.stringify({ items: f.pending ?? [], total: (f.pending ?? []).length }), { status: 200 });
    if (key === "gates") return new Response(JSON.stringify({ items: f.gates ?? [], total: (f.gates ?? []).length }), { status: 200 });
    if (key === "inprogress") return new Response(JSON.stringify({ items: f.inprogress ?? [], total: (f.inprogress ?? []).length }), { status: 200 });
    if (key === "mail") return new Response(JSON.stringify({ items: f.mail?.items ?? [], total: f.mail?.total ?? (f.mail?.items ?? []).length }), { status: 200 });
    if (key === "events") return new Response(JSON.stringify({ items: f.events ?? [], total: (f.events ?? []).length }), { status: 200 });
    if (key === "beads") { const id = decodeURIComponent(p.slice("/bead/".length)); return f.beads?.[id] ? new Response(JSON.stringify({ id, title: f.beads[id], status: "in_progress" }), { status: 200 }) : new Response("{}", { status: 404 }); }
    if (key === "detail") { const m = /^\/session\/([^/]+)(\/pending)?$/.exec(p); if (m?.[2]) return new Response(JSON.stringify({ pending: null, supported: true }), { status: 200 }); const s = (f.sessions ?? []).find((x) => x.id === m?.[1]); return new Response(JSON.stringify({ ...(s ?? {}), ...(f.detail?.[m?.[1] ?? ""] ?? {}) }), { status: 200 }); }
    return new Response("{}", { status: 200 });
  }) as unknown as typeof fetch;
  const clock = { t: T0, now() { return clock.t; }, async sleep(ms: number) { clock.t += ms; } };
  const gc = new GcClient({ baseUrl: "http://127.0.0.1:1", city: "gc3", fetchImpl });
  const deps: Deps = { gc, mailAgent: "human", now: () => new Date(clock.t), clock, briefings: new Map(), defaultSession: "claude-william", ownerName: "William" };
  const tool = (name: string, who = R) => buildTools(deps, who).find((t) => t.name === name)!;
  return { deps, clock, seen, tool, brief: (who = R) => tool("briefing", who).handler({}) };
}
const closedEv = (title: string, type = "task") => ({ type: "bead.closed", payload: { bead: { title, issue_type: type, status: "closed" } } });
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
    { id: "gcw-g001", title: "Gate: Pick the database for gcw-ti563", await_type: "human", status: "open", issue_type: "gate", created_at: iso(T0 - 3_600_000) },
    { id: "gcw-g002", title: "Wait for CI on PR 12", await_type: "gh:run", status: "open", issue_type: "gate", created_at: iso(T0 - 7_200_000) },
    { id: "gcw-g003", title: "Approve the production deploy", await_type: "human", status: "open", issue_type: "gate", created_at: iso(T0 - 7_200_000) },
  ],
  beads: { "gcw-a111": "Fix the login redirect (gcw-a111)", "gci-b222": "Renew the certificate for mcp.gc3", "gcw-c333": "Write the quarterly plan" },
  status: { name: "gc3", agents: { running: 7, total: 9, quarantined: 1 }, work: { in_progress: 3, ready: 5 }, mail: { unread: 3 } },
  mail: { items: [{ from: "gc-architect", subject: "Re: Assigned thing", created_at: iso(T0) }, { from: "devops", subject: "FYI done", created_at: iso(T0) }, { from: "crm/steve", subject: "hello", created_at: iso(T0) }], total: 3 },
  events: [closedEv("Fix the flaky login test"), closedEv("Update the pricing page"), closedEv("order:gc3-merged-pr-open-bead"), closedEv("crm/gastown.ux-reviewer-2")],
};
const sentences = (t: string) => t.split(/(?<=[.!?])\s+/).filter(Boolean);
const ID_LIKE = /\b[a-z0-9]+-wisp-[a-z0-9]+\b|\b(?:gc[a-z0-9]{0,3}|[a-z]{2,4})-(?=[a-z0-9]*\d)[a-z0-9]{4,12}\b|[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}/i;

const CALM: Fix = { ...FIXTURE, pending: [], sessions: FIXTURE.sessions!.filter((s) => s.state !== "stranded" && s.id !== "gc2-wisp-q1"), status: { ...FIXTURE.status, agents: { running: 7, total: 9, quarantined: 0 } } };

describe("briefing: how are my agents doing, in one call", () => {
  it("says what needs the owner first, then who is working and on what, then what finished, then mail", async () => {
    const r = await city(CALM).brief();
    expect(r.structured).toMatchObject({ counts: { working: 3, idle: 2, waiting: 0, stuck: 0, not_running: 1 } });
    expect(r.text.startsWith("Needs you:")).toBe(true);
    expect(r.text).toMatch(/3 agents are working: crm steve on Fix the login redirect, devops on Renew the certificate for mcp.gc3 and mayor on Write the quarterly plan\./);
    expect(r.text).toMatch(/In the last 12 hours, 2 tasks finished, including Fix the flaky login test and Update the pricing page\./);
    expect(r.text).toContain("3 unread mail");
    const order = ["Needs you:", "agents are working", "tasks finished", "unread mail"].map((k) => r.text.indexOf(k));
    expect(order.every((i) => i >= 0)).toBe(true);
    expect([...order].sort((a, b) => a - b)).toEqual(order);
    expect(r.text).not.toMatch(/\bidle\b/);                      // a count of idle agents is not news
    expect((r.structured as any).busiest.map((b: any) => b.name)).toEqual(["crm steve", "devops", "mayor"]);
  });
  it("attention items are ordered: decisions for the owner, then agents waiting for input, then failed or stuck work, then mail for the owner", async () => {
    const f = { ...FIXTURE, mail: { items: [{ from: "mayor", subject: "Plan review needs you", created_at: iso(T0) }, { from: "human", subject: "Reminder — human gate still open: crm-r296os" }], total: 2 } };
    const r = await city(f).brief();
    const att = (r.structured as any).attention as { kind: string; count: number }[];
    expect(att.map((a) => a.kind)).toEqual(["decision", "waiting_for_input", "stuck", "mail"]);
    expect(att[0].count).toBe(2);                                  // only the HUMAN gates; the CI gate is not the owner's
    expect(att[3].count).toBe(1);                                  // the system's own reminder is not mail for the owner
    expect(r.text).toMatch(/Needs you: 2 decisions, newest first: /);
    const t = r.text;
    expect([t.indexOf("decision"), t.indexOf("waiting for input"), t.indexOf("stuck"), t.indexOf("mail about")].every((i, k, a) => i >= 0 && (k === 0 || i > a[k - 1]))).toBe(true);
    expect(t).toMatch(/Pick the database/);
    expect(t).not.toMatch(/Wait for CI/);
    expect(t).toMatch(/mail about Plan review needs you from mayor/);
    expect(t).not.toMatch(/Reminder/);
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
    const gates = Array.from({ length: 30 }, (_, i) => ({ id: `gcw-h${i}00`, title: `Decision number ${i} with an unreasonably long description of what is being asked of you today`, await_type: "human", status: "open", created_at: iso(T0 - i * 60_000) }));
    const big = await city({ ...FIXTURE, sessions: many, beads, gates, pending: many.slice(0, 10).map((m) => ({ kind: "question", request_id: "x", session_id: m.id })) }).brief();
    expect(big.text.length).toBeLessThanOrEqual(400);
    expect(sentences(big.text).length).toBeLessThanOrEqual(5);
    expect(big.text.startsWith("Needs you:")).toBe(true);           // attention survives truncation
  });
  it("never reads out session ids, bead ids, uuids, hashes or raw agent paths, and speaks names naturally", async () => {
    const c = city({ ...FIXTURE, sessions: [...FIXTURE.sessions!, S("gc2-wisp-n1", { activity: "working", active_bead: "gcw-d444" }), S("gc2-wisp-h1", { alias: "slay-chat-v9-1bb9b9ad248e802cb4674560cb3840aea007f749", activity: "in-turn" })], beads: { ...FIXTURE.beads, "gcw-d444": "Do 3f2a9c1e-1b2c-4d5e-8f90-a1b2c3d4e5f6 and gcw-ti563 things at bf8c4fa" } });
    const r = await c.brief();
    expect(r.text).not.toMatch(ID_LIKE);
    expect(r.text).not.toMatch(/gc2-wisp|gcw-|gci-|\/|[0-9a-f]{10,}|bf8c4fa|…/);
    expect(r.text).toContain("crm steve");
    expect(r.text).not.toContain("crm/steve");
  });
  it("what changed: first call looks back 12 hours, later calls back to the previous briefing, per client; closed beads are asked for by type", async () => {
    const c = city(FIXTURE);
    await c.brief();
    const first = c.seen.find((s) => s.path === "/events")!;
    expect(first.query.since).toBe("12h");
    expect(first.query.type).toBe("bead.closed");
    c.seen.length = 0;
    c.clock.t += 2 * 3_600_000;
    const second = await c.brief();
    expect(c.seen.find((s) => s.path === "/events")!.query.since).toBe("120m");
    expect(second.text).toMatch(/Since your last briefing 2 hours ago, 2 tasks finished/);
    c.seen.length = 0;
    await c.brief({ client: "Claude (owner client)", scopes: ["gc:read"] });      // another client has its own memory
    expect(c.seen.find((s) => s.path === "/events")!.query.since).toBe("12h");
    expect((second.structured as any).since_ms).toBe(2 * 3_600_000);
  });
  it("an unavailable source is named in one clause and the rest is still said; nothing is invented", async () => {
    const r = await city({ ...FIXTURE, fail: ["gates"] }).brief();
    expect(r.text).toMatch(/couldn't read the decisions/);
    expect((r.structured as any).unavailable).toEqual(["decisions"]);
    expect(r.text).toMatch(/3 agents are working/);                          // sessions still said
    expect(r.text).not.toMatch(/Pick the database/);                         // and no decision is made up
    const two = await city({ ...FIXTURE, fail: ["sessions", "events"] }).brief();
    expect((two.structured as any).unavailable).toEqual(["sessions", "recent changes"]);
    expect(two.text).toMatch(/couldn't read the sessions and the recent changes/);
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

// ---------------------------------------------------------------- today's noisy shape from the LIVE supervisor (49 identical "Gate: human", 3016 unread, closed-bead noise)
describe("briefing on a noisy real-world city: calm, not alarming", () => {
  const day = 86_400_000;
  const reasons = ["Branch temp/listing-inventory-flash-s4 (PR #2759) is checked out in live attached tmux session list", "UX review needs the admin_faser Statsig gate open for the Demo AB tenant so Admin shows Affärssteg",
    "Bead already satisfied on origin/main; polecats cannot close work beads. Close it"];
  const noisyGates = [
    ...Array.from({ length: 46 }, (_, i) => ({ id: `crm-g${i}xxxx`, title: "Gate: human", await_type: "human", status: "open", created_at: iso(T0 - 600_000 - (i % 3) * 60_000 - i * day / 4), description: `Ad-hoc gate blocking crm-b${i}yyyy\n\nReason: ${reasons[i % 3]}` })),
    { id: "crm-gz1", title: "Gate: William decision on verified-phone permit boundary", await_type: "human", status: "open", created_at: iso(T0 - 5_000), description: "" },
    { id: "crm-gz2", title: "Operational preview admission for PR2237", await_type: "human", status: "open", created_at: iso(T0 - 9_000), description: "Decision needed: approve the preview for the pricing change" },
    { id: "crm-gz3", title: "Gate: human", await_type: "human", status: "open", created_at: iso(T0 - 99 * day), description: "no reason line at all" },
    { id: "crm-gz4", title: "Gate: gh:run 40926139417", await_type: "gh:run", status: "open", created_at: iso(T0 - 1_000) },
  ];
  const noisyMail = { items: Array.from({ length: 50 }, () => ({ from: "human", subject: "Reminder — human gate still open: crm-r296os" })), total: 3016 };
  const noisyEvents = [...Array.from({ length: 90 }, (_, i) => closedEv(`order:gc3-job-${i}`)), ...Array.from({ length: 5 }, () => closedEv("crm/gastown.ux-reviewer-2")), closedEv("Align contact detail groups and comments"), closedEv("Fix the flaky login test"), closedEv("Update the pricing page"),
    closedEv("sling-crm-fzws1h"), closedEv("Judge every segment against the ADRs", "step"), closedEv("orphan-sweep: reset 1 orphaned beads"), closedEv("mol-polecat-work cleanup step")];
  const noisy: Fix = { ...FIXTURE, gates: noisyGates, mail: noisyMail, events: noisyEvents, pending: [], sessions: FIXTURE.sessions!.filter((s) => s.state !== "stranded"), status: { name: "gc2", agents: { running: 22, total: 188, quarantined: 0 }, work: { in_progress: 97, ready: 1394 }, mail: { unread: 3016 } } };

  it("never reads a raw gate title: each decision is described by its question, deduplicated, newest first, at most two named", async () => {
    const r = await city(noisy).brief();
    expect(r.text).not.toMatch(/Gate:|Gate human|gate: human/i);
    expect(r.text).not.toMatch(/Ad-hoc|blocking|crm-/);
    expect(r.text).toMatch(/Needs you: 49 open decisions, but many look stale: the oldest is 99 days old\. Newest: /);
    const newest = /Newest: ([^.]*)\./.exec(r.text)![1].split("; ");
    expect(newest.length).toBe(2);
    expect(newest[0]).toMatch(/^your decision on verified phone permit boundary/i);       // the newest gate has a real title: used (Gate: and William removed)
    expect(newest[1]).toMatch(/approve the preview/);                                          // an explicit "Decision needed:" line beats the title
    expect(new Set(newest).size).toBe(2);
    expect((r.structured as any).decisions).toMatchObject({ open: 49, stale: true });
  });
  it("describes a generic 'Gate: human' by its Reason line, and falls back to the blocked bead's title", async () => {
    const only = (gates: any[], beads: Record<string, string> = {}) => city({ ...noisy, gates, beads: { ...noisy.beads, ...beads } }).brief();
    const a = await only(noisyGates.filter((g) => String(g.id).startsWith("crm-g") && g.id === "crm-g0xxxx"));
    expect(a.text).toMatch(/Needs you: 1 decision, newest first: Branch temp listing inventory flash s4 is checked out/);
    expect(a.text).not.toMatch(/stale/);
    const b = await only([{ id: "g1", title: "Gate: human", await_type: "human", status: "open", created_at: iso(T0), description: "Ad-hoc gate blocking crm-qq1zz" }], { "crm-qq1zz": "Ship the pricing page" });
    expect(b.text).toMatch(/the work on Ship the pricing page/);
  });
  it("the stale warning appears only past ten, with the age of the OLDEST; ten or fewer are just counted", async () => {
    const some = (n: number) => noisyGates.slice(0, n).map((g, i) => ({ ...g, created_at: iso(T0 - i * 3_600_000) }));
    const ten = await city({ ...noisy, gates: some(10) }).brief();
    expect(ten.text).toMatch(/Needs you: 10 decisions, newest first:/);
    expect(ten.text).not.toMatch(/stale/);
    const eleven = await city({ ...noisy, gates: some(11) }).brief();
    expect(eleven.text).toMatch(/11 open decisions, but many look stale: the oldest is 10 hours old/);
  });
  it("does not headline a pile of notices: no number, and nothing 'flagged' from the system's own reminders", async () => {
    const r = await city(noisy).brief();
    expect(r.text).not.toMatch(/3016|3,016|unread mail/);
    expect((r.structured as any).mail).toMatchObject({ unread: 3016, flagged: 0 });
    const quiet = await city({ ...noisy, gates: [], events: [], sessions: [S("gc2-wisp-w1", { alias: "claude-william", activity: "idle" })] }).brief();
    expect(quiet.text).toMatch(/A lot of unread notices, nothing flagged for you\./);
  });
  it("says 'N tasks finished' counting only real tasks, naming at most two by title, never automation or agent identities", async () => {
    const r = await city(noisy).brief();
    expect(r.text).toMatch(/In the last 12 hours, 3 tasks finished, including Align contact detail groups and comments/);   // the count is real tasks only; how many get named depends on the 400-character budget
    expect(r.text).not.toMatch(/order:|sling|ux.?reviewer|195|other change/);
    const roomy = await city({ ...CALM, events: noisyEvents }).brief();                      // with room, two are named
    expect(roomy.text).toMatch(/3 tasks finished, including Align contact detail groups and comments and Fix the flaky login test\./);
    const none = await city({ ...noisy, events: [closedEv("order:x"), closedEv("crm/gastown.furiosa")] }).brief();
    expect(none.text).toMatch(/nothing finished/);
  });
  it("an agent in a turn, or holding a bead, is working, never idle; and 'nobody' is said plainly", async () => {
    const holds = await city({ ...noisy, sessions: [S("gc2-wisp-a1", { alias: "crm/steve", activity: "idle", active_bead: "gcw-a111" }), S("gc2-wisp-a2", { alias: "gc-architect", activity: "in-turn" }),
      S("gc2-wisp-a3", { alias: "devops", activity: "idle" }), S("gc2-wisp-a4", { alias: "marketer" })], inprogress: [{ assignee: "devops", title: "Renew the certificate", updated_at: iso(T0) }] }).brief();
    expect(holds.text).toMatch(/3 agents are working: crm steve on Fix the login redirect.*devops on Renew the certificate|3 agents are working: devops on Renew the certificate.*crm steve on Fix the login redirect/);
    expect((holds.structured as any).counts).toMatchObject({ working: 3, idle: 0, unknown: 1 });
    const nobody = await city({ ...noisy, sessions: [S("gc2-wisp-a3", { alias: "devops", activity: "idle" }), S("gc2-wisp-a4", { alias: "marketer" })] }).brief();
    expect(nobody.text).toContain("unknown activity");
    const idle = await city({ ...noisy, sessions: [S("gc2-wisp-a3", { alias: "devops", activity: "idle" })], inprogress: [] }).brief();
    expect(idle.text).toContain("Nobody is working right now.");
  });
  it("stays within 400 characters, in the order needs, working, finished, with no ellipsis characters and no ids", async () => {
    const r = await city(noisy).brief();
    expect(r.text.length).toBeLessThanOrEqual(400);
    expect(r.text).not.toContain("…");
    expect(r.text).not.toMatch(ID_LIKE);
    expect(sentences(r.text).length).toBeLessThanOrEqual(5);
    const i = ["Needs you:", "working", "finished"].map((k) => r.text.indexOf(k));
    expect(i.every((x) => x >= 0) && i[0] < i[1] && i[1] < i[2]).toBe(true);
  });
  it("mail for the owner (from the mayor or marked for them) is said first, ahead of any count", async () => {
    const r = await city({ ...noisy, mail: { items: [...noisyMail.items.slice(0, 5), { from: "mayor", subject: "Budget sign-off", created_at: iso(T0) }], total: 3016 } }).brief();
    expect(r.text).toMatch(/Needs you:.*mail about Budget sign-off from mayor/);
    expect(r.text).not.toMatch(/notices/);
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
    for (const t of tools.filter(t => !t.name.startsWith("gc_api_"))) {
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

 it("keeps unknown activity and reported program errors out of idle and completed work",async()=>{
  const r=await city({sessions:[S('unknown',{alias:'uncertain'}),S('error',{alias:'reporter',program_status:{state:'error'}})],pending:[],gates:[],events:[],mail:{items:[]}}).brief();
  expect(r.structured.counts).toMatchObject({unknown:1,reported_error:1,idle:0,working:0});
  expect((r.structured.attention as any[]).some(a=>a.kind==='reported_error')).toBe(true);
  expect(r.text).toMatch(/reported an error/);expect(r.text).not.toMatch(/Nobody is working/);
 });
