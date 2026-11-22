import { describe, it, expect } from "vitest";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { GcClient } from "../src/gc-client.js";
import { buildTools, type Deps } from "../src/tools.js";
import { readVerdict } from "../src/access.js";
import { WRITE_SCOPE, WriteGate } from "../src/writes.js";

const T0 = 1_800_000_000_000;
const iso = (ms: number) => new Date(ms).toISOString();
const SESS = [
  { id: "gc2-wisp-www", alias: "claude-william", template: "claude-william", state: "active", running: true, activity: "idle" },
  { id: "gc2-wisp-zg2", alias: "zachris-gemini-2", template: "zachris-gemini-2", state: "active", running: true, activity: "in-turn", active_bead: "gcw-sec1", last_active: iso(T0) },
  { id: "gc2-wisp-ste", template: "crm/steve", state: "active", running: true, activity: "idle" },
  { id: "gc2-wisp-may", alias: "mayor", template: "mayor", state: "active", running: true, activity: "idle" },
];
const TURNS = [{ role: "assistant", text: "Here is what I did.", timestamp: iso(T0 + 1000) }];

function world(over: { peekAllow?: string[] | undefined; readDeny?: string[]; writesCfg?: any; killSwitch?: boolean } = {}) {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "gcmcp-acc-"));
  if (over.killSwitch) fs.writeFileSync(path.join(dir, "WRITES_DISABLED"), "");
  const fetchImpl = (async (url: string, init: RequestInit) => {
    const p = new URL(url).pathname.replace(/^\/v0\/city\/[^/]+/, "");
    if (init?.method === "POST") return new Response(JSON.stringify({ request_id: "r", status: "accepted" }), { status: 202 });
    if (p === "/sessions") return new Response(JSON.stringify({ items: SESS, total: SESS.length }), { status: 200 });
    if (/\/transcript$/.test(p)) return new Response(JSON.stringify({ turns: TURNS }), { status: 200 });
    if (p === "/pending") return new Response(JSON.stringify({ items: [] }), { status: 200 });
    if (/^\/bead\//.test(p)) return new Response(JSON.stringify({ id: "x", title: "Secret roadmap for the quarter" }), { status: 200 });
    if (/^\/session\//.test(p)) { const id = p.split("/")[2]; return new Response(JSON.stringify(SESS.find((s) => s.id === id) ?? {}), { status: 200 }); }
    if (p === "/status") return new Response(JSON.stringify({ agents: { quarantined: 0 }, mail: { unread: 0 } }), { status: 200 });
    return new Response(JSON.stringify({ items: [], total: 0 }), { status: 200 });
  }) as unknown as typeof fetch;
  const gc = new GcClient({ baseUrl: "http://127.0.0.1:1", city: "gc2", fetchImpl });
  const gate = over.writesCfg === null ? undefined : new WriteGate(gc, { enabled: true, controlDir: dir, ...(over.writesCfg ?? {}) }, () => undefined, () => T0);
  const clock = { t: T0, now() { return clock.t; }, async sleep(ms: number) { clock.t += ms; } };
  const deps: Deps = { gc, mailAgent: "human", now: () => new Date(T0), clock, briefings: new Map(), defaultSession: "claude-william", writes: gate, peekAllow: "peekAllow" in over ? over.peekAllow : ["mayor"], readDeny: over.readDeny };
  const RW = { client: "ChatGPT", scopes: ["gc:read", WRITE_SCOPE] }, RO = { client: "ChatGPT", scopes: ["gc:read"] };
  const run = (name: string, who: any, args: any = {}) => buildTools(deps, who).find((t) => t.name === name)!.handler(args);
  return { deps, run, RW, RO, hit: SESS[1], steve: SESS[2] };
}
// every read tool that takes a session
const READ_TOOLS: [string, any][] = [["peek_session", {}], ["session_state", {}], ["latest_reply", {}], ["read_more", { offset: 0 }], ["wait_for_reply", { max_seconds: 1 }], ["what_is_working_on", {}]];
const NOT_VOICE = (t: string) => expect(t).not.toMatch(/\bvoice\b/i);

describe("a denied read says WHY, in one speakable sentence, with a machine reason", () => {
  it("the protected list: every session-reading tool says it belongs to someone else, is on the protected list, and how to change it", async () => {
    const w = world();
    for (const [name, args] of READ_TOOLS) {
      const r = await w.run(name, w.RW, { session: "zachris-gemini-2", ...args });
      expect(r.text, name).toBe("zachris gemini 2 belongs to someone else, so it is on the protected list and I will not read or write it. Ask the owner to change the list if you want that.");
      expect(r.structured, name).toMatchObject({ found: true, allowed: false, reason: "denied_session", name: "zachris gemini 2" });
      NOT_VOICE(r.text);
      expect(r.text.length).toBeLessThanOrEqual(280);
    }
  });
  it("a read-only token and a write token get the SAME protected-list answer (the list wins over any scope)", async () => {
    const w = world();
    const ro = await w.run("latest_reply", w.RO, { session: "zachris-gemini-2" });
    const rw = await w.run("latest_reply", w.RW, { session: "zachris-gemini-2" });
    expect(ro.structured).toMatchObject({ reason: "denied_session" });
    expect(ro.text).toBe(rw.text);
  });
  it("not on the read list and no write scope: says exactly that and to reconnect with gc:write; never blames voice", async () => {
    const w = world();
    for (const [name, args] of READ_TOOLS) {
      const r = await w.run(name, w.RO, { session: "steve", ...args });
      expect(r.structured, name).toMatchObject({ found: true, allowed: false, reason: "no_scope", name: "crm steve" });
      expect(r.text, name).toMatch(/crm steve is not on my read list/);
      expect(r.text, name).toMatch(/reconnect with the gc:write scope/i);
      expect(r.text, name).toMatch(/ask the owner to add it/i);
      NOT_VOICE(r.text);
    }
  });
  it("a write token while writes are switched off: says that is why it cannot read beyond the list", async () => {
    const w = world({ killSwitch: true });
    const r = await w.run("latest_reply", w.RW, { session: "steve" });
    expect(r.structured).toMatchObject({ found: true, allowed: false, reason: "writes_disabled", name: "crm steve" });
    expect(r.text).toMatch(/writes are switched off/i);
    expect(r.text).toMatch(/ask the owner/i);
    NOT_VOICE(r.text);
  });
  it("a write token whose write allow list excludes the session: not_in_allow_list", async () => {
    const w = world({ writesCfg: { allow: ["mayor"] } });
    const r = await w.run("latest_reply", w.RW, { session: "steve" });
    expect(r.structured).toMatchObject({ found: true, allowed: false, reason: "not_in_allow_list", name: "crm steve" });
    expect(r.text).toMatch(/not on the list of agents I may (read|write)/i);
    NOT_VOICE(r.text);
  });
  it("allowed reads are unchanged: the default session, a peek-listed session, and a write token on a permitted session", async () => {
    const w = world();
    expect((await w.run("latest_reply", w.RO, {})).text).toMatch(/claude-william said/);
    expect((await w.run("latest_reply", w.RO, { session: "mayor" })).text).toMatch(/mayor said/);
    expect((await w.run("latest_reply", w.RW, { session: "steve" })).text).toMatch(/crm slash steve said/);
  });
  it("no peek allow list at all: everything is readable EXCEPT the protected list", async () => {
    const w = world({ peekAllow: undefined });
    expect((await w.run("latest_reply", w.RO, { session: "steve" })).text).toMatch(/crm slash steve said/);
    expect((await w.run("latest_reply", w.RO, { session: "zachris-gemini-2" })).structured).toMatchObject({ reason: "denied_session" });
  });
  it("the protected list applies even when writes are not deployed at all (no write gate)", async () => {
    const w = world({ peekAllow: undefined, writesCfg: null });
    expect((await w.run("latest_reply", w.RO, { session: "zachris-gemini-2" })).structured).toMatchObject({ reason: "denied_session" });
  });
});

describe("the READ deny list is separate from the WRITE deny list", () => {
  it("default: the read deny list IS the write deny list (nothing changes today)", async () => {
    const w = world();
    expect((await w.run("latest_reply", w.RW, { session: "zachris-gemini-2" })).structured).toMatchObject({ reason: "denied_session" });
    expect((await w.deps.writes!.submit(w.RW, "zachris-gemini-2", "hi")).structured).toMatchObject({ sent: false, reason: "denied_session" });
    const custom = world({ writesCfg: { deny: ["steve*", "crm/*"] }, peekAllow: undefined });
    expect((await custom.run("latest_reply", custom.RO, { session: "steve" })).structured).toMatchObject({ reason: "denied_session" });   // follows the CUSTOM write list when no read list is given
  });
  it("read deny [] opens READS of colleagues' sessions while WRITES stay blocked", async () => {
    const w = world({ readDeny: [] });
    const read = await w.run("latest_reply", w.RW, { session: "zachris-gemini-2" });
    expect(read.text).toMatch(/zachris-gemini-2 said/);
    const write = await w.deps.writes!.submit(w.RW, "zachris-gemini-2", "hi");
    expect(write.structured).toMatchObject({ sent: false, reason: "denied_session" });
    expect(write.text).toMatch(/do-not-write/);
  });
  it("a custom read deny list replaces the default for reads only", async () => {
    const w = world({ readDeny: ["crm/*"], peekAllow: undefined });
    expect((await w.run("latest_reply", w.RO, { session: "zachris-gemini-2" })).text).toMatch(/said/);          // no longer protected for reads
    const s = await w.run("latest_reply", w.RO, { session: "steve" });
    expect(s.structured).toMatchObject({ reason: "denied_session" });
    expect(s.text).toMatch(/I will not read it\./);                                                            // not write-denied: the sentence says only read
    expect(s.text).not.toMatch(/or write/);
  });
  it("readVerdict is the one place that decides, with the documented reason codes", () => {
    const w = world();
    const v = readVerdict(w.deps, w.RO, w.hit);
    expect(v).toMatchObject({ ok: false, reason: "denied_session", name: "zachris gemini 2" });
    expect(readVerdict(w.deps, w.RO, w.steve)).toMatchObject({ ok: false, reason: "no_scope" });
    expect(readVerdict(w.deps, w.RW, w.steve)).toMatchObject({ ok: true });
  });
});

describe("the briefing never leaks the task of a session the owner may not read", () => {
  const sessions = [...SESS.slice(0, 2), { ...SESS[2], activity: "working", active_bead: "gcw-ok01", last_active: iso(T0 - 5000) }];
  const briefing = (over: Parameters<typeof world>[0]) => {
    const w = world(over);
    const fetchOrig = (w.deps.gc as any).o.fetchImpl;
    (w.deps.gc as any).o.fetchImpl = (async (url: string, init: RequestInit) => {
      const p = new URL(url).pathname.replace(/^\/v0\/city\/[^/]+/, "");
      if (p === "/sessions") return new Response(JSON.stringify({ items: sessions, total: sessions.length }), { status: 200 });
      if (p === "/bead/gcw-sec1") return new Response(JSON.stringify({ title: "Secret roadmap for the quarter" }), { status: 200 });
      if (p === "/bead/gcw-ok01") return new Response(JSON.stringify({ title: "Fix the login page" }), { status: 200 });
      return fetchOrig(url, init);
    }) as unknown as typeof fetch;
    return w.run("briefing", w.RO);
  };
  it("a protected agent is named as busy but its task title is omitted; allowed agents still show theirs", async () => {
    const r = await briefing({});
    expect(r.text).not.toMatch(/Secret roadmap/);
    expect(JSON.stringify(r.structured)).not.toMatch(/Secret roadmap/);
    expect(r.text).toMatch(/crm steve on Fix the login page/);
    expect(r.text).toMatch(/zachris gemini 2/);                       // named: that someone else's agent is busy is fine to say
    const zach = (r.structured as any).busiest.find((b: any) => /zachris/.test(b.name));
    expect(zach).toBeTruthy();
    expect(zach.title).toBe("");
  });
  it("with read deny [] the colleague's task may be spoken again (the owner opened it)", async () => {
    const r = await briefing({ readDeny: [] });
    expect(r.text).toMatch(/zachris gemini 2 on Secret roadmap/);
  });
});
