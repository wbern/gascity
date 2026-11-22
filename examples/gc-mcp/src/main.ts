import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import crypto from "node:crypto";
import { GcClient } from "./gc-client.js";
import { createApp, listen } from "./app.js";
import { validateChatgptRedirects, type ChatgptClientSpec } from "./oauth.js";
import { validatePatterns, type WriteConfig } from "./writes.js";
import { validateExtraRedirects } from "./dcr.js";
import { resolveNetwork, resolveMcpPath, resolveIssuerPath } from "./netconfig.js";

const port = Number(process.env.GC_MCP_PORT ?? 47811);
const ownerHash = process.env.GC_MCP_OWNER_HASH;
if (!ownerHash?.startsWith("scrypt1$")) {
  console.error("GC_MCP_OWNER_HASH is required (create one with: node scripts/hash-passphrase.mjs)");
  process.exit(2);
}
const publicUrlRaw = process.env.GC_MCP_PUBLIC_URL ?? `http://127.0.0.1:${port}`;
let net, mcpPath;
try { net = resolveNetwork(process.env); const issuerPath = resolveIssuerPath(publicUrlRaw); mcpPath = resolveMcpPath(process.env, issuerPath || "/mcp"); } catch (e) { console.error(`gc-mcp: ${(e as Error).message}`); process.exit(2); }
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

// Optional second static client for ChatGPT: OFF unless GC_MCP_CHATGPT_ENABLED=1. Its own persisted client id; redirect URIs are
// configuration (they are per connection and only known after the connector is created), exact https://chatgpt.com/... only;
// an empty list leaves the client unable to authorize. GC_MCP_CHATGPT_SECRET_HASH (scrypt1$, like the owner hash) makes it confidential.
let chatgpt: ChatgptClientSpec | undefined;
if (process.env.GC_MCP_CHATGPT_ENABLED === "1") {
  const gptIdFile = path.join(stateDir, "chatgpt-client-id");
  if (!fs.existsSync(gptIdFile)) {
    fs.writeFileSync(gptIdFile, crypto.randomBytes(18).toString("base64url") + "\n", { mode: 0o600 });
    console.error(`gc-mcp: created ChatGPT OAuth client id (also readable at ${gptIdFile})`);
  }
  const secretHash = process.env.GC_MCP_CHATGPT_SECRET_HASH || undefined;
  const redirectUris = (process.env.GC_MCP_CHATGPT_REDIRECTS ?? "").split(",").map((s) => s.trim()).filter(Boolean);
  try {
    if (secretHash && !secretHash.startsWith("scrypt1$")) throw new Error("GC_MCP_CHATGPT_SECRET_HASH must be an scrypt1$ verifier");
    chatgpt = { clientId: fs.readFileSync(gptIdFile, "utf8").trim(), redirectUris: validateChatgptRedirects(redirectUris), secretHash };
    if (chatgpt.clientId === staticClientId) throw new Error("the ChatGPT client id must differ from the Claude client id");
  } catch (e) { console.error(`gc-mcp: ${(e as Error).message}`); process.exit(2); }
  console.error(`gc-mcp: ChatGPT client enabled (${chatgpt.redirectUris.length} redirect URI(s), ${secretHash ? "confidential" : "public"})` + (chatgpt.redirectUris.length ? "" : ": it cannot authorize until GC_MCP_CHATGPT_REDIRECTS is set"));
}

// Phase 4 write control: OFF unless GC_MCP_WRITES_ENABLED=1. Deny list defaults to the built-in one (other people's and the owner's own
// terminals) and is REPLACED, not extended, by GC_MCP_WRITE_DENY_SESSIONS; GC_MCP_WRITE_ALLOW_SESSIONS optionally restricts writes to a list.
// A session a person is attached to is refused unless GC_MCP_WRITE_ALLOW_ATTACHED=1. The WRITES_DISABLED file in the state dir switches writes off.
let writes: WriteConfig | undefined;
if (process.env.GC_MCP_WRITES_ENABLED === "1") {
  const csv = (v?: string) => (v ? v.split(",").map((s) => s.trim()).filter(Boolean) : undefined);
  try {
    const deny = csv(process.env.GC_MCP_WRITE_DENY_SESSIONS), allow = csv(process.env.GC_MCP_WRITE_ALLOW_SESSIONS);
    if (deny) validatePatterns(deny, "GC_MCP_WRITE_DENY_SESSIONS");
    if (allow) validatePatterns(allow, "GC_MCP_WRITE_ALLOW_SESSIONS");
    writes = { enabled: true, deny, allow, allowAttached: process.env.GC_MCP_WRITE_ALLOW_ATTACHED === "1", controlDir: stateDir };
  } catch (e) { console.error(`gc-mcp: ${(e as Error).message}`); process.exit(2); }
  console.error(`gc-mcp: WRITES ENABLED (send_to_session, respond_to_session; scope gc:write). Kill switch: ${path.join(stateDir, "WRITES_DISABLED")}`);
}

// What an omitted `session` means (voice: nobody says session ids). Empty = no default: tools then ask "which session?".
const defaultSession = (process.env.GC_MCP_DEFAULT_SESSION ?? "").trim() || undefined;
if (defaultSession && !/^[A-Za-z0-9][A-Za-z0-9._:/ -]{0,127}$/.test(defaultSession)) { console.error("gc-mcp: GC_MCP_DEFAULT_SESSION must be a plain session name"); process.exit(2); }
console.error(`gc-mcp: default session: ${defaultSession ?? "(none)"}`);   // ctl status reads this line; the env file (it holds a hash) is never read

// Dynamic client registration (GC_MCP_ALLOW_DCR=1): paste the URL, click Connect, type the passphrase. Redirects must be in the safe set
// (Claude, ChatGPT connector, loopback) plus exact GC_MCP_DCR_EXTRA_REDIRECTS. GC_MCP_DCR_OPEN_REDIRECTS=1 lets ANY https redirect register: UNSAFE, off by default.
let dcr: { extraRedirects: string[]; openRedirects: boolean } | undefined;
if (process.env.GC_MCP_ALLOW_DCR === "1") {
  try {
    const extra = (process.env.GC_MCP_DCR_EXTRA_REDIRECTS ?? "").split(",").map((s) => s.trim()).filter(Boolean);
    dcr = { extraRedirects: validateExtraRedirects(extra), openRedirects: process.env.GC_MCP_DCR_OPEN_REDIRECTS === "1" };
  } catch (e) { console.error(`gc-mcp: ${(e as Error).message}`); process.exit(2); }
  console.error(`gc-mcp: dynamic client registration ON (${dcr.extraRedirects.length} extra redirect(s))`);
  if (dcr.openRedirects) console.error("gc-mcp: WARNING GC_MCP_DCR_OPEN_REDIRECTS=1: ANY https redirect URI may register. This is UNSAFE; registered clients are read-only while it is on.");
}

const list = (v?: string) => (v ? v.split(",").map((s) => s.trim()).filter(Boolean) : undefined);
const { app } = createApp({
  deps: {
    gc: new GcClient({ baseUrl: process.env.GC_MCP_API_URL ?? "http://127.0.0.1:8372", city: process.env.GC_MCP_CITY ?? "gc2" }),
    mailAgent: process.env.GC_MCP_MAIL_AGENT ?? "human",
    now: () => new Date(),
    peekAllow: list(process.env.GC_MCP_PEEK_ALLOW),
    defaultSession,
  },
  publicUrl: new URL(publicUrlRaw),
  ownerHash,
  staticClientId,
  chatgpt,
  writes,
  allowDcr: process.env.GC_MCP_ALLOW_DCR === "1",
  dcr,
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
