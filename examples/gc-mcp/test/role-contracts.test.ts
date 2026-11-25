import {describe,it,expect} from 'vitest';
import fs from 'node:fs';
import crypto from 'node:crypto';
import {ROLE_PROFILE} from '../src/role-contracts-data.js';
import {Client} from '@modelcontextprotocol/sdk/client/index.js';
import {InMemoryTransport} from '@modelcontextprotocol/sdk/inMemory.js';
import {createMcpServer} from '../src/server.js';
import {GcClient} from '../src/gc-client.js';
import {resolveGuideProfile} from '../src/role-contracts.js';
import {publicBinding,verifyRole} from '../scripts/role-verification.mjs';
import {buildTools} from '../src/tools.js';
function guide(items:any[],profile?:string){const gc=new GcClient({baseUrl:'http://127.0.0.1:1',city:'gc2',fetchImpl:(async()=>new Response(JSON.stringify({items,total:items.length})))as typeof fetch});return buildTools({gc,mailAgent:'human',now:()=>new Date(),guideProfile:profile}as any,{client:'owner',scopes:['gc:read']}).find(t=>t.name==='city_guide')!;}
describe('verified role contracts',()=>{
 it('delivers curated standards separately from observations over real MCP transport',async()=>{
  const gc=new GcClient({baseUrl:'http://127.0.0.1:1',city:'gc2',fetchImpl:(async()=>new Response(JSON.stringify({items:[{name:'crm/steve',pack_derived:true}],total:1})))as typeof fetch});
  const server=createMcpServer({gc,mailAgent:'human',now:()=>new Date(),guideProfile:'gc3-2026-11-22'}),client=new Client({name:'role-guide-test',version:'1'});
  const[a,b]=InMemoryTransport.createLinkedPair();await Promise.all([server.connect(b),client.connect(a)]);
  try{const r=await client.callTool({name:'city_guide',arguments:{topic:'roles',agent:'steve'}});expect((r.structuredContent as any).role_contract).toMatchObject({id:'crm-developer'});expect((r.structuredContent as any).observation.untrusted).toBe(true);expect((r.structuredContent as any).observation.role_contract).toBeUndefined();expect(Buffer.byteLength(JSON.stringify(r.structuredContent))).toBeLessThanOrEqual(4096);}finally{await client.close();await server.close();}
 });
 it('pins curated definitions and explicitly identifies subset verification',()=>{
  const raw=fs.readFileSync(new URL('../guide/role-contracts.source.json',import.meta.url));expect(crypto.createHash('sha256').update(raw).digest('hex')).toBe(ROLE_PROFILE.definition_sha256);
  expect(ROLE_PROFILE.contracts.find(c=>c.id==='crm-code-review')!.verification.full_template_match).toBe(true);
  expect(ROLE_PROFILE.contracts.find(c=>c.id==='crm-developer')!.verification.full_template_match).toBe(false);
 });
 it('rejects an unknown profile or wrong city without changing default read/write authority',()=>{
  expect(resolveGuideProfile(undefined,'anything')).toBeUndefined();expect(resolveGuideProfile('gc3-2026-11-22','gc2','clodcod')).toBe('gc3-2026-11-22');
  expect(()=>resolveGuideProfile('unknown','gc2','clodcod')).toThrow();expect(()=>resolveGuideProfile('gc3-2026-11-22','gc2','other-host')).toThrow();expect(()=>resolveGuideProfile('gc3-2026-11-22','other')).toThrow();
 });
 it('verifies selected source clauses and public configuration, while excluding private additions',()=>{
  const def={selector:{rig:'crm',agent:'steve'},template:'agents/steve/prompt.template.md',standards:[{anchor:'use assigned work'}]};
  const fields=publicBinding(' name = steve # pack.toml\n dir = crm # pack.toml\n prompt_template = agents/steve/prompt.template.md # pack.toml\n secret = PRIVATE # local\n');
  expect(fields).not.toHaveProperty('secret');const v=verifyRole(def,fields,'Use assigned work','Use assigned work\nPRIVATE ADDITION');expect(v).toMatchObject({public_fields_verified:true,full_template_match:false,verified_clause_count:1});expect(JSON.stringify(v)).not.toContain('PRIVATE ADDITION');
  expect(()=>verifyRole(def,{...fields,prompt_template:'other'},'use assigned work','use assigned work')).toThrow();
  expect(()=>verifyRole(def,fields,'use assigned work','different instructions')).toThrow();
 });
 it('exposes concrete standards for a configured identity only with an enabled operator profile',async()=>{
  const items=[{name:'crm/steve',pack_derived:true,running:true}];
  const disabled=await guide(items).handler({topic:'roles',agent:'steve'});expect((disabled.structured.observation as any).agents[0].purpose).toBe('unknown');
  const r=await guide(items,'gc3-2026-11-22').handler({topic:'roles',agent:'steve'});
  expect((r.structured.observation as any).agents[0].purpose).toBe('CRM developer');
  expect((r.structured as any).role_contract).toMatchObject({id:'crm-developer',verification:{public_fields_verified:true}});
  expect(JSON.stringify((r.structured as any).role_contract.standards)).toMatch(/worktree/);
  expect(Buffer.byteLength(JSON.stringify(r.structured))).toBeLessThanOrEqual(4096);
 });
 it('describes a verified implementation profile without inventing membership or assignment',async()=>{
  const r=await guide([{name:'gas-city-wbern/pool.worker-default-codex',pack:'pool',pack_derived:true}],'gc3-2026-11-22').handler({topic:'roles',agent:'gas-city-wbern/pool.worker-default-codex'});
  expect((r.structured.observation as any).agents[0]).toMatchObject({purpose:'Scoped implementation worker',membership:'not_reported',persistence:'unknown'});
  expect((r.structured.role_contract as any).id).toBe('gc-implementation-worker');expect(Buffer.byteLength(JSON.stringify(r.structured))).toBeLessThanOrEqual(4096);
 });
 it('makes registered roles discoverable without knowing an agent name first',async()=>{
  const r=await guide([{name:'crm/steve',pack_derived:true},{name:'crm/architect',pack_derived:true},{name:'gas-city-infra/devops',pack_derived:true},{name:'crm/gastown.reviewer',pool:'crm/gastown.reviewer',pack:'gastown',pack_derived:true},{name:'gas-city-wbern/pool.worker-default-codex',pack:'pool',pack_derived:true}],'gc3-2026-11-22').handler({topic:'roles'});
  expect(r.structured.role_catalog).toBeDefined();expect((r.structured.role_catalog as any).contracts).toHaveLength(5);expect((r.structured.role_catalog as any).contracts).toContainEqual({id:'crm-code-review',purpose:'Exact-head read-only CRM review',target:'crm/gastown.reviewer'});
  expect((r.structured.observation as any).role_catalog).toBeUndefined();expect(Buffer.byteLength(JSON.stringify(r.structured))).toBeLessThanOrEqual(4096);
 });
 it('does not infer a role from a familiar name when configuration identity does not match',async()=>{
  const r=await guide([{name:'crm/steve',pack_derived:false,role:'developer'}],'gc3-2026-11-22').handler({topic:'roles',agent:'steve'});
  expect((r.structured.observation as any).agents[0].purpose).toBe('unknown');expect((r.structured as any).role_contract).toBeUndefined();
 });
 it('does not claim one pool contract across conflicting configuration identities',async()=>{
  const r=await guide([{name:'crm/gastown.reviewer-1',pool:'crm/gastown.reviewer',pack:'gastown',pack_derived:true},{name:'other',pool:'crm/gastown.reviewer',pack:'different',pack_derived:true}],'gc3-2026-11-22').handler({topic:'roles',agent:'crm/gastown.reviewer'});expect(r.structured.role_contract).toBeUndefined();
 });
 it('reads a verified pool contract without pretending to choose a member for a send',async()=>{
  const r=await guide(Array.from({length:8},(_,i)=>({name:'crm/gastown.reviewer-'+i,pool:'crm/gastown.reviewer',pack:'gastown',pack_derived:true})),'gc3-2026-11-22').handler({topic:'roles',agent:'crm/gastown.reviewer'});
  expect((r.structured.observation as any).target_kind).toBe('pool');expect((r.structured as any).role_contract.id).toBe('crm-code-review');
  expect(JSON.stringify((r.structured as any).role_contract.standards)).toMatch(/read-only|readback/);expect(Buffer.byteLength(JSON.stringify(r.structured))).toBeLessThanOrEqual(4096);
 });
});
