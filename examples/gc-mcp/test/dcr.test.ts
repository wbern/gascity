import { describe, it, expect, afterAll } from "vitest";
import crypto from "node:crypto";
import fs from "node:fs";
import net from "node:net";
import os from "node:os";
import path from "node:path";
import { createApp, listen, type AppOpts } from "../src/app.js";
import { GcClient } from "../src/gc-client.js";
import { hashPassphrase } from "../src/oauth.js";
import { isSafeRedirect, validateExtraRedirects } from "../src/dcr.js";
import * as F from "./fixtures/index.js";

const PASS = "correct horse battery staple";
const HASH = hashPassphrase(PASS);
const CLAUDE_CB = "https://claude.ai/api/mcp/auth_callback";
const CLAUDE_CB2 = "https://claude.com/api/mcp/auth_callback";
const GPT_CB = "https://chatgpt.com/connector/oauth/cb_123-AB";
const GPT_PLATFORM = "https://chatgpt.com/connector_platform_oauth_redirect";
const LOOP = "http://127.0.0.1:53682/callback";
const b64u = (b: Buffer) => b.toString("base64url");
const cleanups: (() => void)[] = [];
afterAll(() => cleanups.forEach((f) => f()));
const fetchImpl = (async (url: string) => new Response(JSON.stringify(new URL(url).pathname.endsWith("/usage") ? F.usage : F.status), { status: 200 })) as unknown as typeof fetch;
const freePort = () => new Promise<number>((res) => { const s = net.createServer().listen(0, "127.0.0.1", () => { const p = (s.address() as net.AddressInfo).port; s.close(() => res(p)); }); });

async function boot(over: Partial<AppOpts> = {}) {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "gcmcp-dcr-"));
  const port = await freePort();
  const base = `http://127.0.0.1:${port}`;
  const res = `${base}/mcp`;
  const clock = { t: 1_800_000_000_000 };
  const { app, provider } = createApp({
    deps: { gc: new GcClient({ baseUrl: "http://127.0.0.1:1", city: "gc2", fetchImpl }), mailAgent: "human", now: () => new Date() },
    publicUrl: new URL(base), ownerHash: HASH, staticClientId: "static-claude-id-0123456789",
    stateFile: path.join(dir, "state.json"), auditFile: path.join(dir, "audit.jsonl"), controlDir: dir, now: () => clock.t, ...over,
  });
  const srv = await listen(app, port);
  cleanups.push(() => { srv.close(); fs.rmSync(dir, { recursive: true, force: true }); });
  const register = (body: object, raw?: string) => fetch(`${base}/register`, { method: "POST", headers: { "content-type": "application/json" }, body: raw ?? JSON.stringify(body) });
  const reg = async (uris: string[], extra: object = {}) => register({ redirect_uris: uris, client_name: "Some App", token_endpoint_auth_method: "none", ...extra });
  const registered = async (uris: string[], extra: object = {}) => { const r = await reg(uris, extra); expect(r.status, JSON.stringify(uris)).toBe(201); return (await r.json()) as any; };
  const authorize = (cid: string, redirect: string, scope?: string) => {
    const verifier = b64u(crypto.randomBytes(32));
    const u = new URL(`${base}/authorize`);
    const q: Record<string, string> = { response_type: "code", client_id: cid, redirect_uri: redirect, code_challenge: b64u(crypto.createHash("sha256").update(verifier).digest()), code_challenge_method: "S256", state: "s", resource: res };
    if (scope) q.scope = scope;
    Object.entries(q).forEach(([k, v]) => u.searchParams.set(k, v));
    return fetch(u, { redirect: "manual" }).then((r) => ({ r, verifier }));
  };
  const form = (b: Record<string, string>) => ({ method: "POST", headers: { "content-type": "application/x-www-form-urlencoded" }, body: new URLSearchParams(b) });
  const consent = (html: string, passphrase = PASS) => fetch(`${base}/consent`, { ...form({ rid: /name=rid value="([^"]+)"/.exec(html)![1], passphrase, decision: "approve" }), redirect: "manual" });
  // the whole flow for a registered client: returns the page, the consent response and (when approved) the token
  const login = async (cid: string, redirect: string, scope?: string) => {
    const { r, verifier } = await authorize(cid, redirect, scope);
    const html = await r.text();
    expect(r.status).toBe(200);
    const c = await consent(html);
    const code = new URL(c.headers.get("location")!).searchParams.get("code")!;
    const t = await fetch(`${base}/token`, form({ grant_type: "authorization_code", client_id: cid, code, code_verifier: verifier, redirect_uri: redirect, resource: res }));
    return { html, csp: r.headers.get("content-security-policy"), token: (await t.json()) as any };
  };
  const meta = async () => (await (await fetch(`${base}/.well-known/oauth-authorization-server`)).json()) as any;
  const state = () => (fs.existsSync(path.join(dir, "state.json")) ? JSON.parse(fs.readFileSync(path.join(dir, "state.json"), "utf8")) : { clients: {} });
  return { base, dir, clock, provider, register, reg, registered, authorize, consent, login, form, meta, state, res };
}
const ON = { allowDcr: true };

// ---------------------------------------------------------------- the redirect allowlist, as a table
describe("which redirect URIs may register", () => {
  const allowed = [
    CLAUDE_CB, CLAUDE_CB2, GPT_CB, "https://chatgpt.com/connector/oauth/A", "https://chatgpt.com/connector/oauth/" + "a".repeat(128), GPT_PLATFORM,
    "http://127.0.0.1:8080/cb", "http://127.0.0.1:1/x/y", "http://127.0.0.1:65535/", "http://localhost:3000/oauth/callback", "http://localhost:53682", "http://127.0.0.1:8080",
  ];
  const denied = [
    // look-alike hosts and userinfo
    "https://chatgpt.com.evil.com/connector/oauth/abc", "https://claude.ai@evil.com/api/mcp/auth_callback", "https://evil.com@claude.ai/api/mcp/auth_callback", "https://user:pw@chatgpt.com/connector/oauth/abc",
    "https://claude.ai.evil.com/api/mcp/auth_callback", "https://evilclaude.ai/api/mcp/auth_callback", "https://evil.com/https://claude.ai/api/mcp/auth_callback",
    "https://chatgpt.com@evil.com/connector/oauth/abc", "https://chat.openai.com/connector/oauth/abc",
    "http://localhost.evil.com:3000/cb", "http://127.0.0.1.evil.com:3000/cb", "http://localhost@evil.com:3000/cb", "http://127.0.0.1@evil.com/cb", "http://evil.com:80/http://localhost:3000/cb",
    // schemes and transport
    "http://claude.ai/api/mcp/auth_callback", "http://chatgpt.com/connector/oauth/abc", "https://localhost:3000/cb", "https://127.0.0.1:3000/cb", "ftp://127.0.0.1:3000/cb", "javascript:alert(1)", "data:text/html,x", "claude://cb",
    // fragments, queries, wildcards, whitespace, uppercase tricks, wrong paths
    "https://claude.ai/api/mcp/auth_callback#frag", "https://claude.ai/api/mcp/auth_callback?x=1", "https://chatgpt.com/connector/oauth/abc#f", "https://chatgpt.com/connector/oauth/abc?x=1",
    "https://*.claude.ai/api/mcp/auth_callback", "https://claude.ai/api/mcp/*", "https://chatgpt.com/connector/oauth/*", "http://127.0.0.1:*/cb", "http://localhost:3000/*",
    "https://CLAUDE.ai/api/mcp/auth_callback", "https://Claude.AI/api/mcp/auth_callback", "https://chatgpt.com/Connector/oauth/abc", "HTTPS://claude.ai/api/mcp/auth_callback", "http://LOCALHOST:3000/cb", "http://127.0.0.1:3000/CB ",
    " https://claude.ai/api/mcp/auth_callback", "https://claude.ai/api/mcp/auth_callback ", "https://claude.ai/api/mcp/auth_callback\\", "https://claude.ai/api/mcp/auth_callback/", "https://claude.ai/api/mcp/auth_callback/x",
    "https://chatgpt.com/connector/oauth/", "https://chatgpt.com/connector/oauth/a/b", "https://chatgpt.com/connector/oauth/" + "a".repeat(129), "https://chatgpt.com/connector/oauth/a b", "https://chatgpt.com/connector/oauth/a.b",
    "https://chatgpt.com/connector_platform_oauth_redirect/x", "https://chatgpt.com/connector_platform_oauth_redirect?a=1", "https://chatgpt.com/", "https://chatgpt.com", "https://chatgpt.com:8443/connector/oauth/abc",
    // loopback strictness
    "http://127.0.0.1/cb", "http://127.0.0.1:0/cb", "http://127.0.0.1:65536/cb", "http://127.0.0.1:99999/cb", "http://[::1]:8080/cb", "http://0.0.0.0:8080/cb", "http://127.0.0.2:8080/cb", "http://localhost.:8080/cb",
    "http://127.0.0.1:8080/cb?x=1", "http://127.0.0.1:8080/cb#f", "http://127.0.0.1:8080/c b", "http://127.0.0.1:8080/..%2f..", "http://127.0.0.1:08080/cb", "http://127.0.0.1:8080/cb\\x",
    // anything else
    "https://evil.example/cb", "https://example.com/callback", "", "not a url", "//claude.ai/api/mcp/auth_callback",
  ];
  it("every allowed family is accepted", () => { for (const u of allowed) expect(isSafeRedirect(u, {}), u).toBe(true); });
  it("every look-alike, userinfo, fragment, query, wildcard, scheme, case and path trick is rejected", () => { for (const u of denied) expect(isSafeRedirect(u, {}), JSON.stringify(u)).toBe(false); });
  it("extra redirects are EXACT strings and nothing broader", () => {
    const extra = ["https://app.example.com/oauth/cb", "http://127.0.0.1:9999/special"];
    expect(isSafeRedirect("https://app.example.com/oauth/cb", { extra })).toBe(true);
    for (const u of ["https://app.example.com/oauth/cb2", "https://app.example.com/oauth/cb/", "https://app.example.com/", "https://other.example.com/oauth/cb", "https://app.example.com.evil.com/oauth/cb"]) expect(isSafeRedirect(u, { extra }), u).toBe(false);
    expect(validateExtraRedirects(extra)).toEqual(extra);
    for (const bad of ["http://app.example.com/cb", "https://u@app.example.com/cb", "https://app.example.com/cb#f", "https://*.example.com/cb", "https://app.example.com/ cb", "ftp://x/y", "nope"]) expect(() => validateExtraRedirects([bad]), bad).toThrow();
  });
  it("the unsafe OPEN switch lets any clean https URI through, and still refuses the dangerous shapes", () => {
    const open = { open: true };
    for (const u of ["https://evil.example/cb", "https://app.example.com/a/b", "https://example.com:8443/cb"]) expect(isSafeRedirect(u, open), u).toBe(true);
    for (const u of ["https://u@evil.example/cb", "https://evil.example/cb#f", "https://*.evil.example/cb", "javascript:alert(1)", "http://evil.example/cb", "https://evil.example/c b", "https://EVIL.example/cb"]) expect(isSafeRedirect(u, open), u).toBe(false);
  });
});

// ---------------------------------------------------------------- the endpoint
describe("the registration endpoint exists only when switched on", () => {
  it("OFF (the default): no /register route and no registration_endpoint in the metadata", async () => {
    const s = await boot();
    expect((await s.reg([CLAUDE_CB])).status).toBe(404);
    expect((await s.meta()).registration_endpoint).toBeUndefined();
  });
  it("ON: advertised as <issuer>/register and answers 201 for a safe client", async () => {
    const s = await boot(ON);
    expect((await s.meta()).registration_endpoint).toBe(`${s.base}/register`);
    const c = await s.registered([CLAUDE_CB]);
    expect(c.client_id).toBeTruthy();
  });
});

describe("registration accepts only safe redirects, and forces a PUBLIC client", () => {
  it("each safe family registers; a client with ANY unsafe redirect is rejected with invalid_redirect_uri", async () => {
    const s = await boot({ ...ON, dcr: { perHour: 1000 } });
    for (const u of [CLAUDE_CB, CLAUDE_CB2, GPT_CB, GPT_PLATFORM, LOOP, "http://localhost:3000/cb"]) await s.registered([u]);
    for (const bad of [["https://evil.example/cb"], ["https://chatgpt.com.evil.com/connector/oauth/abc"], ["https://claude.ai@evil.com/api/mcp/auth_callback"], ["http://localhost.evil.com:3000/cb"],
      [CLAUDE_CB, "https://evil.example/cb"], ["https://evil.example/cb", CLAUDE_CB], [GPT_CB, "javascript:alert(1)"]]) {
      const r = await s.reg(bad);
      expect(r.status, JSON.stringify(bad)).toBe(400);
      expect(((await r.json()) as any).error, JSON.stringify(bad)).toBe("invalid_redirect_uri");
    }
    expect((await s.reg([])).status).toBe(400);
    expect((await s.reg(Array.from({ length: 6 }, (_, i) => `${GPT_CB}${i}`))).status).toBe(400);     // too many redirect URIs
  });
  it("a requested secret method is ignored: the client is public, PKCE-only, auth-code + refresh, and no secret is ever created or stored", async () => {
    const s = await boot(ON);
    for (const method of ["client_secret_basic", "client_secret_post", "private_key_jwt", "none"]) {
      const c = await s.registered([CLAUDE_CB], { token_endpoint_auth_method: method, grant_types: ["client_credentials", "password"], response_types: ["token"], scope: "everything", client_uri: "https://evil.example", logo_uri: "https://evil.example/l.png", jwks_uri: "https://evil.example/jwks" });
      expect(c.token_endpoint_auth_method).toBe("none");
      expect(c.client_secret).toBeUndefined();
      expect(c.client_secret_expires_at).toBeUndefined();
      expect(c.grant_types).toEqual(["authorization_code", "refresh_token"]);
      expect(c.response_types).toEqual(["code"]);
      expect(c.client_uri).toBeUndefined(); expect(c.logo_uri).toBeUndefined(); expect(c.jwks_uri).toBeUndefined();
      expect(c.scope).toBe("gc:read");
    }
    const st = JSON.stringify(s.state());
    expect(st).not.toContain("client_secret"); expect(st).not.toContain("evil.example"); expect(st).not.toContain("client_credentials");
  });
  it("names are sanitised: control and bidi characters removed, whitespace collapsed, 60 characters at most, never empty", async () => {
    const s = await boot(ON);
    expect((await s.registered([CLAUDE_CB], { client_name: "N".repeat(500) })).client_name.length).toBeLessThanOrEqual(60);
    expect((await s.registered([CLAUDE_CB], { client_name: "Cla\u0000ude‮  \n\t  App​" })).client_name).toBe("Claude App");
    expect((await s.registered([CLAUDE_CB], { client_name: "   " })).client_name).toBe("client");
    expect((await s.registered([CLAUDE_CB], { client_name: undefined })).client_name).toBe("client");
  });
  it("the request size is capped", async () => {
    const s = await boot(ON);
    const big = await s.register({ redirect_uris: [CLAUDE_CB], client_name: "x", padding: "p".repeat(20_000) });
    expect([400, 413]).toContain(big.status);
    expect((await s.register({}, "{not json")).status).toBe(400);
    expect(Object.keys(s.state().clients ?? {}).length).toBe(0);
  });
});

describe("abuse limits", () => {
  it("global registration rate limit (one shared source behind the SNAT): 10 an hour by default, 429 after, then it recovers", async () => {
    const s = await boot(ON);
    for (let i = 0; i < 10; i++) await s.registered([CLAUDE_CB]);
    const r = await s.reg([CLAUDE_CB]);
    expect(r.status).toBe(429);
    s.clock.t += 61 * 60_000;
    expect((await s.reg([CLAUDE_CB])).status).toBe(201);
  });
  it("rejected registrations do not spend the limit (an attacker spraying bad redirects cannot lock the owner out)", async () => {
    const s = await boot(ON);
    for (let i = 0; i < 50; i++) expect((await s.reg(["https://evil.example/cb"])).status).toBe(400);
    for (let i = 0; i < 10; i++) await s.registered([CLAUDE_CB]);
    expect((await s.reg([CLAUDE_CB])).status).toBe(429);
  });
  it("at the cap the OLDEST UNUSED client is pruned; clients that completed a login survive; the count never exceeds the cap", async () => {
    const s = await boot({ ...ON, dcr: { maxClients: 3, perHour: 1000 } });
    const a = await s.registered([CLAUDE_CB]);     // will be used
    await s.login(a.client_id, CLAUDE_CB);
    s.clock.t += 1000;
    const b = await s.registered([CLAUDE_CB]); s.clock.t += 1000;
    const c = await s.registered([CLAUDE_CB]); s.clock.t += 1000;
    const d = await s.registered([CLAUDE_CB]);     // 4th: prunes the oldest UNUSED (b), not the older but used a
    expect(Object.keys(s.state().clients).sort()).toEqual([a.client_id, c.client_id, d.client_id].sort());
    expect((await s.authorize(b.client_id, CLAUDE_CB)).r.status).toBe(400);
    expect((await s.authorize(a.client_id, CLAUDE_CB)).r.status).toBe(200);
    for (let i = 0; i < 20; i++) { s.clock.t += 1000; await s.registered([CLAUDE_CB]); }
    expect(Object.keys(s.state().clients).length).toBeLessThanOrEqual(3);
    expect(Object.keys(s.state().clients)).toContain(a.client_id);          // the used one is still there after 20 more
  });
  it("a client expires after 90 days without use, and use renews it", async () => {
    const s = await boot(ON);
    const stale = await s.registered([CLAUDE_CB]);
    const live = await s.registered([CLAUDE_CB]);
    s.clock.t += 89 * 86_400_000;
    expect((await s.authorize(live.client_id, CLAUDE_CB)).r.status).toBe(200);    // used on day 89: renewed
    s.clock.t += 2 * 86_400_000;                                                  // day 91
    expect((await s.authorize(stale.client_id, CLAUDE_CB)).r.status).toBe(400);   // never used for 91 days: gone
    expect((await s.authorize(live.client_id, CLAUDE_CB)).r.status).toBe(200);    // used 2 days ago: alive
    expect(Object.keys(s.state().clients)).not.toContain(stale.client_id);
  });
});

describe("authorize and the full flow stay gated by the passphrase", () => {
  it("authorize refuses a redirect that was not registered, and an unknown client", async () => {
    const s = await boot(ON);
    const c = await s.registered([CLAUDE_CB]);
    for (const other of [CLAUDE_CB2, GPT_CB, "https://evil.example/cb", CLAUDE_CB + "/", LOOP]) expect((await s.authorize(c.client_id, other)).r.status, other).toBe(400);
    expect((await s.authorize("no-such-client", CLAUDE_CB)).r.status).toBe(400);
  });
  it("register -> authorize -> consent -> token: no passphrase, no token; the right one issues a working token", async () => {
    const s = await boot(ON);
    const c = await s.registered([GPT_CB], { client_name: "ChatGPT" });
    const { r, verifier } = await s.authorize(c.client_id, GPT_CB);
    const html = await r.text();
    const wrong = await s.consent(html, "wrong passphrase");
    expect(wrong.status).toBe(401);
    expect(wrong.headers.get("location")).toBeNull();
    const ok = await s.consent(html);
    expect(ok.status).toBe(302);
    expect(ok.headers.get("location")!.startsWith(GPT_CB + "?")).toBe(true);
    const code = new URL(ok.headers.get("location")!).searchParams.get("code")!;
    const t = await (await fetch(`${s.base}/token`, s.form({ grant_type: "authorization_code", client_id: c.client_id, code, code_verifier: verifier, redirect_uri: GPT_CB, resource: s.res }))).json() as any;
    expect(t.scope).toBe("gc:read");
    const call = await fetch(s.res, { method: "POST", headers: { "content-type": "application/json", accept: "application/json, text/event-stream", authorization: `Bearer ${t.access_token}` }, body: JSON.stringify({ jsonrpc: "2.0", id: 1, method: "tools/list" }) });
    expect(call.status).toBe(200);
  });
  it("PKCE is mandatory: a wrong verifier is refused", async () => {
    const s = await boot(ON);
    const c = await s.registered([CLAUDE_CB]);
    const { r } = await s.authorize(c.client_id, CLAUDE_CB);
    const ok = await s.consent(await r.text());
    const code = new URL(ok.headers.get("location")!).searchParams.get("code")!;
    const t = await fetch(`${s.base}/token`, s.form({ grant_type: "authorization_code", client_id: c.client_id, code, code_verifier: b64u(crypto.randomBytes(32)), redirect_uri: CLAUDE_CB, resource: s.res }));
    expect(t.status).toBe(400);
    const noChallenge = new URL(`${s.base}/authorize`);
    Object.entries({ response_type: "code", client_id: c.client_id, redirect_uri: CLAUDE_CB, state: "s", resource: s.res }).forEach(([k, v]) => noChallenge.searchParams.set(k, v));
    const refused = await fetch(noChallenge, { redirect: "manual" });             // no code_challenge: refused, reported to the registered redirect, never a code
    expect(refused.headers.get("location") ?? "").toContain("error=invalid_request");
    expect(refused.headers.get("location") ?? "").not.toContain("code=");
  });
  it("a code or refresh token from one registered client is useless to another", async () => {
    const s = await boot(ON);
    const a = await s.registered([CLAUDE_CB]), b = await s.registered([CLAUDE_CB]);
    const la = await s.login(a.client_id, CLAUDE_CB);
    const r = await fetch(`${s.base}/token`, s.form({ grant_type: "refresh_token", client_id: b.client_id, refresh_token: la.token.refresh_token, resource: s.res }));
    expect(r.status).toBe(400);
  });
});

describe("the consent page tells you who is asking", () => {
  it("a new dynamic client: UNVERIFIED name, the redirect HOST in bold, 'first time', how long ago it registered, read-only", async () => {
    const s = await boot(ON);
    const c = await s.registered([CLAUDE_CB], { client_name: "Totally Claude" });
    s.clock.t += 5 * 60_000;
    const { r } = await s.authorize(c.client_id, CLAUDE_CB);
    const html = await r.text();
    expect(html).toContain("UNVERIFIED");
    expect(html).toContain("Totally Claude");
    expect(html).toContain("<b>claude.ai</b>");
    expect(html).toMatch(/first time/i);
    expect(html).toMatch(/registered 5 minutes ago/);
    expect(html).toContain("<b>read-only</b>");
    expect(html).toContain("Scopes granted: <code>gc:read</code>");
  });
  it("a client that has logged in before is shown as known, with when it was last used", async () => {
    const s = await boot(ON);
    const c = await s.registered([GPT_CB]);
    await s.login(c.client_id, GPT_CB);
    s.clock.t += 3 * 3_600_000;
    const html = await (await s.authorize(c.client_id, GPT_CB)).r.text();
    expect(html).toMatch(/signed in before/i);
    expect(html).toMatch(/last used 3 hours ago/);
    expect(html).not.toMatch(/first time/i);
    expect(html).toContain("<b>chatgpt.com</b>");
  });
  it("the page escapes a hostile name and its CSP lets the form post only to the registered redirect's origin", async () => {
    const s = await boot(ON);
    const c = await s.registered([LOOP], { client_name: '<script>alert(1)</script>"><img src=x>' });
    const { r } = await s.authorize(c.client_id, LOOP);
    const html = await r.text();
    expect(html).not.toContain("<script>");
    expect(html).not.toContain("<img src=x>");
    expect(r.headers.get("content-security-policy")).toContain("form-action 'self' http://127.0.0.1:53682");
    expect(r.headers.get("content-security-policy")).not.toContain("claude");
    expect(html).toContain("<b>127.0.0.1:53682</b>");
  });
});

describe("write scope for registered clients", () => {
  const writes = { writes: { enabled: true } };
  it("with writes on, a registered client that asks for gc:write sees READ AND WRITE and, after the passphrase, gets it", async () => {
    const s = await boot({ ...ON, ...writes });
    const c = await s.registered([CLAUDE_CB]);
    const { html, token } = await s.login(c.client_id, CLAUDE_CB, "gc:read gc:write");
    expect(html).toContain("<b>READ AND WRITE</b>");
    expect(html).toContain("send messages to your agents");
    expect(token.scope).toBe("gc:read gc:write");
    const ro = await s.login((await s.registered([CLAUDE_CB])).client_id, CLAUDE_CB, "gc:read");
    expect(ro.token.scope).toBe("gc:read");
  });
  it("with the UNSAFE open-redirect switch on, registered clients can never hold gc:write (any site could be the redirect)", async () => {
    const s = await boot({ ...ON, ...writes, dcr: { openRedirects: true } });
    const c = await s.registered(["https://anything.example/cb"]);
    const { html, token } = await s.login(c.client_id, "https://anything.example/cb", "gc:read gc:write");
    expect(token.scope).toBe("gc:read");
    expect(html).toContain("<b>read-only</b>");
  });
});

describe("the unsafe switch is off by default and loudly named", () => {
  it("default: any https redirect is refused; with the switch ON it is accepted", async () => {
    const off = await boot(ON);
    expect((await off.reg(["https://evil.example/cb"])).status).toBe(400);
    const on = await boot({ ...ON, dcr: { openRedirects: true } });
    expect((await on.reg(["https://evil.example/cb"])).status).toBe(201);
    expect((await on.reg(["https://user@evil.example/cb"])).status).toBe(400);   // still not userinfo
  });
  it("GC_MCP_DCR_EXTRA_REDIRECTS: an exact extra URI registers, a neighbour does not", async () => {
    const s = await boot({ ...ON, dcr: { extraRedirects: ["https://app.example.com/oauth/cb"] } });
    expect((await s.reg(["https://app.example.com/oauth/cb"])).status).toBe(201);
    expect((await s.reg(["https://app.example.com/oauth/cb2"])).status).toBe(400);
  });
});

describe("REVOKE_ALL and the static clients", () => {
  it("REVOKE_ALL removes every registered client and its tokens; the static client keeps working", async () => {
    const s = await boot(ON);
    const c = await s.registered([CLAUDE_CB]);
    const l = await s.login(c.client_id, CLAUDE_CB);
    fs.writeFileSync(path.join(s.dir, "REVOKE_ALL"), "");
    const call = await fetch(s.res, { method: "POST", headers: { authorization: `Bearer ${l.token.access_token}` } });
    expect(call.status).toBe(401);
    expect((await s.authorize(c.client_id, CLAUDE_CB)).r.status).toBe(400);        // the client itself is gone
    expect(Object.keys(s.state().clients ?? {}).length).toBe(0);
    expect((await s.authorize("static-claude-id-0123456789", CLAUDE_CB)).r.status).toBe(200);
  });
  it("registered clients survive a restart (state file) until they expire", async () => {
    const s = await boot(ON);
    const c = await s.registered([CLAUDE_CB]);
    expect(s.state().clients[c.client_id].redirect_uris).toEqual([CLAUDE_CB]);
    expect(JSON.stringify(s.state())).not.toMatch(/secret/i);
  });
});
