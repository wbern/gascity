import {describe,it,expect} from 'vitest';
import {GcClient} from '../src/gc-client.js';
import {WriteGate} from '../src/writes.js';
import {resolveSession} from '../src/sessions.js';

describe('session name resolution needs a complete list',()=>{
 for(const mode of ['partial','total-mismatch','page-cap'])it(`refuses submit and respond on ${mode}, even with one visible exact match`,async()=>{
  let posts=0,reads=0;
  const gc=new GcClient({baseUrl:'http://127.0.0.1:1',city:'c',fetchImpl:(async(url:string,init:RequestInit)=>{
   if(init.method==='POST'){posts++;return new Response('{}');}
   reads++;const page=Number(new URL(url).searchParams.get('cursor')??0);
   const item=page===0?{id:'s1',alias:'steve'}:page===5?{id:'s6',alias:'steve'}:{id:'s'+(page+1),alias:'other'+page};
   return new Response(JSON.stringify({items:[item],total:mode==='page-cap'?6:mode==='total-mismatch'?2:1,partial:mode==='partial',next_cursor:mode==='page-cap'&&page<5?String(page+1):undefined}));
  })as typeof fetch});
  const gate=new WriteGate(gc,{enabled:true},()=>{}),who={client:'owner',scopes:['gc:write']};
  for(const result of [await gate.submit(who,'steve','hello'),await gate.respond(who,'steve','allow')])expect(result.structured).toMatchObject({sent:false,reason:'incomplete_sessions'});
  expect(posts).toBe(0);expect(reads).toBe(mode==='page-cap'?10:2);
 });
 it('still resolves an exact name after fetching a complete paginated list',async()=>{
  const gc=new GcClient({baseUrl:'http://127.0.0.1:1',city:'c',fetchImpl:(async(url:string)=>new Response(JSON.stringify(new URL(url).searchParams.has('cursor')?{items:[{id:'s2',alias:'bob'}],total:2}:{items:[{id:'s1',alias:'steve'}],total:2,next_cursor:'next'})))as typeof fetch});
  expect(await resolveSession(gc,'steve')).toMatchObject({kind:'one',hit:{id:'s1'}});
 });
});
