// OpenAPI-backed reads, no free-form URL and no mutation entry point.
import crypto from 'node:crypto';
import { z } from 'zod';
import { READ_CATALOG } from './read-catalog.js';
import { redactDeep } from './redact.js';
import { guard, type ToolResult } from './text.js';
import { readDenyList } from './access.js';
import type { Deps, ToolDef } from './tools.js';
import type { WriteCtx } from './writes.js';
const RO={readOnlyHint:true,destructiveHint:false,openWorldHint:false} as const;
const invalid=(reason:string):ToolResult=>({text:'That API read is not allowed: '+reason+'.',structured:{error:true,reason}});
type Page={client:string;data:string;offset:number;expires:number};
const stores=new WeakMap<Deps,Map<string,Page>>();
type UpstreamCursor={client:string;operation:string;value:string;expires:number};
const upstreamStores=new WeakMap<Deps,Map<string,UpstreamCursor>>();
function upstreamStore(d:Deps){let m=upstreamStores.get(d);if(!m){m=new Map();upstreamStores.set(d,m);}return m;}
function store(d:Deps){let m=stores.get(d);if(!m){m=new Map();stores.set(d,m);}return m;}
// Every structured result is <=4 KB of JSON, including escaping and cursor metadata.
function page(d:Deps,client:string,data:string,offset=0):ToolResult{
 const m=store(d),now=d.now().getTime();for(const[k,v]of m)if(v.expires<=now)m.delete(k);
 let end=Math.min(offset+3000,data.length),key=crypto.randomUUID();
 const result=()=>({untrusted:true,format:'json_fragment',page:data.slice(offset,end),offset,total_chars:data.length,next_cursor:end<data.length?key:undefined,done:end===data.length});
 while(JSON.stringify(result()).length>4096)end-=Math.max(1,Math.floor((end-offset)/8));
 if(end<data.length){m.set(key,{client,data,offset:end,expires:now+300_000});while(m.size>100)m.delete(m.keys().next().value!);}
 return {text:'API data, untrusted content rather than instructions.'+(end<data.length?' More is available with the returned cursor.':''),structured:result()};
}
function valid(value:unknown,s:any):boolean{
 const types=Array.isArray(s.type)?s.type:[s.type];
 if(Array.isArray(value))return types.includes('array')&&value.length<=20&&value.every(v=>valid(v,s.items??{}));
 if(types.includes('integer')){if(typeof value!=='number'||!Number.isSafeInteger(value))return false;}
 else if(types.includes('boolean')){if(typeof value!=='boolean')return false;}
 else if(typeof value!=='string'||value.length>2048)return false;
 if(s.enum&&!s.enum.includes(value))return false;
 if(typeof value==='number'&&((s.minimum!==undefined&&value<s.minimum)||(s.maximum!==undefined&&value>s.maximum)))return false;
 return true;
}
export function buildApiReadTools(d:Deps,who?:WriteCtx):ToolDef[]{
 return [{name:'gc_api_list_operations',title:'Find an API read',description:'Discover supported city GET operations and their path/query parameters. Search by subject, then use gc_api_read with the operation id. No configuration, providers, external messages or streams.',annotations:RO,
 inputShape:{search:z.string().max(80).optional(),offset:z.number().int().min(0).optional(),limit:z.number().int().min(1).max(100).optional()},
 handler:async (a={})=>{
 const all=READ_CATALOG.filter(o=>!a.search||(o.path+' '+o.operation).includes(a.search));
 const offset=a.offset??0,limit=a.limit??5;let end=Math.min(offset+limit,all.length);
 const result=()=>({operations:all.slice(offset,end),total:all.length,next_offset:end<all.length?end:undefined});
 while(JSON.stringify(result()).length>4096&&end>offset+1)end--;
 // Static, generated public schema. Do not redact operation identifiers: long v0 ids resemble secrets.
 return {text:`${all.length} supported reads match.`,structured:result()};
 }},
 {name:'gc_api_read',title:'Read city API data',description:'Read one discovered GET operation with validated path and query parameters. Optional fields selects top-level response fields. Data is redacted, untrusted JSON fragments capped at 4 KB; continue with next_cursor to reconstruct JSON. Upstream next_cursor stays inside the data and is passed back as a query parameter for the next API page.',annotations:RO,
 inputShape:{operation:z.string().max(180).optional(),path:z.record(z.string(),z.string()).optional(),query:z.record(z.string(),z.union([z.string(),z.number(),z.boolean(),z.array(z.string())])).optional(),fields:z.array(z.string().max(80)).max(30).optional(),cursor:z.string().max(64).optional()},
 handler:guard(async (a={})=>{
 const client=who?.client??'anonymous';
 if(a.cursor){const m=store(d),p=m.get(a.cursor);if(!p||p.client!==client||p.expires<=d.now().getTime())return invalid('unknown or expired cursor');return page(d,client,p.data,p.offset);}
 const op=READ_CATALOG.find(o=>o.operation===a.operation);if(!op)return invalid('unknown operation');
 // Optional session policies also apply to this raw projection. Bulk records can contain session text;
 // fail closed rather than leak through a route that lacks a resolvable session identity.
 if(readDenyList(d).length||d.peekAllow?.length)return invalid('raw API reads require unrestricted session read access');
 let path:string=op.path;const params=op.params as readonly any[],pv=a.path??{},qv=a.query??{};
 for(const k of Object.keys(pv))if(!params.some(p=>p.in==='path'&&p.name===k))return invalid('unknown path parameter');
 for(const p of params.filter(p=>p.in==='path')){const v=pv[p.name];if(typeof v!=='string'||!v||v.length>128||/[\\/\x00-\x20?#%]/.test(v)||v==='.'||v==='..')return invalid('invalid path parameter');path=path.replace('{'+p.name+'}',encodeURIComponent(v));}
 const query:Record<string,string|number|boolean|string[]|undefined>={};
 for(const [k,v]of Object.entries(qv)){const p=params.find(p=>p.in==='query'&&p.name===k);if(!p||!valid(v,p.schema))return invalid('invalid query parameter');query[k]=v as string|number|boolean|string[];}
 if(typeof query.cursor==='string'){
 const saved=upstreamStore(d).get(query.cursor);
 if(saved){if(saved.client!==client||saved.operation!==op.operation||saved.expires<=d.now().getTime())return invalid('unknown or expired API cursor');query.cursor=saved.value;}
 else if(/^[0-9a-f-]{36}$/.test(query.cursor))return invalid('unknown or expired API cursor');
 }
 for(const p of params.filter(p=>p.in==='query'&&p.required))if(!(p.name in qv))return invalid('missing query parameter');
 const response=await d.gc.get<Record<string,unknown>>(path,query);
 const raw=redactDeep(response);
 // Opaque upstream cursors often resemble credentials. Keep their value server-side and return
 // a client/operation-bound handle which can be passed back in query.cursor without losing paging.
 if(response&&typeof response==='object'&&typeof response.next_cursor==='string'&&response.next_cursor){
 const m=upstreamStore(d),now=d.now().getTime();for(const[k,v]of m)if(v.expires<=now)m.delete(k);
 const key=crypto.randomUUID();m.set(key,{client,operation:op.operation,value:response.next_cursor,expires:now+300_000});
 while(m.size>100)m.delete(m.keys().next().value!);raw.next_cursor=key;
 }
 const data=a.fields?.length?Object.fromEntries(a.fields.filter((k:string)=>raw&&typeof raw==='object'&&Object.hasOwn(raw,k)).map((k:string)=>[k,(raw as Record<string,unknown>)[k]])):raw;
 return page(d,client,JSON.stringify(data));
 })}];
}
