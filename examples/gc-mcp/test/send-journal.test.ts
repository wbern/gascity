import { afterEach, expect, it, vi } from "vitest";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import crypto from "node:crypto";
import { GcClient } from "../src/gc-client.js";
import { SendMarkers, WriteGate, WRITE_SCOPE, type WriteConfig } from "../src/writes.js";
import { guard } from "../src/text.js";
const dirs: string[] = [];
afterEach(() => { vi.restoreAllMocks(); for (const d of dirs.splice(0)) fs.rmSync(d, { recursive: true, force: true }); });
const A={client:"Same display",principal:"oauth-a",scopes:[WRITE_SCOPE]},B={...A,principal:"oauth-b"};
function fixture() {
 const dir=fs.mkdtempSync(path.join(os.tmpdir(),"gc-send-journal-"));dirs.push(dir);
 const journalFile=path.join(dir,"send-journal.json"),markerFile=path.join(dir,"reply-state.json");
 let now=1_800_000_000_000,posts=0,checkpoint:string|undefined,target="s1",drop=false;
 const gc=new GcClient({baseUrl:"http://127.0.0.1:1",city:"test",fetchImpl:(async(_u:string,init:RequestInit)=>{
  if(init.method==="POST"){
   posts++;checkpoint=fs.existsSync(journalFile)?fs.readFileSync(journalFile,"utf8"):undefined;
   if(drop)throw Error("response lost after acceptance");
   return new Response(JSON.stringify({status:"accepted",request_id:"req-1"}));
  }
  return new Response(JSON.stringify({total:1,items:[{id:target,alias:"agent",running:true}]}));
 })as typeof fetch});
 const make=(over:Partial<WriteConfig>={})=>new WriteGate(gc,{enabled:true,journalFile,replayWindowMs:60000,...over},()=>{},()=>now,new SendMarkers({file:markerFile,now:()=>now}));
 return{make,journalFile,dropResponse:()=>{drop=true;},retarget:(id:string)=>{target=id;},posts:()=>posts,checkpoint:()=>checkpoint,advance:(ms:number)=>{now+=ms;}};
}
it("replays an accepted send across restart with the original handle/key and only one POST",async()=>{
 const f=fixture(),first=await f.make().submit(A,"agent","check tests");
 const retry=await f.make().submit(A,"agent","check tests");
 expect(f.posts()).toBe(1);
 expect(retry.structured).toMatchObject({sent:true,replayed:true,handle:first.structured.handle,idempotency_key:first.structured.idempotency_key});
});
it("restores the pending intent present at POST time and refuses an accepted-but-lost restart resend",async()=>{
 const f=fixture(),first=await f.make().submit(A,"agent","PRIVATE request body must not be stored");
 expect(f.checkpoint()).toBeTypeOf("string");
 expect(f.checkpoint()).not.toContain("PRIVATE request body");
 // This is the durable snapshot at the instant the backend accepted, before outcome/ack storage.
 fs.writeFileSync(f.journalFile,f.checkpoint()!,{mode:0o600});
 const retry=await f.make().submit(A,"agent","PRIVATE request body must not be stored");
 expect(f.posts()).toBe(1);
 expect(retry.structured).toMatchObject({sent:false,reason:"outcome_unknown",replayed:true,handle:first.structured.handle,idempotency_key:first.structured.idempotency_key,completion:"not_proven"});
});
it("keeps OAuth principals and explicit different intents separate, and preserves replay-window expiry",async()=>{
 const f=fixture();await f.make().submit(A,"agent","check tests");
 await f.make().submit(B,"agent","check tests");
 await f.make().submit(A,"agent","check DIFFERENT tests");
 expect(f.posts()).toBe(3);
 f.advance(60001);await f.make().submit(A,"agent","check tests");expect(f.posts()).toBe(4);
});
it("does not POST when the pre-submit durable intent cannot be saved",async()=>{
 const f=fixture(),gate=f.make(),rename=fs.renameSync;
 vi.spyOn(fs,"renameSync").mockImplementation((from,to)=>{if(to===f.journalFile)throw Error("disk unavailable");return rename(from,to);});
 const result=await guard(()=>gate.submit(A,"agent","check tests"))({});
 expect(result.structured).toMatchObject({error:true,kind:"reply_state_unavailable",automatic_retry:false});
 expect(f.posts()).toBe(0);
});

it("does not follow a replacement session under the original spoken target on retry",async()=>{
 const f=fixture(),first=await f.make().submit(A,"agent","check tests");
 f.retarget("s2");
 const retry=await f.make().submit(A,"agent","check tests");
 expect(retry.structured).toMatchObject({sent:false,reason:"retry_target_changed",handle:first.structured.handle});
 expect(f.posts()).toBe(1);
 await f.make().submit(A,"agent","check explicitly DIFFERENT tests");expect(f.posts()).toBe(2);
});
it("retains the ordinary inclusive replay-window boundary across restart",async()=>{
 const f=fixture();await f.make().submit(A,"agent","check tests");
 f.advance(60000);expect((await f.make().submit(A,"agent","check tests")).structured.replayed).toBe(true);
 expect(f.posts()).toBe(1);
 f.advance(1);await f.make().submit(A,"agent","check tests");expect(f.posts()).toBe(2);
});
it("shares an active attempt even after its durable pending intent exists",async()=>{
 const dir=fs.mkdtempSync(path.join(os.tmpdir(),"gc-send-pending-"));dirs.push(dir);
 let enter!:()=>void,release!:()=>void,posts=0;
 const entered=new Promise<void>(r=>{enter=r;}),ready=new Promise<void>(r=>{release=r;});
 const gc=new GcClient({baseUrl:"http://127.0.0.1:1",city:"test",fetchImpl:(async(_u:string,init:RequestInit)=>{
  if(init.method==="POST"){posts++;enter();await ready;return new Response(JSON.stringify({status:"accepted"}));}
  return new Response(JSON.stringify({total:1,items:[{id:"s1",alias:"agent",running:true}]}));
 })as typeof fetch});
 const cfg={enabled:true,journalFile:path.join(dir,"send-journal.json")};
 const gate=new WriteGate(gc,cfg,()=>{});
 const first=gate.submit(A,"agent","check tests");await entered;
 const second=gate.submit(A,"agent","check tests");await new Promise(r=>setImmediate(r));release();
 const [a,b]=await Promise.all([first,second]);
 expect(a.structured.sent).toBe(true);expect(b.structured).toMatchObject({sent:true,handle:a.structured.handle});expect(posts).toBe(1);
});

it("retains uncertainty after response loss and prevents a restart POST",async()=>{
 const f=fixture();f.dropResponse();
 const first=await f.make().submit(A,"agent","check tests");
 expect(first.structured).toMatchObject({sent:false,reason:"outcome_unknown"});
 const retry=await f.make().submit(A,"agent","check tests");
 expect(retry.structured).toMatchObject({sent:false,reason:"outcome_unknown",replayed:true,handle:first.structured.handle,idempotency_key:first.structured.idempotency_key});
 expect(f.posts()).toBe(1);
});
it("a post-POST outcome-save failure leaves the pending guard usable after restart",async()=>{
 const f=fixture(),gate=f.make(),rename=fs.renameSync;let saves=0;
 vi.spyOn(fs,"renameSync").mockImplementation((from,to)=>{if(to===f.journalFile&&++saves===2)throw Error("disk failed after POST");return rename(from,to);});
 const first=await guard(()=>gate.submit(A,"agent","check tests"))({});
 expect(first.structured).toMatchObject({error:true,kind:"reply_state_unavailable",automatic_retry:false});
 expect(f.posts()).toBe(1);
 vi.restoreAllMocks();
 const original=JSON.parse(fs.readFileSync(f.journalFile,"utf8")).attempts[0];
 expect(original.outcome).toBe("pending");
 const retry=await f.make().submit(A,"agent","check tests");
 expect(retry.structured).toMatchObject({sent:false,reason:"outcome_unknown",replayed:true,handle:original.handle,idempotency_key:original.key});
 expect(f.posts()).toBe(1);
});
it("rechecks authorization, write enable and target policy before replaying a durable result",async()=>{
 const f=fixture();await f.make().submit(A,"agent","check tests");
 const readOnly=await f.make().submit({...A,scopes:["gc:read"]},"agent","check tests");
 expect(readOnly.structured).toMatchObject({sent:false,reason:"no_write_scope"});
 const disabled=await f.make({enabled:false}).submit(A,"agent","check tests");
 expect(disabled.structured).toMatchObject({sent:false,reason:"writes_disabled"});
 const denied=await f.make({deny:["agent"]}).submit(A,"agent","check tests");
 expect(denied.structured).toMatchObject({sent:false,reason:"denied_session"});
 expect(f.posts()).toBe(1);
});
it("refuses new submits at journal capacity without evicting unexpired pending guards",async()=>{
 const f=fixture(),hash=(s:string)=>crypto.createHash("sha256").update(s).digest("hex");
 const attempts=Array.from({length:200},(_,i)=>({digest:hash("entry"+i),requestDigest:hash("request"+i),client:A.principal,operation:"submit",session:"s1",key:crypto.randomUUID(),handle:"h"+crypto.randomUUID(),since:1800000000000,expiresAt:1800000060000,outcome:"pending"}));
 fs.writeFileSync(f.journalFile,JSON.stringify({version:1,attempts}),{mode:0o600});
 const result=await guard(()=>f.make().submit(A,"agent","fresh intent"))({});
 expect(result.structured).toMatchObject({error:true,kind:"reply_state_unavailable",automatic_retry:false});
 expect(f.posts()).toBe(0);
 expect(JSON.parse(fs.readFileSync(f.journalFile,"utf8")).attempts).toEqual(attempts);
});
