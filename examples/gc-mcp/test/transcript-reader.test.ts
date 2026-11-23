import {it,expect} from 'vitest';
import {buildTools,type Deps} from '../src/tools.js';
import {GcClient} from '../src/gc-client.js';
import {replyId} from '../src/voice.js';
import {clean} from '../src/text.js';
import {redactText} from '../src/redact.js';
const row=(id:string,text:string)=>({id,role:'assistant',text,timestamp:'2026-11-23T12:48:07Z'});
function world(rows:any[]){
 let now=1800000000000;const calls:URL[]=[];const sessions=[{id:'s1',alias:'helper',state:'active'}];
 const state={rows};
 const gc=new GcClient({baseUrl:'http://127.0.0.1:1',city:'gc2',fetchImpl:(async(url:any)=>{
  const u=new URL(url);calls.push(u);
  if(u.pathname.endsWith('/sessions'))return Response.json({items:sessions,total:sessions.length});
  const before=u.searchParams.get('before');if(before&&!state.rows.some(r=>r.id===before))return Response.json({detail:'cursor invalidated'},{status:409});const source=before?state.rows.slice(0,state.rows.findIndex(r=>r.id===before)):state.rows;
  return Response.json({id:'s1',turns:source,pagination:{has_older_messages:false,total_message_count:source.length,returned_message_count:source.length}});
 })as typeof fetch});
 const d:Deps={gc,mailAgent:'human',now:()=>new Date(now),defaultSession:'helper'};
 const tool=(principal='a',deps=d)=>{const t=buildTools(deps,{client:'same display',principal,scopes:['gc:read']}).find(t=>t.name==='read_session');expect(t,'faithful reader is discoverable').toBeDefined();return t!;};
 return {tool,d,calls,state,sessions,advance:()=>{now+=300001;}};
}
it('reads a long report once without flattening, clipping links or inventing summaries',async()=>{
 const text='# PR #2740 — show/hide Övrigt column\n\n- Diagnosis confidence: moderate.\n- Evidence: https://example.com/pr/2740\n\n```text\n  keep whitespace\n```\n'+('Useful evidence. '.repeat(300));
 const w=world([row('native-entry',text)]);const r=await w.tool().handler({last:1});
 expect(r.structured.messages).toMatchObject([{text,source_entry_id:'native-entry',message_end:true}]);
 expect(r.structured).toMatchObject({snapshot_end:true,completion:'not_proven',projection:'conversation',transformation:'secret_redaction_only'});
 expect((r.structured.terminal as any).argv).toEqual(['gc','session','logs','s1','--tail','0']);
 expect(r.text.length).toBeLessThanOrEqual(280);expect(Buffer.byteLength(JSON.stringify(r.structured))).toBeLessThanOrEqual(16384);
});
it('splits only oversized messages, reconstructs Unicode/escaped text and redacts before slicing',async()=>{
 for(const page_bytes of [8192,16384,32768]){
  const text=('漢字 😀 "\\\n'.repeat(7000))+'\nBearer abcdefghijklmnopqrstuvwxyz012345';const w=world([row('e',text)]);let r=await w.tool().handler({last:1,page_bytes});let combined='',count=0;
  for(;;){expect(Buffer.byteLength(JSON.stringify(r.structured))).toBeLessThanOrEqual(page_bytes);
   for(const m of r.structured.messages as any[]){expect(m.offset).toBe(combined.length);expect(m.text).not.toMatch(/[\uD800-\uDBFF]$/);combined+=m.text;}
   if(r.structured.snapshot_end)break;const args=(r.structured.continue_read as any).arguments;r=await w.tool().handler(args);if(++count>100)throw Error('pagination stalled');
  }
  expect(combined).toBe(redactText(text));expect(combined).not.toContain('abcdefghijklmnopqrstuvwxyz012345');
 }
});
it('pins snapshot pages across transcript growth/compaction and lost response retries',async()=>{
 const text='old passage '.repeat(4000);const w=world([row('e',text)]);const first=await w.tool().handler({last:1,page_bytes:8192});const args=(first.structured.continue_read as any).arguments;
 w.state.rows=[row('new','different conversation segment')];const a=await w.tool().handler(args),b=await w.tool().handler(args);
 expect(a).toEqual(b);expect(JSON.stringify(a)).not.toContain('different conversation segment');
 expect(w.calls.filter(u=>u.pathname.endsWith('/transcript'))).toHaveLength(1);
});
it('keeps cursors private to actual client IDs, expires them and loses them honestly on restart',async()=>{
 const w=world([row('e','long '.repeat(9000))]);const first=await w.tool().handler({page_bytes:8192});const args=(first.structured.continue_read as any).arguments;
 expect((await w.tool('b').handler(args)).structured.reason).toBe('cursor_unavailable');
 expect((await w.tool('a',{...w.d}).handler(args)).structured.reason).toBe('cursor_unavailable');
 w.advance();expect((await w.tool().handler(args)).structured.reason).toBe('cursor_unavailable');
});
it('rechecks current access and exact session identity before returning cached pages',async()=>{
 const w=world([row('e','long '.repeat(9000))]);const first=await w.tool().handler({page_bytes:8192});const args=(first.structured.continue_read as any).arguments;
 w.d.readDeny=['helper'];expect((await w.tool().handler(args)).structured.reason).toBe('denied_session');
 w.d.readDeny=[];w.sessions.splice(0,1,{id:'replacement',alias:'s1',state:'active'});
 expect((await w.tool().handler(args)).structured.reason).toBe('snapshot_target_unavailable');
});
it('preserves window and message boundaries, and provides a client-bound older-history read',async()=>{
 const w=world([row('a','older'),row('b','middle'),row('c','latest')]);const r=await w.tool().handler({last:1});
 expect(r.structured.messages).toMatchObject([{text:'latest',message_end:true}]);expect((r.structured.history as any).omitted_in_segment).toBe(2);
 const older=await w.tool().handler((r.structured.older_read as any).arguments);
 expect(older.structured.messages).toMatchObject([{text:'middle'}]);expect(w.calls.at(-1)?.searchParams.get('before')).toBe('c');
 expect((await w.tool('b').handler((r.structured.older_read as any).arguments)).structured.reason).toBe('cursor_unavailable');
});
it('discloses missing native IDs and rejects conflicting continuation arguments',async()=>{
 const w=world([{role:'assistant',text:'no stable native id'}]);const r=await w.tool().handler({});
 expect(r.structured.older_read).toBeUndefined();expect((r.structured.messages as any[])[0].source_entry_id).toBeUndefined();
 const l=world([row('e','x'.repeat(40000))]);const first=await l.tool().handler({});const cursor=(first.structured.continue_read as any).arguments.cursor;
 expect((await l.tool().handler({cursor,session:'other'})).structured.reason).toBe('conflicting_cursor');
});

it('reads the selected older reply faithfully and never substitutes a newer reply after compaction',async()=>{
 const original='## Evidence\n\nPR #2740 — show/hide Övrigt column. https://example.com/pr/2740';
 const w=world([row('a',original),row('b','newer')]);const id=replyId('2026-11-23T12:48:07Z',clean(original));
 const selected=await w.tool().handler({reply_id:id});expect(selected.structured.messages).toMatchObject([{text:original}]);
 w.state.rows=[row('b','newer')];const missing=await w.tool().handler({reply_id:id});expect(missing.structured.reason).toBe('reply_unavailable');expect(missing.structured.messages).toBeUndefined();
});

it('evicts bounded snapshots explicitly instead of silently replacing an old passage',async()=>{
 const w=world([row('e','content '.repeat(7000))]);const first=await w.tool().handler({page_bytes:8192});const args=(first.structured.continue_read as any).arguments;
 for(let i=0;i<16;i++)await w.tool().handler({page_bytes:8192});
 expect((await w.tool().handler(args)).structured.reason).toBe('cursor_unavailable');
});

it('reports an invalidated older-history boundary separately from an empty/end snapshot',async()=>{
 const w=world([row('a','older'),row('b','latest')]);const first=await w.tool().handler({last:1});const args=(first.structured.older_read as any).arguments;
 w.state.rows=[row('replacement','compacted segment')];const gone=await w.tool().handler(args);
 expect(gone.structured).toMatchObject({found:false,reason:'history_unavailable'});expect(gone.structured.snapshot_end).toBeUndefined();expect(gone.structured.messages).toBeUndefined();
});
