import { describe, it, expect, afterAll } from "vitest";
import crypto from "node:crypto";
import fs from "node:fs";
import net from "node:net";
import os from "node:os";
import path from "node:path";
import { createApp, listen, type AppOpts } from "../src/app.js";
import { GcClient, WRITE_OPS } from "../src/gc-client.js";
import { hashPassphrase } from "../src/oauth.js";
import { buildTools } from "../src/tools.js";
import { DEFAULT_WRITE_DENY, MAX_WRITE_TEXT, WRITE_SCOPE, WriteGate, confirmWrite, globMatch, validatePatterns, type WriteConfig } from "../src/writes.js";
import { AuditLog } from "../src/audit.js";
import * as F from "./fixtures/index.js";

// ---------------------------------------------------------------- a fake supervisor that records every call
const SESSIONS = {
  total: 9,
  items: [
    { id: "gc2-wisp-aaa", state: "active", alias: "gc-architect", template: "gas-city-wbern/architect", running: true },
    { id: "gc2-wisp-ccc", state: "active", alias: "mayor", template: "mayor", running: true },
    { id: "gc2-wisp-ddd", state: "asleep", alias: "devops", template: "gas-city-infra/devops", running: false },
    { id: "gc2-wisp-www", state: "active", alias: "claude-william", template: "claude-william", running: true },
    { id: "gc2-wisp-xxx", state: "active", alias: "mayor-william", template: "x", running: true },
    { id: "gc2-wisp-zzz", state: "active", alias: "zachris-dev", template: "zachris-dev", running: true },
    { id: "gc2-wisp-eee", state: "active", alias: "elsakert-ops", template: "elsakert-ops", running: true },
    { id: "gc2-wisp-att", state: "active", alias: "human-attached", template: "hat", running: true, attached: true },
    { id: "gc2-wisp-nnn", state: "active", alias: "deployer", template: "gas-city-infra/deployer", running: true },
  ],
};
interface Call { method: string; path: string; headers: Record<string, string>; body?: any; redirect?: string }
function supervisor(post: (path: string, body: any) => { status: number; json: unknown } = () => ({ status: 202, json: { request_id: "req-1", status: "accepted" } })) {
  const calls: Call[] = [];
  const fetchImpl = (async (url: string, init: RequestInit) => {
    const headers = Object.fromEntries(Object.entries((init?.headers ?? {}) as Record<string, string>).map(([k, v]) => [k.toLowerCase(), v]));
    const p = new URL(url).pathname;
    const call: Call = { method: String(init?.method), path: p, headers, body: init?.body ? JSON.parse(String(init.body)) : undefined, redirect: init?.redirect };
    calls.push(call);
    if (call.method === "POST") { const r = post(p, call.body); return new Response(JSON.stringify(r.json), { status: r.status }); }
    const body = p.endsWith("/sessions") ? SESSIONS : p.endsWith("/usage") ? F.usage : F.status;
    return new Response(JSON.stringify(body), { status: 200 });
  }) as unknown as typeof fetch;
  return { calls, posts: () => calls.filter((c) => c.method === "POST"), fetchImpl };
}
const dirs: string[] = [];
const tmp = () => { const d = fs.mkdtempSync(path.join(os.tmpdir(), "gcmcp-w-")); dirs.push(d); return d; };
afterAll(() => dirs.forEach((d) => fs.rmSync(d, { recursive: true, force: true })));

function gate(cfg: Partial<WriteConfig> = {}, sup = supervisor(), now = () => Date.now()) {
  const events: any[] = [];
  const dir = tmp();
  const g = new WriteGate(new GcClient({ baseUrl: "http://127.0.0.1:1", city: "gc2", fetchImpl: sup.fetchImpl }), { enabled: true, controlDir: dir, ...cfg }, (e) => events.push(e), now);
  return { g, events, sup, dir };
}
const W = { client: "ChatGPT (owner client)", scopes: ["gc:read", WRITE_SCOPE] };
const R = { client: "Claude (owner client)", scopes: ["gc:read"] };

// ---------------------------------------------------------------- the client: a fixed allowlist of POST operations
describe("GcClient: the only mutations are the fixed write operations", () => {
  const mk = () => { const s = supervisor(); return { s, c: new GcClient({ baseUrl: "http://127.0.0.1:1", city: "gc2", fetchImpl: s.fetchImpl }) }; };
  it("the allowlist is exactly submit and respond", () => {
    expect(Object.keys(WRITE_OPS).sort()).toEqual(["respond", "submit"]);
    expect(WRITE_OPS.submit.operationId).toBe("submit-session");
    expect(WRITE_OPS.respond.operationId).toBe("respond-session");
  });
  it("sends exactly POST /v0/city/<city>/session/<id>/submit|respond with the CSRF header, an idempotency key, JSON, and no redirects", async () => {
    const { s, c } = mk();
    await c.write("submit", "gc2-wisp-aaa", { message: "hi", intent: "default" }, crypto.randomUUID());
    await c.write("respond", "gc2-wisp-aaa", { action: "allow" }, crypto.randomUUID());
    expect(s.posts().map((p) => p.path)).toEqual(["/v0/city/gc2/session/gc2-wisp-aaa/submit", "/v0/city/gc2/session/gc2-wisp-aaa/respond"]);
    for (const p of s.posts()) {
      expect(p.headers["x-gc-request"]).toBeTruthy();
      expect(p.headers["idempotency-key"]).toMatch(/^[0-9a-f-]{36}$/);
      expect(p.headers["content-type"]).toBe("application/json");
      expect(p.redirect).toBe("error");
    }
  });
  it("cannot be pointed at any other operation or path: stop, kill, reset, close, config, beads, traversal", async () => {
    const { s, c } = mk();
    for (const op of ["stop", "kill", "reset", "close", "suspend", "wake", "pin", "rename", "messages", "ops", "permission-mode", "../kill", "submit/../kill"]) {
      await expect((c as any).write(op, "gc2-wisp-aaa", { message: "x" }, crypto.randomUUID())).rejects.toThrow();
    }
    for (const id of ["../kill", "x/stop", "a/../b", "x%2Fkill", "x?y=1", "x#y", "", " ", "a b", "x\nkill", ".hidden", "a".repeat(129)]) {
      await expect(c.write("submit", id, { message: "x" }, crypto.randomUUID())).rejects.toThrow(/invalid session id/);
    }
    await expect(c.write("submit", "gc2-wisp-aaa", { message: "x" }, "not a key!")).rejects.toThrow(/idempotency/);
    await expect(c.write("submit", "gc2-wisp-aaa", { message: "x".repeat(9000) }, crypto.randomUUID())).rejects.toMatchObject({ message: "request too large" });
    expect(s.calls.length).toBe(0);   // nothing was ever sent
  });
  it("has no public generic mutation method", () => {
    expect(Object.getOwnPropertyNames(GcClient.prototype).filter((n) => n !== "constructor").sort()).toEqual(["get", "send", "write"]);
    const src = fs.readFileSync(path.join(__dirname, "../src/gc-client.ts"), "utf8");
    expect([...src.matchAll(/this\.send\(/g)].length).toBe(2);   // only get() and write() reach the transport
    expect(src).not.toMatch(/"(PUT|PATCH|DELETE)"/);
  });
  it("maps a refusal from the supervisor to a GcError and never retries", async () => {
    const s = supervisor(() => ({ status: 409, json: { detail: "session is busy" } }));
    const c = new GcClient({ baseUrl: "http://127.0.0.1:1", city: "gc2", fetchImpl: s.fetchImpl });
    await expect(c.write("submit", "gc2-wisp-aaa", { message: "x" }, crypto.randomUUID())).rejects.toMatchObject({ status: 409, message: "session is busy" });
    expect(s.posts().length).toBe(1);
  });
});

// ---------------------------------------------------------------- the gate: rails
describe("WriteGate: send_to_session", () => {
  it("sends the message to the resolved session id with the chosen intent and answers in one speakable line", async () => {
    const { g, sup, events } = gate();
    for (const intent of ["default", "follow_up", "interrupt_now"] as const) {
      const r = await g.submit(W, "gc-architect", "please run the tests", intent);
      expect(r.structured).toMatchObject({ sent: true, session: "gc2-wisp-aaa", intent });
      expect(r.text.split(/[.!?]\s/).length).toBeLessThanOrEqual(2);
      expect(r.text).toContain("gc-architect");
    }
    expect(sup.posts().map((p) => [p.path, p.body])).toEqual([
      ["/v0/city/gc2/session/gc2-wisp-aaa/submit", { message: "please run the tests", intent: "default" }],
      ["/v0/city/gc2/session/gc2-wisp-aaa/submit", { message: "please run the tests", intent: "follow_up" }],
      ["/v0/city/gc2/session/gc2-wisp-aaa/submit", { message: "please run the tests", intent: "interrupt_now" }],
    ]);
    expect(new Set(sup.posts().map((p) => p.headers["idempotency-key"])).size).toBe(3);   // a fresh key per call
    expect(events.every((e) => e.kind === "write" && e.result === "sent")).toBe(true);
  });
  it("resolves by alias, id, template or session name, case-insensitively, but sends to the canonical id", async () => {
    const { g, sup } = gate();
    for (const q of ["MAYOR", "gc2-wisp-ccc"]) await g.submit(W, q, `hello via ${q}`);   // different texts: an identical resend would be deduplicated
    expect(sup.posts().map((p) => p.path)).toEqual(["/v0/city/gc2/session/gc2-wisp-ccc/submit", "/v0/city/gc2/session/gc2-wisp-ccc/submit"]);
  });
  it("a session that does not exist is never written to", async () => {
    const { g, sup, events } = gate();
    const r = await g.submit(W, "nobody-home", "hello");
    expect(r.structured).toMatchObject({ sent: false, reason: "no_such_session" });
    expect(sup.posts().length).toBe(0);
    expect(events[0]).toMatchObject({ result: "no_such_session", ok: false });
  });
  it("a name with unsafe characters is refused before any call; anything else is only ever MATCHED against the live list, never put in a URL", async () => {
    const { g, sup } = gate();
    for (const q of ["a;b", "x`y", "a\"b", "$(id)", "a\nb", "a".repeat(200)]) expect((await g.submit(W, q, "hello")).structured, q).toMatchObject({ sent: false, reason: "bad_session_name" });
    expect(sup.calls.length).toBe(0);
    for (const q of ["../kill", "a/b", "x y"]) expect((await g.submit(W, q, "hello")).structured, q).toMatchObject({ sent: false, reason: "no_such_session" });
    expect(sup.posts().length).toBe(0);                                     // nothing was ever sent to a made-up path
    expect((await g.submit(W, "", "hello")).structured).toMatchObject({ sent: false, reason: "no_session_given" });   // no default configured
  });
  it("text: 1..4000 characters of plain text; whitespace-only, control characters and 4001 are refused; 4000 and newlines are fine", async () => {
    const { g, sup } = gate();
    for (const bad of ["", "   \n\t ", "x".repeat(MAX_WRITE_TEXT + 1), "a\u0000b", "a\u001bb[31m", "a\u007fb"]) {
      expect((await g.submit(W, "mayor", bad)).structured, JSON.stringify(bad.slice(0, 12))).toMatchObject({ sent: false, reason: "bad_text" });
    }
    expect((await g.submit(W, "mayor", "x".repeat(MAX_WRITE_TEXT))).structured).toMatchObject({ sent: true });
    expect((await g.submit(W, "mayor", "line one\nline two\ttabbed")).structured).toMatchObject({ sent: true });
    expect(sup.posts().length).toBe(2);
  });
  it("an unknown intent is refused", async () => {
    const { g, sup } = gate();
    expect((await g.submit(W, "mayor", "hi", "now" as any)).structured).toMatchObject({ sent: false, reason: "bad_intent" });
    expect(sup.posts().length).toBe(0);
  });
  it("a supervisor refusal or outage is spoken as nothing sent, never thrown, and audited", async () => {
    for (const [status, json] of [[409, { detail: "busy" }], [503, { detail: "down" }]] as const) {
      const { g, events } = gate({}, supervisor(() => ({ status, json })));
      const r = await g.submit(W, "mayor", "hi");
      expect(r.structured).toMatchObject({ sent: false, reason: "error", status });
      expect(r.text).toContain("Nothing was sent");
      expect(events.at(-1)).toMatchObject({ result: `error:${status}`, ok: false });
    }
    const down = gate({}, { ...supervisor(), fetchImpl: (async () => { throw new Error("connect refused"); }) as unknown as typeof fetch, calls: [], posts: () => [] });
    expect((await down.g.submit(W, "mayor", "hi")).structured).toMatchObject({ sent: false });
    expect(down.events.at(-1).result).toMatch(/^lookup_failed/);
  });
  it("tells you when the session is not running (the message is queued)", async () => {
    const { g } = gate();
    expect((await g.submit(W, "devops", "hi")).text).toContain("queued");
  });
});

describe("WriteGate: respond_to_session", () => {
  it("answers a waiting session with an action word, optional text and request id", async () => {
    const { g, sup } = gate();
    const r = await g.respond(W, "gc-architect", "allow", "go ahead", "req-77");
    expect(r.structured).toMatchObject({ sent: true, action: "allow", session: "gc2-wisp-aaa" });
    expect(sup.posts()[0].path).toBe("/v0/city/gc2/session/gc2-wisp-aaa/respond");
    expect(sup.posts()[0].body).toEqual({ action: "allow", text: "go ahead", request_id: "req-77" });
    await g.respond(W, "mayor", "deny");
    expect(sup.posts()[1].body).toEqual({ action: "deny" });
  });
  it("refuses a malformed action, request id or oversized text; same rails apply", async () => {
    const { g, sup } = gate();
    for (const action of ["Allow", "allow now", "a".repeat(40), "../kill", "", "1x"]) expect((await g.respond(W, "mayor", action)).structured, action).toMatchObject({ sent: false, reason: "bad_action" });
    expect((await g.respond(W, "mayor", "allow", undefined, "bad id!")).structured).toMatchObject({ reason: "bad_request_id" });
    expect((await g.respond(W, "mayor", "allow", "x".repeat(MAX_WRITE_TEXT + 1))).structured).toMatchObject({ reason: "bad_text" });
    expect((await g.respond(W, "zachris-dev", "allow")).structured).toMatchObject({ reason: "denied_session" });
    expect(sup.posts().length).toBe(0);
  });
});

describe("WriteGate: deny and allow lists", () => {
  it("the built-in deny list protects OTHER PEOPLE's sessions only; the owner's own session is the target", async () => {
    expect(DEFAULT_WRITE_DENY).toEqual(["zachris-*", "elsakert-*"]);
    const { g, sup, events } = gate();
    for (const s of ["zachris-dev", "elsakert-ops"]) {
      const r = await g.submit(W, s, "hi");
      expect(r.structured, s).toMatchObject({ sent: false, reason: "denied_session" });
      expect(r.text).toContain("do-not-write");
    }
    expect(sup.posts().length).toBe(0);
    expect(events.map((e) => e.result)).toEqual(["denied_session", "denied_session"]);
    for (const s of ["claude-william", "mayor-william"]) expect((await g.submit(W, s, `hi ${s}`)).structured, s).toMatchObject({ sent: true });   // no longer blocked
  });
  it("matches ANY identifier of the session: id, alias, template, session name (so an alias cannot dodge the list)", async () => {
    const { g, sup } = gate({ deny: ["gas-city-infra/deployer", "gc2-wisp-ddd"] });
    expect((await g.submit(W, "deployer", "hi")).structured).toMatchObject({ reason: "denied_session" });   // by template
    expect((await g.submit(W, "devops", "hi")).structured).toMatchObject({ reason: "denied_session" });     // by id
    expect(sup.posts().length).toBe(0);
  });
  it("GC_MCP_WRITE_DENY_SESSIONS replaces the defaults", async () => {
    const { g } = gate({ deny: ["mayor"] });
    expect((await g.submit(W, "mayor", "hi")).structured).toMatchObject({ reason: "denied_session" });
    expect((await g.submit(W, "zachris-dev", "hi")).structured).toMatchObject({ sent: true });   // the replaced list no longer names it
  });
  it("an allow list restricts writes to matching sessions, and deny still wins over allow", async () => {
    const { g, sup } = gate({ allow: ["gc-architect", "mayor-*"], deny: ["*-william"] });
    expect((await g.submit(W, "gc-architect", "hi")).structured).toMatchObject({ sent: true });
    expect((await g.submit(W, "devops", "hi")).structured).toMatchObject({ reason: "not_in_allow_list" });
    expect((await g.submit(W, "mayor-william", "hi")).structured).toMatchObject({ reason: "denied_session" });   // allowed by mayor-*, denied by *-william
    expect(sup.posts().length).toBe(1);
  });
  it("a session a person is attached to is refused unless explicitly allowed", async () => {
    expect((await gate().g.submit(W, "human-attached", "hi")).structured).toMatchObject({ reason: "session_attached" });
    expect((await gate({ allowAttached: true }).g.submit(W, "human-attached", "hi")).structured).toMatchObject({ sent: true });
  });
  it("glob matching is anchored, case-insensitive and treats everything but * literally", () => {
    expect(globMatch("*-william", "claude-WILLIAM")).toBe(true);
    expect(globMatch("*-william", "william-claude")).toBe(false);
    expect(globMatch("zachris-*", "xzachris-a")).toBe(false);
    expect(globMatch("a.c", "abc")).toBe(false);
    expect(() => validatePatterns(["ok-*", "a b"], "t")).toThrow();
    expect(() => validatePatterns(["rig/agent", "ok-*"], "t")).not.toThrow();
    expect(() => validatePatterns([".*"], "t")).not.toThrow();   // '.' and '*' are plain characters of the alphabet, matched as glob/literal
    expect(globMatch(".*", "abc")).toBe(false);                    // so ".*" is NOT a regex wildcard
  });
});

describe("WriteGate: scope, kill switch, rate limit, second-factor hook", () => {
  it("a token without gc:write cannot write, and the attempt is audited", async () => {
    const { g, sup, events } = gate();
    const r = await g.submit(R, "mayor", "hi");
    expect(r.structured).toMatchObject({ sent: false, reason: "no_write_scope" });
    expect(sup.calls.length).toBe(0);
    expect(events[0]).toMatchObject({ kind: "write", app: "Claude (owner client)", result: "no_write_scope", ok: false });
  });
  it("disabled by config: writes are off and tools do not exist", async () => {
    const { g, sup } = gate({ enabled: false });
    expect(g.active).toBe(false);
    expect((await g.submit(W, "mayor", "hi")).structured).toMatchObject({ reason: "writes_disabled" });
    expect(sup.calls.length).toBe(0);
    expect(buildTools({ gc: new GcClient({ baseUrl: "http://127.0.0.1:1", city: "gc2", fetchImpl: sup.fetchImpl }), mailAgent: "human", now: () => new Date(), writes: g }, W).map((t) => t.name)).not.toContain("send_to_session");
  });
  it("WRITES_DISABLED switches writes off immediately and back on when removed; reads keep working", async () => {
    const { g, dir, sup } = gate();
    const gc = new GcClient({ baseUrl: "http://127.0.0.1:1", city: "gc2", fetchImpl: sup.fetchImpl });
    const names = () => buildTools({ gc, mailAgent: "human", now: () => new Date(), writes: g }, W).map((t) => t.name);
    expect(names()).toContain("send_to_session");
    fs.writeFileSync(path.join(dir, "WRITES_DISABLED"), "");
    expect(g.active).toBe(false);
    expect(names()).not.toContain("send_to_session");
    expect(names()).toContain("city_status");
    expect((await g.submit(W, "mayor", "hi")).structured).toMatchObject({ sent: false, reason: "writes_disabled" });   // even a stale tool listing cannot send
    expect(sup.posts().length).toBe(0);
    fs.unlinkSync(path.join(dir, "WRITES_DISABLED"));
    expect((await g.submit(W, "mayor", "hi")).structured).toMatchObject({ sent: true });
  });
  it("rate limit: 20 a minute by default, then refused until the window passes; a refused call sends nothing", async () => {
    const t = { now: 1_000_000 };
    const { g, sup } = gate({}, supervisor(), () => t.now);
    for (let i = 0; i < 20; i++) expect((await g.submit(W, "mayor", `m${i}`)).structured.sent).toBe(true);
    const postsBefore = sup.posts().length;
    expect((await g.submit(W, "mayor", "one too many")).structured).toMatchObject({ sent: false, reason: "rate_limited" });
    expect(sup.posts().length).toBe(postsBefore);   // refused: nothing was sent
    t.now += 61_000;
    expect((await g.submit(W, "mayor", "later")).structured.sent).toBe(true);
  });
  it("the rate limit is configurable", async () => {
    const { g } = gate({ perMinute: 2 });
    await g.submit(W, "mayor", "1"); await g.submit(W, "mayor", "2");
    expect((await g.submit(W, "mayor", "3")).structured).toMatchObject({ reason: "rate_limited" });
  });
  it("confirmWrite is the single second-factor hook: it allows today and a replacement can refuse", async () => {
    expect(await confirmWrite({ client: "c", session: "s", kind: "submit", intent: "default", length: 1 })).toEqual({ allow: true });
    const seen: any[] = [];
    const { g, sup, events } = gate({ confirm: async (c) => { seen.push(c); return { allow: false, reason: "needs a code" }; } });
    const r = await g.submit(W, "mayor", "do it", "follow_up");
    expect(r.structured).toMatchObject({ sent: false, reason: "confirmation_refused" });
    expect(r.text).toContain("needs a code");
    expect(sup.posts().length).toBe(0);
    expect(seen).toEqual([{ client: "ChatGPT (owner client)", session: "gc2-wisp-ccc", kind: "submit", intent: "follow_up", length: 5 }]);   // metadata only, never the text
    expect(events.at(-1).result).toBe("confirmation_refused");
  });
});

describe("WriteGate: audit", () => {
  it("every attempt, sent or refused, writes one line with who, session, intent, length, a short redacted preview and the outcome", async () => {
    const { g, events } = gate();
    const secret = "ghp_" + "A1b2C3d4E5f6G7h8I9j0K1l2M3n4";
    await g.submit(W, "gc-architect", `please use ${secret} and then ` + "y".repeat(500), "follow_up");
    await g.submit(R, "mayor", "no scope");
    await g.submit(W, "zachris-dev", "denied");
    await g.submit(W, "ghost", "missing");
    expect(events.length).toBe(4);
    for (const e of events) expect(e).toMatchObject({ kind: "write", app: expect.any(String), session: expect.any(String), length: expect.any(Number), result: expect.any(String), ok: expect.any(Boolean) });
    expect(events[0]).toMatchObject({ app: "ChatGPT (owner client)", session: "gc2-wisp-aaa", intent: "follow_up", result: "sent", ok: true });
    expect(events[0].length).toBe(`please use ${secret} and then `.length + 500);
    expect(events.map((e) => e.result)).toEqual(["sent", "no_write_scope", "denied_session", "no_such_session"]);
  });
  it("the audit file keeps at most 80 redacted characters of the text on one line and never the secret", () => {
    const dir = tmp();
    const file = path.join(dir, "audit.jsonl");
    const a = new AuditLog(file);
    const secret = "ghp_" + "A1b2C3d4E5f6G7h8I9j0K1l2M3n4";
    a.write({ kind: "write", app: "Claude (owner client)", session: "gc2-wisp-aaa", intent: "default", length: 700, preview: `please\nuse ${secret} ` + "z".repeat(600), result: "sent", ok: true });
    const line = fs.readFileSync(file, "utf8");
    const rec = JSON.parse(line);
    expect(line.trim().split("\n").length).toBe(1);
    expect(rec.preview.length).toBeLessThanOrEqual(80);
    expect(rec.preview).toContain("[redacted]");
    expect(line).not.toContain(secret);
    expect(line).not.toContain("z".repeat(100));
    expect(rec).toMatchObject({ kind: "write", app: "Claude (owner client)", session: "gc2-wisp-aaa", intent: "default", length: 700, result: "sent" });
  });
});

// ---------------------------------------------------------------- end to end through OAuth: scopes and tool listing
const PASS = "correct horse battery staple";
const CLAUDE = "claude-client-id-0123456789ab";
const GPT = "chatgpt-client-id-0123456789";
const CB = "https://claude.ai/api/mcp/auth_callback";
const GPT_CB = "https://chatgpt.com/connector/oauth/cb123abc";
const HASH = hashPassphrase(PASS);
const b64u = (b: Buffer) => b.toString("base64url");
const freePort = () => new Promise<number>((res) => { const s = net.createServer().listen(0, "127.0.0.1", () => { const p = (s.address() as net.AddressInfo).port; s.close(() => res(p)); }); });
const cleanups: (() => void)[] = [];
afterAll(() => cleanups.forEach((f) => f()));

async function boot(over: Partial<AppOpts> = {}) {
  const dir = tmp();
  const port = await freePort();
  const base = `http://127.0.0.1:${port}`;
  const res = `${base}/mcp`;
  const sup = supervisor();
  const { app, provider } = createApp({
    deps: { gc: new GcClient({ baseUrl: "http://127.0.0.1:1", city: "gc2", fetchImpl: sup.fetchImpl }), mailAgent: "human", now: () => new Date() },
    publicUrl: new URL(base), ownerHash: HASH, staticClientId: CLAUDE, chatgpt: { clientId: GPT, redirectUris: [GPT_CB] },
    stateFile: path.join(dir, "state.json"), auditFile: path.join(dir, "audit.jsonl"), controlDir: dir, ...over,
  });
  const srv = await listen(app, port);
  cleanups.push(() => srv.close());
  const form = (b: Record<string, string>) => ({ method: "POST", headers: { "content-type": "application/x-www-form-urlencoded" }, body: new URLSearchParams(b) });
  // log in as `cid` requesting `scope`; returns the token response and the consent page html
  const login = async (cid: string, redirect: string, scope?: string) => {
    const verifier = b64u(crypto.randomBytes(32));
    const u = new URL(`${base}/authorize`);
    const q: Record<string, string> = { response_type: "code", client_id: cid, redirect_uri: redirect, code_challenge: b64u(crypto.createHash("sha256").update(verifier).digest()), code_challenge_method: "S256", state: "s", resource: res };
    if (scope) q.scope = scope;
    Object.entries(q).forEach(([k, v]) => u.searchParams.set(k, v));
    const a = await fetch(u, { redirect: "manual" });
    expect(a.status).toBe(200);
    const html = await a.text();
    const rid = /name=rid value="([^"]+)"/.exec(html)![1];
    const c = await fetch(`${base}/consent`, { ...form({ rid, passphrase: PASS, decision: "approve" }), redirect: "manual" });
    expect(c.status).toBe(302);
    const code = new URL(c.headers.get("location")!).searchParams.get("code")!;
    const t = await fetch(`${base}/token`, form({ grant_type: "authorization_code", client_id: cid, code, code_verifier: verifier, redirect_uri: redirect, resource: res }));
    return { html, token: (await t.json()) as { access_token: string; refresh_token: string; scope: string }, csp: a.headers.get("content-security-policy") };
  };
  const rpc = async (tok: string, method: string, params?: unknown) => {
    const r = await fetch(res, { method: "POST", headers: { "content-type": "application/json", accept: "application/json, text/event-stream", authorization: `Bearer ${tok}` }, body: JSON.stringify({ jsonrpc: "2.0", id: 1, method, params }) });
    const text = await r.text();
    const data = text.startsWith("event") || text.includes("\ndata:") ? text.split("\n").filter((l) => l.startsWith("data:")).map((l) => l.slice(5).trim()).pop()! : text;
    return JSON.parse(data) as any;
  };
  const tools = async (tok: string) => ((await rpc(tok, "tools/list")).result.tools as any[]);
  const call = async (tok: string, name: string, args: unknown) => rpc(tok, "tools/call", { name, arguments: args });
  const audit = () => fs.readFileSync(path.join(dir, "audit.jsonl"), "utf8").trim().split("\n").map((l) => JSON.parse(l));
  return { base, dir, sup, login, tools, call, rpc, audit, provider, res };
}
const WRITE_NAMES = ["respond_to_session", "send_to_session"];
// the MCP SDK answers an unknown tool with either a JSON-RPC error or a result flagged isError: both mean "not callable"
const failed = (r: any) => !!(r.error || r.result?.isError);

describe("writes OFF (the default): the server stays read-only and does not even advertise gc:write", () => {
  it("metadata advertises gc:read only; asking for gc:write still yields gc:read; no write tools; the page says read-only", async () => {
    const s = await boot();
    const meta = (await (await fetch(`${s.base}/.well-known/oauth-authorization-server`)).json()) as any;
    expect(meta.scopes_supported).toEqual(["gc:read"]);
    const prm = (await (await fetch(`${s.base}/.well-known/oauth-protected-resource/mcp`)).json()) as any;
    expect(prm.scopes_supported).toEqual(["gc:read"]);
    const { html, token } = await s.login(CLAUDE, CB, "gc:read gc:write");
    expect(token.scope).toBe("gc:read");
    expect(html).toContain("read-only");
    expect(html).toContain("Scopes granted: <code>gc:read</code>");
    const names = (await s.tools(token.access_token)).map((t) => t.name);
    expect(names).toContain("city_status");
    for (const w of WRITE_NAMES) expect(names).not.toContain(w);
    expect(failed(await s.call(token.access_token, "send_to_session", { session: "mayor", text: "hi" }))).toBe(true);
    expect(s.sup.posts().length).toBe(0);
  });
});

describe("writes ON: scope gating end to end", () => {
  const on = { writes: { enabled: true } };
  it("advertises both scopes; the consent page shows exactly which are granted", async () => {
    const s = await boot(on);
    expect(((await (await fetch(`${s.base}/.well-known/oauth-authorization-server`)).json()) as any).scopes_supported).toEqual(["gc:read", "gc:write"]);
    expect(((await (await fetch(`${s.base}/.well-known/oauth-protected-resource/mcp`)).json()) as any).scopes_supported).toEqual(["gc:read", "gc:write"]);
    const ro = await s.login(CLAUDE, CB, "gc:read");
    expect(ro.token.scope).toBe("gc:read");
    expect(ro.html).toContain("Scopes granted: <code>gc:read</code>");
    expect(ro.html).toContain("read-only");
    const rw = await s.login(CLAUDE, CB, "gc:read gc:write");
    expect(rw.token.scope).toBe("gc:read gc:write");
    expect(rw.html).toContain("READ AND WRITE");
    expect(rw.html).toContain("Scopes granted: <code>gc:read gc:write</code>");
    expect(rw.html).toContain("never stop, kill, reset");
    const none = await s.login(CLAUDE, CB);   // no scope requested at all: read only, never write by default
    expect(none.token.scope).toBe("gc:read");
  });
  it("a gc:read token sees only read tools and cannot call a write tool; a gc:write token sees both, and the write tools carry no readOnlyHint", async () => {
    const s = await boot(on);
    const ro = (await s.login(CLAUDE, CB, "gc:read")).token.access_token;
    const rw = (await s.login(CLAUDE, CB, "gc:read gc:write")).token.access_token;
    const roTools = await s.tools(ro), rwTools = await s.tools(rw);
    for (const w of WRITE_NAMES) expect(roTools.map((t) => t.name)).not.toContain(w);
    expect(failed(await s.call(ro, "send_to_session", { session: "mayor", text: "hi" }))).toBe(true);
    expect(rwTools.map((t) => t.name)).toEqual(expect.arrayContaining(["city_status", ...WRITE_NAMES]));
    for (const t of rwTools) {
      if (WRITE_NAMES.includes(t.name)) { expect(t.annotations?.readOnlyHint).not.toBe(true); expect("readOnlyHint" in (t.annotations ?? {})).toBe(false); }
      else expect(t.annotations.readOnlyHint).toBe(true);
    }
    expect(s.sup.posts().length).toBe(0);
  });
  it("send_to_session works end to end with a gc:write token, and the audit trail names the client without the text", async () => {
    const s = await boot(on);
    const tok = (await s.login(CLAUDE, CB, "gc:read gc:write")).token.access_token;
    const r = await s.call(tok, "send_to_session", { session: "gc-architect", text: "please look at PR 286", intent: "follow_up" });
    expect(r.result.structuredContent).toMatchObject({ sent: true, session: "gc2-wisp-aaa", intent: "follow_up" });
    expect(r.result.content[0].text).toContain("gc-architect");
    expect(s.sup.posts().map((p) => p.path)).toEqual(["/v0/city/gc2/session/gc2-wisp-aaa/submit"]);
    const w = s.audit().filter((e) => e.kind === "write");
    expect(w).toHaveLength(1);
    expect(w[0]).toMatchObject({ app: "Claude (owner client)", session: "gc2-wisp-aaa", intent: "follow_up", result: "sent", ok: true, length: 21 });
    const tool = s.audit().filter((e) => e.kind === "tool" && e.tool === "send_to_session");
    expect(tool.every((e) => e.args === undefined)).toBe(true);   // the generic tool line never carries the arguments of a write
    const r2 = await s.call(tok, "respond_to_session", { session: "mayor", action: "allow" });
    expect(r2.result.structuredContent).toMatchObject({ sent: true, action: "allow" });
  });
  it("an over-long text is refused by the tool schema before the handler, and a deny-listed session is refused with nothing sent", async () => {
    const s = await boot(on);
    const tok = (await s.login(CLAUDE, CB, "gc:read gc:write")).token.access_token;
    const long = await s.call(tok, "send_to_session", { session: "mayor", text: "x".repeat(MAX_WRITE_TEXT + 1) });
    expect(failed(long)).toBe(true);
    const denied = await s.call(tok, "send_to_session", { session: "zachris-dev", text: "hi" });
    expect(denied.result.structuredContent).toMatchObject({ sent: false, reason: "denied_session" });
    expect(s.sup.posts().length).toBe(0);
  });
  it("WRITES_DISABLED in the state dir removes the write tools for the very next request; reads and the owner's other switches are unaffected", async () => {
    const s = await boot(on);
    const tok = (await s.login(CLAUDE, CB, "gc:read gc:write")).token.access_token;
    expect((await s.tools(tok)).map((t) => t.name)).toContain("send_to_session");
    fs.writeFileSync(path.join(s.dir, "WRITES_DISABLED"), "");
    const after = (await s.tools(tok)).map((t) => t.name);
    for (const w of WRITE_NAMES) expect(after).not.toContain(w);
    expect(after).toContain("city_status");
    expect((await s.call(tok, "city_status", {})).result.content[0].text).toMatch(/agents running/);
    expect(failed(await s.call(tok, "send_to_session", { session: "mayor", text: "hi" }))).toBe(true);
    expect(s.sup.posts().length).toBe(0);
    fs.unlinkSync(path.join(s.dir, "WRITES_DISABLED"));
    expect((await s.tools(tok)).map((t) => t.name)).toContain("send_to_session");
    fs.writeFileSync(path.join(s.dir, "DISABLED"), "");   // the existing kill switch still stops everything
    expect((await fetch(`${s.base}/mcp`, { method: "POST", headers: { authorization: `Bearer ${tok}` } })).status).toBe(503);
  });
  it("the ChatGPT client can hold gc:write too, and its writes are audited under its own name", async () => {
    const s = await boot(on);
    const g = await s.login(GPT, GPT_CB, "gc:read gc:write");
    expect(g.token.scope).toBe("gc:read gc:write");
    expect(g.html).toContain("ChatGPT");
    expect(g.csp).toContain("form-action 'self' https://chatgpt.com");
    await s.call(g.token.access_token, "send_to_session", { session: "mayor", text: "from the phone" });
    expect(s.audit().find((e) => e.kind === "write")).toMatchObject({ app: "ChatGPT (owner client)", result: "sent" });
  });
  it("a refresh keeps exactly the scopes that were granted: a read-only token can never become a write token", async () => {
    const s = await boot(on);
    const ro = (await s.login(CLAUDE, CB, "gc:read")).token;
    const r = await fetch(`${s.base}/token`, { method: "POST", headers: { "content-type": "application/x-www-form-urlencoded" },
      body: new URLSearchParams({ grant_type: "refresh_token", client_id: CLAUDE, refresh_token: ro.refresh_token, scope: "gc:read gc:write", resource: `${s.base}/mcp` }) });
    expect(r.status).toBe(200);
    const t = (await r.json()) as { access_token: string; scope: string };
    expect(t.scope).toBe("gc:read");
    expect((await s.tools(t.access_token)).map((x) => x.name)).not.toContain("send_to_session");
  });
});

describe("the 401 challenge asks for the scopes the server can grant (Claude's connector has no scope field)", () => {
  const challenge = async (s: Awaited<ReturnType<typeof boot>>, auth?: string) => {
    const r = await fetch(`${s.base}/mcp`, { method: "POST", headers: { "content-type": "application/json", ...(auth ? { authorization: auth } : {}) }, body: "{}" });
    return { status: r.status, header: r.headers.get("www-authenticate") ?? "" };
  };
  it("writes ON: a request with no token, a bad token and a malformed header are all challenged with scope=\"gc:read gc:write\" and still point at the resource metadata", async () => {
    const s = await boot({ writes: { enabled: true } });
    for (const auth of [undefined, "Bearer not-a-real-token", "Basic abc"]) {
      const c = await challenge(s, auth);
      expect(c.status, String(auth)).toBe(401);
      expect(c.header).toContain('scope="gc:read gc:write"');
      expect(c.header).not.toMatch(/scope="gc:read"/);
      expect(c.header).toContain(`resource_metadata="${s.base}/.well-known/oauth-protected-resource/mcp"`);
    }
    const prm = (await (await fetch(`${s.base}/.well-known/oauth-protected-resource/mcp`)).json()) as any;
    expect(prm.scopes_supported).toEqual(["gc:read", "gc:write"]);   // the metadata asks for the same set
  });
  it("writes OFF: the challenge stays gc:read", async () => {
    const s = await boot();
    const c = await challenge(s);
    expect(c.status).toBe(401);
    expect(c.header).toContain('scope="gc:read"');
    expect(c.header).not.toContain("gc:write");
    expect(((await (await fetch(`${s.base}/.well-known/oauth-protected-resource/mcp`)).json()) as any).scopes_supported).toEqual(["gc:read"]);
  });
  it("a gc:read-only token still READS and still cannot write; a token WITHOUT gc:read is refused even if it holds gc:write", async () => {
    const s = await boot({ writes: { enabled: true } });
    const ro = (await s.login(CLAUDE, CB, "gc:read")).token.access_token;
    expect((await s.call(ro, "city_status", {})).result.content[0].text).toMatch(/agents running/);       // reads work
    expect(failed(await s.call(ro, "send_to_session", { session: "mayor", text: "hi" }))).toBe(true);     // writes do not
    expect(s.sup.posts().length).toBe(0);
    const writeOnly = (s.provider as any).issue(CLAUDE, ["gc:write"], "fam-test") as { access_token: string };
    const r = await fetch(`${s.base}/mcp`, { method: "POST", headers: { "content-type": "application/json", accept: "application/json, text/event-stream", authorization: `Bearer ${writeOnly.access_token}` },
      body: JSON.stringify({ jsonrpc: "2.0", id: 1, method: "tools/list" }) });
    expect(r.status).toBe(403);
    expect(r.headers.get("www-authenticate")).toContain("insufficient_scope");
    expect(s.sup.posts().length).toBe(0);
  });
  it("a token granted both scopes (what Claude now asks for from the challenge) can read and write", async () => {
    const s = await boot({ writes: { enabled: true } });
    const scope = /scope="([^"]+)"/.exec((await challenge(s)).header)![1];     // a client that follows the challenge verbatim
    const tok = (await s.login(CLAUDE, CB, scope)).token;
    expect(tok.scope).toBe("gc:read gc:write");
    expect((await s.call(tok.access_token, "send_to_session", { session: "mayor", text: "hello" })).result.structuredContent).toMatchObject({ sent: true });
  });
});

describe("the consent page says plainly what is being granted", () => {
  it("read only: says so and that nothing can be sent", async () => {
    const s = await boot({ writes: { enabled: true } });
    const { html } = await s.login(CLAUDE, CB, "gc:read");
    expect(html).toContain("<b>read-only</b>");
    expect(html).toContain("cannot send anything");
    expect(html).not.toContain("READ AND WRITE");
  });
  it("read and write: says READ AND WRITE and that it can send messages to your agents", async () => {
    const s = await boot({ writes: { enabled: true } });
    const { html } = await s.login(CLAUDE, CB, "gc:read gc:write");
    expect(html).toContain("<b>READ AND WRITE</b>");
    expect(html).toContain("send messages to your agents");
    expect(html).toContain("never stop, kill, reset");
    expect(html).toContain("Scopes granted: <code>gc:read gc:write</code>");
  });
});
