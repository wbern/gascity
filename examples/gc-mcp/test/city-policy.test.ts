import {it,expect} from 'vitest';
import fs from 'node:fs';
import crypto from 'node:crypto';
import {buildGuideTools} from '../src/guide.js';
import {GcClient} from '../src/gc-client.js';
import {ROLE_PROFILE} from '../src/role-contracts-data.js';
import {CITY_POLICY} from '../src/city-policy-data.js';
const tool=(profile:string|undefined=ROLE_PROFILE.id)=>buildGuideTools({gc:new GcClient({baseUrl:'http://127.0.0.1:1',city:'gc2',fetchImpl:(async()=>{throw Error('static policy must not query runtime/config');})as typeof fetch}),guideProfile:profile,mailAgent:'human',now:()=>new Date()},{client:'owner',scopes:['gc:read']})[0];
it('distinguishes the verified deployment host/label from a shared internal city name',async()=>{
 const r=await tool().handler({topic:'overview'});
 expect(r.structured.connection).toMatchObject({verified:true,operator_city:'GC3',connector_host:'clodcod',api_city:'gc2'});
 expect(r.text).toContain('GC3 on clodcod');expect(r.text).toContain('API city gc2');
});
it('does not infer host or city policy from an absent or unknown deployment profile',async()=>{
 for(const profile of ['', 'unknown-profile']){
  const r=await tool(profile).handler({topic:'work'});
  expect(r.structured.connection).toMatchObject({verified:false});expect(r.structured.city_policy).toBeUndefined();
  expect(r.text).not.toContain('Connected to GC3');
 }
});
it('separates core mechanics from source-backed city review custody and selector constraints',async()=>{
 const r=await tool().handler({topic:'work',repository:'wbern/gascity',review_context:'code-review'});
 expect(r.structured.rules_scope).toMatch(/core mechanics/);
 const p=r.structured.city_policy as any;
 expect(p.origin).toBe('installed_city_policy_snapshot');
 expect(p.review.lanes).toEqual([{repository:'wbern/gascity',review_context:'code-review',owner_city:'gc3',owner_order:'gas-city-wbern-pr-review'}]);
 expect(p.dispatch.selector).toBe('worker-level-sling.py');expect(p.dispatch.transport_boundary).toMatch(/does not execute/);
 expect(p.sources.every((s:any)=>s.repository==='wbern/gas-city-infra'&&/^[a-f0-9]{40}$/.test(s.revision)&&/^[a-f0-9]{64}$/.test(s.sha256))).toBe(true);
});
it('preserves distinct code and UX review owners and refuses guessed repository/context fallbacks',async()=>{
 const code=await tool().handler({topic:'work',repository:'wbern/crm',review_context:'code-review'});
 const ux=await tool().handler({topic:'work',repository:'wbern/crm',review_context:'ux-review'});
 expect((code.structured.city_policy as any).review.lanes[0].owner_order).toBe('crm-pr-review');
 expect((ux.structured.city_policy as any).review.lanes[0].owner_order).toBe('crm-ux-review');
 for(const query of [{repository:'other/repo'},{repository:'wbern/crm',review_context:'invented'}]){
  const r=await tool().handler({topic:'work',...query});expect((r.structured.city_policy as any).review).toMatchObject({status:'unknown_repository_context',lanes:[]});
 }
});
it('does not advertise unverified provider defaults as installed committed policy',async()=>{
 const r=await tool().handler({topic:'work'}),p=r.structured.city_policy as any;
 expect(CITY_POLICY.provider_defaults_verified).toBe(false);
 expect(p.dispatch.provider_defaults).toBe('unverified_installed_source_drift');
 expect(JSON.stringify(r)).not.toMatch(/default_provider|model_id|api_key|raw_prompt/);
});
it('keeps all topics and filtered policy within the 4096-byte and spoken budgets',async()=>{
 for(const args of [{topic:'overview'},{topic:'work'},{topic:'work',repository:'wbern/crm'},{topic:'operations'}]){
  const r=await tool().handler(args);expect(Buffer.byteLength(JSON.stringify(r.structured))).toBeLessThanOrEqual(4096);expect(r.text.length).toBeLessThanOrEqual(280);
 }
});

it('requires review/regeneration when curated city policy definitions change',()=>{
 const raw=fs.readFileSync(new URL('../guide/city-policy.source.json',import.meta.url));
 expect(crypto.createHash('sha256').update(raw).digest('hex')).toBe(CITY_POLICY.definition_sha256);
});
