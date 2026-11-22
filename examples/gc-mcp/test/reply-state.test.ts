import { afterEach, expect, it, vi } from "vitest";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { SendMarkers, WriteGate, WRITE_SCOPE } from "../src/writes.js";
import { guard } from "../src/text.js";
import { GcClient } from "../src/gc-client.js";

const dirs: string[] = [];
afterEach(() => { vi.restoreAllMocks(); for (const dir of dirs.splice(0)) fs.rmSync(dir, { recursive: true, force: true }); });
function fixture() {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "gc-replies-")); dirs.push(dir);
  let time = 1_800_000_000_000;
  return { file: path.join(dir, "reply-state.json"), now: () => time, advance: (ms: number) => { time += ms; } };
}

it("restores client-bound handles and pinned candidate identity without storing message bodies", () => {
  const f = fixture(), first = new SendMarkers({ file: f.file, now: f.now });
  const handle = first.create("oauth-client-a", "s1", f.now());
  first.pinFirstReply("oauth-client-a", handle, "abcdef012345");
  const restored = new SendMarkers({ file: f.file, now: f.now });
  expect(restored.resolve("oauth-client-a", handle)).toMatchObject({ session: "s1", since: f.now(), firstReplyId: "abcdef012345" });
  expect(restored.resolve("oauth-client-b", handle)).toBeUndefined();
  expect(restored.get("oauth-client-a", "s1")).toBe(f.now());
  expect(fs.statSync(f.file).mode & 0o777).toBe(0o600);
  const raw = JSON.parse(fs.readFileSync(f.file, "utf8"));
  expect(Object.keys(raw.handles[0]).sort()).toEqual(["client", "expiresAt", "firstReplyId", "handle", "session", "since"]);
});

it("bounds restored handles and expires them without renewing TTL when read or pinned", () => {
  const f = fixture(), first = new SendMarkers({ file: f.file, now: f.now, ttlMs: 1000, maxHandles: 2 });
  const a = first.create("owner", "s1", f.now());
  const b = first.create("owner", "s2", f.now());
  const c = first.create("owner", "s3", f.now());
  expect(first.resolve("owner", a)).toBeUndefined();
  f.advance(900); first.pinFirstReply("owner", b, "abcdef012345");
  const restored = new SendMarkers({ file: f.file, now: f.now, ttlMs: 1000, maxHandles: 2 });
  expect(restored.resolve("owner", c)).toBeDefined();
  f.advance(101);
  expect(restored.resolve("owner", b)).toBeUndefined();
  expect(restored.resolve("owner", c)).toBeUndefined();
  expect(restored.get("owner", "s2")).toBeUndefined();
  expect(JSON.parse(fs.readFileSync(f.file, "utf8")).handles).toEqual([]);
});

it("refuses corrupt, incompatible, oversized or nonprivate state instead of starting empty", () => {
  const f = fixture();
  for (const text of ["{broken", JSON.stringify({ version: 99, handles: [] }), "x".repeat(1_000_001)]) {
    fs.writeFileSync(f.file, text, { mode: 0o600 });
    expect(() => new SendMarkers({ file: f.file, now: f.now })).toThrow(/reply state/i);
  }
  fs.writeFileSync(f.file, JSON.stringify({ version: 1, handles: [] })); fs.chmodSync(f.file, 0o644);
  expect(() => new SendMarkers({ file: f.file, now: f.now })).toThrow(/reply state/i);
});

it("refuses symlinks and leaves their target untouched", () => {
  const f = fixture(), target = f.file + ".target";
  const original = JSON.stringify({ version: 1, handles: [] });
  fs.writeFileSync(target, original, { mode: 0o600 }); fs.symlinkSync(target, f.file);
  expect(() => new SendMarkers({ file: f.file, now: f.now })).toThrow(/reply state/i);
  expect(fs.readFileSync(target, "utf8")).toBe(original);
});

it("fails closed after an atomic save failure and retains the prior on-disk snapshot", () => {
  const f = fixture(), first = new SendMarkers({ file: f.file, now: f.now });
  const handle = first.create("owner", "s1", f.now());
  const before = fs.readFileSync(f.file, "utf8");
  vi.spyOn(fs, "renameSync").mockImplementation(() => { throw new Error("disk unavailable"); });
  expect(() => first.pinFirstReply("owner", handle, "abcdef012345")).toThrow(/reply state/i);
  expect(fs.readFileSync(f.file, "utf8")).toBe(before);
  expect(() => first.resolve("owner", handle)).toThrow(/reply state/i);
  expect(fs.readdirSync(path.dirname(f.file))).toEqual(["reply-state.json"]);
});

it("does not POST or retry when handle persistence fails before submission", async () => {
  const f = fixture(), markers = new SendMarkers({ file: f.file, now: f.now });
  let posts = 0;
  const gc = new GcClient({ baseUrl: "http://127.0.0.1:1", city: "test", fetchImpl: (async (_url: string, init: RequestInit) => {
    if (init.method === "POST") posts++;
    return new Response(JSON.stringify({ total: 1, items: [{ id: "s1", alias: "agent", running: true }] }));
  }) as typeof fetch });
  const gate = new WriteGate(gc, { enabled: true }, () => {}, f.now, markers);
  vi.spyOn(fs, "renameSync").mockImplementation(() => { throw new Error("disk unavailable"); });
  const who = { client: "owner", scopes: [WRITE_SCOPE] };
  await expect(gate.submit(who, "s1", "check the tests")).rejects.toThrow(/reply state/i);
  const retry = await guard(() => gate.submit(who, "s1", "check the tests"))({});
  expect(retry.structured).toMatchObject({ error: true, kind: "reply_state_unavailable", automatic_retry: false });
  expect(posts).toBe(0);
});
