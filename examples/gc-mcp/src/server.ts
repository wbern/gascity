import { McpServer } from "@modelcontextprotocol/sdk/server/mcp.js";
import { buildTools, type Deps } from "./tools.js";
import type { WriteCtx } from "./writes.js";
import { INSTRUCTIONS } from "./instructions.js";

export interface CallInfo { tool: string; args: unknown; ok: boolean; ms: number; chars: number }

export function createMcpServer(deps: Deps, onCall?: (c: CallInfo) => void, who?: WriteCtx): McpServer {
  const server = new McpServer({ name: "gas-city", version: "0.3.0" }, { instructions: INSTRUCTIONS });
  for (const t of buildTools(deps, who)) {
    server.registerTool(
      t.name,
      { title: t.title, description: t.description, inputSchema: t.inputShape, annotations: t.annotations },
      async (args: unknown) => {
        const t0 = Date.now();
        try {
          const r = await t.handler(args ?? {});
          // write tools audit themselves (redacted preview, outcome); their arguments never go through the generic line
          onCall?.({ tool: t.name, args: t.isWrite ? undefined : args, ok: true, ms: Date.now() - t0, chars: r.text.length });
          return { content: [{ type: "text" as const, text: r.text }], structuredContent: r.structured };
        } catch (e) {
          onCall?.({ tool: t.name, args: t.isWrite ? undefined : args, ok: false, ms: Date.now() - t0, chars: 0 });
          throw e;
        }
      },
    );
  }
  return server;
}
