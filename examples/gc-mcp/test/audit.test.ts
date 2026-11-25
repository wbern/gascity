import { describe, it, expect } from "vitest";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { AuditLog } from "../src/audit.js";
describe("AuditLog", () => {
  it("rotates by size and keeps a bounded number of files", () => {
    const dir = fs.mkdtempSync(path.join(os.tmpdir(), "gcaudit-"));
    const f = path.join(dir, "a.jsonl");
    const a = new AuditLog(f, () => new Date(0), 400);
    for (let i = 0; i < 60; i++) a.write({ kind: "auth", note: "x".repeat(40) });
    const files = fs.readdirSync(dir).sort();
    expect(files).toEqual(["a.jsonl", "a.jsonl.1", "a.jsonl.2", "a.jsonl.3"]);
    fs.rmSync(dir, { recursive: true, force: true });
  });
  it("redacts secrets in args and notes", () => {
    const dir = fs.mkdtempSync(path.join(os.tmpdir(), "gcaudit-"));
    const f = path.join(dir, "a.jsonl");
    new AuditLog(f).write({ kind: "tool", args: { q: "Bearer abcdefghijklmnop1234567890" }, note: "password=hunter2hunter2" });
    const s = fs.readFileSync(f, "utf8");
    expect(s).not.toContain("abcdefghijklmnop1234567890");
    expect(s).not.toContain("hunter2hunter2");
    fs.rmSync(dir, { recursive: true, force: true });
  });
});
