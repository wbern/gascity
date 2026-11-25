import {it,expect} from 'vitest';
import {GcClient,GcError} from '../src/gc-client.js';
import {waitTranscript} from '../src/wait-transcript.js';
const at=(n:number)=>new Date(n).toISOString();
const turn=(id:string,n:number,text=id)=>({id,role:'assistant',timestamp:at(n),text});
function fixture(pages:any[]){
 const queries:URL[]=[];
 const gc=new GcClient({baseUrl:'http://127.0.0.1:1',city:'gc2',fetchImpl:(async(url:any)=>{
  queries.push(new URL(url));const p=pages.shift();return new Response(JSON.stringify(p?.error?{detail:'cursor invalidated'}:p),{status:p?.error?409:200});
 })as typeof fetch});
 return {gc,queries};
}
it('re-reads the mutable tail with a fixed anchor instead of consuming candidates',async()=>{
 const old=[turn('a',1),turn('b',2),turn('c',3)];
 const f=fixture([{id:'s',turns:old},{id:'s',turns:[turn('c',3,'edited'),turn('d',11,'interim')]},{id:'s',turns:[turn('c',3),turn('d',11,'expanded'),turn('e',12,'later')]}]);
 const read=waitTranscript(f.gc,'s',10);
 await read(100);expect((await read(100)).map(t=>t.text)).toEqual(['edited','interim']);
 expect((await read(100)).map(t=>t.text)).toEqual(['c','expanded','later']);
 expect(f.queries.map(u=>u.searchParams.get('after'))).toEqual([null,'b','b']);
});
it('anchors before ALL fresh candidates rather than the last observed candidate',async()=>{
 const f=fixture([{id:'s',turns:[turn('a',1),turn('b',2),turn('c',11),turn('d',12)]},{id:'s',turns:[turn('b',2),turn('c',11,'edited interim'),turn('d',12),turn('e',13)]}]);
 const read=waitTranscript(f.gc,'s',10);await read(100);const rows=await read(100);
 expect(f.queries[1].searchParams.get('after')).toBe('a');expect(rows[1].text).toBe('edited interim');
});
it('starts a fresh snapshot for each wait/client and for a lost-response retry',async()=>{
 const snapshot={id:'s',turns:[turn('a',1),turn('b',2),turn('c',3)]},suffix={id:'s',turns:[turn('c',3)]};
 const f=fixture([snapshot,snapshot,suffix,suffix,snapshot]);
 const one=waitTranscript(f.gc,'s',10),two=waitTranscript(f.gc,'s',10);
 await one(100);await two(100);await one(100);await two(100);await waitTranscript(f.gc,'s',10)(100);
 expect(f.queries.map(u=>u.searchParams.get('after'))).toEqual([null,null,'b','b',null]);
});
it('keeps legacy snapshots when IDs are absent, duplicated or the relevant window begins at the start',async()=>{
 for(const rows of [[{text:'legacy',timestamp:at(1)}],[turn('same',1),turn('same',2)],[turn('a',11),turn('b',12)]]){
  const f=fixture([{id:'s',turns:rows},{id:'s',turns:rows}]);const read=waitTranscript(f.gc,'s',10);
  await read(100);await read(100);expect(f.queries[1].searchParams.has('after')).toBe(false);
 }
});
it('does not fall back to unrelated/latest output when the anchor is invalidated',async()=>{
 const f=fixture([{id:'s',turns:[turn('a',1),turn('b',2)]},{error:true}]);const read=waitTranscript(f.gc,'s',10);
 await read(100);await expect(read(100)).rejects.toMatchObject({status:409});expect(f.queries).toHaveLength(2);
});
it('refuses wrong targets, truncated forward pages and cursor echoes',async()=>{
 for(const page of [{id:'other',turns:[]},{id:'s',turns:[],pagination:{has_newer_messages:true}},{id:'s',turns:[turn('a',1)]}]){
  const f=fixture([{id:'s',turns:[turn('a',1),turn('b',2)]},page]);const read=waitTranscript(f.gc,'s',10);
  await read(100);await expect(read(100)).rejects.toBeInstanceOf(GcError);
 }
});
it('passes cancellation to the transport rather than leaving a cursor read running',async()=>{
 let aborted=false;
 const gc=new GcClient({baseUrl:'http://127.0.0.1:1',city:'gc2',fetchImpl:((_:any,init:any)=>new Promise((_,reject)=>{
  init.signal.addEventListener('abort',()=>{aborted=true;reject(new DOMException('aborted','AbortError'));});
 }))as typeof fetch});
 const ctl=new AbortController();const pending=waitTranscript(gc,'s',10)(100,ctl.signal);ctl.abort();
 await expect(pending).rejects.toBeInstanceOf(GcError);expect(aborted).toBe(true);
});

import {buildVoiceTools,replyId} from '../src/voice.js';
import {SendMarkers} from '../src/reply-state.js';
function voiceFixture(invalidate=false){
 let now=100,reads=0;const queries:URL[]=[];
 const old=[turn('a',1),turn('b',2),turn('c',3)],fresh=[turn('d',101,"I'll check"),turn('e',102,'Result candidate')];
 const gc=new GcClient({baseUrl:'http://127.0.0.1:1',city:'gc2',fetchImpl:(async(url:any)=>{
  const u=new URL(url);queries.push(u);let value:any;
  if(u.pathname.endsWith('/sessions'))value={items:[{id:'fixed',alias:'helper'}],total:1};
  else if(u.pathname.endsWith('/transcript')){
   if(invalidate&&u.searchParams.has('after'))return new Response(JSON.stringify({detail:'cursor invalidated'}),{status:409});
   value={id:'fixed',turns:reads++===0?old:u.searchParams.has('after')?[old[2],...fresh]:[...old,...fresh]};
  }else value={id:'fixed',last_active:String(now),activity:'working'};
  return new Response(JSON.stringify(value));
 })as typeof fetch});
 const markers=new SendMarkers({now:()=>now});const handle=markers.create('client-a','fixed',100);
 const d={gc,markers,mailAgent:'human',now:()=>new Date(now),clock:{now:()=>now,sleep:async(ms:number)=>{now+=ms;}}};
 const tool=(client:string,name='wait_for_reply')=>buildVoiceTools(d,{client:'same display',principal:client,scopes:['gc:read']}).find(t=>t.name===name)!;
 return {tool,handle,queries};
}
it('preserves first-candidate retry, explicit successor and cross-tool reply identity through optimized polling',async()=>{
 const f=voiceFixture(),wait=f.tool('client-a');
 const first=await wait.handler({handle:f.handle,max_seconds:3});
 expect(first.structured).toMatchObject({page:"I'll check",complete:false,fetches:2});
 expect(f.queries.filter(u=>u.pathname.endsWith('/transcript')).map(u=>u.searchParams.get('after'))).toEqual([null,'b']);
 const retry=await wait.handler({handle:f.handle,max_seconds:3});expect(retry.structured.reply_id).toBe(first.structured.reply_id);
 const next=await wait.handler({handle:f.handle,after_reply_id:first.structured.reply_id,max_seconds:3});
 expect(next.structured.page).toBe('Result candidate');
 const more=await f.tool('client-a','read_more').handler({session:'fixed',reply_id:first.structured.reply_id,offset:0});
 expect(more.structured.reply_id).toBe(replyId(at(101),"I'll check"));expect(more.structured.page).toBe("I'll check");
 const count=f.queries.length;const other=await f.tool('client-b').handler({handle:f.handle,max_seconds:3});
 expect(other.structured.reason).toBe('unknown_handle');expect(f.queries).toHaveLength(count);
});
it('reports cursor loss without a fallback read or completion claim',async()=>{
 const f=voiceFixture(true);const r=await f.tool('client-a').handler({handle:f.handle,max_seconds:3});
 expect(r.structured).toMatchObject({reason:'reply_cursor_unavailable',complete:false,reply_available:false});
 expect(f.queries.filter(u=>u.pathname.endsWith('/transcript'))).toHaveLength(2);
});
