import { it, expect } from "vitest";
import { GcClient } from "../src/gc-client.js";
import { buildTools } from "../src/tools.js";

it("includes paged lookup in one wait deadline and aborts the outstanding read before returning", async () => {
  let active = 0, aborted = false;
  const paths: string[] = [];
  const fetchImpl = (async (url: string, init: RequestInit) => {
    paths.push(new URL(url).pathname);
    if (paths.length <= 2) {
      await new Promise<void>((resolve, reject) => {
        const timer = setTimeout(resolve, 200);
        init.signal!.addEventListener("abort", () => { clearTimeout(timer); reject(new DOMException("aborted", "AbortError")); }, { once: true });
      });
      return new Response(JSON.stringify({ total: 5, items: [{ id: `s${paths.length}` }], next_cursor: `page${paths.length}` }));
    }
    active++;
    return await new Promise<Response>((_resolve, reject) => {
      init.signal!.addEventListener("abort", () => {
        active--; aborted = true;
        reject(new DOMException("aborted", "AbortError"));
      }, { once: true });
    });
  }) as typeof fetch;
  const gc = new GcClient({ baseUrl: "http://127.0.0.1:1", city: "gc2", timeoutMs: 1500, fetchImpl });
  const tool = buildTools({ gc, mailAgent: "human", now: () => new Date(), defaultSession: "agent" }, { client: "reader", scopes: ["gc:read"] }).find(t => t.name === "wait_for_reply")!;
  const started = performance.now();
  const result = await tool.handler({ max_seconds: 1 });
  const elapsed = performance.now() - started;
  expect(elapsed).toBeLessThan(1300);
  expect(result.structured).toMatchObject({ reason: "wait_deadline", complete: false });
  expect(Number(result.structured.waited_ms)).toBeGreaterThanOrEqual(900);
  expect(Number(result.structured.waited_ms)).toBeLessThan(1300);
  expect(aborted).toBe(true);
  expect(active).toBe(0);
  expect(paths).toHaveLength(3);
  expect(paths.every(p => p.endsWith("/sessions"))).toBe(true);
}, 4000);

it("carries the remaining budget through lookup, polling and a streaming transcript body", async () => {
  let bodyActive = false, bodyAborted = false;
  const paths: string[] = [];
  const fetchImpl = (async (url: string, init: RequestInit) => {
    const p = new URL(url).pathname;
    paths.push(p);
    if (!p.endsWith("/transcript")) {
      await new Promise<void>((resolve, reject) => {
        const timer = setTimeout(resolve, 200);
        init.signal!.addEventListener("abort", () => { clearTimeout(timer); reject(new DOMException("aborted", "AbortError")); }, { once: true });
      });
      return new Response(JSON.stringify(p.endsWith("/sessions") ? { total: 1, items: [{ id: "s1", alias: "agent", running: true }] } : { id: "s1", state: "active" }));
    }
    const body = new ReadableStream({ start(controller) {
      bodyActive = true;
      controller.enqueue(new TextEncoder().encode('{"turns":['));
      init.signal!.addEventListener("abort", () => {
        bodyActive = false; bodyAborted = true;
        controller.error(new DOMException("aborted", "AbortError"));
      }, { once: true });
    } });
    return new Response(body);
  }) as typeof fetch;
  const gc = new GcClient({ baseUrl: "http://127.0.0.1:1", city: "gc2", fetchImpl });
  const tool = buildTools({ gc, mailAgent: "human", now: () => new Date(), defaultSession: "agent" }, { client: "reader", scopes: ["gc:read"] }).find(t => t.name === "wait_for_reply")!;
  const start = performance.now();
  const result = await tool.handler({ max_seconds: 1 });
  const elapsed = performance.now() - start;
  expect(elapsed).toBeLessThan(1300);
  expect(result.structured).toMatchObject({ reason: "wait_deadline", complete: false });
  expect(Math.abs(elapsed - Number(result.structured.waited_ms))).toBeLessThan(50);
  expect(bodyAborted).toBe(true);
  expect(bodyActive).toBe(false);
  expect(paths).toEqual(["/v0/city/gc2/sessions", "/v0/city/gc2/session/s1", "/v0/city/gc2/session/s1/transcript"]);
}, 4000);
