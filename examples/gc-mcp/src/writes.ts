// Phase 4: write control. Two operations only (submit a message to a session, respond to a pending interaction), each behind the same rails:
// a separate OAuth scope, an explicit enable flag, a kill-switch file, a default-deny list for other people's and the owner's own terminals,
// a rate limit, a size cap, and an audit line for EVERY attempt (sent or refused). Nothing destructive exists here: no stop, kill, reset,
// close, config, hold release or bead edit, and gc-client.ts cannot send them (WRITE_OPS is a fixed table).
import crypto from "node:crypto";
import fs from "node:fs";
import path from "node:path";
import { GcClient, GcError } from "./gc-client.js";
import { RateLimiter, WRITE_SCOPE } from "./oauth.js";
import { redactText } from "./redact.js";
import type { AuditEvent } from "./audit.js";

export { WRITE_SCOPE };
export const MAX_WRITE_TEXT = 4000;
export const INTENTS = ["default", "follow_up", "interrupt_now"] as const;
export type Intent = (typeof INTENTS)[number];
/** Sessions that belong to other people or to the owner's own interactive terminals. Replaced (not extended) by GC_MCP_WRITE_DENY_SESSIONS. */
export const DEFAULT_WRITE_DENY = ["*-william", "claude-william", "zachris-*", "elsakert-*"];
const PATTERN = /^[A-Za-z0-9._:/*-]{1,128}$/;   // "/" because templates look like rig/agent
const SESSION_QUERY = /^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$/;
const ACTION = /^[a-z][a-z0-9_-]{0,31}$/;
const REQUEST_ID = /^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$/;
const CONTROL = /[\u0000-\u0008\u000B\u000C\u000E-\u001F\u007F]/;
const clip = (s: string, n: number) => (s.length > n ? s.slice(0, n - 1).trimEnd() + "…" : s);

/** Glob with `*` only, anchored, case-insensitive. Patterns are validated, so nothing here can be a regex. */
export function globMatch(pattern: string, value: string): boolean {
  const re = new RegExp("^" + pattern.split("*").map((p) => p.replace(/[.*+?^${}()|[\]\\:-]/g, "\\$&")).join(".*") + "$", "i");
  return re.test(value);
}
export function validatePatterns(list: string[], what: string): string[] {
  for (const p of list) if (!PATTERN.test(p)) throw new Error(`${what}: ${JSON.stringify(p).slice(0, 60)} is not a plain session pattern (letters, digits and ._:/*- only)`);
  return list;
}

export interface ConfirmCtx { client: string; session: string; kind: "submit" | "respond"; intent?: string; length: number }
export interface Confirmation { allow: boolean; reason?: string }
/**
 * THE hook for a second factor. Today every write that passed the rails is allowed. A TOTP or push check slots in HERE (and only here):
 * return { allow: false, reason } to refuse, or await the owner's code first. Nothing else needs to change.
 */
export async function confirmWrite(_ctx: ConfirmCtx): Promise<Confirmation> {
  return { allow: true };
}

export interface WriteConfig {
  enabled: boolean;
  deny?: string[];            // default DEFAULT_WRITE_DENY
  allow?: string[];           // optional: when set, ONLY these (and never a denied one)
  allowAttached?: boolean;    // default false: a session a person is attached to (their live terminal) is refused
  perMinute?: number;         // default 20
  controlDir?: string;        // holds WRITES_DISABLED
  confirm?: (ctx: ConfirmCtx) => Promise<Confirmation>;
}
export interface WriteCtx { client: string; scopes: string[] }
export interface WriteResult { text: string; structured: Record<string, unknown> }

export class WriteGate {
  private limiter: RateLimiter;
  private deny: string[];
  private allow?: string[];
  constructor(private gc: GcClient, private cfg: WriteConfig, private audit: (e: AuditEvent) => void, nowMs: () => number = Date.now) {
    this.deny = validatePatterns(cfg.deny ?? DEFAULT_WRITE_DENY, "write deny list");
    this.allow = cfg.allow?.length ? validatePatterns(cfg.allow, "write allow list") : undefined;
    this.limiter = new RateLimiter(cfg.perMinute ?? 20, 60_000, nowMs);
  }

  /** Writes are on: enabled by config and the WRITES_DISABLED kill switch is absent. Reads are never affected by it. */
  get active(): boolean {
    if (!this.cfg.enabled) return false;
    const f = this.cfg.controlDir ? path.join(this.cfg.controlDir, "WRITES_DISABLED") : null;
    return !(f && fs.existsSync(f));
  }

  submit(ctx: WriteCtx, session: string, text: string, intent: Intent = "default"): Promise<WriteResult> {
    return this.run(ctx, "submit", session, text, intent, async (id) => {
      const key = crypto.randomUUID();
      const r = (await this.gc.write("submit", id, { message: text, intent }, key)) as Record<string, unknown>;
      return { key, r };
    });
  }

  respond(ctx: WriteCtx, session: string, action: string, text?: string, requestId?: string): Promise<WriteResult> {
    return this.run(ctx, "respond", session, text ?? action, undefined, async (id) => {
      const key = crypto.randomUUID();
      const body: Record<string, unknown> = { action };
      if (text) body.text = text;
      if (requestId) body.request_id = requestId;
      const r = (await this.gc.write("respond", id, body, key)) as Record<string, unknown>;
      return { key, r };
    }, { action, text, requestId });
  }

  private async run(ctx: WriteCtx, kind: "submit" | "respond", query: string, preview: string, intent: Intent | undefined,
    send: (id: string) => Promise<{ key: string; r: Record<string, unknown> }>, extra?: { action: string; text?: string; requestId?: string }): Promise<WriteResult> {
    const text = kind === "submit" ? preview : (extra?.text ?? "");
    const length = text.length;
    const log = (result: string, ok: boolean, session: string) => this.audit({ kind: "write", app: ctx.client, session: clip(session, 64), intent: intent ?? extra?.action, length, preview, result, ok });
    const refuse = (result: string, say: string, session = query): WriteResult => { log(result, false, session); return { text: say, structured: { sent: false, reason: result } }; };

    // 1. input shape (the tool schema already limits these; this is the second wall)
    if (!SESSION_QUERY.test(query)) return refuse("bad_session_name", "That is not a valid session name.");
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
    // 3. rate limit (before any API call)
    if (!this.limiter.hit("writes")) return refuse("rate_limited", "Too many messages sent in the last minute. Try again shortly.");
    // 4. the session must exist, per the API
    let hit: any;
    try {
      const r: any = await this.gc.get("/sessions", { limit: 200 });
      const q = query.toLowerCase();
      hit = (r.items ?? []).find((i: any) => [i.id, i.alias, i.session_name, i.template].some((x) => x && String(x).toLowerCase() === q));
    } catch (e) {
      return refuse(`lookup_failed:${e instanceof GcError ? e.status : "error"}`, "Gas City could not be reached to find that session, so nothing was sent.");
    }
    if (!hit || !SESSION_QUERY.test(String(hit.id))) return refuse("no_such_session", `No session matches "${clip(redactText(query), 40)}", so nothing was sent.`);
    const names = [hit.id, hit.alias, hit.session_name, hit.template, hit.agent_name].filter((x) => x).map(String);
    const label = clip(redactText(String(hit.alias || hit.template || hit.id)), 60);
    // 5. deny / allow lists and attached terminals (deny always wins)
    if (names.some((n) => this.deny.some((p) => globMatch(p, n)))) return refuse("denied_session", `${label} is on the do-not-write list, so nothing was sent.`, hit.id);
    if (this.allow && !names.some((n) => this.allow!.some((p) => globMatch(p, n)))) return refuse("not_in_allow_list", `${label} is not on the write allow list, so nothing was sent.`, hit.id);
    if (hit.attached === true && !this.cfg.allowAttached) return refuse("session_attached", `A person is attached to ${label} right now, so nothing was sent.`, hit.id);
    // 6. the second-factor hook (allow today)
    const c = await (this.cfg.confirm ?? confirmWrite)({ client: ctx.client, session: String(hit.id), kind, intent, length });
    if (!c.allow) return refuse("confirmation_refused", `The owner confirmation was not given${c.reason ? ` (${clip(redactText(c.reason), 60)})` : ""}, so nothing was sent.`, hit.id);
    // 7. send
    try {
      const { key, r } = await send(String(hit.id));
      log("sent", true, hit.id);
      const queued = hit.running === false ? " It is not running, so the message is queued." : "";
      const say = kind === "submit"
        ? (intent === "interrupt_now" ? `Interrupted ${label} and sent your message.` : `Sent to ${label}${intent === "follow_up" ? " as a follow-up" : ""}.`) + queued
        : `Answered ${label} with "${clip(redactText(extra!.action), 32)}".`;
      return { text: say, structured: { sent: true, session: hit.id, name: label, ...(kind === "submit" ? { intent } : { action: extra!.action }),
        request_id: typeof r.request_id === "string" ? r.request_id : undefined, idempotency_key: key, status: typeof r.status === "string" ? r.status : "accepted" } };
    } catch (e) {
      const status = e instanceof GcError ? e.status : 0;
      log(`error:${status || "no_response"}`, false, hit.id);
      const why = e instanceof GcError ? clip(redactText(e.message), 80) : "unexpected error";
      return { text: `Gas City refused or could not take that for ${label} (${status || "no response"}: ${why}). Nothing was sent.`, structured: { sent: false, reason: "error", status } };
    }
  }
}
