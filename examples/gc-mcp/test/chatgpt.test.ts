import { describe, it, expect, afterAll } from "vitest";
import crypto from "node:crypto";
import fs from "node:fs";
import net from "node:net";
import os from "node:os";
import path from "node:path";
import { createApp, listen, type AppOpts } from "../src/app.js";
import { GcClient } from "../src/gc-client.js";
import { hashPassphrase, validateChatgptRedirects } from "../src/oauth.js";
import * as F from "./fixtures/index.js";

const PASS = "correct horse battery staple";
const CLAUDE = "claude-client-id-0123456789ab";
const GPT = "chatgpt-client-id-0123456789";
const SECRET = "a-long-chatgpt-client-secret-value";
const CLAUDE_CB = "https://claude.ai/api/mcp/auth_callback";
const GPT_CB = "https://chatgpt.com/connector/oauth/cb123abc";
const b64u = (b: Buffer) => b.toString("base64url");
const HASH = hashPassphrase(PASS);
const SECRET_HASH = hashPassphrase(SECRET);
const cleanups: (() => void)[] = [];
afterAll(() => cleanups.forEach((f) => f()));

const fetchImpl = (async (url: string) => {
  const p = new URL(url).pathname;
  return new Response(JSON.stringify(p.endsWith("/status") ? F.status : p.endsWith("/usage") ? F.usage : { items: [] }), { status: 200 });
}) as unknown as typeof fetch;
const freePort = () => new Promise<number>((res) => { const s = net.createServer().listen(0, "127.0.0.1", () => { const p = (s.address() as net.AddressInfo).port; s.close(() => res(p)); }); });

async function boot(over: Partial<AppOpts> = {}) {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "gcmcp-gpt-"));
  const port = await freePort();
  const base = `http://127.0.0.1:${port}`;
  const res = `${base}/mcp`;
  const { app } = createApp({
    deps: { gc: new GcClient({ baseUrl: "http://127.0.0.1:1", city: "gc2", fetchImpl }), mailAgent: "human", now: () => new Date() },
    publicUrl: new URL(base), ownerHash: HASH, staticClientId: CLAUDE, stateFile: path.join(dir, "state.json"),
    auditFile: path.join(dir, "audit.jsonl"), controlDir: dir, ...over,
  });
  const srv = await listen(app, port);
  cleanups.push(() => { srv.close(); fs.rmSync(dir, { recursive: true, force: true }); });
  const authorize = (cid: string, redirect: string, challenge = "x".repeat(43), resource = res) => {
    const u = new URL(`${base}/authorize`);
    Object.entries({ response_type: "code", client_id: cid, redirect_uri: redirect, code_challenge: challenge, code_challenge_method: "S256", state: "st8", scope: "gc:read", resource })
      .forEach(([k, v]) => u.searchParams.set(k, v));
    return fetch(u, { redirect: "manual" });
  };
  const ridOf = (html: string) => /name=rid value="([^"]+)"/.exec(html)![1];
  const consent = (rid: string, passphrase = PASS) => fetch(`${base}/consent`, { method: "POST", redirect: "manual",
    headers: { "content-type": "application/x-www-form-urlencoded" }, body: new URLSearchParams({ rid, passphrase, decision: "approve" }) });
  const form = (b: Record<string, string>) => ({ method: "POST", headers: { "content-type": "application/x-www-form-urlencoded" }, body: new URLSearchParams(b) });
  // full approved flow for one client; returns the code and verifier
  const approve = async (cid: string, redirect: string) => {
    const verifier = b64u(crypto.randomBytes(32));
    const a = await authorize(cid, redirect, b64u(crypto.createHash("sha256").update(verifier).digest()));
    expect(a.status).toBe(200);
    const c = await consent(ridOf(await a.text()));
    expect(c.status).toBe(302);
    return { verifier, code: new URL(c.headers.get("location")!).searchParams.get("code")!, location: c.headers.get("location")! };
  };
  const exchange = (cid: string, redirect: string, code: string, verifier: string, extra: Record<string, string> = {}) =>
    fetch(`${base}/token`, form({ grant_type: "authorization_code", client_id: cid, code, code_verifier: verifier, redirect_uri: redirect, resource: res, ...extra }));
  const refresh = (cid: string, rt: string, extra: Record<string, string> = {}) =>
    fetch(`${base}/token`, form({ grant_type: "refresh_token", client_id: cid, refresh_token: rt, resource: res, ...extra }));
  const mcp = (tok: string) => fetch(res, { method: "POST", headers: { "content-type": "application/json", accept: "application/json, text/event-stream", authorization: `Bearer ${tok}` },
    body: JSON.stringify({ jsonrpc: "2.0", id: 1, method: "tools/list" }) });
  const audit = () => fs.readFileSync(path.join(dir, "audit.jsonl"), "utf8");
  return { base, res, dir, authorize, consent, approve, exchange, refresh, mcp, form, audit };
}
const on = { clientId: GPT, redirectUris: [GPT_CB] };

describe("ChatGPT static client: off by default", () => {
  it("its client id is unknown when not enabled", async () => {
    const s = await boot();
    const r = await s.authorize(GPT, GPT_CB);
    expect(r.status).toBe(400);
    expect(await r.text()).not.toContain("Approve Gas City");
  });
  it("no /register even when ChatGPT is enabled (no dynamic registration)", async () => {
    const s = await boot({ chatgpt: on });
    expect((await fetch(`${s.base}/register`, { method: "POST", headers: { "content-type": "application/json" }, body: JSON.stringify({ redirect_uris: [GPT_CB] }) })).status).toBe(404);
  });
});

describe("redirect allowlist fails closed and is chatgpt.com only", () => {
  it("enabled with NO redirects cannot authorize", async () => {
    const s = await boot({ chatgpt: { clientId: GPT, redirectUris: [] } });
    const r = await s.authorize(GPT, GPT_CB);
    expect(r.status).toBe(400);
    expect(await r.text()).not.toContain("Approve Gas City");
  });
  it("only the exact configured URI is accepted", async () => {
    const s = await boot({ chatgpt: on });
    expect((await s.authorize(GPT, GPT_CB)).status).toBe(200);
    for (const other of [GPT_CB + "x", "https://chatgpt.com/connector/oauth/other", "https://chatgpt.com.evil.example/connector/oauth/cb123abc", CLAUDE_CB]) {
      expect((await s.authorize(GPT, other)).status, other).toBe(400);
    }
  });
  it("validateChatgptRedirects accepts exact chatgpt.com https URIs and rejects everything else", () => {
    expect(validateChatgptRedirects([GPT_CB, "https://chatgpt.com/connector/oauth/zzz"])).toEqual([GPT_CB, "https://chatgpt.com/connector/oauth/zzz"]);
    for (const bad of ["http://chatgpt.com/connector/oauth/a", "https://chat.openai.com/a/b", "https://chatgpt.com.evil.example/a", "https://evil.example/https://chatgpt.com/a",
      "https://user@chatgpt.com/a", "https://chatgpt.com:8443/a", "https://chatgpt.com/a#frag", "https://chatgpt.com/", "https://chatgpt.com", "https://*.chatgpt.com/a",
      CLAUDE_CB, "javascript:alert(1)", "https://chatgpt.com/a b", ""]) {
      expect(() => validateChatgptRedirects([bad]), bad).toThrow();
    }
  });
  it("createApp refuses a ChatGPT client id equal to the Claude client id", () => {
    expect(() => createApp({ deps: { gc: new GcClient({ baseUrl: "http://127.0.0.1:1", city: "gc2", fetchImpl }), mailAgent: "human", now: () => new Date() },
      publicUrl: new URL("http://127.0.0.1:1"), ownerHash: HASH, staticClientId: CLAUDE, chatgpt: { clientId: CLAUDE, redirectUris: [GPT_CB] } })).toThrow();
  });
});

describe("a full ChatGPT login (public client, PKCE)", () => {
  it("consent names ChatGPT, the CSP allows only chatgpt.com, tokens work, and the audit log names the client", async () => {
    const s = await boot({ chatgpt: on });
    const a = await s.authorize(GPT, GPT_CB);
    const csp = a.headers.get("content-security-policy")!;
    expect(csp).toContain("form-action 'self' https://chatgpt.com");
    expect(csp).not.toContain("claude");
    const html = await a.text();
    expect(html).toContain("ChatGPT");
    expect(html).not.toContain("Claude");
    const { code, verifier, location } = await s.approve(GPT, GPT_CB);
    expect(location.startsWith(GPT_CB + "?")).toBe(true);
    const t = await s.exchange(GPT, GPT_CB, code, verifier);
    expect(t.status).toBe(200);
    const tok = (await t.json()) as { access_token: string; refresh_token: string };
    expect((await s.mcp(tok.access_token)).status).toBe(200);
    const log = s.audit();
    expect(log).toContain("ChatGPT");
    expect(log).not.toContain(tok.access_token);
  });
  it("a wrong resource is refused for ChatGPT too", async () => {
    const s = await boot({ chatgpt: on });
    expect(await (await s.authorize(GPT, GPT_CB, undefined, "https://evil.example/mcp")).text()).toContain("invalid_target");
  });
  it("a wrong passphrase never reaches a redirect", async () => {
    const s = await boot({ chatgpt: on });
    const a = await s.authorize(GPT, GPT_CB);
    const c = await s.consent(/name=rid value="([^"]+)"/.exec(await a.text())![1], "wrong");
    expect(c.status).toBe(401);
  });
});

describe("Claude is unchanged and the two clients are isolated", () => {
  it("the Claude page keeps the Claude CSP and name; Claude cannot use the ChatGPT redirect", async () => {
    const s = await boot({ chatgpt: on });
    const a = await s.authorize(CLAUDE, CLAUDE_CB);
    expect(a.status).toBe(200);
    const csp = a.headers.get("content-security-policy")!;
    expect(csp).toContain("form-action 'self' https://claude.ai https://claude.com");
    expect(csp).not.toContain("chatgpt");
    expect(await a.text()).toContain("Claude (owner client)");
    expect((await s.authorize(CLAUDE, GPT_CB)).status).toBe(400);
  });
  it("a code issued to one client cannot be exchanged by the other", async () => {
    const s = await boot({ chatgpt: on });
    const { code, verifier } = await s.approve(GPT, GPT_CB);
    const stolen = await s.exchange(CLAUDE, CLAUDE_CB, code, verifier);
    expect(stolen.status).toBe(400);
    expect(((await stolen.json()) as { error: string }).error).toBe("invalid_grant");
  });
  it("a refresh token minted for one client cannot be refreshed by the other, in either direction", async () => {
    const s = await boot({ chatgpt: on });
    const g = await s.approve(GPT, GPT_CB);
    const gTok = (await (await s.exchange(GPT, GPT_CB, g.code, g.verifier)).json()) as { refresh_token: string };
    const c = await s.approve(CLAUDE, CLAUDE_CB);
    const cTok = (await (await s.exchange(CLAUDE, CLAUDE_CB, c.code, c.verifier)).json()) as { refresh_token: string };
    for (const [who, rt] of [[CLAUDE, gTok.refresh_token], [GPT, cTok.refresh_token]] as const) {
      const r = await s.refresh(who, rt);
      expect(r.status).toBe(400);
      expect(((await r.json()) as { error: string }).error).toBe("invalid_grant");
    }
    // and each still refreshes for its own client
    expect((await s.refresh(GPT, gTok.refresh_token)).status).toBe(200);
  });
});

describe("optional confidential mode (GC_MCP_CHATGPT_SECRET_HASH)", () => {
  const conf = { ...on, secretHash: SECRET_HASH };
  it("token exchange requires the secret; a wrong or missing one is invalid_client", async () => {
    const s = await boot({ chatgpt: conf });
    const a = await s.approve(GPT, GPT_CB);
    for (const extra of [{}, { client_secret: "wrong" }]) {
      const r = await s.exchange(GPT, GPT_CB, a.code, a.verifier, extra);
      expect(r.status, JSON.stringify(extra)).toBe(401);
      expect(((await r.json()) as { error: string }).error).toBe("invalid_client");
    }
    // the code was NOT consumed by the rejected attempts (the secret is checked before the grant)
    const ok = await s.exchange(GPT, GPT_CB, a.code, a.verifier, { client_secret: SECRET });
    expect(ok.status).toBe(200);
    const tok = (await ok.json()) as { refresh_token: string };
    expect((await s.refresh(GPT, tok.refresh_token)).status).toBe(401);
    expect((await s.refresh(GPT, tok.refresh_token, { client_secret: SECRET })).status).toBe(200);
  });
  it("revocation also needs the secret, and Claude stays public", async () => {
    const s = await boot({ chatgpt: conf });
    const g = await s.approve(GPT, GPT_CB);
    const tok = (await (await s.exchange(GPT, GPT_CB, g.code, g.verifier, { client_secret: SECRET })).json()) as { access_token: string };
    expect((await fetch(`${s.base}/revoke`, s.form({ client_id: GPT, token: tok.access_token }))).status).toBe(401);
    expect((await s.mcp(tok.access_token)).status).toBe(200);   // still alive
    expect((await fetch(`${s.base}/revoke`, s.form({ client_id: GPT, token: tok.access_token, client_secret: SECRET }))).status).toBe(200);
    expect((await s.mcp(tok.access_token)).status).toBe(401);
    const c = await s.approve(CLAUDE, CLAUDE_CB);
    expect((await s.exchange(CLAUDE, CLAUDE_CB, c.code, c.verifier)).status).toBe(200);   // no secret needed for Claude
  });
  it("repeated wrong secrets are rate limited", async () => {
    const s = await boot({ chatgpt: conf });
    const codes: number[] = [];
    for (let i = 0; i < 8; i++) codes.push((await s.refresh(GPT, "nope", { client_secret: "x" + i })).status);
    expect(codes.slice(0, 5).every((c) => c === 401)).toBe(true);
    expect(codes[7]).toBe(429);
  });
});
