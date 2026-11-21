// Drives every read tool through a real MCP client against a running gc-mcp.
// Usage: node scripts/recorded-run.mjs http://127.0.0.1:47811/mcp
import { Client } from "@modelcontextprotocol/sdk/client/index.js";
import { StreamableHTTPClientTransport } from "@modelcontextprotocol/sdk/client/streamableHttp.js";
const c = new Client({ name: "recorded-run", version: "1" });
await c.connect(new StreamableHTTPClientTransport(new URL(process.argv[2])));
const calls = [
  ["city_status", {}], ["list_sessions", { limit: 4 }], ["peek_session", { session: "gc-architect", tail: 2 }],
  ["mail_inbox", { limit: 3 }], ["ready_work", { limit: 3 }], ["bead_show", { id: "gcw-ti563" }], ["whats_new", { since: "30m" }],
];
for (const [name, args] of calls) {
  const t0 = Date.now();
  const r = await c.callTool({ name, arguments: args });
  const text = r.content[0].text;
  console.log(`## ${name} ${JSON.stringify(args)}  (${Date.now() - t0}ms, ${text.length} chars)\n${text}\n`);
}
await c.close();
