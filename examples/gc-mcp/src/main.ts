import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import crypto from "node:crypto";
import { GcClient } from "./gc-client.js";
import { createApp, listen } from "./app.js";
import { resolveNetwork, resolveMcpPath } from "./netconfig.js";

const port = Number(process.env.GC_MCP_PORT ?? 47811);
const ownerHash = process.env.GC_MCP_OWNER_HASH;
if (!ownerHash?.startsWith("scrypt1$")) {
  console.error("GC_MCP_OWNER_HASH is required (create one with: node scripts/hash-passphrase.mjs)");
  process.exit(2);
}
let net, mcpPath;
try { net = resolveNetwork(process.env); mcpPath = resolveMcpPath(process.env); } catch (e) { console.error(`gc-mcp: ${(e as Error).message}`); process.exit(2); }
const stateDir = process.env.GC_MCP_STATE_DIR ?? path.join(os.homedir(), ".local/state/gc-mcp");
fs.mkdirSync(stateDir, { recursive: true, mode: 0o700 });

// The owner's own OAuth client id: pasted into Claude's "Use your own OAuth client" field.
// Generated once, kept 0600; dynamic registration is OFF unless GC_MCP_ALLOW_DCR=1.
const idFile = path.join(stateDir, "client-id");
let staticClientId = process.env.GC_MCP_CLIENT_ID;
if (!staticClientId) {
  if (!fs.existsSync(idFile)) {
    fs.writeFileSync(idFile, crypto.randomBytes(18).toString("base64url") + "\n", { mode: 0o600 });
    console.error(`gc-mcp: created OAuth client id (also readable at ${idFile})`);
  }
  staticClientId = fs.readFileSync(idFile, "utf8").trim();
}

const list = (v?: string) => (v ? v.split(",").map((s) => s.trim()).filter(Boolean) : undefined);
const { app } = createApp({
  deps: {
    gc: new GcClient({ baseUrl: process.env.GC_MCP_API_URL ?? "http://127.0.0.1:8372", city: process.env.GC_MCP_CITY ?? "gc2" }),
    mailAgent: process.env.GC_MCP_MAIL_AGENT ?? "human",
    now: () => new Date(),
    peekAllow: list(process.env.GC_MCP_PEEK_ALLOW),
  },
  publicUrl: new URL(process.env.GC_MCP_PUBLIC_URL ?? `http://127.0.0.1:${port}`),
  ownerHash,
  staticClientId,
  allowDcr: process.env.GC_MCP_ALLOW_DCR === "1",
  stateFile: path.join(stateDir, "oauth-state.json"),
  controlDir: stateDir,
  auditFile: path.join(stateDir, "audit.jsonl"),
  trustProxy: net.trustProxy,
  peerAllow: net.peerAllow,
  mcpPath,
  allowCidrs: list(process.env.GC_MCP_ALLOW_CIDRS),
  extraHosts: list(process.env.GC_MCP_EXTRA_HOSTS),
});
// Loopback unless GC_MCP_BIND_HOST names ONE private address (validated, peer-pinned, fail-closed).
const srv = await listen(app, port, net.bindHost);
console.error(`gc-mcp listening on ${net.bindHost}:${port}`);
process.on("SIGTERM", () => srv.close());
