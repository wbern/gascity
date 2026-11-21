import os from "node:os";
import path from "node:path";
import { GcClient } from "./gc-client.js";
import { createApp, listen } from "./app.js";

const port = Number(process.env.GC_MCP_PORT ?? 47811);
const ownerHash = process.env.GC_MCP_OWNER_HASH;
if (!ownerHash?.startsWith("scrypt1$")) {
  console.error("GC_MCP_OWNER_HASH is required (create one with: node scripts/hash-passphrase.mjs)");
  process.exit(2);
}
const stateDir = process.env.GC_MCP_STATE_DIR ?? path.join(os.homedir(), ".local/state/gc-mcp");
const { app } = createApp({
  deps: {
    gc: new GcClient({ baseUrl: process.env.GC_MCP_API_URL ?? "http://127.0.0.1:8372", city: process.env.GC_MCP_CITY ?? "gc2" }),
    mailAgent: process.env.GC_MCP_MAIL_AGENT ?? "human",
    now: () => new Date(),
  },
  publicUrl: new URL(process.env.GC_MCP_PUBLIC_URL ?? `http://127.0.0.1:${port}`),
  ownerHash,
  stateFile: path.join(stateDir, "oauth-state.json"),
  auditFile: path.join(stateDir, "audit.jsonl"),
});
// Loopback only: exposure is a separate, human-approved step (see docs/runbook).
const srv = await listen(app, port, "127.0.0.1");
console.error(`gc-mcp listening on 127.0.0.1:${port}`);
process.on("SIGTERM", () => srv.close());
