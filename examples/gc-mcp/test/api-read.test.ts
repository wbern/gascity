import { describe, it, expect } from 'vitest';
import fs from 'node:fs';
import { execFileSync } from 'node:child_process';
import { GcClient } from '../src/gc-client.js';
import { buildTools } from '../src/tools.js';
const spec = JSON.parse(fs.readFileSync(new URL('../../../internal/api/openapi.json', import.meta.url), 'utf8'));
const calls: URL[] = [];
const gc = new GcClient({baseUrl:'http://127.0.0.1:1',city:'c',fetchImpl: (async (u: string, init: RequestInit) => {
  expect(init.method).toBe('GET'); calls.push(new URL(u));
  return new Response(JSON.stringify({items:[{id:'b1',title:'hello',secret:'sensitive',description:'\"\\'.repeat(4500)}],total:42,next_cursor:'upstream-next',partial:true}));
}) as typeof fetch});
const deps={gc,mailAgent:'human',now:()=>new Date()};
const tools = (client='alice') => buildTools(deps, {client,scopes:['gc:read']});
const call = (name:string,a:any={},client='alice') => tools(client).find(t=>t.name===name)!.handler(a);
describe('OpenAPI read projection',()=>{
 it('generated catalogue stays synchronized with the checked-in API contract',()=>{
  expect(()=>execFileSync(process.execPath,['scripts/gen-read-catalog.mjs','--check'])).not.toThrow();
 });
 it('supports repeated query parameters and preserves their values',async()=>{
  await call('gc_api_read',{operation:'get-v0-city-by-city-name-work-active-step',query:{assignee:['crm/steve','gc-architect']}});
  expect(calls.at(-1)!.searchParams.getAll('assignee')).toEqual(['crm/steve','gc-architect']);
 });
 it('raw reads cannot bypass optional session read restrictions',async()=>{
  const restricted=buildTools({...deps,readDeny:['protected-*']},{client:'alice',scopes:['gc:read']});
  const n=calls.length;const r=await restricted.find(t=>t.name==='gc_api_read')!.handler({operation:'get-v0-city-by-city-name-sessions'});
  expect(r.structured.error).toBe(true);expect(calls.length).toBe(n);
 });
 it('catalogue binds every advertised GET to the real specification and excludes sensitive and streaming routes',async()=>{
  const ops:any[]=[];let offset:any=0;
  do {const r=await call('gc_api_list_operations',{limit:100,offset});expect(JSON.stringify(r.structured).length).toBeLessThanOrEqual(4096);ops.push(...r.structured.operations as any[]);offset=r.structured.next_offset;}while(offset!==undefined);
  expect(ops.length).toBeGreaterThan(30);
  for(const op of ops){expect(spec.paths['/v0/city/{cityName}'+op.path].get.operationId).toBe(op.operation);expect(op.path).not.toMatch(/config|patches|provider|extmsg|stream/);}
 });
 it('validates operation, path components and query names before sending anything',async()=>{
  for(const a of [{operation:'submit-session'},{operation:'get-v0-city-by-city-name-config'},{operation:'get-v0-city-by-city-name-bead-by-id',path:{id:'../config'}},{operation:'get-v0-city-by-city-name-beads',query:{nonsense:'yes'}}]){
   const n=calls.length;const r=await call('gc_api_read',a);expect(r.structured.error).toBe(true);expect(calls.length).toBe(n);
  }
 });
 it('passes query and upstream paging metadata; redacts before returning bounded cursor pages',async()=>{
  let r=await call('gc_api_read',{operation:'get-v0-city-by-city-name-beads',query:{rig:'gas-city-wbern',limit:2}});
  expect(calls.at(-1)!.searchParams.get('rig')).toBe('gas-city-wbern');
  expect(JSON.stringify(r.structured).length).toBeLessThanOrEqual(4096);
  let joined=String(r.structured.page);let cursor=r.structured.next_cursor;
  expect(cursor).toBeTypeOf('string');
  const alien=await call('gc_api_read',{cursor},'bob');expect(alien.structured.error).toBe(true);
  while(cursor){r=await call('gc_api_read',{cursor});joined+=r.structured.page;cursor=r.structured.next_cursor;}
  const data=JSON.parse(joined);expect(data).toMatchObject({total:42,partial:true});
  await call('gc_api_read',{operation:'get-v0-city-by-city-name-beads',query:{cursor:data.next_cursor}});expect(calls.at(-1)!.searchParams.get('cursor')).toBe('upstream-next');
  expect(data.items[0].secret).toBe('[redacted]');expect(joined).not.toContain('sensitive');
 });
 it('projects only requested top-level fields without inventing summaries',async()=>{
  const r=await call('gc_api_read',{operation:'get-v0-city-by-city-name-beads',fields:['total','partial','next_cursor']});
  const data=JSON.parse(String(r.structured.page));expect(data).toMatchObject({total:42,partial:true});expect(data.next_cursor).toMatch(/^[0-9a-f-]{36}$/);
 });
});
