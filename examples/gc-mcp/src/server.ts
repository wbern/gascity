import { McpServer } from "@modelcontextprotocol/sdk/server/mcp.js";
import { buildTools, type Deps } from "./tools.js";

export interface CallInfo { tool: string; args: unknown; ok: boolean; ms: number; chars: number }

export function createMcpServer(deps: Deps, onCall?: (c: CallInfo) => void): McpServer {
  const server = new McpServer({ name: "gas-city", version: "0.2.0" });
  for (const t of buildTools(deps)) {
    server.registerTool(
      t.name,
      { title: t.title, description: t.description, inputSchema: t.inputShape, annotations: t.annotations },
      async (args: unknown) => {
        const t0 = Date.now();
        try {
          const r = await t.handler(args ?? {});
          onCall?.({ tool: t.name, args, ok: true, ms: Date.now() - t0, chars: r.text.length });
          return { content: [{ type: "text" as const, text: r.text }], structuredContent: r.structured };
        } catch (e) {
          onCall?.({ tool: t.name, args, ok: false, ms: Date.now() - t0, chars: 0 });
          throw e;
        }
      },
    );
  }
  return server;
}
