// Session submit/respond replay protection, not proof of delivery or task completion.
import { z } from "zod";
import { loadPrivateJson, savePrivateJson, ReplyStateError } from "./private-json.js";
const attempt=z.object({
 digest:z.string().regex(/^[a-f0-9]{64}$/),requestDigest:z.string().regex(/^[a-f0-9]{64}$/),client:z.string().min(1).max(256),
 operation:z.enum(['submit','respond']),session:z.string().regex(/^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$/),
 key:z.string().uuid(),handle:z.string().regex(/^h[0-9a-f-]{36}$/),since:z.number().int().nonnegative(),expiresAt:z.number().int().positive(),
 outcome:z.enum(['pending','accepted','unknown']),requestId:z.string().regex(/^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$/).optional(),
}).strict();
type Attempt=z.infer<typeof attempt>;
const snapshot=z.object({version:z.literal(1),attempts:z.array(attempt).max(200)}).strict();
export class SendJournal{
 private entries=new Map<string,Attempt>();
 private failed=false;
 constructor(private file:string,private now:()=>number=Date.now){
  const raw=loadPrivateJson(file);if(raw===undefined)return;
  try{for(const e of snapshot.parse(raw).attempts){if(this.entries.has(e.digest)||[...this.entries.values()].some(v=>v.client===e.client&&v.requestDigest===e.requestDigest))throw Error('duplicate digest');this.entries.set(e.digest,e);}}
  catch{throw new ReplyStateError('Invalid reply state send journal; refusing unsafe retries');}
  this.prune();
 }
 private healthy(){if(this.failed)throw new ReplyStateError('Reply state send guard is unavailable; do not retry automatically');}
 private save(next:Map<string,Attempt>){
  this.healthy();
  try{savePrivateJson(this.file,{version:1,attempts:[...next.values()]});}
  catch(e){this.failed=true;throw e;}
  this.entries=next;
 }
 private prune(){
  this.healthy();const next=new Map(this.entries);
  for(const[k,e]of next)if(e.expiresAt<this.now())next.delete(k);
  if(next.size!==this.entries.size)this.save(next);
 }
 lookup(client:string,digest:string,requestDigest:string):Attempt|undefined{
  this.prune();const e=this.entries.get(digest)??[...this.entries.values()].find(e=>e.client===client&&e.requestDigest===requestDigest);return e?.client===client?{...e}:undefined;
 }
 begin(e:Omit<Attempt,'outcome'|'requestId'>){
  this.prune();
  if(this.entries.has(e.digest)||[...this.entries.values()].some(v=>v.client===e.client&&v.requestDigest===e.requestDigest))throw new ReplyStateError('Reply state intent already exists; no second submission allowed');
  // Never evict an unexpired intent to make room for a new write.
  if(this.entries.size>=200)throw new ReplyStateError('Reply state send journal is full; nothing was submitted');
  const next=new Map(this.entries);next.set(e.digest,attempt.parse({...e,outcome:'pending'}));this.save(next);
 }
 finish(client:string,digest:string,outcome:'accepted'|'unknown',requestId?:string){
  this.healthy();const e=this.entries.get(digest);
  if(!e||e.client!==client)throw new ReplyStateError('Reply state send intent is unavailable');
  const next=new Map(this.entries);next.set(digest,attempt.parse({...e,outcome,requestId}));this.save(next);
 }
}
