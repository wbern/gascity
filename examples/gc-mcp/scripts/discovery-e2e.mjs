// End-to-end OAuth DISCOVERY proof for the final shape (gcw-ti563): ONE hostname per server, issuer at the host
// root, MCP endpoint exactly /mcp, resource and audience https://<host>/mcp.
//
//   npm run build && node scripts/discovery-e2e.mjs --generator <gas-city-infra>/scripts/gc-mcp-nginx-vhost.py [--host mcp.gc3.enmansbyra.se]
//
// Starts the REAL gc-mcp app and a REAL nginx container (config from the infra generator), then lets the
// MCP TypeScript SDK's own OAuth client (the reference implementation of RFC 9728 / 8414 discovery) connect
// knowing only https://<host>:<port>/mcp. Nothing leaves this machine: the hostname is mapped to
// 127.0.0.1 inside this process only. Needs docker with the nginx:stable-alpine image, python3, openssl.
//
// What it does NOT prove: that Claude's own connector behaves the same. Claude's documented behaviour is
// quoted in docs/runbook.md; this is the reference client against the same server and proxy.
import { spawnSync } from "node:child_process";
import crypto from "node:crypto";
import fs from "node:fs";
import net from "node:net";
import os from "node:os";
import path from "node:path";
import { Agent, fetch as undiciFetch } from "undici";
import { auth, discoverOAuthServerInfo } from "@modelcontextprotocol/sdk/client/auth.js";
import { Client } from "@modelcontextprotocol/sdk/client/index.js";
import { StreamableHTTPClientTransport } from "@modelcontextprotocol/sdk/client/streamableHttp.js";
import { createApp, listen } from "../dist/app.js";
import { GcClient } from "../dist/gc-client.js";
import { hashPassphrase } from "../dist/oauth.js";

const arg = (name, dflt) => { const i = process.argv.indexOf(`--${name}`); return i > 0 ? process.argv[i + 1] : dflt; };
const GENERATOR = arg("generator"); const HOST = arg("host", "mcp.gc3.enmansbyra.se");
if (!GENERATOR) { console.error("--generator <path to gc-mcp-nginx-vhost.py> is required"); process.exit(2); }
const IMAGE = "nginx:stable-alpine", PASS = "e2e-" + crypto.randomBytes(9).toString("hex"), CLIENT_ID = crypto.randomBytes(18).toString("base64url");
const CALLBACK = "https://claude.ai/api/mcp/auth_callback";

const freePort = () => new Promise((r) => { const s = net.createServer().listen(0, "127.0.0.1", () => { const p = s.address().port; s.close(() => r(p)); }); });
const lines = []; let failed = 0;
const step = (ok, what, detail = "") => { if (!ok) failed++; lines.push(`${ok ? "PASS" : "FAIL"}  ${what}${detail ? "  -> " + detail : ""}`); };

const [UP, TLS, HTTP] = [await freePort(), await freePort(), await freePort()];
const publicBase = `https://${HOST}:${TLS}`;
const serverUrl = `${publicBase}/mcp`;                  // resource == audience
const issuerUrl = `${publicBase}/`;                    // issuer == the host root
const tmp = fs.mkdtempSync(path.join(os.tmpdir(), "gcmcp-e2e-")); fs.mkdirSync(path.join(tmp, "logs"));
const container = `gcmcp-e2e-${TLS}`;
let srv;
try {
  // 1. the real app, fronting a fake supervisor
  const fake = async (url) => {
    const p = new URL(url).pathname;
    const body = p.endsWith("/status") ? { name: "gc3", agents: { total: 40, running: 23, quarantined: 0 }, work: { in_progress: 5, ready: 12 }, mail: { unread: 3 } }
      : p.endsWith("/usage") ? { available: true, today: { invocations: 80 } } : { items: [] };
    return new Response(JSON.stringify(body), { status: 200 });
  };
  const { app } = createApp({
    deps: { gc: new GcClient({ baseUrl: "http://127.0.0.1:1", city: "gc3", fetchImpl: fake }), mailAgent: "human", now: () => new Date() },
    publicUrl: new URL(issuerUrl), ownerHash: hashPassphrase(PASS), staticClientId: CLIENT_ID,
    trustProxy: 1, peerAllow: ["127.0.0.1"], callsPerMinute: 1000,
  });
  srv = await listen(app, UP, "127.0.0.1");

  // 2. the real nginx, configured by the infra generator
  spawnSync("openssl", ["req", "-x509", "-newkey", "rsa:2048", "-nodes", "-keyout", `${tmp}/k.pem`, "-out", `${tmp}/c.pem`, "-days", "1",
    "-subj", `/CN=${HOST}`, "-addext", `subjectAltName=DNS:${HOST}`], { stdio: "ignore" });
  const gen = spawnSync("python3", [GENERATOR, "--stage", "live", "--hostname", HOST, "--upstream", `127.0.0.1:${UP}`,
    "--cert", "/etc/gcmcp/c.pem", "--key", "/etc/gcmcp/k.pem", "--http-listen", `127.0.0.1:${HTTP}`, "--https-listen", `127.0.0.1:${TLS}`,
    "--allow", "127.0.0.1/32"], { encoding: "utf8" });
  if (gen.status !== 0) throw new Error("vhost generator failed: " + gen.stderr);
  fs.writeFileSync(`${tmp}/vhost.conf`, gen.stdout);
  fs.writeFileSync(`${tmp}/nginx.conf`, `worker_processes 1;\npid /tmp/nginx.pid;\nerror_log /dev/stderr warn;\nevents { worker_connections 64; }\n` +
    `http { log_format gc '$request_method $uri $status'; access_log /var/log/gcmcp/access.log gc; client_body_temp_path /tmp/c; proxy_temp_path /tmp/p; include /etc/gcmcp/vhost.conf; }\n`);
  const run = spawnSync("docker", ["run", "-d", "--rm", "--pull=never", "--network", "host", "--name", container, "-v", `${tmp}:/etc/gcmcp:ro`,
    "-v", `${tmp}/logs:/var/log/gcmcp`, IMAGE, "nginx", "-c", "/etc/gcmcp/nginx.conf", "-g", "daemon off;"], { encoding: "utf8" });
  if (run.status !== 0) throw new Error("docker run failed: " + run.stderr);
  for (let i = 0; i < 60; i++) { const ok = await new Promise((r) => { const s = net.connect(TLS, "127.0.0.1", () => { s.destroy(); r(true); }); s.on("error", () => r(false)); }); if (ok) break; await new Promise((r) => setTimeout(r, 200)); }

  // 3. the client: knows ONLY the public URL; the hostname is mapped to the local nginx inside this process
  const dispatcher = new Agent({ connect: { rejectUnauthorized: false, lookup: (h, o, cb) => (o && o.all ? cb(null, [{ address: "127.0.0.1", family: 4 }]) : cb(null, "127.0.0.1", 4)) } });
  const fetchFn = (u, init) => undiciFetch(u, { ...init, dispatcher });
  const store = {};
  const provider = {
    get redirectUrl() { return CALLBACK; },
    get clientMetadata() { return { redirect_uris: [CALLBACK], client_name: "discovery-e2e", token_endpoint_auth_method: "none", grant_types: ["authorization_code", "refresh_token"], response_types: ["code"] }; },
    clientInformation: () => ({ client_id: CLIENT_ID }), tokens: () => store.tokens, saveTokens: (t) => { store.tokens = t; },
    redirectToAuthorization: (u) => { store.authUrl = u; }, saveCodeVerifier: (v) => { store.verifier = v; }, codeVerifier: () => store.verifier,
  };

  // a. an unauthenticated call is answered 401 with a pointer to the path-inserted metadata document
  const unauth = await fetchFn(serverUrl, { method: "POST", headers: { "content-type": "application/json", accept: "application/json, text/event-stream" }, body: "{}" });
  const challenge = unauth.headers.get("www-authenticate") ?? "";
  const expectPrm = `${publicBase}/.well-known/oauth-protected-resource/mcp`;
  step(unauth.status === 401 && challenge.includes(`resource_metadata="${expectPrm}"`), "401 carries resource_metadata at the path-inserted URL", challenge.slice(0, 150));

  // b. the reference client discovers resource + authorization server from nothing but the URL
  const info = await discoverOAuthServerInfo(serverUrl, { fetchFn });
  step(String(info.resourceMetadata?.resource) === serverUrl, "protected-resource document: resource equals the exact URL the user types", String(info.resourceMetadata?.resource));
  step(String(info.authorizationServerUrl) === issuerUrl, "issuer is the host root (no path)", String(info.authorizationServerUrl));
  step(info.authorizationServerMetadata?.issuer === issuerUrl, "AS metadata document: issuer matches the URL it was discovered at (RFC 8414)", String(info.authorizationServerMetadata?.issuer));
  step(info.authorizationServerMetadata?.authorization_endpoint === `${publicBase}/authorize` && info.authorizationServerMetadata?.token_endpoint === `${publicBase}/token`
    && !info.authorizationServerMetadata?.registration_endpoint, "AS metadata: /authorize and /token at the root (so a client that falls back to {issuer}/authorize still lands right), no registration endpoint");

  // c. the full authorization-code flow: client -> owner consent in the browser -> code -> tokens
  const first = await auth(provider, { serverUrl, fetchFn });
  step(first === "REDIRECT" && store.authUrl, "client starts the flow and asks for a browser redirect");
  const page = await (await fetchFn(store.authUrl)).text();
  const rid = /name=rid value="([^"]+)"/.exec(page)?.[1];
  step(!!rid && page.includes("claude.ai/api/mcp/auth_callback"), "consent page is served through nginx and shows the redirect target");
  const consent = await fetchFn(`${publicBase}/consent`, { method: "POST", redirect: "manual", headers: { "content-type": "application/x-www-form-urlencoded" }, body: new URLSearchParams({ rid, passphrase: PASS, decision: "approve" }) });
  const location = new URL(consent.headers.get("location") ?? "http://none/");
  step(consent.status === 302 && location.origin + location.pathname === CALLBACK && !!location.searchParams.get("code"), "owner passphrase approves and the callback carries a code");
  const second = await auth(provider, { serverUrl, authorizationCode: location.searchParams.get("code"), fetchFn });
  step(second === "AUTHORIZED" && !!store.tokens?.access_token, "token exchange succeeds (PKCE, audience-bound to the exact /mcp resource)");

  // d. an authenticated MCP session through the proxy
  const client = new Client({ name: "e2e", version: "1" });
  await client.connect(new StreamableHTTPClientTransport(new URL(serverUrl), { authProvider: provider, fetch: fetchFn }));
  const tools = (await client.listTools()).tools;
  const result = await client.callTool({ name: "city_status", arguments: {} });
  step(tools.length === 7 && tools.every((t) => t.annotations?.readOnlyHint), "tools/list over /mcp returns 7 read-only tools");
  step(/23 of 40 agents running/.test(result.content[0].text), "tools/call city_status answers", result.content[0].text.slice(0, 70));
  await client.close();

  // e0. no GET stream: 405 + Allow (spec), not 403
  const getResp = await fetchFn(serverUrl, { method: "GET" });
  step(getResp.status === 405 && (getResp.headers.get("allow") ?? "").includes("POST"), "GET on the MCP URL answers 405 with Allow: POST (no GET stream)", `${getResp.status} ${getResp.headers.get("allow")}`);

  // e. nothing else is public on this host
  // e1. the fallback documents Claude may probe are served through nginx, identical to the primary ones
  const aliasPrm = await (await fetchFn(`${publicBase}/.well-known/oauth-protected-resource`)).json();
  step(aliasPrm.resource === serverUrl, "host-root protected-resource alias answers with the same resource", String(aliasPrm.resource));
  const oidc = await fetchFn(`${publicBase}/.well-known/openid-configuration`);
  step(oidc.status === 200 && (await oidc.json()).issuer === issuerUrl, "OIDC discovery path answers with the same issuer", String(oidc.status));
  // e2. nothing else is public on this host
  const probes = ["/", "/healthz", "/register", "/admin", "/mcp/", "/mcp/x", "/mcp2", "/gc3", "/gc3/authorize", "/gc2", "/gc2/authorize", "/authorize/x",
    "/.well-known/jwks.json", "/.well-known/oauth-authorization-server/mcp", "/.well-known/oauth-protected-resource/other"];
  for (const p of probes) { const r = await fetchFn(publicBase + p, { method: "GET" }); step(r.status === 404 || r.status === 403, `GET ${p} is not served`, String(r.status)); }

  // f. what nginx actually served, in order: the real discovery sequence
  spawnSync("docker", ["exec", container, "sh", "-c", "sleep 0.2"]);
  const log = fs.readFileSync(`${tmp}/logs/access.log`, "utf8").trim().split("\n");
  lines.push("", "nginx access log (method path status), in order:", ...log.slice(0, 40).map((l) => "  " + l));
} catch (e) {
  step(false, "e2e aborted", String(e?.stack ?? e).split("\n").slice(0, 3).join(" | "));
} finally {
  spawnSync("docker", ["rm", "-f", container], { stdio: "ignore" });
  srv?.close();
  fs.rmSync(tmp, { recursive: true, force: true });
}
console.log(`# discovery e2e: host=${HOST} endpoint=/mcp issuer=host root (public URL ${serverUrl})\n` + lines.join("\n") + `\n\nRESULT: ${failed === 0 ? "PASS" : `FAIL (${failed})`}`);
process.exit(failed === 0 ? 0 : 1);
