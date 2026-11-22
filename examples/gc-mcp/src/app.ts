import fs from "node:fs";
import crypto from "node:crypto";
import path from "node:path";
import express from "express";
import type http from "node:http";
import { createOAuthMetadata, getOAuthProtectedResourceMetadataUrl } from "@modelcontextprotocol/sdk/server/auth/router.js";
import { authorizationHandler } from "@modelcontextprotocol/sdk/server/auth/handlers/authorize.js";
import { tokenHandler } from "@modelcontextprotocol/sdk/server/auth/handlers/token.js";
import { revocationHandler } from "@modelcontextprotocol/sdk/server/auth/handlers/revoke.js";
import { clientRegistrationHandler } from "@modelcontextprotocol/sdk/server/auth/handlers/register.js";
import { metadataHandler } from "@modelcontextprotocol/sdk/server/auth/handlers/metadata.js";
import { requireBearerAuth } from "@modelcontextprotocol/sdk/server/auth/middleware/bearerAuth.js";
import { StreamableHTTPServerTransport } from "@modelcontextprotocol/sdk/server/streamableHttp.js";
import { createMcpServer } from "./server.js";
import { OwnerApprovalProvider, RateLimiter, SCOPE, type ChatgptClientSpec } from "./oauth.js";
import { WriteGate, type WriteConfig } from "./writes.js";
import { AuditLog } from "./audit.js";
import { parseCidr, inAnyCidr } from "./cidr.js";
import type { Deps } from "./tools.js";

export interface AppOpts {
  deps: Deps;
  publicUrl: URL;              // externally visible ISSUER URL; may carry a path (https://host/gc3). OAuth endpoints live under it
  ownerHash: string;
  staticClientId?: string;     // the owner's own OAuth client id (recommended)
  allowDcr?: boolean;          // dynamic client registration: OFF unless explicitly enabled
  writes?: WriteConfig;        // Phase 4 write tools (send_to_session, respond_to_session): absent or enabled:false = read-only server, gc:write not advertised
  chatgpt?: ChatgptClientSpec; // optional second STATIC client for ChatGPT (exact https://chatgpt.com/ redirects; empty = cannot authorize)
  stateFile?: string;
  controlDir?: string;         // holds the DISABLED and REVOKE_ALL flag files
  auditFile?: string | null;
  callsPerMinute?: number;     // per OAuth client
  trustProxy?: number;         // trusted reverse-proxy hops; 0 = none (default). Needed to know real client IPs.
  allowCidrs?: string[];       // restrict /mcp and /token to these sources (requires trustProxy >= 1)
  extraHosts?: string[];       // extra accepted Host names (publicUrl host + loopback are always accepted)
  peerAllow?: string[];        // exact TCP peer addresses allowed to connect at all (the reverse proxy); others are dropped
  mcpPath?: string;            // public path of the MCP endpoint (default: the issuer's path, or /mcp when the issuer is the host root)
  now?: () => number;
}

export function createApp(o: AppOpts): { app: express.Express; provider: OwnerApprovalProvider; audit: AuditLog } {
  const now = o.now ?? Date.now;
  const trust = o.trustProxy ?? 0;
  if (o.allowCidrs?.length && trust < 1) throw new Error("allowCidrs requires trustProxy >= 1: without it every caller looks like the proxy");
  const cidrs = (o.allowCidrs ?? []).map(parseCidr);
  const origin = o.publicUrl.origin;
  const issuerPath = o.publicUrl.pathname.replace(/\/$/, "");           // "" when the issuer is the host root
  const issuerStr = issuerPath ? `${origin}${issuerPath}` : `${origin}/`;  // exactly what clients derive metadata URLs from
  const mcpPath = o.mcpPath ?? (issuerPath || "/mcp");
  const resource = new URL(mcpPath === "/" ? `${origin}/` : `${origin}${mcpPath}`);
  const oauthPath = (name: string) => `${issuerPath}/${name}`;
  const isMcp = (p: string) => p === mcpPath;
  const securityPaths = new Set([mcpPath, ...["authorize", "token", "revoke", "register", "consent"].map(oauthPath)]);
  const audit = new AuditLog(o.auditFile ?? null);
  const writesOn = !!o.writes?.enabled;
  const deps = writesOn ? { ...o.deps, writes: new WriteGate(o.deps.gc, { ...o.writes!, controlDir: o.writes!.controlDir ?? o.controlDir }, (e) => audit.write(e), now) } : o.deps;
  const provider = new OwnerApprovalProvider({
    writesEnabled: writesOn,
    resource, ownerHash: o.ownerHash, staticClientId: o.staticClientId, chatgpt: o.chatgpt, allowDcr: o.allowDcr, stateFile: o.stateFile, now,
    consentPath: oauthPath("consent"),
    onEvent: (note, ok, app) => audit.write({ kind: "auth", ok, note, app }),
  });
  const callLimiter = new RateLimiter(o.callsPerMinute ?? 30, 60_000, now);
  // A per-IP limiter is only meaningful when the REAL client IP is known. Behind a tunnel with
  // trustProxy=0 every caller is 127.0.0.1, so a shared bucket would be a denial-of-service lever.
  const ipLimiter = trust > 0 ? new RateLimiter(300, 60_000, now) : null;
  const hosts = new Set(["localhost", "127.0.0.1", "[::1]", o.publicUrl.hostname, ...(o.extraHosts ?? [])]);
  const flag = (n: string) => (o.controlDir ? path.join(o.controlDir, n) : null);

  const app = express();
  app.disable("x-powered-by");
  app.set("trust proxy", trust);
  // Pin the TCP peer BEFORE anything else (even /healthz): when bound to a LAN address, any
  // neighbour that is not the reverse proxy gets its connection dropped without a response.
  if (o.peerAllow?.length) {
    const allowed = new Set(o.peerAllow);
    const lastNote = new Map<string, number>();
    app.use((req, _res, next) => {
      const peer = (req.socket.remoteAddress ?? "").replace(/^::ffff:/, "");
      if (allowed.has(peer)) { next(); return; }
      const t = now();
      if (t - (lastNote.get(peer) ?? 0) > 60_000) { lastNote.set(peer, t); audit.write({ kind: "auth", ok: false, note: `connection from unpinned peer ${peer} dropped` }); }
      req.socket.destroy();
    });
  }

  app.get("/healthz", (_q, r) => void r.send("ok"));

  // owner kill switch: touch DISABLED to stop serving, touch REVOKE_ALL to drop every token.
  app.use((_req, res, next) => {
    const rv = flag("REVOKE_ALL"), dis = flag("DISABLED");
    if (rv && fs.existsSync(rv)) { try { fs.unlinkSync(rv); } catch { /* ignore */ } provider.revokeAll(); }
    if (dis && fs.existsSync(dis)) { res.status(503).type("text/plain").send("disabled by owner"); return; }
    next();
  });

  // DNS-rebinding / wrong-vhost protection.
  app.use((req, res, next) => {
    const h = (req.headers.host ?? "").replace(/:\d+$/, "").toLowerCase();
    if (!hosts.has(h)) { res.status(421).type("text/plain").send("misdirected request"); return; }
    next();
  });

  // audit every failed request to the security-relevant paths
  app.use((req, res, next) => {
    if (securityPaths.has(req.path)) {
      res.on("finish", () => { if (res.statusCode >= 400) audit.write({ kind: "auth", ok: false, note: `${req.method} ${req.path} -> ${res.statusCode} ip=${req.ip}` }); });
    }
    next();
  });

  app.use((req, res, next) => {
    if (ipLimiter && !ipLimiter.hit(req.ip ?? "?")) { res.status(429).send("rate limited"); return; }
    next();
  });

  // server-to-server endpoints may be pinned to Anthropic's egress ranges; browser-driven
  // /authorize and /consent must NOT be (they come from the owner's phone).
  if (cidrs.length) {
    app.use((req, res, next) => {
      if ((isMcp(req.path) || req.path === oauthPath("token") || req.path === oauthPath("revoke")) && !inAnyCidr(req.ip, cidrs)) { res.status(403).type("text/plain").send("forbidden"); return; }
      next();
    });
  }

  // OAuth 2.1 authorization server, assembled from the SDK's own handlers and mounted UNDER the issuer path.
  // (The SDK's all-in-one router assumes the host root, so it cannot serve https://host/gc3 as an issuer.)
  // Metadata follows RFC 8414 / RFC 9728 path insertion; nothing is served at the host root in path mode.
  const ep = (name: string) => `${origin}${oauthPath(name)}`;
  const asMeta = {
    ...createOAuthMetadata({ provider, issuerUrl: new URL(issuerStr), baseUrl: new URL(issuerStr), scopesSupported: provider.supportedScopes() }),
    issuer: issuerStr,
    authorization_endpoint: ep("authorize"),
    token_endpoint: ep("token"),
    revocation_endpoint: ep("revoke"),
    registration_endpoint: provider.clientsStore.registerClient ? ep("register") : undefined,
  };
  const prm = { resource: resource.href, authorization_servers: [issuerStr], scopes_supported: provider.supportedScopes(), resource_name: writesOn ? "Gas City" : "Gas City (read-only)" };
  app.use(`/.well-known/oauth-protected-resource${mcpPath === "/" ? "" : mcpPath}`, metadataHandler(prm));
  app.use(`/.well-known/oauth-authorization-server${issuerPath}`, metadataHandler(asMeta));
  if (issuerPath === "") {
    // Root-issuer mode: clients that fall back to the host root (Claude has open reports of this, e.g. anthropics/claude-ai-mcp #716)
    // must find the same documents there, and the OIDC discovery path is answered with the same metadata.
    if (mcpPath !== "/") app.use("/.well-known/oauth-protected-resource", metadataHandler(prm));
    app.use("/.well-known/openid-configuration", metadataHandler(asMeta));
  }
  app.use(oauthPath("authorize"), authorizationHandler({ provider, rateLimit: false }));
  // Confidential mode (optional, ChatGPT only): the SDK compares client_secret as plain text, so the secret is checked HERE
  // against an scrypt verifier BEFORE the grant is touched (a rejected attempt does not consume a code or a refresh token).
  // Public clients (Claude, and ChatGPT unless GC_MCP_CHATGPT_SECRET_HASH is set) pass straight through.
  const secretFails = new RateLimiter(5, 5 * 60_000, now);
  const secretGate = [express.urlencoded({ extended: false, limit: "4kb" }), (req: express.Request, res: express.Response, next: express.NextFunction) => {
    const b = (req.body ?? {}) as Record<string, unknown>;
    const cid = typeof b.client_id === "string" ? b.client_id : "";
    if (!provider.clientNeedsSecret(cid)) { next(); return; }
    const src = req.ip ?? "?";
    res.set("cache-control", "no-store");
    if (secretFails.blocked(src)) { res.status(429).json({ error: "temporarily_unavailable", error_description: "too many failed client authentications" }); return; }
    if (!provider.verifyClientSecret(cid, typeof b.client_secret === "string" ? b.client_secret : undefined)) {
      secretFails.hit(src);
      audit.write({ kind: "auth", ok: false, note: "client secret rejected", app: provider.clientName(cid) });
      res.status(401).json({ error: "invalid_client", error_description: "client authentication failed" });
      return;
    }
    next();
  }];
  app.use(oauthPath("token"), ...secretGate, tokenHandler({ provider, rateLimit: false }));
  app.use(oauthPath("revoke"), ...secretGate, revocationHandler({ provider, rateLimit: false }));
  if (provider.clientsStore.registerClient) {
    app.use(oauthPath("register"), clientRegistrationHandler({ clientsStore: provider.clientsStore, clientSecretExpirySeconds: 0, rateLimit: false }));
  }

  app.post(oauthPath("consent"), express.urlencoded({ extended: false, limit: "4kb" }), (req, res) => {
    const b = req.body as Record<string, string>;
    const r = provider.consent(String(b.rid ?? ""), String(b.passphrase ?? ""), String(b.decision ?? ""), req.ip ?? "?");
    res.set({ "cache-control": "no-store", "x-frame-options": "DENY" });
    if (r.redirect) { res.redirect(302, r.redirect); return; }
    res.status(r.status).type("text/plain").send(r.message ?? "error");
  });

  const bearer = requireBearerAuth({
    verifier: provider,
    requiredScopes: [SCOPE],
    resourceMetadataUrl: getOAuthProtectedResourceMetadataUrl(resource),
    expectedResource: resource,
  });

  app.all(mcpPath, (req, res, next) => {
    if (req.method !== "POST") { res.status(405).set("allow", "POST").send("method not allowed"); return; }
    next();
  }, bearer, express.json({ limit: "64kb" }), async (req, res) => {
    const client = req.auth?.clientId ?? "?";
    if (!callLimiter.hit(client)) { res.status(429).json({ error: "rate_limited" }); return; }
    const mcp = createMcpServer(deps, (c) => audit.write({ kind: "tool", client: crypto.createHash("sha256").update(client).digest("hex").slice(0, 8), app: provider.clientName(client), tool: c.tool, args: c.args, ok: c.ok, ms: c.ms, chars: c.chars }),
      { client: provider.clientName(client) ?? "unknown client", scopes: req.auth?.scopes ?? [] });
    const transport = new StreamableHTTPServerTransport({ sessionIdGenerator: undefined });
    res.on("close", () => { void transport.close(); void mcp.close(); });
    try {
      await mcp.connect(transport);
      await transport.handleRequest(req, res, req.body);
    } catch (e) {
      if (!res.headersSent) res.status(500).json({ error: "internal" });
      console.error("mcp error:", (e as Error).message);
    }
  });
  return { app, provider, audit };
}

export function listen(app: express.Express, port: number, host = "127.0.0.1"): Promise<http.Server> {
  return new Promise((resolve) => {
    const s = app.listen(port, host, () => resolve(s));
    s.headersTimeout = 10_000;
    s.requestTimeout = 30_000;
    s.keepAliveTimeout = 5_000;
    s.maxConnections = 200;
  });
}
