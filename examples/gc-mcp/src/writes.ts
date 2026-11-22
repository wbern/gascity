// Approved writes share scope, enable flag, kill switch, rate limit, size cap,
// replay protection, confirmation hook and audit. Session lists apply to terminal
// input; domain actions have strict schemas and a fixed transport table.
import crypto from "node:crypto";
import { DOMAIN_SCHEMAS, type DomainOp } from "./domain.js";
import { redactDeep } from "./redact.js";
import fs from "node:fs";
import path from "node:path";
import { GcClient, GcError } from "./gc-client.js";
import { RateLimiter, WRITE_SCOPE } from "./oauth.js";
import { redactText } from "./redact.js";
import { disambiguation, identifiers, labelOf, resolveSession, type SessionRef } from "./sessions.js";
import { clip, speakName } from "./text.js";
import type { AuditEvent } from "./audit.js";

export { WRITE_SCOPE };
export const MAX_WRITE_TEXT = 4000;
export const INTENTS = ["default", "follow_up", "interrupt_now"] as const;
export type Intent = (typeof INTENTS)[number];
/** NO session is protected by default: the server has no opinion about whose agent is whose (gc3 is the owner's machine). An operator may set a list. */
export const DEFAULT_WRITE_DENY: string[] = [];
export const REPLAY_WINDOW_MS = 60_000;
const PATTERN = /^[A-Za-z0-9._:/*-]{1,128}$/;
const ACTION = /^[a-z][a-z0-9_-]{0,31}$/;
const REQUEST_ID = /^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$/;
const SESSION_ID = /^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$/;
const CONTROL = /[\u0000-\u0008\u000B\u000C\u000E-\u001F\u007F]/;

/** Glob with `*` only, anchored, case-insensitive. Patterns are validated, so nothing here can be a regex. */
export function globMatch(pattern: string, value: string): boolean {
  const re = new RegExp("^" + pattern.split("*").map((p) => p.replace(/[.*+?^${}()|[\]\\:/-]/g, "\\$&")).join(".*") + "$", "i");
  return re.test(value);
}
export function validatePatterns(list: string[], what: string): string[] {
  for (const p of list) if (!PATTERN.test(p)) throw new Error(`${what}: ${JSON.stringify(p).slice(0, 60)} is not a plain session pattern (letters, digits and ._:/*- only)`);
  return list;
}

export interface ConfirmCtx { client: string; session: string; kind: "submit" | "respond" | DomainOp; intent?: string; length: number }
export interface Confirmation { allow: boolean; reason?: string }
/**
 * THE hook for a second factor. Today every write that passed the rails is allowed. A TOTP or push check slots in HERE (and only here):
 * return { allow: false, reason } to refuse, or await the owner's code first. Nothing else needs to change.
 */
export async function confirmWrite(_ctx: ConfirmCtx): Promise<Confirmation> {
  return { allow: true };
}

/** When each client last sent to each session, so wait_for_reply knows "since when". In memory: a restart forgets it, which only means "from now". */
export class SendMarkers {
  private m = new Map<string, number>();
  private handles = new Map<string, { client: string; session: string; since: number; firstReplyId?: string }>();
  create(client: string, session: string, since: number): string {
    const handle = `h${crypto.randomUUID()}`;
    this.handles.set(handle, { client, session, since });
    if (this.handles.size > 500) this.handles.delete(this.handles.keys().next().value!);
    return handle;
  }
  resolve(client: string, handle: string) {
    const entry = this.handles.get(handle);
    return entry?.client === client ? { session: entry.session, since: entry.since, firstReplyId: entry.firstReplyId } : undefined;
  }
  pinFirstReply(client: string, handle: string, id: string): string | undefined {
    const entry = this.handles.get(handle);
    if (!entry || entry.client !== client) return undefined;
    entry.firstReplyId ??= id;
    return entry.firstReplyId;
  }
  set(client: string, sessionId: string, ms: number) { this.m.set(`${client}|${sessionId}`, ms); if (this.m.size > 500) this.m.delete(this.m.keys().next().value as string); }
  get(client: string, sessionId: string): number | undefined { return this.m.get(`${client}|${sessionId}`); }
}
export const markerOf = (ms: number) => `m${ms}`;
export const parseMarker = (s?: string): number | undefined => (s && /^m\d{10,16}$/.test(s) ? Number(s.slice(1)) : undefined);

export interface WriteConfig {
  enabled: boolean;
  deny?: string[];            // optional operator list; default none
  allow?: string[];           // optional: when set, ONLY these (and never a denied one)
  allowAttached?: boolean;    // default TRUE: every session is treated the same; false refuses a session a person is attached to (optional operator setting)
  perMinute?: number;         // default 20
  controlDir?: string;        // holds WRITES_DISABLED
  defaultSession?: string;    // what an omitted `session` means
  replayWindowMs?: number;    // default 60 s
  confirm?: (ctx: ConfirmCtx) => Promise<Confirmation>;
}
export interface WriteCtx { client: string; principal?: string; scopes: string[] }
export interface WriteResult { text: string; structured: Record<string, unknown> }

export class WriteGate {
  private limiter: RateLimiter;
  private deny: string[];
  private allow?: string[];
  private sessionPending = new Map<string, Promise<WriteResult>>();
  private domainPending = new Map<string, Promise<WriteResult>>();
  private replays = new Map<string, { at: number; res: WriteResult }>();
  readonly markers: SendMarkers;
  constructor(private gc: GcClient, private cfg: WriteConfig, private audit: (e: AuditEvent) => void, private nowMs: () => number = Date.now, markers?: SendMarkers) {
    this.deny = validatePatterns(cfg.deny ?? DEFAULT_WRITE_DENY, "write deny list");
    this.allow = cfg.allow?.length ? validatePatterns(cfg.allow, "write allow list") : undefined;
    this.limiter = new RateLimiter(cfg.perMinute ?? 20, 60_000, nowMs);
    this.markers = markers ?? new SendMarkers();
  }

  /** Writes are on: enabled by config and the WRITES_DISABLED kill switch is absent. Reads are never affected by it. */
  get active(): boolean {
    if (!this.cfg.enabled) return false;
    const f = this.cfg.controlDir ? path.join(this.cfg.controlDir, "WRITES_DISABLED") : null;
    return !(f && fs.existsSync(f));
  }

  /** The write deny list (also the default for reads) and the allow-list test alone, for the read check. */
  get denyList(): string[] { return this.deny; }
  allowListOk(hit: SessionRef): boolean { return !this.allow || identifiers(hit).some((n) => this.allow!.some((p) => globMatch(p, n))); }

  /** The deny and allow lists (deny always wins). Used by writes, and by the voice read tools to decide what a gc:write holder may read. */
  permits(hit: SessionRef): { ok: true } | { ok: false; reason: "denied_session" | "not_in_allow_list" } {
    const names = identifiers(hit);
    if (names.some((n) => this.deny.some((p) => globMatch(p, n)))) return { ok: false, reason: "denied_session" };
    if (this.allow && !names.some((n) => this.allow!.some((p) => globMatch(p, n)))) return { ok: false, reason: "not_in_allow_list" };
    return { ok: true };
  }

  submit(ctx: WriteCtx, session: string | undefined, text: string, intent: Intent = "default"): Promise<WriteResult> {
    return this.run(ctx, "submit", session, text, intent, async (id, key) => {
      const r = (await this.gc.write("submit", id, { message: text, intent }, key)) as Record<string, unknown>;
      return r;
    });
  }

  respond(ctx: WriteCtx, session: string | undefined, action: string, text?: string, requestId?: string): Promise<WriteResult> {
    return this.run(ctx, "respond", session, text ?? action, undefined, async (id, key) => {
      const body: Record<string, unknown> = { action };
      if (text) body.text = text;
      if (requestId) body.request_id = requestId;
      return (await this.gc.write("respond", id, body, key)) as Record<string, unknown>;
    }, { action, text, requestId });
  }

  /** Domain writes enter the same run boundary and share its limiter, replay cache, switches and audit. */
  operation(ctx:WriteCtx,kind:DomainOp,id:string,body:Record<string,unknown>):Promise<WriteResult>{
    return this.run(ctx,kind,id,JSON.stringify(body),undefined,async(target,key)=>
      await this.gc.write(kind,target,kind==='mail_send'||kind==='mail_reply'?{...body,from:'human'}:body,key) as Record<string,unknown>);
  }

  private async runDomain(ctx:WriteCtx,kind:DomainOp,id:string,preview:string,send:(id:string,key:string)=>Promise<Record<string,unknown>>):Promise<WriteResult>{
    const log=(result:string,ok:boolean)=>this.audit({kind:'write',app:ctx.client,session:id,intent:kind,length:preview.length,result,ok});
    const refuse=(reason:string,text:string):WriteResult=>{log(reason,false);return{text,structured:{sent:false,reason}};};
    if(!ctx.scopes.includes(WRITE_SCOPE))return refuse('no_write_scope','This connection only has read access.');
    if(!this.active)return refuse('writes_disabled','Writes are switched off by the owner.');
    if(!Object.hasOwn(DOMAIN_SCHEMAS,kind)||!SESSION_ID.test(id)||Buffer.byteLength(preview,'utf8')>8000)return refuse('bad_input','That action has invalid or oversized input.');
    let body:unknown;try{body=JSON.parse(preview);}catch{return refuse('bad_input','That action has invalid input.');}
    const parsed=DOMAIN_SCHEMAS[kind].safeParse(body);
    if(!parsed.success)return refuse('bad_input','That action has invalid or unapproved fields.');
    const digest=crypto.createHash('sha256').update([ctx.principal??ctx.client,kind,id,JSON.stringify(parsed.data)].join('\0')).digest('hex');
    const now=this.nowMs(),windowMs=this.cfg.replayWindowMs??REPLAY_WINDOW_MS;
    for(const[k,v]of this.replays)if(now-v.at>windowMs)this.replays.delete(k);
    const seen=this.replays.get(digest);if(seen){log('replayed',seen.res.structured.sent===true);return{...seen.res,structured:{...seen.res.structured,replayed:true}};}
    const pending=this.domainPending.get(digest);if(pending){const r=await pending;log('replayed',r.structured.sent===true);return{...r,structured:{...r.structured,replayed:true}};}
    if(!this.limiter.hit('writes'))return refuse('rate_limited','Too many actions in the last minute. Try again shortly.');
    const attempt=(async():Promise<WriteResult>=>{
      let confirmation:Confirmation;
      try{confirmation=await(this.cfg.confirm??confirmWrite)({client:ctx.client,session:id,kind,length:preview.length});}
      catch{return refuse('confirmation_failed','Owner confirmation could not be checked, so nothing was sent.');}
      if(!confirmation.allow)return refuse('confirmation_refused','The owner confirmation was not given, so nothing was sent.');
      if(!this.active)return refuse('writes_disabled','Writes are switched off by the owner.');
      // A failure may have happened after acceptance. Cache that uncertain outcome too: never blindly resend.
      const key=crypto.randomUUID();let result:WriteResult;
      try{
        const raw=redactDeep(await send(id,key));
        const response=Object.fromEntries(['id','status','request_id','bead_id','convoy_id','run_id'].filter(k=>typeof raw[k]==='string').map(k=>[k,clip(String(raw[k]),128)]));log('sent',true);
        result={text:'Gas City accepted that action.',structured:{sent:true,operation:kind,response,untrusted:true}};
      }catch(e){log('outcome_unknown',false);result={text:'The action could not be confirmed. Check its result before trying again.',structured:{sent:false,reason:'outcome_unknown',status:e instanceof GcError?e.status:0}};}
      this.replays.set(digest,{at:this.nowMs(),res:result});if(this.replays.size>200)this.replays.delete(this.replays.keys().next().value!);
      return result;
    })();
    this.domainPending.set(digest,attempt);
    try{return await attempt;}finally{this.domainPending.delete(digest);}
  }

  private async run(ctx: WriteCtx, kind: "submit" | "respond" | DomainOp, query: string | undefined, preview: string, intent: Intent | undefined,
    send: (id: string, key: string) => Promise<Record<string, unknown>>, extra?: { action: string; text?: string; requestId?: string }): Promise<WriteResult> {
    if(kind!=='submit'&&kind!=='respond')return this.runDomain(ctx,kind,query??'city',preview,send);
    const text = kind === "submit" ? preview : (extra?.text ?? "");
    const length = text.length;
    const asked = (query ?? "").trim() || (this.cfg.defaultSession ?? "");
    const log = (result: string, ok: boolean, session: string) => this.audit({ kind: "write", app: ctx.client, session: clip(session, 64), intent: intent ?? extra?.action, length, preview, result, ok });
    const refuse = (result: string, say: string, session = asked, more: Record<string, unknown> = {}): WriteResult => { log(result, false, session); return { text: say, structured: { sent: false, reason: result, ...more } }; };

    // 1. input shape (the tool schema already limits these; this is the second wall)
    if (kind === "submit") {
      if (!text.trim() || text.length > MAX_WRITE_TEXT || CONTROL.test(text)) return refuse("bad_text", `The message must be 1 to ${MAX_WRITE_TEXT} characters of plain text.`);
      if (!INTENTS.includes(intent as Intent)) return refuse("bad_intent", "The intent must be default, follow_up or interrupt_now.");
    } else {
      if (!extra || !ACTION.test(extra.action)) return refuse("bad_action", "The action must be a short lowercase word such as allow or deny.");
      if (text.length > MAX_WRITE_TEXT || CONTROL.test(text)) return refuse("bad_text", `The reply text must be at most ${MAX_WRITE_TEXT} characters of plain text.`);
      if (extra.requestId && !REQUEST_ID.test(extra.requestId)) return refuse("bad_request_id", "That request id is not valid.");
    }
    // 2. scope, enable flag and kill switch
    if (!ctx.scopes.includes(WRITE_SCOPE)) return refuse("no_write_scope", "This connection only has read access. Reconnect and grant the gc:write scope to send messages.");
    if (!this.active) return refuse("writes_disabled", "Writes are switched off by the owner.");
    // 3. which session: a spoken name resolved against the live list; ambiguity or no match means NOTHING is sent
    let res;
    try { res = await resolveSession(this.gc, query, this.cfg.defaultSession); }
    catch (e) { return refuse(`lookup_failed:${e instanceof GcError ? e.status : "error"}`, "Gas City could not be reached to find that session, so nothing was sent."); }
    if (res.kind === "incomplete") return refuse("incomplete_sessions", "The session list is incomplete, so I cannot safely choose an agent. Nothing was sent.");
    if (res.kind === "nodefault") return refuse("no_session_given", "Which session? No default session is set, so nothing was sent.", "");
    if (res.kind === "bad") return refuse("bad_session_name", "That is not a valid session name, so nothing was sent.", res.query);
    if (res.kind === "none") return refuse("no_such_session", `No session matches "${clip(redactText(res.query), 40)}", so nothing was sent.`, res.query);
    if (res.kind === "many") return refuse("ambiguous_session", `${disambiguation(res.hits)} Nothing was sent.`, asked, { candidates: res.hits.slice(0, 6).map((h) => labelOf(h)) });
    const hit = res.hit;
    if (!SESSION_ID.test(String(hit.id))) return refuse("no_such_session", "That session has an id I cannot use, so nothing was sent.", asked);
    const label = clip(redactText(labelOf(hit)), 60);
    // 4. deny / allow lists (deny wins) and attached terminals
    const perm = this.permits(hit);
    if (!perm.ok) return refuse(perm.reason, perm.reason === "denied_session" ? `${label} is on the do-not-write list, so nothing was sent.` : `${label} is not on the write allow list, so nothing was sent.`, hit.id);
    if (hit.attached === true && this.cfg.allowAttached === false) return refuse("session_attached", `A person is attached to ${label} right now, so nothing was sent.`, hit.id);
    // 5. a repeated identical send within the window (voice clients retry) returns the first acknowledgement instead of sending twice
    const now = this.nowMs();
    const windowMs = this.cfg.replayWindowMs ?? REPLAY_WINDOW_MS;
    const rkey = crypto.createHash("sha256").update([ctx.principal??ctx.client, kind, hit.id, intent ?? "", extra?.action ?? "", extra?.requestId ?? "", text].join("\u0000")).digest("hex");
    for (const [k, v] of this.replays) if (now - v.at > windowMs) this.replays.delete(k);
    const seen = this.replays.get(rkey);
    if (seen) { log("replayed", seen.res.structured.sent === true, hit.id); return { text: seen.res.text, structured: { ...seen.res.structured, replayed: true } }; }
    const pending = this.sessionPending.get(rkey);
    if (pending) {
      const result = await pending;
      log("replayed", result.structured.sent === true, hit.id);
      return { text: result.text, structured: { ...result.structured, replayed: true } };
    }
    const attempt = (async (): Promise<WriteResult> => {
      // 6. rate limit (a replay or a refusal above never spends it)
      if (!this.limiter.hit("writes")) return refuse("rate_limited", "Too many messages sent in the last minute. Try again shortly.", hit.id);
      // 7. the second-factor hook (allow today)
      const c = await (this.cfg.confirm ?? confirmWrite)({ client: ctx.client, session: String(hit.id), kind, intent, length });
      if (!c.allow) return refuse("confirmation_refused", `The owner confirmation was not given${c.reason ? ` (${clip(redactText(c.reason), 60)})` : ""}, so nothing was sent.`, hit.id);
      if (!this.active) return refuse("writes_disabled", "Writes are switched off by the owner.", hit.id);
      // 8. send: a fresh idempotency key per call, and a marker so wait_for_reply knows "since when"
      const key = crypto.randomUUID();
      const sentAt = this.nowMs();
      const handle = this.markers.create(ctx.principal??ctx.client, String(hit.id), sentAt);
      this.markers.set(ctx.principal??ctx.client, String(hit.id), sentAt);
      try {
        const r = await send(String(hit.id), key);
        log("sent", true, hit.id);
        const queued = hit.running === false ? " The session is not running; delivery is not confirmed." : "";
        const said = speakName(label);   // "crm/steve" is spoken "crm slash steve"
        const say = kind === "submit"
          ? (intent === "interrupt_now" ? `Interrupted ${said} and sent your message.` : `Sent to ${said}${intent === "follow_up" ? " as a follow-up" : ""}.`) + queued + " Ask me to wait for its reply."
          : `Answered ${said} with "${clip(redactText(extra!.action), 32)}".`;
        const out: WriteResult = { text: clip(say, 280), structured: { sent: true, session: hit.id, name: label, ...(kind === "submit" ? { intent } : { action: extra!.action }),
          handle, completion: "not_proven", marker: markerOf(sentAt), request_id: typeof r.request_id === "string" ? r.request_id : undefined, idempotency_key: key, status: typeof r.status === "string" ? r.status : "accepted" } };
        this.replays.set(rkey, { at: now, res: out });
        if (this.replays.size > 200) this.replays.delete(this.replays.keys().next().value as string);
        return out;
      } catch (e) {
        const status = e instanceof GcError ? e.status : 0;
        log(`error:${status || "no_response"}`, false, hit.id);
        const why = e instanceof GcError ? clip(redactText(e.message), 80) : "unexpected error";
        const out: WriteResult = { text: clip(`Delivery to ${label} could not be confirmed (${status || "no response"}: ${why}). Check before sending again.`, 280), structured: { sent: false, reason: "outcome_unknown", status, handle, marker: markerOf(sentAt), completion: "not_proven" } };
        this.replays.set(rkey, { at: now, res: out });
        if (this.replays.size > 200) this.replays.delete(this.replays.keys().next().value!);
        return out;
      }
    })();
    this.sessionPending.set(rkey, attempt);
    try { return await attempt; } finally { this.sessionPending.delete(rkey); }
  }
}
