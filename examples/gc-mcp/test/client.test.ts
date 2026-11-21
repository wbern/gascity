import { describe, it, expect } from "vitest";
import { GcClient, GcError } from "../src/gc-client.js";

describe("GcClient hardening", () => {
  it("refuses non-loopback hosts, including lookalikes and a bare label", () => {
    for (const u of ["http://x", "http://127.0.0.1.evil.com", "http://evil.com", "http://127.0.0.1@evil.com", "http://10.0.0.5:8372"]) {
      expect(() => new GcClient({ baseUrl: u, city: "c" })).toThrow(/loopback/);
    }
  });
  it("accepts real loopback", () => {
    for (const u of ["http://127.0.0.1:8372", "http://localhost:8372", "http://[::1]:8372"]) {
      expect(() => new GcClient({ baseUrl: u, city: "c" })).not.toThrow();
    }
  });
  it("does not follow redirects and caps response size", async () => {
    const seen: RequestInit[] = [];
    const big = "x".repeat(3_000_000);
    const c = new GcClient({ baseUrl: "http://127.0.0.1:1", city: "c", fetchImpl: (async (_u: string, init: RequestInit) => { seen.push(init); return new Response(big, { status: 200 }); }) as unknown as typeof fetch });
    await expect(c.get("/status")).rejects.toThrow(GcError);
    expect(seen[0].redirect).toBe("error");
  });
  it("maps a thrown network error to GcError(0)", async () => {
    const c = new GcClient({ baseUrl: "http://127.0.0.1:1", city: "c", fetchImpl: (async () => { throw new Error("boom"); }) as unknown as typeof fetch });
    await expect(c.get("/status")).rejects.toMatchObject({ status: 0 });
  });
});
