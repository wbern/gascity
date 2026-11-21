// NOTE: credential-shaped fixtures are assembled from fragments at runtime so that
// no secret-looking literal exists in the repository (GitHub push protection).
import { describe, it, expect } from "vitest";
import { redactText, redactDeep } from "../src/redact.js";

describe("redactText", () => {
  const cases: [string, string][] = [
    ["Authorization: Bearer abc123DEF456ghi789jkl012mno345", "Bearer"],
    ["token " + "gh" + "p_" + "abcdefghijklmnopqrstuvwxyz0123456789", "gh" + "p_"],
    ["key " + "sk" + "-ant-api03-abcdefghijklmnopqrstuvwxyz012345", "sk" + "-ant"],
    ["slack " + "xo" + "xb-1234567890-abcdefghijklmnop", "xo" + "xb"],
    ["aws " + "AK" + "IAABCDEFGHIJKLMNOP", "AK" + "IA"],
    ["jwt " + "ey" + "JhbGciOiJIUzI1NiJ9." + "ey" + "JzdWIiOiIxMjM0NTY3ODkwIn0.abcdefghijk", "ey" + "JhbGci"],
    ["VERCEL_BYPASS=supersecretvalue123", "supersecretvalue123"],
    ["password: hunter2hunter2", "hunter2hunter2"],
    ["https://x.app/?x-vercel-protection-bypass=abcDEF123456", "abcDEF123456"],
    ["blob 0123456789abcdef0123456789abcdef0123456789abcdef", "0123456789abcdef0123456789abcdef"],
  ];
  for (const [input, leaked] of cases) {
    it(`redacts ${leaked}`, () => {
      const out = redactText(input);
      expect(out).not.toContain(leaked);
      expect(out).toContain("[redacted]");
    });
  }
  it("leaves ordinary prose and short ids alone", () => {
    const s = "gc2-wisp-wmt4w6 is active; 3 unread mails, bead gcw-ti563 open.";
    expect(redactText(s)).toBe(s);
  });
});

describe("redactDeep", () => {
  it("redacts nested strings and secret-named keys", () => {
    const out = redactDeep({ a: ["Bearer abcdefghijklmnop1234567890"], api_key: "plainvalue", n: 3 });
    expect(JSON.stringify(out)).not.toContain("abcdefghijklmnop1234567890");
    expect(out.api_key).toBe("[redacted]");
    expect(out.n).toBe(3);
  });
});

describe("redactText false positives", () => {
  it("keeps long file paths and kebab ids readable", () => {
    const s = "updated /home/fishyguy/gc2/.gc/worktrees/gas-city-wbern/tasks/gcw-ti563/integrations/gc-mcp/test/tools.test.ts and gas-city-wbern-crew-architect-session-name-here";
    expect(redactText(s)).toBe(s);
  });
  it("still redacts a 40+ char mixed alnum token", () => {
    expect(redactText("tok Zk3jQ9vLx2mP7aRt5NwB8cYd4HeUf6GsIo1VnXq0 end")).toContain("[redacted]");
  });
});
