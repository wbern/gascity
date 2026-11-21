import express from "express";
import type http from "node:http";
import { mcpAuthRouter, getOAuthProtectedResourceMetadataUrl } from "@modelcontextprotocol/sdk/server/auth/router.js";
import { requireBearerAuth } from "@modelcontextprotocol/sdk/server/auth/middleware/bearerAuth.js";
import { StreamableHTTPServerTransport } from "@modelcontextprotocol/sdk/server/streamableHttp.js";
import { createMcpServer } from "./server.js";
import { OwnerApprovalProvider, RateLimiter, SCOPE } from "./oauth.js";
import { AuditLog } from "./audit.js";
import type { Deps } from "./tools.js";

export interface AppOpts {
  deps: Deps;
  publicUrl: URL;             // externally visible base URL (issuer); resource = <publicUrl>/mcp
  ownerHash: string;
  stateFile?: string;
  auditFile?: string | null;
  callsPerMinute?: number;    // per OAuth client
  now?: () => number;
}

export function createApp(o: AppOpts): { app: express.Express; provider: OwnerApprovalProvider; audit: AuditLog } {
  const now = o.now ?? Date.now;
  const base = o.publicUrl.href.replace(/\/$/, "");
  const resource = new URL(`${base}/mcp`);
  const provider = new OwnerApprovalProvider({ resource, ownerHash: o.ownerHash, stateFile: o.stateFile, now });
  const audit = new AuditLog(o.auditFile ?? null);
  const limiter = new RateLimiter(o.callsPerMinute ?? 30, 60_000, now);
  const ipLimiter = new RateLimiter(120, 60_000, now); // blunt cap on unauthenticated endpoints

  const app = express();
  app.disable("x-powered-by");
  app.set("trust proxy", false); // callers: set explicitly once the tunnel is known
  app.get("/healthz", (_q, r) => void r.send("ok"));

  app.use((req, res, next) => {
    if (!ipLimiter.hit(req.ip ?? "?")) { res.status(429).send("rate limited"); return; }
    next();
  });

  app.use(mcpAuthRouter({
    provider, issuerUrl: new URL(base), baseUrl: new URL(base), resourceServerUrl: resource,
    scopesSupported: [SCOPE], resourceName: "Gas City (read-only)",
    clientRegistrationOptions: { clientSecretExpirySeconds: 0 },
  }));

  app.post("/consent", express.urlencoded({ extended: false, limit: "4kb" }), (req, res) => {
    const b = req.body as Record<string, string>;
    const r = provider.consent(String(b.rid ?? ""), String(b.passphrase ?? ""), String(b.decision ?? ""), req.ip ?? "?");
    audit.write({ kind: "auth", ok: r.status === 302, note: `consent ${r.status} ${b.decision ?? ""}` });
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
    if (!limiter.hit(client)) { audit.write({ kind: "auth", client, ok: false, note: "rate limited" }); res.status(429).json({ error: "rate_limited" }); return; }
    const mcp = createMcpServer(o.deps, (c) => audit.write({ kind: "tool", client, tool: c.tool, args: c.args, ok: c.ok, ms: c.ms, chars: c.chars }));
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
  return new Promise((resolve) => { const s = app.listen(port, host, () => resolve(s)); });
}
