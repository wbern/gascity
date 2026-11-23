import { describe, it, expect, afterAll } from "vitest";
import crypto from "node:crypto";
import fs from "node:fs";
import net from "node:net";
import http from "node:http";
import os from "node:os";
import path from "node:path";
import { Client } from "@modelcontextprotocol/sdk/client/index.js";
import { StreamableHTTPClientTransport } from "@modelcontextprotocol/sdk/client/streamableHttp.js";
import { createApp, listen, type AppOpts } from "../src/app.js";
import { GcClient } from "../src/gc-client.js";
import { hashPassphrase } from "../src/oauth.js";
import * as F from "./fixtures/index.js";

const PASS = "correct horse battery staple";
const CID = "owner-client-id-0123456789ab";
const CB = "https://claude.ai/api/mcp/auth_callback";
const b64u = (b: Buffer) => b.toString("base64url");
const HASH = hashPassphrase(PASS); // scrypt once for the whole file
const cleanups: (() => void)[] = [];
afterAll(() => cleanups.forEach((f) => f()));

const fetchImpl = (async (url: string) => {
  const p = new URL(url).pathname;
  const body = p.endsWith("/status") ? F.status : p.endsWith("/usage") ? F.usage : { items: [] };
  return new Response(JSON.stringify(body), { status: 200 });
}) as unknown as typeof fetch;

async function freePort() {
  return new Promise<number>((res) => { const s = net.createServer().listen(0, "127.0.0.1", () => { const p = (s.address() as net.AddressInfo).port; s.close(() => res(p)); }); });
}

async function boot(opts: Partial<AppOpts> & { issuerPath?: string } = {}) {
  const { issuerPath: ip = "", ...over } = opts;
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "gcmcp-"));
  const port = await freePort();
  const base = `http://127.0.0.1:${port}`;
  const mcpPath = over.mcpPath ?? (ip || "/mcp");
  const res = `${base}${mcpPath}`;
  const t = { clock: Date.now() };
  const { app, provider } = createApp({
    deps: { gc: new GcClient({ baseUrl: "http://127.0.0.1:1", city: "gc2", fetchImpl }), mailAgent: "human", now: () => new Date() },
    publicUrl: new URL(base + ip), ownerHash: HASH, staticClientId: CID,
    stateFile: path.join(dir, "state.json"), auditFile: path.join(dir, "audit.jsonl"), controlDir: dir,
    callsPerMinute: 5, now: () => t.clock, ...over,
  });
  const srv = await listen(app, port);
  cleanups.push(() => { srv.close(); fs.rmSync(dir, { recursive: true, force: true }); });
  const H = (xff?: string) => (xff ? { "x-forwarded-for": xff } : {});
  const authorize = (cid = CID, challenge = "x".repeat(43), resource = res, redirect = CB, xff?: string) => {
    const u = new URL(`${base}${ip}/authorize`);
    Object.entries({ response_type: "code", client_id: cid, redirect_uri: redirect, code_challenge: challenge, code_challenge_method: "S256", state: "st8", scope: "gc:read", resource }).forEach(([k, v]) => u.searchParams.set(k, v));
    return fetch(u, { redirect: "manual", headers: H(xff) });
  };
  const ridOf = (html: string) => /name=rid value="([^"]+)"/.exec(html)![1];
  const consent = (rid: string, passphrase: string, decision = "approve", xff?: string) =>
    fetch(`${base}${ip}/consent`, { method: "POST", redirect: "manual", headers: { "content-type": "application/x-www-form-urlencoded", ...H(xff) }, body: new URLSearchParams({ rid, passphrase, decision }) });
  const flow = async (passphrase = PASS) => {
    const verifier = b64u(crypto.randomBytes(32));
    const challenge = b64u(crypto.createHash("sha256").update(verifier).digest());
    const a = await authorize(CID, challenge);
    expect(a.status).toBe(200);
    const c = await consent(ridOf(await a.text()), passphrase);
    return { verifier, c };
  };
  const token = (code: string, verifier: string, extra: Record<string, string> = {}) =>
    fetch(`${base}${ip}/token`, { method: "POST", headers: { "content-type": "application/x-www-form-urlencoded" },
      body: new URLSearchParams({ grant_type: "authorization_code", client_id: CID, code, code_verifier: verifier, redirect_uri: CB, resource: res, ...extra }) });
  const refresh = (rt: string) => fetch(`${base}${ip}/token`, { method: "POST", headers: { "content-type": "application/x-www-form-urlencoded" },
    body: new URLSearchParams({ grant_type: "refresh_token", client_id: CID, refresh_token: rt, resource: res }) });
  const getCode = (c: Response) => new URL(c.headers.get("location")!).searchParams.get("code")!;
  const mcpCall = (tok: string | null, xff?: string) => fetch(res, { method: "POST",
    headers: { "content-type": "application/json", accept: "application/json, text/event-stream", ...(tok ? { authorization: `Bearer ${tok}` } : {}), ...H(xff) },
    body: JSON.stringify({ jsonrpc: "2.0", id: 1, method: "tools/list" }) });
  const login = async () => { const { verifier, c } = await flow(); return (await (await token(getCode(c), verifier)).json()) as { access_token: string; refresh_token: string }; };
  return { base, ip, res, mcpPath, dir, t, provider, authorize, consent, ridOf, flow, token, refresh, getCode, mcpCall, login };
}

describe("discovery and static client", () => {
  it("serves RFC 9728 / 8414 metadata and has NO registration endpoint by default", async () => {
    const s = await boot();
    const pr = (await (await fetch(`${s.base}/.well-known/oauth-protected-resource/mcp`)).json()) as any;
    expect(pr.resource).toBe(`${s.base}/mcp`);
    const as = (await (await fetch(`${s.base}/.well-known/oauth-authorization-server`)).json()) as any;
    expect(as.code_challenge_methods_supported).toContain("S256");
    expect(as.registration_endpoint).toBeUndefined();
    expect((await fetch(`${s.base}/register`, { method: "POST", headers: { "content-type": "application/json" }, body: "{}" })).status).toBe(404);
  });
  it("401s without a token and points at the resource metadata", async () => {
    const s = await boot();
    const r = await s.mcpCall(null);
    expect(r.status).toBe(401);
    expect(r.headers.get("www-authenticate")).toContain("resource_metadata=");
  });
  it("unknown client id and non-allowlisted redirect are refused at authorize", async () => {
    const s = await boot();
    expect((await s.authorize("someone-else")).status).toBe(400);
    expect((await s.authorize(CID, "x".repeat(43), `${s.base}/mcp`, "https://evil.example/cb")).status).toBe(400);
  });
});

describe("authorization-code + PKCE flow", () => {
  it("owner passphrase -> code -> token -> tool call via a real MCP client", async () => {
    const s = await boot();
    const { verifier, c } = await s.flow();
    expect(c.status).toBe(302);
    expect(new URL(c.headers.get("location")!).searchParams.get("state")).toBe("st8");
    const tok = (await (await s.token(s.getCode(c), verifier)).json()) as { access_token: string };
    const client = new Client({ name: "t", version: "1" });
    await client.connect(new StreamableHTTPClientTransport(new URL(`${s.base}/mcp`), { requestInit: { headers: { authorization: `Bearer ${tok.access_token}` } } }));
    expect((await client.listTools()).tools).toHaveLength(17);   // Includes faithful transcript reader; no write tool without gc:write
    const orientation=await client.callTool({name:'city_guide',arguments:{topic:'overview'}});
    expect(orientation.structuredContent).toMatchObject({origin:'curated_operating_guide',topic:'overview'});
    expect(Buffer.byteLength(JSON.stringify(orientation.structuredContent))).toBeLessThanOrEqual(4096);
    const r = await client.callTool({ name: "city_status", arguments: {} });
    expect((r.content as { text: string }[])[0].text).toMatch(/23 of 40 agents running/);
    await client.close();
  });
  it("wrong passphrase issues no code; deny returns access_denied", async () => {
    const s = await boot();
    const { c } = await s.flow("nope nope nope nope");
    expect(c.status).toBe(401);
    expect(c.headers.get("location")).toBeNull();
    const a = await s.authorize();
    const d = await s.consent(s.ridOf(await a.text()), PASS, "deny");
    expect(new URL(d.headers.get("location")!).searchParams.get("error")).toBe("access_denied");
  });
  it("code is single-use; wrong PKCE verifier fails; foreign audience refused", async () => {
    const s = await boot();
    const a = await s.flow();
    const code = s.getCode(a.c);
    expect((await s.token(code, a.verifier)).status).toBe(200);
    expect((await s.token(code, a.verifier)).status).toBe(400);
    const b = await s.flow();
    expect((await s.token(s.getCode(b.c), b64u(crypto.randomBytes(32)))).status).toBe(400);
    expect((await s.authorize(CID, "x".repeat(43), "https://evil.example/mcp")).status).toBe(400);
  });
  it("expired and garbage tokens are refused", async () => {
    const s = await boot();
    const { access_token } = await s.login();
    expect((await s.mcpCall(access_token)).status).toBe(200);
    s.t.clock += 2 * 3600 * 1000;
    expect((await s.mcpCall(access_token)).status).toBe(401);
    expect((await s.mcpCall("not-a-real-token")).status).toBe(401);
  });
  it("consent page allows the Claude callback in form-action (Chrome applies it to the redirect)", async () => {
    const s = await boot();
    const a = await s.authorize();
    const csp = a.headers.get("content-security-policy") ?? "";
    expect(csp).toContain("form-action 'self' https://claude.ai");
    expect(a.headers.get("x-frame-options")).toBe("DENY");
  });
});

describe("root issuer with the MCP endpoint at exactly /mcp (final shape: one host per server)", () => {
  it("serves the protected-resource document at BOTH the path-inserted URL and the host root, with the same content", async () => {
    const s = await boot();
    const a = (await (await fetch(`${s.base}/.well-known/oauth-protected-resource/mcp`)).json()) as any;
    const b = (await (await fetch(`${s.base}/.well-known/oauth-protected-resource`)).json()) as any;
    expect(a.resource).toBe(`${s.base}/mcp`);
    expect(b).toEqual(a);                                   // clients that fall back to the root see the same document
    expect(a.authorization_servers).toEqual([`${s.base}/`]);
  });
  it("serves the authorization-server document at the RFC 8414 path and at the OIDC discovery path, identically", async () => {
    const s = await boot();
    const a = (await (await fetch(`${s.base}/.well-known/oauth-authorization-server`)).json()) as any;
    const b = (await (await fetch(`${s.base}/.well-known/openid-configuration`)).json()) as any;
    expect(b).toEqual(a);
    expect(a.issuer).toBe(`${s.base}/`);
    expect(a.authorization_endpoint).toBe(`${s.base}/authorize`);   // so a client that falls back to {issuer}/authorize lands on the real endpoint
    expect(a.token_endpoint).toBe(`${s.base}/token`);
  });
  it("audience is exactly the /mcp URL: the host root and look-alikes are refused", async () => {
    const s = await boot();
    for (const r of [`${s.base}/`, `${s.base}`, `${s.base}/mcp/`, `${s.base}/mcp2`, "https://evil.example/mcp"]) {
      expect((await s.authorize(CID, "x".repeat(43), r)).status, r).toBe(400);
    }
  });
});

describe("MCP endpoint under a path prefix (issuer stays at the host root)", () => {
  const TEXT = { "content-type": "application/json", accept: "application/json, text/event-stream" };

  it("serves the protected-resource document at the path-inserted URL and points the 401 at it", async () => {
    const s = await boot({ mcpPath: "/mcp/gc" });
    const pr = (await (await fetch(`${s.base}/.well-known/oauth-protected-resource/mcp/gc`)).json()) as any;
    expect(pr.resource).toBe(`${s.base}/mcp/gc`);
    expect(pr.authorization_servers).toEqual([`${s.base}/`]);       // issuer = host root, never the path
    const r = await s.mcpCall(null);
    expect(r.status).toBe(401);
    expect(r.headers.get("www-authenticate")).toContain(`resource_metadata="${s.base}/.well-known/oauth-protected-resource/mcp/gc"`);
    expect((await fetch(`${s.base}/.well-known/oauth-protected-resource/mcp`)).status).toBe(404);   // no accidental /mcp alias
    const as = (await (await fetch(`${s.base}/.well-known/oauth-authorization-server`)).json()) as any;
    expect(as.issuer).toBe(`${s.base}/`);
    expect(as.authorization_endpoint).toBe(`${s.base}/authorize`);
  });

  it("runs the full flow with a real MCP client against the prefixed URL, and the old /mcp path is gone", async () => {
    const s = await boot({ mcpPath: "/mcp/gc" });
    const tok = await s.login();
    const client = new Client({ name: "t", version: "1" });
    await client.connect(new StreamableHTTPClientTransport(new URL(s.res), { requestInit: { headers: { authorization: `Bearer ${tok.access_token}` } } }));
    expect((await client.listTools()).tools).toHaveLength(17);   // Includes faithful transcript reader; no write tool without gc:write
    await client.close();
    expect((await fetch(`${s.base}/mcp`, { method: "POST", headers: { ...TEXT, authorization: `Bearer ${tok.access_token}` }, body: "{}" })).status).toBe(404);
  });

  it("binds the token audience to the prefixed resource, so the unprefixed resource is refused", async () => {
    const s = await boot({ mcpPath: "/mcp/gc" });
    const a = await s.authorize(CID, "x".repeat(43), `${s.base}/mcp`);
    expect(a.status).toBe(400);   // invalid_target
  });

  it("supports the MCP endpoint at the host root", async () => {
    const s = await boot({ mcpPath: "/" });
    const pr = (await (await fetch(`${s.base}/.well-known/oauth-protected-resource`)).json()) as any;
    expect(pr.resource).toBe(`${s.base}/`);
    const tok = await s.login();
    const r = await fetch(`${s.base}/`, { method: "POST", headers: { ...TEXT, authorization: `Bearer ${tok.access_token}` }, body: JSON.stringify({ jsonrpc: "2.0", id: 1, method: "tools/list" }) });
    expect(r.status).toBe(200);
    expect((await fetch(`${s.base}/authorize`, { redirect: "manual" })).status).toBe(400);   // OAuth routes still answer (missing params), they are not shadowed by the root MCP path
  });

  it("applies the source gate to the prefixed MCP path as well as /token", async () => {
    const s = await boot({ mcpPath: "/mcp/gc", trustProxy: 1, allowCidrs: ["203.0.113.0/24"] });
    expect((await s.mcpCall(null, "198.51.100.7")).status).toBe(403);
    expect((await s.mcpCall(null, "203.0.113.9")).status).toBe(401);
  });
});

describe("issuer AND resource under one path prefix (final naming: https://host/gc3)", () => {
  const TEXT = { "content-type": "application/json", accept: "application/json, text/event-stream" };

  it("serves path-inserted metadata and keeps every OAuth endpoint under the prefix", async () => {
    const s = await boot({ issuerPath: "/gc3" });
    const prm = (await (await fetch(`${s.base}/.well-known/oauth-protected-resource/gc3`)).json()) as any;
    expect(prm.resource).toBe(`${s.base}/gc3`);
    expect(prm.authorization_servers).toEqual([`${s.base}/gc3`]);
    const as = (await (await fetch(`${s.base}/.well-known/oauth-authorization-server/gc3`)).json()) as any;
    expect(as.issuer).toBe(`${s.base}/gc3`);
    expect(as.authorization_endpoint).toBe(`${s.base}/gc3/authorize`);
    expect(as.token_endpoint).toBe(`${s.base}/gc3/token`);
    expect(as.revocation_endpoint).toBe(`${s.base}/gc3/revoke`);
    expect(as.registration_endpoint).toBeUndefined();
    expect(as.code_challenge_methods_supported).toEqual(["S256"]);
  });

  it("serves NOTHING at the host root: no root metadata, no root OAuth endpoints, no old /mcp", async () => {
    const s = await boot({ issuerPath: "/gc3" });
    for (const p of ["/.well-known/oauth-authorization-server", "/.well-known/oauth-protected-resource", "/.well-known/oauth-protected-resource/mcp",
                     "/authorize", "/token", "/revoke", "/consent", "/register", "/mcp", "/gc3/register", "/gc3/nothing"]) {
      const r = await fetch(`${s.base}${p}`, { method: p === "/token" || p === "/consent" || p === "/mcp" ? "POST" : "GET", redirect: "manual" });
      expect(r.status, p).toBe(404);
    }
  });

  it("answers 401 with a resource_metadata pointer at the path-inserted URL and 405 to a GET", async () => {
    const s = await boot({ issuerPath: "/gc3" });
    const r = await s.mcpCall(null);
    expect(r.status).toBe(401);
    expect(r.headers.get("www-authenticate")).toContain(`resource_metadata="${s.base}/.well-known/oauth-protected-resource/gc3"`);
    expect((await fetch(s.res)).status).toBe(405);
  });

  it("runs the full flow: consent form posts back under the prefix, tokens work, a real MCP client lists the tools", async () => {
    const s = await boot({ issuerPath: "/gc3" });
    const a = await s.authorize();
    expect(await a.text()).toContain('action="/gc3/consent"');
    const tok = await s.login();
    const client = new Client({ name: "t", version: "1" });
    await client.connect(new StreamableHTTPClientTransport(new URL(s.res), { requestInit: { headers: { authorization: `Bearer ${tok.access_token}` } } }));
    expect((await client.listTools()).tools).toHaveLength(17);   // Includes faithful transcript reader; no write tool without gc:write
    await client.close();
  });

  it("binds the audience to the prefixed resource: any other resource is refused", async () => {
    const s = await boot({ issuerPath: "/gc3" });
    expect((await s.authorize(CID, "x".repeat(43), `${s.base}/mcp`)).status).toBe(400);
    expect((await s.authorize(CID, "x".repeat(43), `${s.base}/`)).status).toBe(400);
  });

  it("applies the source gate to /gc3 and /gc3/token but never to /gc3/authorize", async () => {
    const s = await boot({ issuerPath: "/gc3", trustProxy: 1, allowCidrs: ["203.0.113.0/24"] });
    expect((await s.mcpCall(null, "198.51.100.7")).status).toBe(403);
    expect((await s.mcpCall(null, "203.0.113.9")).status).toBe(401);
    const t = await fetch(`${s.base}/gc3/token`, { method: "POST", headers: { "x-forwarded-for": "198.51.100.7", "content-type": "application/x-www-form-urlencoded" }, body: "grant_type=authorization_code" });
    expect(t.status).toBe(403);
    expect((await s.authorize(CID, "x".repeat(43), s.res, CB, "198.51.100.7")).status).toBe(200);
  });

  it("with dynamic registration enabled, /register also lives under the prefix only", async () => {
    const s = await boot({ issuerPath: "/gc3", allowDcr: true });
    const as = (await (await fetch(`${s.base}/.well-known/oauth-authorization-server/gc3`)).json()) as any;
    expect(as.registration_endpoint).toBe(`${s.base}/gc3/register`);
    expect((await fetch(`${s.base}/register`, { method: "POST", headers: { "content-type": "application/json" }, body: "{}" })).status).toBe(404);
    expect((await fetch(`${s.base}/gc3/register`, { method: "POST", headers: { "content-type": "application/json" }, body: JSON.stringify({ redirect_uris: ["https://evil.example/cb"] }) })).status).toBe(400);
  });

  it("failed requests to the prefixed security paths are audited", async () => {
    const s = await boot({ issuerPath: "/gc3" });
    await s.mcpCall("garbage");
    await s.consent("nope", "wrong-passphrase-value");
    const log = fs.readFileSync(path.join(s.dir, "audit.jsonl"), "utf8");
    expect(log).toContain("/gc3 -> 401");
    expect(log).toContain("/gc3/consent -> 400");
  });

  it("the root-path variant of the same design (prefix /) keeps OAuth at the root", async () => {
    const s = await boot({ issuerPath: "", mcpPath: "/" });
    const as = (await (await fetch(`${s.base}/.well-known/oauth-authorization-server`)).json()) as any;
    expect(as.authorization_endpoint).toBe(`${s.base}/authorize`);
    const tok = await s.login();
    const r = await fetch(`${s.base}/`, { method: "POST", headers: { ...TEXT, authorization: `Bearer ${tok.access_token}` }, body: JSON.stringify({ jsonrpc: "2.0", id: 1, method: "tools/list" }) });
    expect(r.status).toBe(200);
  });
});

describe("refresh rotation and reuse detection", () => {
  it("rotates, and a replayed old refresh token revokes the whole family", async () => {
    const s = await boot();
    const t1 = await s.login();
    const r1 = await s.refresh(t1.refresh_token);
    expect(r1.status).toBe(200);
    const t2 = (await r1.json()) as { access_token: string; refresh_token: string };
    expect((await s.mcpCall(t2.access_token)).status).toBe(200);
    expect((await s.refresh(t1.refresh_token)).status).toBe(400); // replay
    expect((await s.refresh(t2.refresh_token)).status).toBe(400); // family revoked
    expect((await s.mcpCall(t2.access_token)).status).toBe(401);
  });
});

describe("consent abuse limits", () => {
  it("burns a consent request after 5 wrong passphrases even from different sources", async () => {
    const s = await boot({ trustProxy: 1 });
    const a = await s.authorize(CID, "x".repeat(43), `${s.base}/mcp`, CB, "198.51.100.1");
    const rid = s.ridOf(await a.text());
    for (let i = 0; i < 5; i++) expect((await s.consent(rid, "wrong wrong wrong", "approve", `198.51.100.${10 + i}`)).status).toBe(401);
    expect((await s.consent(rid, PASS, "approve", "198.51.100.99")).status).toBe(400);
  });
  it("locks one source out but does not affect another", async () => {
    const s = await boot({ trustProxy: 1 });
    const a = await s.authorize();
    const rid = s.ridOf(await a.text());
    for (let i = 0; i < 4; i++) await s.consent(rid, "wrong wrong wrong", "approve", "198.51.100.7");
    const b = await s.authorize();
    const rid2 = s.ridOf(await b.text());
    for (let i = 0; i < 5; i++) await s.consent(rid2, "wrong wrong wrong", "approve", "198.51.100.7");
    expect((await s.consent(s.ridOf(await (await s.authorize()).text()), PASS, "approve", "198.51.100.7")).status).toBe(429);
    expect((await s.consent(s.ridOf(await (await s.authorize()).text()), PASS, "approve", "203.0.113.5")).status).toBe(302);
  });
});

describe("shared-IP denial of service (security review finding 1)", () => {
  it("with no trusted proxy, flooding public endpoints never blocks the owner's /mcp calls", async () => {
    const s = await boot();
    const tok = await s.login();
    for (let b = 0; b < 8; b++) await Promise.all(Array.from({ length: 50 }, () => fetch(`${s.base}/.well-known/oauth-authorization-server`)));
    expect((await s.mcpCall(tok.access_token)).status).toBe(200);
  });
  it("with a trusted proxy, limits are per real client IP", async () => {
    const s = await boot({ trustProxy: 1 });
    const tok = await s.login();
    for (let i = 0; i < 305; i++) await fetch(`${s.base}/.well-known/oauth-authorization-server`, { headers: { "x-forwarded-for": "198.51.100.50" } });
    expect((await fetch(`${s.base}/healthz`, { headers: { "x-forwarded-for": "198.51.100.50" } })).status).toBe(200); // healthz is exempt
    expect((await s.mcpCall(tok.access_token, "198.51.100.50")).status).toBe(429);
    expect((await s.mcpCall(tok.access_token, "203.0.113.77")).status).toBe(200);
  });
});

describe("dynamic registration (opt-in, restricted)", () => {
  const reg = (base: string, body: object) => fetch(`${base}/register`, { method: "POST", headers: { "content-type": "application/json" }, body: JSON.stringify(body) });
  it("rejects an attacker redirect, accepts the Claude callback, clips names, rate-limits registration (cap and pruning: dcr.test.ts)", async () => {
    const s = await boot({ allowDcr: true });
    expect((await reg(s.base, { redirect_uris: ["https://evil.example/cb"], client_name: "Claude", token_endpoint_auth_method: "none" })).status).toBe(400);
    const ok = await reg(s.base, { redirect_uris: [CB], client_name: "N".repeat(500), token_endpoint_auth_method: "none" });
    expect(ok.status).toBe(201);
    expect(((await ok.json()) as { client_name: string }).client_name.length).toBeLessThanOrEqual(60);
    let last = 201;
    for (let i = 0; i < 30; i++) last = (await reg(s.base, { redirect_uris: [CB], client_name: "c", token_endpoint_auth_method: "none" })).status;
    expect(last).toBe(429);   // the global registration limit (10 an hour) answers before the cap matters
    const state = fs.readFileSync(path.join(s.dir, "state.json"), "utf8");
    expect(state.length).toBeLessThan(20_000);
  });
});

describe("owner controls and perimeter", () => {
  it("DISABLED flag stops serving immediately; healthz stays up", async () => {
    const s = await boot();
    const tok = await s.login();
    fs.writeFileSync(path.join(s.dir, "DISABLED"), "");
    expect((await s.mcpCall(tok.access_token)).status).toBe(503);
    expect((await fetch(`${s.base}/healthz`)).status).toBe(200);
    fs.unlinkSync(path.join(s.dir, "DISABLED"));
    expect((await s.mcpCall(tok.access_token)).status).toBe(200);
  });
  it("REVOKE_ALL flag drops every token and is consumed", async () => {
    const s = await boot();
    const tok = await s.login();
    fs.writeFileSync(path.join(s.dir, "REVOKE_ALL"), "");
    expect((await s.mcpCall(tok.access_token)).status).toBe(401);
    expect(fs.existsSync(path.join(s.dir, "REVOKE_ALL"))).toBe(false);
    expect((await s.refresh(tok.refresh_token)).status).toBe(400);
  });
  it("rejects an unexpected Host header (421)", async () => {
    const s = await boot();
    const port = new URL(s.base).port;
    const status = await new Promise<number>((res) => {
      const rq = http.request({ host: "127.0.0.1", port, path: "/.well-known/oauth-authorization-server", headers: { host: "evil.example" } }, (r) => { r.resume(); res(r.statusCode!); });
      rq.end();
    });
    expect(status).toBe(421);
  });
  it("pins /mcp and /token to allowed source ranges but never /authorize", async () => {
    const s = await boot({ trustProxy: 1, allowCidrs: ["203.0.113.0/24"] });
    expect((await s.mcpCall(null, "198.51.100.7")).status).toBe(403);
    expect((await s.mcpCall(null, "203.0.113.9")).status).toBe(401);
    const t = await fetch(`${s.base}/token`, { method: "POST", headers: { "x-forwarded-for": "198.51.100.7", "content-type": "application/x-www-form-urlencoded" }, body: "grant_type=authorization_code" });
    expect(t.status).toBe(403);
    expect((await s.authorize(CID, "x".repeat(43), `${s.base}/mcp`, CB, "198.51.100.7")).status).toBe(200);
  });
  it("refuses a CIDR allowlist without a trusted proxy (every caller would look like the proxy)", () => {
    expect(() => createApp({ deps: {} as never, publicUrl: new URL("http://127.0.0.1:1"), ownerHash: HASH, allowCidrs: ["203.0.113.0/24"] })).toThrow(/trustProxy/);
  });
  it("rate-limits tool calls per client", async () => {
    const s = await boot();
    const tok = await s.login();
    const codes: number[] = [];
    for (let i = 0; i < 7; i++) codes.push((await s.mcpCall(tok.access_token)).status);
    expect(codes.slice(0, 5).every((c) => c === 200)).toBe(true);
    expect(codes.slice(5)).toEqual([429, 429]);
  });
});

describe("peer pinning (the reverse proxy is the only allowed TCP peer)", () => {
  it("drops connections from any other peer without answering", async () => {
    const s = await boot({ peerAllow: ["10.9.9.9"] });
    await expect(fetch(`${s.base}/healthz`)).rejects.toThrow();
    await expect(fetch(`${s.base}/.well-known/oauth-authorization-server`)).rejects.toThrow();
  });
  it("serves the pinned peer normally", async () => {
    const s = await boot({ peerAllow: ["127.0.0.1"] });
    expect((await fetch(`${s.base}/healthz`)).status).toBe(200);
  });
  it("is off by default", async () => {
    const s = await boot();
    expect((await fetch(`${s.base}/healthz`)).status).toBe(200);
  });
});

describe("audit log and state at rest", () => {
  it("records failures, token events and tool calls; never a token or passphrase", async () => {
    const s = await boot();
    const tok = await s.login();
    await s.mcpCall("garbage-token");
    await s.consent("nope", "wrong-passphrase-value");
    const client = new Client({ name: "t", version: "1" });
    await client.connect(new StreamableHTTPClientTransport(new URL(`${s.base}/mcp`), { requestInit: { headers: { authorization: `Bearer ${tok.access_token}` } } }));
    await client.callTool({ name: "city_status", arguments: {} });
    await client.close();
    const log = fs.readFileSync(path.join(s.dir, "audit.jsonl"), "utf8");
    expect(log).toContain("/mcp -> 401");
    expect(log).toContain("consent approved");
    expect(log).toContain("token issued from code");
    expect(log).toContain('"tool":"city_status"');
    for (const secret of [tok.access_token, tok.refresh_token, PASS, "wrong-passphrase-value", "garbage-token", CID]) expect(log).not.toContain(secret);
    expect((fs.statSync(path.join(s.dir, "audit.jsonl")).mode & 0o777).toString(8)).toBe("600");
  });
  it("state file holds hashes only and is mode 0600", async () => {
    const s = await boot();
    const tok = await s.login();
    const f = path.join(s.dir, "state.json");
    expect((fs.statSync(f).mode & 0o777).toString(8)).toBe("600");
    const raw = fs.readFileSync(f, "utf8");
    expect(raw).not.toContain(tok.refresh_token);
    expect(raw).not.toContain(tok.access_token);
  });
});
