// Reproducible synthetic call/byte comparison, NOT phone/model latency evidence.
import assert from 'node:assert/strict';
import {buildTools} from '../dist/tools.js';
import {GcClient} from '../dist/gc-client.js';
import {redactText} from '../dist/redact.js';
const reports=[['short report',('PR #2740 — show/hide Övrigt column.\nEvidence https://example.com/pr/2740\n').repeat(60)],['long report',('## Evidence\n\nPR #2774 — review packet infrastructure\n- Preserve this paragraph and exact identifier.\n').repeat(320)],['Unicode report',('漢字 😀 — quoted evidence\n  "keep formatting"\n').repeat(400)]];
function fixture(text){let reads=0;const gc=new GcClient({baseUrl:'http://127.0.0.1:1',city:'gc2',fetchImpl:async url=>{reads++;return Response.json(new URL(url).pathname.endsWith('/sessions')?{items:[{id:'s1',alias:'fixture'}],total:1}:{id:'s1',turns:[{id:'e1',role:'assistant',text,timestamp:'2026-11-23T12:48:07Z'}]});}});const d={gc,mailAgent:'human',defaultSession:'fixture',now:()=>new Date()};return {tool:name=>buildTools(d,{client:'fixture',scopes:['gc:read']}).find(t=>t.name===name),reads:()=>reads};}
const results=[];
for(const[label,text]of reports){
 const old=fixture(text);let r=await old.tool('latest_reply').handler({}),oldCalls=1;
 while(!r.structured.done){r=await old.tool('read_more').handler({session:'fixture',offset:r.structured.next_offset,reply_id:r.structured.reply_id});assert(++oldCalls<1000);}
 const variants=[];
 for(const page_bytes of [8192,16384,32768]){
  const f=fixture(text);let r=await f.tool('read_session').handler({last:1,page_bytes}),calls=0,bytes=0,combined='';
  for(;;){calls++;const n=Buffer.byteLength(JSON.stringify(r.structured));assert(n<=page_bytes);bytes+=n;combined+=r.structured.messages.map(m=>m.text).join('');if(r.structured.snapshot_end)break;r=await f.tool('read_session').handler(r.structured.continue_read.arguments);assert(calls<1000);}
  assert.equal(combined,redactText(text));variants.push({page_bytes,tool_calls:calls,upstream_reads:f.reads(),structured_bytes:bytes});
 }
 results.push({case:label,original_utf8_bytes:Buffer.byteLength(text),legacy_voice_tool_calls:oldCalls,legacy_upstream_reads:old.reads(),faithful:variants});
}
console.log(JSON.stringify({kind:'synthetic_read_budget_benchmark',phone_or_model_run:false,latency_measured:false,results},null,2));
