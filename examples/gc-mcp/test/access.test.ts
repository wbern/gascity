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

describe("by default NOTHING is protected: gc3 is the owner's machine and gc:read reads any session", () => {
  it("every session-reading tool reads any session with a plain gc:read token (including the ones that used to be on a default list)", async () => {
    const w = world({ peekAllow: undefined });
    for (const session of ["zachris-gemini-2", "steve", "mayor", "claude-william"]) {
      for (const [name, args] of READ_TOOLS) {
        const r = await w.run(name, w.RO, { session, ...args });
        expect((r.structured as any).allowed, `${name} ${session}`).not.toBe(false);
        expect(r.text, `${name} ${session}`).not.toMatch(/protected|not on the list|will not read/i);
      }
    }
    expect((await w.run("latest_reply", w.RO, { session: "zachris-gemini-2" })).text).toMatch(/zachris-gemini-2 said/);
  });
  it("the shipped defaults carry no opinion: empty write deny list, no read deny list, attached sessions allowed", async () => {
    const { DEFAULT_WRITE_DENY } = await import("../src/writes.js");
    expect(DEFAULT_WRITE_DENY).toEqual([]);
    const w = world({ peekAllow: undefined });
    expect(readVerdict(w.deps, w.RO, w.hit)).toMatchObject({ ok: true });                         // zachris-gemini-2, read-only token
    expect(readVerdict({ ...w.deps, writes: undefined }, undefined, w.hit)).toMatchObject({ ok: true });   // not even a write gate is needed
    const sent = await w.deps.writes!.submit(w.RW, "zachris-gemini-2", "hello");
    expect(sent.structured).toMatchObject({ sent: true });                                         // gc:write sends to any session
  });
  it("an empty peek allow list means everything too (the old default of [mayor] is gone)", async () => {
    const w = world({ peekAllow: [] });
    expect((await w.run("latest_reply", w.RO, { session: "steve" })).text).toMatch(/crm slash steve said/);
  });
  it("the briefing does not hide or blur any agent or task title by default", async () => {
    const r = await briefingWith({});
    expect(r.text).toMatch(/zachris gemini 2 on Secret roadmap/);
    expect((r.structured as any).busiest.find((b: any) => /zachris/.test(b.name)).title).toBe("Secret roadmap for the quarter");
  });
});

describe("an OPERATOR list, when configured, refuses with a clear speakable reason and a machine reason", () => {
  it("the optional deny list: every session-reading tool says it is on the protected list and how to change it", async () => {
    const w = world({ writesCfg: { deny: ["zachris-*"] }, peekAllow: undefined });
    for (const [name, args] of READ_TOOLS) {
      const r = await w.run(name, w.RW, { session: "zachris-gemini-2", ...args });
      expect(r.text, name).toBe("zachris gemini 2 is on the protected list, so I will not read or write it. Ask the owner to change the list if you want that.");
      expect(r.structured, name).toMatchObject({ found: true, allowed: false, reason: "denied_session", name: "zachris gemini 2" });
      NOT_VOICE(r.text);
      expect(r.text.length).toBeLessThanOrEqual(280);
    }
    expect((await w.run("latest_reply", w.RO, { session: "steve" })).text).toMatch(/crm slash steve said/);      // everything else is untouched
  });
  it("the optional peek allow list is a plain list for every token: not_in_allow_list, with the way out", async () => {
    const w = world({ peekAllow: ["mayor"] });
    for (const who of [w.RO, w.RW]) {
      for (const [name, args] of READ_TOOLS) {
        const r = await w.run(name, who, { session: "steve", ...args });
        expect(r.structured, name).toMatchObject({ found: true, allowed: false, reason: "not_in_allow_list", name: "crm steve" });
        expect(r.text, name).toBe("crm steve is not on the list of agents I may read. Ask the owner to add it.");
        NOT_VOICE(r.text);
      }
    }
    expect((await w.run("latest_reply", w.RO, { session: "mayor" })).text).toMatch(/mayor said/);
  });
  it("the read deny list is separate from the write deny list when both are configured", async () => {
    const w = world({ writesCfg: { deny: ["zachris-*"] }, readDeny: [], peekAllow: undefined });
    expect((await w.run("latest_reply", w.RO, { session: "zachris-gemini-2" })).text).toMatch(/said/);       // reads open
    const write = await w.deps.writes!.submit(w.RW, "zachris-gemini-2", "hi");
    expect(write.structured).toMatchObject({ sent: false, reason: "denied_session" });                      // writes still blocked by the operator's list
    const custom = world({ readDeny: ["crm/*"], peekAllow: undefined });
    const s = await custom.run("latest_reply", custom.RO, { session: "steve" });
    expect(s.structured).toMatchObject({ reason: "denied_session" });
    expect(s.text).toBe("crm steve is on the protected list, so I will not read it. Ask the owner to change the list if you want that.");   // not write-denied: only "read"
  });
  it("with no read list given the read deny list follows the write deny list", async () => {
    const w = world({ writesCfg: { deny: ["crm/*"] }, peekAllow: undefined });
    expect((await w.run("latest_reply", w.RO, { session: "steve" })).structured).toMatchObject({ reason: "denied_session" });
  });
});

describe("the briefing hides a task title only when the OPERATOR protected that session", () => {
  it("a configured protected agent is named as busy but its task is omitted; others show theirs", async () => {
    const r = await briefingWith({ writesCfg: { deny: ["zachris-*"] } });
    expect(r.text).not.toMatch(/Secret roadmap/);
    expect(JSON.stringify(r.structured)).not.toMatch(/Secret roadmap/);
    expect(r.text).toMatch(/crm steve on Fix the login page/);
    expect((r.structured as any).busiest.find((b: any) => /zachris/.test(b.name)).title).toBe("");
  });
});

function briefingWith(over: Parameters<typeof world>[0]) {
  const sessions = [...SESS.slice(0, 2), { ...SESS[2], activity: "working", active_bead: "gcw-ok01", last_active: iso(T0 - 5000) }];
  const w = world({ peekAllow: undefined, ...over });
  const fetchOrig = (w.deps.gc as any).o.fetchImpl;
  (w.deps.gc as any).o.fetchImpl = (async (url: string, init: RequestInit) => {
    const p = new URL(url).pathname.replace(/^\/v0\/city\/[^/]+/, "");
    if (p === "/sessions") return new Response(JSON.stringify({ items: sessions, total: sessions.length }), { status: 200 });
    if (p === "/bead/gcw-sec1") return new Response(JSON.stringify({ title: "Secret roadmap for the quarter" }), { status: 200 });
    if (p === "/bead/gcw-ok01") return new Response(JSON.stringify({ title: "Fix the login page" }), { status: 200 });
    return fetchOrig(url, init);
  }) as unknown as typeof fetch;
  return w.run("briefing", w.RO);
}
