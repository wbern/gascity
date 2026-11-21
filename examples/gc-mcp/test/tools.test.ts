import { describe, it, expect } from "vitest";
import { buildTools } from "../src/tools.js";
import { GcClient, GcError } from "../src/gc-client.js";
import * as F from "./fixtures/index.js";

function fakeClient(routes: Record<string, unknown | (() => never)>) {
  const calls: string[] = [];
  const fetchImpl = (async (url: string) => {
    const u = new URL(url);
    calls.push(u.pathname + u.search);
    for (const [k, v] of Object.entries(routes)) {
      if (u.pathname.endsWith(k)) {
        if (typeof v === "function") (v as () => never)();
        return new Response(JSON.stringify(v), { status: 200, headers: { "content-type": "application/json" } });
      }
    }
    return new Response(JSON.stringify({ detail: "nope" }), { status: 404 });
  }) as unknown as typeof fetch;
  return { gc: new GcClient({ baseUrl: "http://x", city: "gc2", fetchImpl }), calls };
}
const tools = (routes: Record<string, unknown>) => {
  const c = fakeClient(routes);
  return { t: buildTools({ gc: c.gc, mailAgent: "human", now: () => new Date("2026-11-21T16:30:00Z") }), calls: c.calls };
};
const speakable = (s: string) => expect(s.length).toBeLessThan(400);

describe("tools", () => {
  it("every tool is read-only annotated", () => {
    const { t } = tools({});
    expect(t.map((x) => x.name).sort()).toEqual(
      ["bead_show", "city_status", "list_sessions", "mail_inbox", "peek_session", "ready_work", "whats_new"],
    );
    for (const x of t) expect(x.annotations.readOnlyHint).toBe(true);
  });

  it("city_status speaks counts and degrades when census is warming", async () => {
    const { t } = tools({ "/status": F.status, "/usage": F.usage });
    const r = await t.find((x) => x.name === "city_status")!.handler({});
    expect(r.text).toMatch(/23 of 40 agents running/);
    expect(r.text).toMatch(/29 unread/);
    expect(r.text).toMatch(/1171 ready/);
    speakable(r.text);
    expect(r.structured).toHaveProperty("agents");
  });

  it("list_sessions summarises by state and names the most recent", async () => {
    const { t } = tools({ "/sessions": F.sessions });
    const r = await t.find((x) => x.name === "list_sessions")!.handler({ limit: 2 });
    expect(r.text).toMatch(/2 active/);
    expect(r.text).toMatch(/mayor/);
    speakable(r.text);
  });

  it("peek_session resolves alias, tails turns, trims and redacts", async () => {
    const { t } = tools({ "/sessions": F.sessions, "/transcript": F.transcript });
    const r = await t.find((x) => x.name === "peek_session")!.handler({ session: "gc-architect", tail: 2 });
    expect(r.text).not.toContain("abcdefghijklmnop1234567890");
    expect(r.text).toContain("[redacted]");
    expect(r.text.length).toBeLessThan(1200);
    expect((r.structured as { turns: unknown[] }).turns).toHaveLength(2);
  });

  it("peek_session says so for an unknown session", async () => {
    const { t } = tools({ "/sessions": F.sessions });
    const r = await t.find((x) => x.name === "peek_session")!.handler({ session: "nobody" });
    expect(r.text).toMatch(/no session/i);
  });

  it("mail_inbox gives unread count and subjects only (no bodies)", async () => {
    const { t } = tools({ "/mail": F.mail, "/mail/count": { total: 40, unread: 1 } });
    const r = await t.find((x) => x.name === "mail_inbox")!.handler({});
    expect(r.text).toMatch(/1 unread/);
    expect(r.text).toContain("Campaign phase 1 done");
    expect(JSON.stringify(r)).not.toContain("details");
    speakable(r.text);
  });

  it("ready_work reports total and top titles", async () => {
    const { t } = tools({ "/beads/ready/summary": F.readySummary, "/status": F.status });
    const r = await t.find((x) => x.name === "ready_work")!.handler({ limit: 2 });
    expect(r.text).toMatch(/^1171 ready/);
    expect(r.text).toContain("Fix the thing");
  });

  it("bead_show truncates the description and redacts", async () => {
    const { t } = tools({ "/bead/gcw-ti563": F.bead });
    const r = await t.find((x) => x.name === "bead_show")!.handler({ id: "gcw-ti563" });
    expect(r.text).not.toContain("abcdef123456");
    expect(r.text.length).toBeLessThan(700);
  });

  it("bead_show rejects ids with path characters", async () => {
    const { t, calls } = tools({});
    await expect(t.find((x) => x.name === "bead_show")!.handler({ id: "../config" })).rejects.toThrow();
    expect(calls).toHaveLength(0);
  });

  it("whats_new groups events by type within the window", async () => {
    const { t, calls } = tools({ "/events": F.events });
    const r = await t.find((x) => x.name === "whats_new")!.handler({ since: "30m" });
    expect(r.text).toMatch(/2 bead\.closed/);
    expect(calls[0]).toContain("since=");
  });

  it("surfaces upstream failure as a speakable message, not a stack", async () => {
    const { t } = tools({ "/status": (() => { throw new GcError(503, "warming"); }) as never });
    const r = await t.find((x) => x.name === "city_status")!.handler({});
    expect(r.text).toMatch(/unavailable|couldn't/i);
  });
});

describe("voice quality", () => {
  it("peek_session skips bare tool/thinking placeholders", async () => {
    const tr = { turns: [
      { role: "assistant", text: "Build is green.", timestamp: "t1" },
      { role: "assistant", text: "[thinking]", timestamp: "t2" },
      { role: "assistant", text: "[Bash]", timestamp: "t3" },
    ] };
    const { t } = tools({ "/sessions": F.sessions, "/transcript": tr });
    const r = await t.find((x) => x.name === "peek_session")!.handler({ session: "mayor", tail: 2 });
    expect(r.text).toContain("Build is green.");
    expect(r.text).not.toContain("[thinking]");
    expect(r.text).not.toContain("[Bash]");
  });
  it("bead_show stays short enough to speak", async () => {
    const { t } = tools({ "/bead/gcw-ti563": F.bead });
    const r = await t.find((x) => x.name === "bead_show")!.handler({ id: "gcw-ti563" });
    expect(r.text.length).toBeLessThan(380);
  });
});
