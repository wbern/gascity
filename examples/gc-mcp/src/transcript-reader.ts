// Faithful conversation projection. Cached text is redacted, ephemeral and principal-bound.
import crypto from 'node:crypto';
import {z} from 'zod';
import {GcError,TRANSCRIPT_MAX_BYTES} from './gc-client.js';
import {connectionIdentity} from './city-policy.js';
import {redactText} from './redact.js';
import {clean,clip,guard,type ToolResult} from './text.js';
import {target,replyId} from './voice.js';
import type {Deps,ToolDef} from './tools.js';
import type {WriteCtx} from './writes.js';
const TTL=300_000,MAX_SNAPSHOTS=16,MAX_CACHE_BYTES=16_000_000;
const RO={readOnlyHint:true,destructiveHint:false,openWorldHint:false}as const;
type Entry={native?:string;ref:string;role:string;at?:string;text:string};
type Position={index:number;offset:number};
type Snapshot={owner:string;session:string;name:string;observed:string;expires:number;bytes:number;budget:number;last:number;omitted:number;older:string;entries:Entry[];tokens:Map<string,Position>;positions:Map<string,string>};
const stores=new WeakMap<Deps,Map<string,Snapshot>>();
function cache(d:Deps){let m=stores.get(d);if(!m){m=new Map();stores.set(d,m);}for(const[k,s]of m)if(s.expires<=d.now().getTime())m.delete(k);return m;}
const unavailable=():ToolResult=>({text:'That transcript reference expired or is unavailable for this connection. Start a fresh read; no output was substituted.',structured:{found:false,reason:'cursor_unavailable',fresh_read_required:true}});
function token(s:Snapshot,p:Position){const key=`${p.index}:${p.offset}`;let t=s.positions.get(key);if(!t){if(s.tokens.size>=2048)throw new GcError(413,'Transcript continuation limit reached; start a fresh read.');t=crypto.randomUUID();s.positions.set(key,t);s.tokens.set(t,p);}return t;}
function safeEnd(text:string,end:number){return end>0&&end<text.length&&/[\uD800-\uDBFF]/.test(text[end-1])&&/[\uDC00-\uDFFF]/.test(text[end])?end-1:end;}
function render(d:Deps,id:string,s:Snapshot,start:Position):ToolResult{
 const placeholder='00000000-0000-4000-8000-000000000000';
 const first=s.entries[0];
 const base={found:true,untrusted:true,completion:'not_proven',projection:'conversation',transformation:'secret_redaction_only',snapshot_id:id,observed_at:s.observed,
  session:{id:s.session,name:s.name},connection:connectionIdentity(d.guideProfile),
  terminal:{run_on:'connector_host',argv:['gc','session','logs',s.session,'--tail','0']},
  history:{scope:'one_compaction_segment',omitted_in_segment:s.omitted,older_history:s.older},
  older_read:first?.native?{tool:'read_session',arguments:{before:first.ref}}:undefined};
 const entries:Record<string,unknown>[]=[];
 const body=(next:Position|null)=>({...base,messages:entries,snapshot_end:!next,next_cursor:next?placeholder:undefined,continue_read:next?{tool:'read_session',arguments:{cursor:placeholder}}:undefined});
 let i=start.index,offset=start.offset;
 while(i<s.entries.length){
  const e=s.entries[i];
  const part=(end:number)=>({entry_ref:e.ref,source_entry_id:e.native&&e.native.length<=160?redactText(e.native):undefined,role:e.role,at:e.at,text:e.text.slice(offset,end),offset,next_offset:end,message_end:end===e.text.length});
  entries.push(part(e.text.length));
  const next=i+1<s.entries.length?{index:i+1,offset:0}:null;
  if(Buffer.byteLength(JSON.stringify(body(next)))<=s.budget){i++;offset=0;continue;}
  entries.pop();if(entries.length)break;
  // An oversized individual message is the only case split inside message text.
  let lo=offset,hi=e.text.length;
  while(lo<hi){const mid=Math.ceil((lo+hi)/2);entries.push(part(safeEnd(e.text,mid)));const fits=Buffer.byteLength(JSON.stringify(body({index:i,offset:mid})))<=s.budget;entries.pop();if(fits)lo=mid;else hi=mid-1;}
  const end=safeEnd(e.text,lo);if(end<=offset)throw new GcError(413,'Transcript metadata exceeds the selected response budget.');
  entries.push(part(end));offset=end;break;
 }
 const next=i<s.entries.length?{index:i,offset}:null;
 const result=body(next);if(next){const t=token(s,next);result.next_cursor=t;result.continue_read={tool:'read_session',arguments:{cursor:t}};}
 return {text:`${entries.length} transcript passage${entries.length===1?'':'s'}, with formatting preserved after secret redaction. ${next?'Continue with the supplied arguments.':'End of this selected snapshot; this does not prove task completion.'}`,structured:result};
}

/** Bounded faithful message reads, independent of how much the host chooses to speak. */
export function buildTranscriptTools(d:Deps,who?:WriteCtx):ToolDef[]{return [{
 name:'read_session',title:'Read original conversation passages',
 description:'Use for detailed original messages, wording, links and IDs. Preserve text after secret redaction; not raw frames/thinking. last:1 selects latest, reply_id selects an earlier answer. Follow continue_read; retry cursor arguments after a lost response. older_read starts a fresh earlier snapshot. References expire after five minutes/restart. Terminal argv returns to this session.',
 inputShape:{session:z.string().min(1).max(128).optional(),reply_id:z.string().regex(/^[a-f0-9]{12}$/).optional(),last:z.number().int().min(1).max(50).optional(),page_bytes:z.union([z.literal(8192),z.literal(16384),z.literal(32768)]).optional(),cursor:z.string().uuid().optional(),before:z.string().uuid().optional()},annotations:RO,
 handler:guard(async(a={})=>{
  const m=cache(d),owner=who?.principal??who?.client??'anonymous';
  if((a.cursor&&(a.session!==undefined||a.last!==undefined||a.page_bytes!==undefined||a.reply_id!==undefined||a.before))||(a.before&&(a.session!==undefined||a.last!==undefined||a.page_bytes!==undefined||a.reply_id!==undefined)))return {text:'Use transcript continuation arguments unchanged.',structured:{found:false,reason:'conflicting_cursor'}};
  let saved:[string,Snapshot]|undefined;
  if(a.cursor||a.before){saved=[...m].find(([,s])=>s.owner===owner&&(a.cursor?s.tokens.has(a.cursor):s.entries.some(e=>e.ref===a.before)));if(!saved)return unavailable();}
  const end=Date.now()+8000;
  const budget=()=>{const remaining=end-Date.now();if(remaining<=0)throw new GcError(0,'Transcript read deadline reached','timeout');return Math.min(4000,remaining);};
  const f=await target(d,who,saved?.[1].session??a.session,budget);if('say'in f)return f.say;
  if(saved&&f.hit.id!==saved[1].session)return {text:'The original session is unavailable; no replacement conversation was read.',structured:{found:false,reason:'snapshot_target_unavailable'}};
  if(!/^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$/.test(f.hit.id))throw new GcError(422,'Invalid resolved session identity');
  if(saved&&(!m.has(saved[0])||saved[1].expires<=d.now().getTime()))return unavailable();
  if(a.cursor&&saved)return render(d,saved[0],saved[1],saved[1].tokens.get(a.cursor)!);
  const native=a.before?saved![1].entries.find(e=>e.ref===a.before)?.native:undefined;
  if(a.before&&!native)return unavailable();
  let page:any;
  try { page=await d.gc.get(`/session/${encodeURIComponent(f.hit.id)}/transcript`,{format:'conversation',tail:1,before:native},budget(),TRANSCRIPT_MAX_BYTES); }
  catch(e){if(native&&e instanceof GcError&&[404,409].includes(e.status))return {text:'The older history boundary changed or is unavailable. Start a fresh read; no newer output was substituted.',structured:{found:false,reason:'history_unavailable'}};throw e;}
  if(page.id!==f.hit.id||!Array.isArray(page.turns)||page.turns.some((e:any)=>!e||typeof e.text!=='string')||(native&&page.turns.some((e:any)=>e.id===native)))throw new GcError(409,'Transcript identity or history boundary could not be verified');
  const last=saved?.[1].last??a.last??10;
  const selected=a.reply_id?page.turns.filter((e:any)=>/assistant|agent/i.test(String(e.role??''))&&replyId(e.timestamp,clean(e.text))===a.reply_id):page.turns.slice(-last);
  if(a.reply_id&&selected.length!==1)return {text:'The selected reply changed or is unavailable in this segment. No newer reply was substituted.',structured:{found:false,reason:'reply_unavailable'}};
  const entries:Entry[]=selected.map((e:any)=>({native:typeof e.id==='string'&&e.id.length>0&&e.id.length<=2048?e.id:undefined,ref:crypto.randomUUID(),role:redactText(String(e.role??'unknown')).slice(0,32),at:typeof e.timestamp==='string'?redactText(e.timestamp).slice(0,80):undefined,text:redactText(e.text)}));
  // Never retain ambiguous native cursors for older-history requests.
  const ids=entries.map(e=>e.native).filter(Boolean);if(new Set(ids).size!==ids.length)for(const e of entries)e.native=undefined;
  const bytes=Buffer.byteLength(JSON.stringify(entries));if(bytes>TRANSCRIPT_MAX_BYTES)throw new GcError(413,'Transcript snapshot is too large','too_large');
  const s:Snapshot={owner,session:f.hit.id,name:clip(clean(f.hit.alias??f.hit.template??f.hit.id),60),observed:d.now().toISOString(),expires:d.now().getTime()+TTL,bytes,budget:saved?.[1].budget??a.page_bytes??16384,last,omitted:page.turns.length-selected.length,older:page.turns.length>selected.length||page.pagination?.has_older_messages===true?'available':page.pagination?.has_older_messages===false?'none_reported':'unknown',entries,tokens:new Map(),positions:new Map()};
  const id=crypto.randomUUID();m.set(id,s);
  while(m.size>MAX_SNAPSHOTS||[...m.values()].reduce((n,s)=>n+s.bytes,0)>MAX_CACHE_BYTES)m.delete(m.keys().next().value!);
  return render(d,id,s,{index:0,offset:0});
 })}];}
