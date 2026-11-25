import {it,expect} from "vitest";
import {GcClient} from "../src/gc-client.js";
import {SendMarkers} from "../src/writes.js";
import {buildTools} from "../src/tools.js";
it("continues the selected handle candidate when a newer reply already exists",async()=>{
 const base=1800000000000,markers=new SendMarkers(),handle=markers.create("owner","s1",base);
 const long="The first long reply continues with useful words. ".repeat(25);
 const turns=[{role:"assistant",text:long,timestamp:new Date(base+10).toISOString()},{role:"assistant",text:"A newer reply exists too.",timestamp:new Date(base+20).toISOString()}];
 const gc=new GcClient({baseUrl:"http://127.0.0.1:1",city:"c",fetchImpl:(async u=>{
  const p=new URL(String(u)).pathname;
  return new Response(JSON.stringify(p.endsWith("/sessions")?{items:[{id:"s1",alias:"agent",state:"active"}],total:1}:p.endsWith("/transcript")?{turns}:{id:"s1",state:"active",running:true}));
 })as typeof fetch});
 const tools=buildTools({gc,markers,mailAgent:"human",now:()=>new Date(base+30),clock:{now:()=>base+30,sleep:async()=>{}}},{client:"owner",scopes:["gc:read"]});
 const first=await tools.find(t=>t.name==="wait_for_reply")!.handler({handle,max_seconds:1});
 expect(first.structured.reply_available).toBe(true);expect(first.structured.done).toBe(false);
 const more=await tools.find(t=>t.name==="read_more")!.handler({session:"agent",reply_id:first.structured.reply_id,offset:first.structured.next_offset});
 expect(more.structured.changed).not.toBe(true);
 expect(more.structured.page).toContain("first long reply");
 const pieces=[String(first.structured.page),String(more.structured.page)];
 let page=more;
 for(let n=0;!page.structured.done;n++){
  expect(n).toBeLessThan(30);
  page=await tools.find(t=>t.name==="read_more")!.handler({session:"agent",reply_id:first.structured.reply_id,offset:page.structured.next_offset});
  expect(page.structured.reply_id).toBe(first.structured.reply_id);
  expect(page.structured.changed).not.toBe(true);
  pieces.push(String(page.structured.page));
 }
 expect(pieces.join(" ")).toBe(long.trim());
 const latest=await tools.find(t=>t.name==="read_more")!.handler({session:"agent",offset:0});
 expect(latest.structured.page).toBe("A newer reply exists too.");
 turns.shift();
 const missing=await tools.find(t=>t.name==="read_more")!.handler({session:"agent",reply_id:first.structured.reply_id,offset:first.structured.next_offset});
 expect(missing.structured).toMatchObject({changed:true,reason:"reply_unavailable",reply_id:first.structured.reply_id});
 expect(missing.structured.page).toBeUndefined();
 turns.length=0;
 const gone=await tools.find(t=>t.name==="read_more")!.handler({session:"agent",reply_id:first.structured.reply_id,offset:0});
 expect(gone.structured).toMatchObject({reason:"reply_unavailable"});
 expect(gone.structured.done).not.toBe(true);
});
