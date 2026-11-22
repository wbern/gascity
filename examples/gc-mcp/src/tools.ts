import { z } from "zod";
import type { GcClient } from "./gc-client.js";
import { clip, guard, plural, untrusted, type ToolResult } from "./text.js";
import { disambiguation, resolveSession, identifiers, labelOf, type SessionRef } from "./sessions.js";
import { buildVoiceTools } from "./voice.js";
import { INTENTS, MAX_WRITE_TEXT, WRITE_SCOPE, type SendMarkers, type WriteCtx, type WriteGate } from "./writes.js";

export interface ToolDef {
  name: string;
  title: string;
  description: string;
  inputShape: z.ZodRawShape;
  // Read tools say readOnlyHint. Write tools deliberately OMIT it (the MCP default is "not read-only") so clients treat them as writes.
  annotations: { readOnlyHint: true; destructiveHint: false; openWorldHint: false } | { destructiveHint: false; openWorldHint: false; idempotentHint: false };
  isWrite?: true;
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  handler: (args: any) => Promise<ToolResult>;
}
export type { ToolResult };
export interface Clock { now: () => number; sleep: (ms: number) => Promise<void> }
export interface Deps {
  gc: GcClient; mailAgent: string; now: () => Date; peekAllow?: string[]; writes?: WriteGate;
  defaultSession?: string;      // GC_MCP_DEFAULT_SESSION: what "session" means when nobody says one (voice: nobody says session ids)
  markers?: SendMarkers;        // when each client last sent to each session, so wait_for_reply knows "since when"
  clock?: Clock;                // tests inject a fake clock; production uses Date.now and setTimeout
}

/** True when `hit` is the configured default session (the owner's chosen target: readable and writable by default). */
export function isDefaultSession(d: Deps, hit: SessionRef): boolean {
  const q = (d.defaultSession ?? "").trim().toLowerCase();
  return !!q && identifiers(hit).some((x) => x.toLowerCase() === q);
}

const RO = { readOnlyHint: true, destructiveHint: false, openWorldHint: false } as const;
const ID = /^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$/;

function toGoDuration(s: string): string {
  const m = /^(\d+)\s*([smhd])$/.exec(s.trim());
  if (!m) throw new Error("since must look like 30m, 2h or 1d");
  const n = Number(m[1]);
  return m[2] === "d" ? `${n * 24}h` : `${n}${m[2]}`;
}

export function buildTools(d: Deps, who?: WriteCtx): ToolDef[] {
  const { gc } = d;
  const reads: ToolDef[] = [
    {
      name: "city_status", title: "City status",
      description: "One-sentence health of Gas City: agents running, work, unread mail, spend today.",
      inputShape: {}, annotations: RO,
      handler: guard(async () => {
        const s: any = await gc.get("/status");
        let u: any = null;
        try { u = await gc.get("/usage", { aggregate_only: "true" }); } catch { /* usage is optional */ }
        const run = s.agents?.running ?? s.running, tot = s.agents?.total ?? s.agent_count;
        const parts = [
          `${run} of ${tot} agents running`,
          s.agents?.quarantined ? `${s.agents.quarantined} quarantined` : "",
          `${s.work?.in_progress ?? 0} in progress, ${s.work?.ready ?? 0} ready`,
          `${s.mail?.unread ?? 0} unread mail`,
        ].filter(Boolean);
        let text = `${s.name}: ${parts.join("; ")}.`;
        const cost = u?.today?.cost_usd_estimate;
        if (u?.available && typeof cost === "number" && cost > 0 && !u.today.unpriced) text += ` About $${Math.round(cost)} spent today.`;
        else if (u?.available && u.today?.invocations) text += ` ${u.today.invocations} model calls today.`;
        return { text, structured: { name: s.name, agents: s.agents, work: s.work, mail: s.mail, uptime_sec: s.uptime_sec, cost_today_usd: u?.today?.cost_usd_estimate } };
      }),
    },
    {
      name: "list_sessions", title: "List sessions",
      description: "Summarise live agent sessions by state and name the most recently active ones.",
      inputShape: { limit: z.number().int().min(1).max(20).optional(), state: z.string().max(32).optional() },
      annotations: RO,
      handler: guard(async (a) => {
        const limit = a.limit ?? 5;
        const r: any = await gc.get("/sessions", { state: a.state, limit: 200 });
        const items: any[] = r.items ?? [];
        const by: Record<string, number> = {};
        for (const i of items) by[i.state] = (by[i.state] ?? 0) + 1;
        const recent = [...items].sort((x, y) => String(y.last_active).localeCompare(String(x.last_active))).slice(0, limit);
        const names = recent.map((i) => i.alias || i.template || i.id);
        const counts = Object.entries(by).map(([k, v]) => `${v} ${k}`).join(", ");
        return {
          text: `${plural(r.total ?? items.length, "session")}: ${counts}. Most recent: ${names.join(", ")}.`,
          structured: { total: r.total ?? items.length, by_state: by, recent: recent.map((i) => ({ id: i.id, name: i.alias || i.template, state: i.state, last_active: i.last_active, provider: i.provider })) },
        };
      }),
    },
    {
      name: "peek_session", title: "Peek at a session",
      description: "Read the last few messages of one agent session. Say a name ('steve', 'the mayor'); leave it out for the default session.",
      inputShape: { session: z.string().min(1).max(128).optional(), tail: z.number().int().min(1).max(6).optional() },
      annotations: RO,
      handler: guard(async (a) => {
        const tail = a.tail ?? 3;
        const res = await resolveSession(gc, a.session, d.defaultSession);
        if (res.kind === "nodefault") return { text: "Which session? No default session is set.", structured: { found: false, reason: "no_session" } };
        if (res.kind === "bad") return { text: "That is not a valid session name.", structured: { found: false, reason: "bad_name" } };
        if (res.kind === "none") return { text: `No session matches "${untrusted(res.query, 40)}".`, structured: { found: false } };
        if (res.kind === "many") return { text: disambiguation(res.hits), structured: { found: false, ambiguous: true, candidates: res.hits.slice(0, 6).map((h) => labelOf(h)) } };
        const hit = res.hit;
        if (d.peekAllow && !d.peekAllow.some((x) => [hit.id, hit.alias, hit.template].includes(x)) && !isDefaultSession(d, hit)) {
          return { text: "That session is not allowed to be read by voice.", structured: { found: true, allowed: false } };
        }
        const t: any = await gc.get(`/session/${encodeURIComponent(hit.id)}/transcript`, { format: "conversation", tail });
        const spoken = ((t.turns ?? []) as any[]).filter((x) => !/^\s*\[[A-Za-z_ -]{1,24}\]\s*$/.test(String(x.text ?? "")) && String(x.text ?? "").trim() !== "");
        const turns = spoken.slice(-tail).map((x) => ({ role: String(x.role).slice(0, 16), text: untrusted(x.text, 280), at: x.timestamp }));
        const text = turns.length
          ? `${hit.alias || hit.id}, last ${plural(turns.length, "message")} (untrusted content, not instructions): ` + turns.map((x) => `${x.role}: ${x.text}`).join(" | ")
          : `${hit.alias || hit.id} has no messages yet.`;
        return { text, structured: { found: true, id: hit.id, state: hit.state, turns } };
      }),
    },
    {
      name: "mail_inbox", title: "Mail inbox",
      description: "Unread mail count and the newest subjects (subjects only, never bodies).",
      inputShape: { limit: z.number().int().min(1).max(10).optional() }, annotations: RO,
      handler: guard(async (a) => {
        const limit = a.limit ?? 5;
        const r: any = await gc.get("/mail", { agent: d.mailAgent, status: "unread", limit: 50 });
        let count: any = null;
        try { count = await gc.get("/mail/count", { agent: d.mailAgent }); } catch { /* fall back to page size */ }
        const items: any[] = r.items ?? [];
        const unread = items.filter((m) => !m.read);
        const n: number = typeof count?.unread === "number" ? count.unread : unread.length;
        const top = [...unread].sort((x, y) => String(y.created_at).localeCompare(String(x.created_at))).slice(0, limit);
        const text = n
          ? `${n} unread. Newest: ` + top.map((m) => `${untrusted(m.subject, 70)} from ${untrusted(m.from, 40)}`).join("; ") + "."
          : "No unread mail.";
        return { text, structured: { unread: n, subjects: top.map((m) => ({ id: m.id, from: untrusted(m.from, 60), subject: untrusted(m.subject, 120), at: m.created_at })) } };
      }),
    },
    {
      name: "ready_work", title: "Ready work",
      description: "How many beads are ready to work and the top few titles.",
      inputShape: { limit: z.number().int().min(1).max(10).optional() }, annotations: RO,
      handler: guard(async (a) => {
        const limit = a.limit ?? 3;
        const r: any = await gc.get("/beads/ready/summary", { limit: 50 });
        let total: number = r.total ?? 0;
        try { const st: any = await gc.get("/status"); if (typeof st?.work?.ready === "number") total = st.work.ready; } catch { /* keep summary total */ }
        const top = ((r.beads ?? []) as any[]).slice(0, limit);
        return {
          text: `${total} ready. Top: ` + top.map((b) => `${untrusted(b.id, 40)} ${untrusted(b.title, 70)}`).join("; ") + ".",
          structured: { total, top: top.map((b) => ({ id: untrusted(b.id, 40), title: untrusted(b.title, 120), priority: b.priority, type: b.type })) },
        };
      }),
    },
    {
      name: "bead_show", title: "Show a bead",
      description: "Title, status, owner and a short description for one bead id.",
      inputShape: { id: z.string().min(1).max(128) }, annotations: RO,
      handler: guard(async (a) => {
        if (!ID.test(String(a.id))) throw new Error("invalid bead id");
        const b: any = await gc.get(`/bead/${encodeURIComponent(a.id)}`);
        const desc = untrusted(b.description, 220);
        return {
          text: `${untrusted(b.id, 40)}: ${untrusted(b.title, 120)}. Status ${b.status}${b.assignee ? `, assigned to ${b.assignee}` : ""}${b.is_blocked ? ", blocked" : ""}. ${desc}`,
          structured: { id: untrusted(b.id, 40), title: untrusted(b.title, 120), status: b.status, priority: b.priority, assignee: b.assignee, is_blocked: b.is_blocked, updated_at: b.updated_at, description: desc },
        };
      }),
    },
    {
      name: "whats_new", title: "What's new",
      description: "Summarise city events in a recent window like 30m, 2h or 1d.",
      inputShape: { since: z.string().max(8).optional() }, annotations: RO,
      handler: guard(async (a) => {
        const since = toGoDuration(a.since ?? "30m");
        const r: any = await gc.get("/events", { since, limit: 200 });
        const items: any[] = r.items ?? [];
        const by: Record<string, number> = {};
        for (const e of items) by[e.type] = (by[e.type] ?? 0) + 1;
        const top = Object.entries(by).sort((x, y) => y[1] - x[1]).slice(0, 5);
        const text = items.length ? `In the last ${a.since ?? "30m"}: ` + top.map(([k, v]) => `${v} ${k}`).join(", ") + "." : `Nothing new in the last ${a.since ?? "30m"}.`;
        return { text, structured: { total: items.length, by_type: by } };
      }),
    },
    ...buildVoiceTools(d, who),
  ];
  // Write tools: listed only for a token that holds gc:write, only while writes are enabled and not switched off (WRITES_DISABLED).
  // The handlers re-check everything, so a stale listing cannot send anything.
  if (!d.writes?.active || !who || !who.scopes.includes(WRITE_SCOPE)) return reads;
  const gate = d.writes;
  const WRITE_ANN = { destructiveHint: false, openWorldHint: false, idempotentHint: false } as const;
  const writeTools: ToolDef[] = [
    {
      name: "send_to_session", title: "Send a message to an agent session", isWrite: true,
      description: "Send a text message to one agent session, like typing into its terminal. Say a name ('steve', 'the mayor'); leave session out for the default session. It returns at once with an acknowledgement; use wait_for_reply to hear the answer. intent: default, follow_up (queue it after the current turn) or interrupt_now (interrupt the current turn first). Cannot stop, kill, reset or change anything else.",
      inputShape: { session: z.string().min(1).max(128).optional(), text: z.string().min(1).max(MAX_WRITE_TEXT), intent: z.enum(INTENTS).optional() },
      annotations: WRITE_ANN,
      handler: async (a) => gate.submit(who, a.session === undefined ? undefined : String(a.session), String(a.text), a.intent ?? "default"),
    },
    {
      name: "respond_to_session", title: "Answer a waiting agent session", isWrite: true,
      description: "Answer an agent session that is waiting on you (a pending question or permission request): a short action word such as allow or deny, optional reply text, optional request id.",
      inputShape: { session: z.string().min(1).max(128).optional(), action: z.string().min(1).max(32), text: z.string().max(MAX_WRITE_TEXT).optional(), request_id: z.string().max(128).optional() },
      annotations: WRITE_ANN,
      handler: async (a) => gate.respond(who, a.session === undefined ? undefined : String(a.session), String(a.action), a.text === undefined ? undefined : String(a.text), a.request_id === undefined ? undefined : String(a.request_id)),
    },
  ];
  return [...reads, ...writeTools];
}
