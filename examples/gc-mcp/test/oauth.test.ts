import { describe, it, expect, beforeAll, afterAll } from "vitest";
import crypto from "node:crypto";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { Client } from "@modelcontextprotocol/sdk/client/index.js";
import { StreamableHTTPClientTransport } from "@modelcontextprotocol/sdk/client/streamableHttp.js";
import { createApp, listen } from "../src/app.js";
import { GcClient } from "../src/gc-client.js";
import { hashPassphrase } from "../src/oauth.js";
import * as F from "./fixtures/index.js";
import type http from "node:http";

const PASS = "correct horse battery staple";
let srv: http.Server, base: string, dir: string, clock: number;
const b64u = (b: Buffer) => b.toString("base64url");

beforeAll(async () => {
  dir = fs.mkdtempSync(path.join(os.tmpdir(), "gcmcp-"));
  const fetchImpl = (async (url: string) => {
    const p = new URL(url).pathname;
    const body = p.endsWith("/status") ? F.status : p.endsWith("/usage") ? F.usage : { items: [] };
    return new Response(JSON.stringify(body), { status: 200 });
  }) as unknown as typeof fetch;
  clock = Date.now();
  // port 0 then patch publicUrl: listen first on a fixed ephemeral port
  const probe = await new Promise<http.Server>((r) => { const s = (require("node:http") as typeof http).createServer().listen(0, "127.0.0.1", () => r(s)); });
  const port = (probe.address() as { port: number }).port; probe.close();
  base = `http://127.0.0.1:${port}`;
  const { app } = createApp({
    deps: { gc: new GcClient({ baseUrl: "http://x", city: "gc2", fetchImpl }), mailAgent: "human", now: () => new Date() },
    publicUrl: new URL(base), ownerHash: hashPassphrase(PASS),
    stateFile: path.join(dir, "state.json"), auditFile: path.join(dir, "audit.jsonl"),
    callsPerMinute: 5, now: () => clock,
  });
  srv = await listen(app, port);
});
afterAll(() => { srv.close(); fs.rmSync(dir, { recursive: true, force: true }); });

async function register() {
  const r = await fetch(`${base}/register`, { method: "POST", headers: { "content-type": "application/json" },
    body: JSON.stringify({ redirect_uris: ["https://claude.ai/api/mcp/auth_callback"], client_name: "Claude", token_endpoint_auth_method: "none", grant_types: ["authorization_code", "refresh_token"], response_types: ["code"] }) });
  expect(r.status).toBe(201);
  return (await r.json()) as { client_id: string };
}
async function authorize(clientId: string, challenge: string, resource = `${base}/mcp`) {
  const u = new URL(`${base}/authorize`);
  Object.entries({ response_type: "code", client_id: clientId, redirect_uri: "https://claude.ai/api/mcp/auth_callback", code_challenge: challenge, code_challenge_method: "S256", state: "st8", scope: "gc:read", resource }).forEach(([k, v]) => u.searchParams.set(k, v));
  return fetch(u, { redirect: "manual" });
}
const ridOf = (html: string) => /name=rid value="([^"]+)"/.exec(html)![1];
async function consent(rid: string, passphrase: string, decision = "approve") {
  return fetch(`${base}/consent`, { method: "POST", redirect: "manual", headers: { "content-type": "application/x-www-form-urlencoded" }, body: new URLSearchParams({ rid, passphrase, decision }) });
}
async function fullFlow(passphrase = PASS) {
  const { client_id } = await register();
  const verifier = b64u(crypto.randomBytes(32));
  const challenge = b64u(crypto.createHash("sha256").update(verifier).digest());
  const a = await authorize(client_id, challenge);
  expect(a.status).toBe(200);
  const c = await consent(ridOf(await a.text()), passphrase);
  return { client_id, verifier, c };
}
async function token(client_id: string, code: string, verifier: string, extra: Record<string, string> = {}) {
  return fetch(`${base}/token`, { method: "POST", headers: { "content-type": "application/x-www-form-urlencoded" },
    body: new URLSearchParams({ grant_type: "authorization_code", client_id, code, code_verifier: verifier, redirect_uri: "https://claude.ai/api/mcp/auth_callback", resource: `${base}/mcp`, ...extra }) });
}
const getCode = (c: Response) => new URL(c.headers.get("location")!).searchParams.get("code")!;
const mcpCall = (tok: string | null) => fetch(`${base}/mcp`, { method: "POST",
  headers: { "content-type": "application/json", accept: "application/json, text/event-stream", ...(tok ? { authorization: `Bearer ${tok}` } : {}) },
  body: JSON.stringify({ jsonrpc: "2.0", id: 1, method: "tools/list" }) });

describe("discovery", () => {
  it("serves RFC 9728 protected-resource and RFC 8414 AS metadata", async () => {
    const pr = await (await fetch(`${base}/.well-known/oauth-protected-resource/mcp`)).json() as any;
    expect(pr.resource).toBe(`${base}/mcp`);
    expect(pr.authorization_servers).toContain(`${base}/`);
    const as = await (await fetch(`${base}/.well-known/oauth-authorization-server`)).json() as any;
    expect(as.code_challenge_methods_supported).toContain("S256");
    expect(as.registration_endpoint).toBe(`${base}/register`);
  });
  it("401s without a token and points at the resource metadata", async () => {
    const r = await mcpCall(null);
    expect(r.status).toBe(401);
    expect(r.headers.get("www-authenticate")).toContain("resource_metadata=");
  });
});

describe("authorization-code + PKCE flow", () => {
  it("owner passphrase -> code -> token -> tool call, via a real MCP client", async () => {
    const { client_id, verifier, c } = await fullFlow();
    expect(c.status).toBe(302);
    expect(new URL(c.headers.get("location")!).searchParams.get("state")).toBe("st8");
    const t = await token(client_id, getCode(c), verifier);
    expect(t.status).toBe(200);
    const tok = (await t.json()) as { access_token: string; refresh_token: string };
    const client = new Client({ name: "t", version: "1" });
    await client.connect(new StreamableHTTPClientTransport(new URL(`${base}/mcp`), { requestInit: { headers: { authorization: `Bearer ${tok.access_token}` } } }));
    expect((await client.listTools()).tools).toHaveLength(7);
    const r = await client.callTool({ name: "city_status", arguments: {} });
    expect((r.content as { text: string }[])[0].text).toMatch(/23 of 40 agents running/);
    await client.close();
  });
  it("wrong passphrase issues no code", async () => {
    const { c } = await fullFlow("nope nope nope nope");
    expect(c.status).toBe(401);
    expect(c.headers.get("location")).toBeNull();
  });
  it("deny returns access_denied", async () => {
    const { client_id } = await register();
    const a = await authorize(client_id, "x".repeat(43));
    const c = await consent(ridOf(await a.text()), PASS, "deny");
    expect(new URL(c.headers.get("location")!).searchParams.get("error")).toBe("access_denied");
  });
  it("authorization code is single-use", async () => {
    const { client_id, verifier, c } = await fullFlow();
    const code = getCode(c);
    expect((await token(client_id, code, verifier)).status).toBe(200);
    expect((await token(client_id, code, verifier)).status).toBe(400);
  });
  it("wrong PKCE verifier is rejected", async () => {
    const { client_id, c } = await fullFlow();
    expect((await token(client_id, getCode(c), b64u(crypto.randomBytes(32)))).status).toBe(400);
  });
  it("rejects a different audience at authorize time (RFC 8707)", async () => {
    const { client_id } = await register();
    const r = await authorize(client_id, "x".repeat(43), "https://evil.example/mcp");
    expect(r.status).toBe(400);
  });
  it("refresh rotates: the old refresh token dies", async () => {
    const { client_id, verifier, c } = await fullFlow();
    const t = (await (await token(client_id, getCode(c), verifier)).json()) as { refresh_token: string };
    const refresh = (rt: string) => fetch(`${base}/token`, { method: "POST", headers: { "content-type": "application/x-www-form-urlencoded" }, body: new URLSearchParams({ grant_type: "refresh_token", client_id, refresh_token: rt, resource: `${base}/mcp` }) });
    const r1 = await refresh(t.refresh_token);
    expect(r1.status).toBe(200);
    expect((await refresh(t.refresh_token)).status).toBe(400);
  });
  it("expired access tokens are refused", async () => {
    const { client_id, verifier, c } = await fullFlow();
    const t = (await (await token(client_id, getCode(c), verifier)).json()) as { access_token: string };
    expect((await mcpCall(t.access_token)).status).toBe(200);
    clock += 2 * 3600 * 1000;
    expect((await mcpCall(t.access_token)).status).toBe(401);
    clock -= 2 * 3600 * 1000;
  });
  it("garbage bearer tokens are refused", async () => {
    expect((await mcpCall("not-a-real-token")).status).toBe(401);
  });
});

describe("abuse limits", () => {
  it("locks out repeated wrong passphrases from one source", async () => {
    const { client_id } = await register();
    const a = await authorize(client_id, "x".repeat(43));
    const rid = ridOf(await a.text());
    let last = 0;
    for (let i = 0; i < 8; i++) last = (await consent(rid, "wrong wrong wrong")).status;
    expect(last).toBe(429);
    // even the right passphrase is refused while locked out
    expect((await consent(rid, PASS)).status).toBe(429);
    clock += 6 * 60_000;
  });
  it("rate-limits tool calls per client", async () => {
    const { client_id, verifier, c } = await fullFlow();
    const tok = ((await (await token(client_id, getCode(c), verifier)).json()) as { access_token: string }).access_token;
    const codes: number[] = [];
    for (let i = 0; i < 7; i++) codes.push((await mcpCall(tok)).status);
    expect(codes.slice(0, 5).every((s) => s === 200)).toBe(true);
    expect(codes.slice(5)).toEqual([429, 429]);
    clock += 61_000;
  });
});

describe("audit log + persistence", () => {
  it("records tool calls without any token or passphrase", async () => {
    const { client_id, verifier, c } = await fullFlow();
    const tok = ((await (await token(client_id, getCode(c), verifier)).json()) as { access_token: string }).access_token;
    clock += 61_000;
    const client = new Client({ name: "t", version: "1" });
    await client.connect(new StreamableHTTPClientTransport(new URL(`${base}/mcp`), { requestInit: { headers: { authorization: `Bearer ${tok}` } } }));
    await client.callTool({ name: "city_status", arguments: {} });
    await client.close();
    const log = fs.readFileSync(path.join(dir, "audit.jsonl"), "utf8");
    expect(log).toContain('"tool":"city_status"');
    expect(log).not.toContain(tok);
    expect(log).not.toContain(PASS);
  });
  it("state file holds hashes only and is mode 0600", () => {
    const f = path.join(dir, "state.json");
    expect((fs.statSync(f).mode & 0o777).toString(8)).toBe("600");
    const s = fs.readFileSync(f, "utf8");
    expect(s).not.toMatch(/access_token|code_verifier/);
  });
});
