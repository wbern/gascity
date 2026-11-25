import { describe, it, expect } from "vitest";
import { GcClient, GcError } from "../src/gc-client.js";
import { gcErrorResult } from "../src/text.js";
import { listPages, resolveSession } from "../src/sessions.js";
import { resolveCity } from "../src/city.js";
import { decisionQuestion, isFlaggedMail, buildBriefingTools } from "../src/briefing.js";
import { buildVoiceTools, replyId } from "../src/voice.js";
import type { Deps } from "../src/tools.js";

const json = (b: unknown, status = 200) => new Response(JSON.stringify(b), { status });
const client = (route: (path: string, q: URLSearchParams) => Response) => new GcClient({ baseUrl: "http://127.0.0.1:1", city: "c", fetchImpl: (async (u: string) => { const x = new URL(u); return route(x.pathname.replace(/^\/v0\/city\/[^/]+/, ""), x.searchParams); }) as unknown as typeof fetch });

describe("errors say what actually went wrong", () => {
  it("not found, too large, timed out, down and refused are told apart, each with its own kind and sentence", async () => {
    const kinds: [Response | Error, string, RegExp][] = [
      [json({ detail: "no such session" }, 404), "not_found", /was not found/],
      [json({ detail: "x" }, 413), "too_large", /too large to read/],
      [json({ detail: "boom" }, 503), "unavailable", /not responding right now \(503\)/],
      [json({ detail: "bad id" }, 400), "refused", /refused that \(400: bad id\)/],
      [Object.assign(new Error("t"), { name: "AbortError" }), "timeout", /took too long/],
      [new Error("ECONNREFUSED"), "unavailable", /not responding/],
    ];
    for (const [res, kind, say] of kinds) {
      const gc = new GcClient({ baseUrl: "http://127.0.0.1:1", city: "c", fetchImpl: (async () => { if (res instanceof Error) throw res; return res.clone(); }) as unknown as typeof fetch });
      const e = await gc.get("/x").catch((x) => x);
      expect(e, kind).toBeInstanceOf(GcError);
      expect(e.kind, String(say)).toBe(kind);
      const r = gcErrorResult(e);
      expect(r.text).toMatch(say);
      expect(r.structured).toMatchObject({ error: true, kind });
      expect(r.text).not.toMatch(/unavailable right now/);
    }
  });
  it("a response over the cap is 'too large', not 'down'", async () => {
    const gc = new GcClient({ baseUrl: "http://127.0.0.1:1", city: "c", maxBytes: 100, fetchImpl: (async () => new Response("x".repeat(500), { status: 200 })) as unknown as typeof fetch });
    const e = await gc.get("/x").catch((x) => x);
    expect(e.kind).toBe("too_large");
  });
});

describe("lists are read across pages and never silently truncated", () => {
  it("listPages follows next_cursor, passes total/partial through and says when it stopped early", async () => {
    const seen: string[] = [];
    const gc = client((_p, q) => { seen.push(q.get("cursor") ?? ""); const c = q.get("cursor"); return json(c === "p2" ? { total: 450, items: [{ id: "c" }] } : c === "p1" ? { total: 450, next_cursor: "p2", partial: true, partial_errors: ["rig x"], items: [{ id: "b" }] } : { total: 450, next_cursor: "p1", items: [{ id: "a" }] }); });
    const all = await listPages(gc, "/sessions");
    expect(all.items.map((i: any) => i.id)).toEqual(["a", "b", "c"]);
    expect(all).toMatchObject({ total: 450, partial: true, partial_errors: ["rig x"], truncated: true });     // 450 total, 3 read: say so
    expect(seen).toEqual(["", "p1", "p2"]);
    const cut = await listPages(gc, "/sessions", {}, 1);
    expect(cut).toMatchObject({ truncated: true, next_cursor: "p1" });
  });
  it("name resolution sees sessions beyond the first page", async () => {
    const gc = client((_p, q) => json(q.get("cursor") === "p1" ? { total: 3, items: [{ id: "gc2-wisp-zz", template: "late/arrival" }] } : { total: 3, next_cursor: "p1", items: [{ id: "gc2-wisp-a", alias: "mayor" }, { id: "gc2-wisp-b", alias: "devops" }] }));
    expect(await resolveSession(gc, "late arrival")).toMatchObject({ kind: "one", hit: { id: "gc2-wisp-zz" } });
  });
});

describe("transcripts: `tail` is compaction SEGMENTS, so one segment is asked for and no more than three reads happen per wait", () => {
  const mk = (turns: any[], detail: any = {}) => {
    const calls: string[] = [];
    const gc = client((p, q) => { calls.push(`${p}?${q}`); if (p === "/sessions") return json({ total: 1, items: [{ id: "gc2-wisp-a", alias: "agent", state: "active", running: true }] }); if (/transcript$/.test(p)) return json({ turns }); return json({ id: "gc2-wisp-a", ...detail }); });
    const clock = { t: 1_800_000_000_000, now() { return clock.t; }, async sleep(ms: number) { clock.t += ms; } };      // a clock that advances, like a real one
    const d: Deps = { gc, mailAgent: "human", now: () => new Date(), defaultSession: "agent", clock };
    return { calls, tools: buildVoiceTools(d, { client: "c", scopes: ["gc:read"] }) };
  };
  it("latest_reply asks for tail=1 (one segment) and never tail=8", async () => {
    const { calls, tools } = mk([{ role: "assistant", text: "Hello there.", timestamp: "2026-01-01T00:00:00Z" }]);
    await tools.find((t) => t.name === "latest_reply")!.handler({});
    expect(calls.filter((c) => c.includes("transcript")).every((c) => /tail=1(&|$)/.test(c))).toBe(true);
  });
  it("read_more is pinned to a reply: if the agent said something newer the offset is NOT applied to a different message", async () => {
    const long = "A sentence that goes on. ".repeat(40);
    const a = mk([{ role: "assistant", text: long, timestamp: "2026-01-01T00:00:00Z" }]);
    const first: any = await a.tools.find((t) => t.name === "latest_reply")!.handler({});
    const id = first.structured.reply_id;
    expect(id).toBe(replyId("2026-01-01T00:00:00Z", long.replace(/\s+/g, " ").trim()));
    const same: any = await a.tools.find((t) => t.name === "read_more")!.handler({ offset: first.structured.next_offset, reply_id: id });
    expect(same.structured.changed).toBeUndefined();
    const b = mk([{ role: "assistant", text: "Something else entirely.", timestamp: "2026-01-01T00:05:00Z" }]);
    const changed: any = await b.tools.find((t) => t.name === "read_more")!.handler({ offset: first.structured.next_offset, reply_id: id });
    expect(changed.structured.changed).toBe(true);
    expect(changed.text).toMatch(/reply changed/);
    expect(changed.structured.page).toBeUndefined();
  });
  it("wait_for_reply reads the transcript only when the session changed, at most 3 times", async () => {
    const { calls, tools } = mk([], { last_active: "x", activity: "working" });     // nothing ever changes and nothing is said
    const r: any = await tools.find((t) => t.name === "wait_for_reply")!.handler({ max_seconds: 20 });
    expect(r.structured.fetches).toBeLessThanOrEqual(3);
    expect(calls.filter((c) => c.includes("transcript")).length).toBeLessThanOrEqual(3);
    expect(calls.filter((c) => /^\/session\/gc2-wisp-a\?/.test(c)).length).toBeGreaterThan(5);        // the cheap session record is what is polled
  });
});

describe("no hard-coded owner or city", () => {
  it("the owner is addressed by name only when one is configured", () => {
    const gate = { title: "Gate: Dana decision on the budget", await_type: "human", description: "" };
    expect(decisionQuestion(gate, "Dana")).toBe("your decision on the budget");
    expect(decisionQuestion(gate)).toBe("Dana decision on the budget");
    expect(decisionQuestion({ title: "Gate: William decision on x", description: "" })).toBe("William decision on x");   // no built-in name
  });
  it("flagged mail uses the configured owner name and the mayor, never a built-in person", () => {
    expect(isFlaggedMail({ from: "mayor", subject: "Plan" })).toBe(true);
    expect(isFlaggedMail({ from: "dana", subject: "hello" }, "Dana")).toBe(true);
    expect(isFlaggedMail({ from: "dana", subject: "hello" })).toBe(false);
    expect(isFlaggedMail({ from: "william", subject: "hello" })).toBe(false);                    // not special any more
    expect(isFlaggedMail({ from: "x", subject: "This needs Dana" }, "Dana")).toBe(true);
    expect(isFlaggedMail({ from: "mayor", subject: "Reminder — human gate still open" })).toBe(false);
  });
  it("the city comes from GC_MCP_CITY, else the supervisor's list when it is exactly one; several cities is an error naming them", async () => {
    const f = (items: any[]) => (async () => json({ items })) as unknown as typeof fetch;
    expect(await resolveCity("http://x", "mine")).toBe("mine");
    expect(await resolveCity("http://x", undefined, f([{ name: "only" }]))).toBe("only");
    await expect(resolveCity("http://x", "", f([{ name: "a" }, { name: "b" }]))).rejects.toThrow(/runs 2 cities \(a, b\); set GC_MCP_CITY/);
    await expect(resolveCity("http://x", "", f([]))).rejects.toThrow(/no cities/);
    await expect(resolveCity("http://x", "", (async () => { throw new Error("down"); }) as unknown as typeof fetch)).rejects.toThrow(/could not be read/);
    expect((await import("node:fs")).readFileSync(new URL("../src/main.ts", import.meta.url), "utf8")).not.toMatch(/\?\? "gc2"/);
  });
});

describe("derived values say they are derived", () => {
  it("the briefing states how each number was worked out, and says 'at least' when pages were truncated", async () => {
    const gc = client((p, q) => { if (p === "/status") return json({ agents: { quarantined: 0 } }); if (p === "/sessions") return json({ total: 1, items: [{ id: "s", alias: "a", state: "active", running: true, activity: "in-turn" }] });
      if (p === "/beads" && q.get("type") === "gate") return json({ total: 400, next_cursor: "more", items: [{ id: "g", title: "Gate: human", await_type: "human", status: "open", created_at: "2026-01-01T00:00:00Z", description: "Reason: pick one" }] });
      return json({ items: [], total: 0 }); });
    const d: Deps = { gc, mailAgent: "human", now: () => new Date(), briefings: new Map(), clock: { now: () => 1_800_000_000_000, sleep: async () => undefined } };
    const r: any = await buildBriefingTools(d, { client: "c", scopes: ["gc:read"] }).find((t) => t.name === "briefing")!.handler({});
    expect(r.structured.derived).toMatchObject({ working: expect.stringContaining("in-turn"), flagged_mail: expect.stringContaining("heuristic"), finished: expect.stringContaining("heuristic filter") });
    expect(r.structured.truncated.decisions).toBe(true);
    expect(r.text).toMatch(/at least \d+ decisions?/);
  });
});
