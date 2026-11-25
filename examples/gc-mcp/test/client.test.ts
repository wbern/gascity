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

it('enforces the byte limit on undeclared multi-byte responses instead of counting characters',async()=>{
 const payload=JSON.stringify({text:'漢'.repeat(20)});expect(payload.length).toBeLessThan(60);expect(Buffer.byteLength(payload)).toBeGreaterThan(60);
 const c=new GcClient({baseUrl:'http://127.0.0.1:1',city:'c',maxBytes:60,fetchImpl:(async()=>new Response(payload))as typeof fetch});
 await expect(c.get('/status')).rejects.toMatchObject({kind:'too_large'});
});
it('cancels an oversized chunked response before consuming the remainder',async()=>{
 let cancelled=false,pulls=0;
 const body=new ReadableStream<Uint8Array>({pull(c){pulls++;c.enqueue(new TextEncoder().encode('x'.repeat(80)));if(pulls===10)c.close();},cancel(){cancelled=true;}});
 const c=new GcClient({baseUrl:'http://127.0.0.1:1',city:'c',maxBytes:100,fetchImpl:(async()=>new Response(body))as typeof fetch});
 await expect(c.get('/status')).rejects.toMatchObject({kind:'too_large'});expect(cancelled).toBe(true);expect(pulls).toBeLessThan(10);
});
it('cancels a stalled body on deadline even after fetch has already returned',async()=>{
 let cancelled=false;const body=new ReadableStream<Uint8Array>({cancel(){cancelled=true;}});
 const c=new GcClient({baseUrl:'http://127.0.0.1:1',city:'c',timeoutMs:20,fetchImpl:(async()=>new Response(body))as typeof fetch});
 await expect(c.get('/status')).rejects.toMatchObject({kind:'timeout'});expect(cancelled).toBe(true);
});
