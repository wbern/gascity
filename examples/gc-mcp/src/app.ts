import fs from "node:fs";
import crypto from "node:crypto";
import path from "node:path";
import express from "express";
import type http from "node:http";
import { mcpAuthRouter, getOAuthProtectedResourceMetadataUrl } from "@modelcontextprotocol/sdk/server/auth/router.js";
import { requireBearerAuth } from "@modelcontextprotocol/sdk/server/auth/middleware/bearerAuth.js";
import { StreamableHTTPServerTransport } from "@modelcontextprotocol/sdk/server/streamableHttp.js";
import { createMcpServer } from "./server.js";
import { OwnerApprovalProvider, RateLimiter, SCOPE } from "./oauth.js";
import { AuditLog } from "./audit.js";
import { parseCidr, inAnyCidr } from "./cidr.js";
import type { Deps } from "./tools.js";

export interface AppOpts {
  deps: Deps;
  publicUrl: URL;              // externally visible base URL (issuer); resource = <publicUrl>/mcp
  ownerHash: string;
  staticClientId?: string;     // the owner's own OAuth client id (recommended)
  allowDcr?: boolean;          // dynamic client registration: OFF unless explicitly enabled
  stateFile?: string;
  controlDir?: string;         // holds the DISABLED and REVOKE_ALL flag files
  auditFile?: string | null;
  callsPerMinute?: number;     // per OAuth client
  trustProxy?: number;         // trusted reverse-proxy hops; 0 = none (default). Needed to know real client IPs.
  allowCidrs?: string[];       // restrict /mcp and /token to these sources (requires trustProxy >= 1)
  extraHosts?: string[];       // extra accepted Host names (publicUrl host + loopback are always accepted)
  peerAllow?: string[];        // exact TCP peer addresses allowed to connect at all (the reverse proxy); others are dropped
  now?: () => number;
}

export function createApp(o: AppOpts): { app: express.Express; provider: OwnerApprovalProvider; audit: AuditLog } {
  const now = o.now ?? Date.now;
  const trust = o.trustProxy ?? 0;
  if (o.allowCidrs?.length && trust < 1) throw new Error("allowCidrs requires trustProxy >= 1: without it every caller looks like the proxy");
  const cidrs = (o.allowCidrs ?? []).map(parseCidr);
  const base = o.publicUrl.href.replace(/\/$/, "");
  const resource = new URL(`${base}/mcp`);
  const audit = new AuditLog(o.auditFile ?? null);
  const provider = new OwnerApprovalProvider({
    resource, ownerHash: o.ownerHash, staticClientId: o.staticClientId, allowDcr: o.allowDcr, stateFile: o.stateFile, now,
    onEvent: (note, ok) => audit.write({ kind: "auth", ok, note }),
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
    if (/^\/(mcp|token|authorize|register|consent|revoke)/.test(req.path)) {
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
    app.use(["/mcp", "/token"], (req, res, next) => {
      if (!inAnyCidr(req.ip, cidrs)) { res.status(403).type("text/plain").send("forbidden"); return; }
      next();
    });
  }

  app.use(mcpAuthRouter({
    provider, issuerUrl: new URL(base), baseUrl: new URL(base), resourceServerUrl: resource,
    scopesSupported: [SCOPE], resourceName: "Gas City (read-only)",
    authorizationOptions: { rateLimit: false },
    tokenOptions: { rateLimit: false },
    revocationOptions: { rateLimit: false },
    clientRegistrationOptions: { rateLimit: false, clientSecretExpirySeconds: 0 },
  }));

  app.post("/consent", express.urlencoded({ extended: false, limit: "4kb" }), (req, res) => {
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

  app.all("/mcp", (req, res, next) => {
    if (req.method !== "POST") { res.status(405).set("allow", "POST").send("method not allowed"); return; }
    next();
  }, bearer, express.json({ limit: "64kb" }), async (req, res) => {
    const client = req.auth?.clientId ?? "?";
    if (!callLimiter.hit(client)) { res.status(429).json({ error: "rate_limited" }); return; }
    const mcp = createMcpServer(o.deps, (c) => audit.write({ kind: "tool", client: crypto.createHash("sha256").update(client).digest("hex").slice(0, 8), tool: c.tool, args: c.args, ok: c.ok, ms: c.ms, chars: c.chars }));
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
