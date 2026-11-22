import { describe, it, expect, afterAll } from "vitest";
import crypto from "node:crypto";
import fs from "node:fs";
import net from "node:net";
import os from "node:os";
import path from "node:path";
import { createApp, listen, type AppOpts } from "../src/app.js";
import { GcClient } from "../src/gc-client.js";
import { hashPassphrase } from "../src/oauth.js";
import { isSafeRedirect, validateExtraRedirects } from "../src/dcr.js";
import * as F from "./fixtures/index.js";

const PASS = "correct horse battery staple";
const HASH = hashPassphrase(PASS);
const CLAUDE_CB = "https://claude.ai/api/mcp/auth_callback";
const CLAUDE_CB2 = "https://claude.com/api/mcp/auth_callback";
const GPT_CB = "https://chatgpt.com/connector/oauth/cb_123-AB";
const GPT_PLATFORM = "https://chatgpt.com/connector_platform_oauth_redirect";
const LOOP = "http://127.0.0.1:53682/callback";
const b64u = (b: Buffer) => b.toString("base64url");
const cleanups: (() => void)[] = [];
afterAll(() => cleanups.forEach((f) => f()));
const fetchImpl = (async (url: string) => new Response(JSON.stringify(new URL(url).pathname.endsWith("/usage") ? F.usage : F.status), { status: 200 })) as unknown as typeof fetch;
const freePort = () => new Promise<number>((res) => { const s = net.createServer().listen(0, "127.0.0.1", () => { const p = (s.address() as net.AddressInfo).port; s.close(() => res(p)); }); });

async function boot(over: Partial<AppOpts> = {}) {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "gcmcp-dcr-"));
  const port = await freePort();
  const base = `http://127.0.0.1:${port}`;
  const res = `${base}/mcp`;
  const clock = { t: 1_800_000_000_000 };
  const { app, provider } = createApp({
    deps: { gc: new GcClient({ baseUrl: "http://127.0.0.1:1", city: "gc2", fetchImpl }), mailAgent: "human", now: () => new Date() },
    publicUrl: new URL(base), ownerHash: HASH, staticClientId: "static-claude-id-0123456789",
    stateFile: path.join(dir, "state.json"), auditFile: path.join(dir, "audit.jsonl"), controlDir: dir, now: () => clock.t, ...over,
  });
  const srv = await listen(app, port);
  cleanups.push(() => { srv.close(); fs.rmSync(dir, { recursive: true, force: true }); });
  const register = (body: object, raw?: string) => fetch(`${base}/register`, { method: "POST", headers: { "content-type": "application/json" }, body: raw ?? JSON.stringify(body) });
  const reg = async (uris: string[], extra: object = {}) => register({ redirect_uris: uris, client_name: "Some App", token_endpoint_auth_method: "none", ...extra });
  const registered = async (uris: string[], extra: object = {}) => { const r = await reg(uris, extra); expect(r.status, JSON.stringify(uris)).toBe(201); return (await r.json()) as any; };
  const authorize = (cid: string, redirect: string, scope?: string) => {
    const verifier = b64u(crypto.randomBytes(32));
    const u = new URL(`${base}/authorize`);
    const q: Record<string, string> = { response_type: "code", client_id: cid, redirect_uri: redirect, code_challenge: b64u(crypto.createHash("sha256").update(verifier).digest()), code_challenge_method: "S256", state: "s", resource: res };
    if (scope) q.scope = scope;
    Object.entries(q).forEach(([k, v]) => u.searchParams.set(k, v));
    return fetch(u, { redirect: "manual" }).then((r) => ({ r, verifier }));
  };
  const form = (b: Record<string, string>) => ({ method: "POST", headers: { "content-type": "application/x-www-form-urlencoded" }, body: new URLSearchParams(b) });
  const consent = (html: string, passphrase = PASS) => fetch(`${base}/consent`, { ...form({ rid: /name=rid value="([^"]+)"/.exec(html)![1], passphrase, decision: "approve" }), redirect: "manual" });
  // the whole flow for a registered client: returns the page, the consent response and (when approved) the token
  const login = async (cid: string, redirect: string, scope?: string) => {
    const { r, verifier } = await authorize(cid, redirect, scope);
    const html = await r.text();
    expect(r.status).toBe(200);
    const c = await consent(html);
    const code = new URL(c.headers.get("location")!).searchParams.get("code")!;
    const t = await fetch(`${base}/token`, form({ grant_type: "authorization_code", client_id: cid, code, code_verifier: verifier, redirect_uri: redirect, resource: res }));
    return { html, csp: r.headers.get("content-security-policy"), token: (await t.json()) as any };
  };
  const meta = async () => (await (await fetch(`${base}/.well-known/oauth-authorization-server`)).json()) as any;
  const state = () => (fs.existsSync(path.join(dir, "state.json")) ? JSON.parse(fs.readFileSync(path.join(dir, "state.json"), "utf8")) : { clients: {} });
  return { base, dir, clock, provider, register, reg, registered, authorize, consent, login, form, meta, state, res };
}
const ON = { allowDcr: true };


describe("review: OAuth client identity through HTTP MCP",()=>{
 it("does not let two registered clients with the same display name share cursor state",async()=>{
  const fetchImpl=(async()=>new Response(JSON.stringify({items:[{description:"ordinary words here ".repeat(900)}]}))) as typeof fetch;
  const s=await boot({allowDcr:true,deps:{gc:new GcClient({baseUrl:"http://127.0.0.1:1",city:"gc2",fetchImpl}),mailAgent:"human",now:()=>new Date()}});
  const a=await s.registered([CLAUDE_CB],{client_name:"Identical name"});
  const b=await s.registered([CLAUDE_CB],{client_name:"Identical name"});
  expect(a.client_id).not.toBe(b.client_id);
  const ta=await s.login(a.client_id,CLAUDE_CB),tb=await s.login(b.client_id,CLAUDE_CB);
  const call=async(token:string,args:object)=>{
   const r=await fetch(s.res,{method:"POST",headers:{"content-type":"application/json",accept:"application/json, text/event-stream",authorization:"Bearer "+token},body:JSON.stringify({jsonrpc:"2.0",id:1,method:"tools/call",params:{name:"gc_api_read",arguments:args}})});
   expect(r.status).toBe(200);const text=await r.text(),line=text.split("\n").find(x=>x.startsWith("data:"));
   return JSON.parse(line?line.slice(5):text).result.structuredContent;
  };
  const page=await call(ta.token.access_token,{operation:"get-v0-city-by-city-name-beads"});
  expect(page.next_cursor).toBeTypeOf("string");
  const stolen=await call(tb.token.access_token,{cursor:page.next_cursor});
  expect(stolen.error).toBe(true);
  const continued=await call(ta.token.access_token,{cursor:page.next_cursor});
  expect(continued.error).not.toBe(true);
  expect(continued.page).toContain("ordinary words here");
 });
});

describe('OAuth client send-marker identity through HTTP MCP',()=>{
 it('only the client that sent sees replies between its send and the next wait',async()=>{
  const sent=1_800_000_000_000,waitStart=sent+5000;
  const clock={t:waitStart,now(){return this.t;},async sleep(ms:number){this.t+=ms;}};
  let posts=0;
  const fetchImpl=(async(url:string,init:RequestInit)=>{
   const p=new URL(url).pathname;
   if(init.method==='POST'){posts++;return new Response(JSON.stringify({status:'accepted'}));}
   const body=p.endsWith('/sessions')?{total:1,items:[{id:'s1',alias:'agent',state:'active',running:true}]}
    :p.endsWith('/transcript')?{turns:[{role:'assistant',text:'Reply for the sending client.',timestamp:new Date(sent+1000).toISOString()}]}
    :{state:'active',program_status:{state:'idle'},last_active:new Date(sent+1000).toISOString()};
   return new Response(JSON.stringify(body));
  })as typeof fetch;
  const s=await boot({allowDcr:true,writes:{enabled:true},deps:{gc:new GcClient({baseUrl:'http://127.0.0.1:1',city:'gc2',fetchImpl}),mailAgent:'human',now:()=>new Date(sent),clock}});
  const a=await s.registered([CLAUDE_CB],{client_name:'Same display name'}),b=await s.registered([CLAUDE_CB],{client_name:'Same display name'});
  const ta=await s.login(a.client_id,CLAUDE_CB,'gc:read gc:write'),tb=await s.login(b.client_id,CLAUDE_CB,'gc:read gc:write');
  const call=async(token:string,name:string,args:object)=>{
   const r=await fetch(s.res,{method:'POST',headers:{'content-type':'application/json',accept:'application/json, text/event-stream',authorization:'Bearer '+token},body:JSON.stringify({jsonrpc:'2.0',id:1,method:'tools/call',params:{name,arguments:args}})});
   expect(r.status).toBe(200);const text=await r.text(),line=text.split('\n').find(x=>x.startsWith('data:'));
   return JSON.parse(line?line.slice(5):text).result.structuredContent;
  };
  expect((await call(ta.token.access_token,'send_to_session',{session:'agent',text:'Hello'})).sent).toBe(true);
  const own=await call(ta.token.access_token,'wait_for_reply',{session:'agent',max_seconds:1});expect(own.page).toContain('Reply for the sending client.');
  const other=await call(tb.token.access_token,'wait_for_reply',{session:'agent',max_seconds:1});expect(other.page).toBe('');expect(other.since).toBe('m'+waitStart);expect(posts).toBe(1);
 });
});
