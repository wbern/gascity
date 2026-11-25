import {describe,it,expect} from 'vitest';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import {GcClient,WRITE_OPS} from '../src/gc-client.js';
import {WriteGate} from '../src/writes.js';
import {buildTools} from '../src/tools.js';
const spec=JSON.parse(fs.readFileSync(new URL('../../../internal/api/openapi.json',import.meta.url),'utf8'));
const cases=[['mail_send','mail_send','city',{to:'crm/steve',subject:'Hi',body:'Hello'},'/mail'],['mail_reply','mail_reply','m1',{body:'Yes'},'/mail/m1/reply'],['mail_mark_read','mail_mark_read','m1',{},'/mail/m1/read'],['bead_create','bead_create','city',{title:'A task',rig:'gas-city-wbern'},'/beads'],['bead_sling','bead_sling','city',{target:'worker',bead:'b1',rig:'gas-city-wbern'},'/sling']] as const;
function setup(cfg:any={}){const posts:any[]=[],audit:any[]=[];const gc=new GcClient({baseUrl:'http://127.0.0.1:1',city:'c',fetchImpl:(async(u:string,o:RequestInit)=>{expect(o.method).toBe('POST');posts.push({path:new URL(u).pathname,body:JSON.parse(String(o.body)),headers:o.headers});return new Response(JSON.stringify({id:'created-1',status:'accepted'}));})as typeof fetch});const gate=new WriteGate(gc,{enabled:true,...cfg},e=>audit.push(e));const deps={gc,writes:gate,mailAgent:'human',now:()=>new Date()};const who={client:'owner',scopes:['gc:read','gc:write']};return{posts,audit,gc,gate,deps,who};}
describe('approved domain writes',()=>{
 it('binds every new mutation to its OpenAPI operation and sends only the approved path/body',async()=>{
  for(const[name,op,id,body,route]of cases){const s=setup();const t=buildTools(s.deps,s.who).find(t=>t.name===name)!;expect(t.isWrite).toBe(true);const r=await t.handler({id,...body});expect(r.structured.sent,name).toBe(true);expect(s.posts).toHaveLength(1);expect(s.posts[0].path).toBe('/v0/city/c'+route);expect(s.posts[0].headers['x-gc-request']).toBeTruthy();if(op==='mail_send'||op==='mail_reply')expect(s.posts[0].body.from).toBe('human');const def=(WRITE_OPS as any)[op];const template='/v0/city/{cityName}'+def.path;expect(spec.paths[template].post.operationId).toBe(def.operationId);expect(s.audit.at(-1)).toMatchObject({kind:'write',ok:true});}
 });
 it('requires write scope and rechecks the kill switch for a previously listed tool',async()=>{
  const dir=fs.mkdtempSync(path.join(os.tmpdir(),'gcmcp-domain-'));
  try{const s=setup({controlDir:dir});expect(buildTools(s.deps,{client:'ro',scopes:['gc:read']}).some(t=>t.isWrite)).toBe(false);const t=buildTools(s.deps,s.who).find(t=>t.name==='mail_send')!;
   fs.writeFileSync(path.join(dir,'WRITES_DISABLED'),'');const r=await t.handler({to:'crm/steve',subject:'Hi'});expect(r.structured.sent).toBe(false);expect(s.posts).toHaveLength(0);
  }finally{fs.rmSync(dir,{recursive:true});}
 });
 it('rejects force/reassign/traversal/oversize and never exposes destructive operations',async()=>{
  const s=setup();for(const[op,id,body]of [['bead_sling','city',{target:'worker',bead:'b1',force:true}],['mail_reply','../close',{body:'hi'}],['mail_send','city',{to:'x',subject:'hi',body:'x'.repeat(4001)}],['bead_close','b1',{}]]){
   const r=await (s.gate as any).operation(s.who,op,id,body);expect(r.structured.sent).toBe(false);
  }expect(s.posts).toHaveLength(0);
 });
 it('enforces direct-call scope, confirmation failure and UTF-8 size limits',async()=>{
  const s=setup();const r=await s.gate.operation({client:'ro',scopes:['gc:read']},'mail_send','city',{to:'x',subject:'Hi'});
  expect(r.structured.reason).toBe('no_write_scope');expect(s.posts).toHaveLength(0);
  const c=setup({confirm:async()=>{throw Error('offline');}});expect((await c.gate.operation(c.who,'mail_send','city',{to:'x',subject:'Hi'})).structured.reason).toBe('confirmation_failed');expect(c.posts).toHaveLength(0);
  const big=await s.gate.operation(s.who,'mail_send','city',{to:'x',subject:'Hi',body:'漢'.repeat(3000)});expect(big.structured.reason).toBe('bad_input');expect(s.posts).toHaveLength(0);
 });
 it('retains uncertain outcomes for retries without sending a second action',async()=>{
  let calls=0;const gc=new GcClient({baseUrl:'http://127.0.0.1:1',city:'c',fetchImpl:(async()=>{calls++;throw Error('connection lost after accept');})as typeof fetch});
  const events:any[]=[],gate=new WriteGate(gc,{enabled:true},e=>events.push(e)),who={client:'c',scopes:['gc:write']},body={to:'x',subject:'Hi'};
  const first=await gate.operation(who,'mail_send','city',body),retry=await gate.operation(who,'mail_send','city',body);
  expect(first.structured.reason).toBe('outcome_unknown');expect(retry.structured.replayed).toBe(true);expect(calls).toBe(1);expect(events.at(-1).ok).toBe(false);
 });
 it('keeps replay caches separate for distinct OAuth clients with identical names',async()=>{
  const s=setup(),body={to:'x',subject:'Hello'};
  for(const principal of ['oauth-a','oauth-b'])expect((await s.gate.operation({...s.who,client:'ChatGPT',principal},'mail_send','city',body)).structured.sent).toBe(true);
  expect(s.posts).toHaveLength(2);
 });
 it('deduplicates simultaneous retries, shares the rate limit and audits replay outcomes',async()=>{
  const s=setup({perMinute:1});const t=buildTools(s.deps,s.who).find(t=>t.name==='mail_send')!;const a={to:'crm/steve',subject:'Hello',body:'Hi'};
  const r=await Promise.all([t.handler(a),t.handler(a)]);expect(r.every(x=>x.structured.sent)).toBe(true);expect(s.posts).toHaveLength(1);
  const next=await t.handler({...a,subject:'Different'});expect(next.structured.reason).toBe('rate_limited');expect(s.posts).toHaveLength(1);
 });
});
