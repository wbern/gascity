// The voice-first read experience: ONE call that answers "how are my agents doing?" as a few short spoken sentences, and "what is steve working on?".
// Only data the supervisor already provides (status, sessions, pending interactions, human gates, work titles, recent events); a source that cannot be
// read is named in one clause and never guessed. Nothing here reads out an id: names are spoken the way people say them, titles are scrubbed.
import { z } from "zod";
import type { SessionRef } from "./sessions.js";
import { clean, clip, guard, type ToolResult } from "./text.js";
import { deriveState, realClock, target } from "./voice.js";
import type { Deps, ToolDef } from "./tools.js";
import type { WriteCtx } from "./writes.js";

export const BRIEFING_MAX = 400;
const FIRST_WINDOW_MS = 12 * 3_600_000;
const MAX_WINDOW_MS = 7 * 86_400_000;
const RO = { readOnlyHint: true, destructiveHint: false, openWorldHint: false } as const;

const UUID = /\b[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\b/gi;
const WISP = /\b[a-z0-9]+-wisp-[a-z0-9]+\b/gi;
const ID_TOKEN = /\b(?:gc[a-z0-9]{0,3}|[a-z]{2,4})-(?=[a-z0-9]*\d)[a-z0-9]{4,12}\b/gi;
const looksLikeId = (s: string) => { UUID.lastIndex = WISP.lastIndex = ID_TOKEN.lastIndex = 0; return UUID.test(s) || WISP.test(s) || ID_TOKEN.test(s); };

/** Spoken form of a title from beads, mail or agents: untrusted text, redacted, with ids and links removed. */
export function spokenTitle(raw: unknown, max: number): string {
  const s = clean(raw).replace(UUID, "").replace(WISP, "").replace(ID_TOKEN, "").replace(/\(\s*\)|\[\s*\]/g, "").replace(/\s+/g, " ").replace(/[\s,;:(-]+$/g, "")
    .replace(/\s+(for|on|of|to|and|with|in|at|by)$/i, "").trim();
  return clip(s, max);
}
/** "crm/steve" is said "crm steve"; a session with no name of its own is never announced by its id. */
export function spokenName(s: SessionRef): string {
  const label = String(s.alias || s.template || s.agent_name || "").trim();
  if (!label || looksLikeId(label)) return "an unnamed agent";
  return clip(label.replace(/[/_-]+/g, " ").replace(/\s+/g, " ").trim(), 40);
}
const plural = (n: number, one: string, many = `${one}s`) => `${n} ${n === 1 ? one : many}`;
const andList = (xs: string[]) => (xs.length < 2 ? xs.join("") : `${xs.slice(0, -1).join(", ")} and ${xs[xs.length - 1]}`);
function howLong(ms: number): string {
  const m = Math.max(1, Math.round(ms / 60_000));
  if (m < 90) return plural(m, "minute");
  const h = Math.round(ms / 3_600_000);
  return h < 48 ? plural(h, "hour") : plural(Math.round(h / 24), "day");
}
const itemsOf = (x: any): any[] => (Array.isArray(x?.items) ? x.items : []);

export function buildBriefingTools(d: Deps, who?: WriteCtx): ToolDef[] {
  const clock = d.clock ?? realClock;
  const memory = d.briefings ?? new Map<string, number>();
  const gc = d.gc;

  return [
    {
      name: "briefing", title: "Ask how the agents are doing",
      description: "Use when the person asks how are my agents doing, how are things, or is anything going on. One call gives a few short sentences: what needs them first, how many agents are working, idle or waiting, what the busiest are doing, unread mail and what changed since last time.",
      inputShape: {}, annotations: RO,
      handler: guard(async () => {
        const now = clock.now();
        const client = who?.client ?? "";
        const last = memory.get(client);
        const windowMs = Math.min(MAX_WINDOW_MS, last === undefined ? FIRST_WINDOW_MS : Math.max(60_000, now - last));
        const since = last === undefined ? "12h" : `${Math.max(1, Math.round(windowMs / 60_000))}m`;
        const unavailable: string[] = [];
        const read = async <T>(name: string, f: () => Promise<T>): Promise<T | undefined> => { try { return await f(); } catch { unavailable.push(name); return undefined; } };
        const [status, sess, pend, gates, events] = await Promise.all([
          read("city status", () => gc.get("/status") as Promise<any>),
          read("sessions", () => gc.get("/sessions", { limit: 200 }) as Promise<any>),
          read("agents waiting for input", () => gc.get("/pending") as Promise<any>),
          read("decisions", () => gc.get("/beads", { type: "gate", status: "open", limit: 50 }) as Promise<any>),
          read("recent changes", () => gc.get("/events", { since, limit: 200 }) as Promise<any>),
        ]);
        memory.set(client, now);

        // ---- classify sessions (waiting > stuck > not running > working > idle)
        const sessions: SessionRef[] = sess ? itemsOf(sess) : [];
        const waitingIds = new Set<string>(itemsOf(pend).map((p: any) => String(p.session_id)));
        const counts = { working: 0, idle: 0, waiting: 0, stuck: 0, not_running: 0 };
        const working: SessionRef[] = [], waiting: SessionRef[] = [], stuck: SessionRef[] = [];
        for (const s of sessions) {
          const st = deriveState(s, s, waitingIds.has(s.id));
          if (st === "waiting for input") { counts.waiting++; waiting.push(s); }
          else if (st === "stuck") { counts.stuck++; stuck.push(s); }
          else if (st === "not running") counts.not_running++;
          else if (st === "working") { counts.working++; working.push(s); }
          else counts.idle++;
        }
        // waiting sessions the list did not include still count (the pending list is authoritative)
        for (const id of waitingIds) if (!sessions.some((s) => s.id === id)) { counts.waiting++; waiting.push({ id }); }

        // ---- what needs the owner, in this order: decisions, agents waiting for input, failed or stuck work
        const decisions = itemsOf(gates).filter((b: any) => String(b.await_type) === "human" && String(b.status ?? "open") !== "closed");
        const quarantined = Number(status?.agents?.quarantined ?? 0);
        const attention: { kind: string; count: number; items: string[] }[] = [];
        if (decisions.length) attention.push({ kind: "decision", count: decisions.length, items: decisions.map((b: any) => spokenTitle(b.title, 40)).filter(Boolean) });
        if (waiting.length) attention.push({ kind: "waiting_for_input", count: waiting.length, items: waiting.map(spokenName) });
        const stuckCount = Math.max(stuck.length, quarantined);
        if (stuckCount) attention.push({ kind: "stuck", count: stuckCount, items: stuck.map(spokenName) });

        // ---- the busiest few, by what they work on (titles, not ids)
        const busiest = [...working].sort((a, b) => String(b.last_active).localeCompare(String(a.last_active))).slice(0, 3);
        const titled = await Promise.all(busiest.map(async (s) => {
          const id = s.active_bead ? String(s.active_bead) : "";
          let title = "";
          if (id) { try { title = spokenTitle(((await gc.get(`/bead/${encodeURIComponent(id)}`)) as any).title, 40); } catch { /* no title: still named */ } }
          return { name: spokenName(s), title };
        }));

        // ---- sentences, most important first
        const names = (xs: string[], n = 2) => { const sh = [...new Set(xs)].slice(0, n); return sh.join(", ") + (xs.length > n ? ` and ${xs.length - n} more` : ""); };
        const needsParts = attention.map((a) => a.kind === "decision" ? `${plural(a.count, "decision")} (${names(a.items)})`
          : a.kind === "waiting_for_input" ? `${plural(a.count, "agent")} waiting for input${a.items.length ? ` (${names(a.items)})` : ""}`
          : `${a.count} stuck${a.items.length ? ` (${names(a.items)})` : ""}`);
        const needs = attention.length ? `Needs you: ${andList(needsParts)}.` : (gates && pend && sess ? "Nothing needs you right now." : "");
        const countLine = sess ? `${counts.working} working, ${counts.idle} idle${counts.waiting ? `, ${counts.waiting} waiting` : ""}.` : "";
        const busyLine = (n: number) => { const xs = titled.slice(0, n).map((t) => (t.title ? `${t.name} on ${t.title}` : `${t.name} is busy`)); return xs.length ? `Busiest: ${andList(xs)}.` : ""; };
        const unread = status?.mail?.unread;
        const unavailPhrase = unavailable.length ? `I couldn't read ${andList(unavailable.map((u) => `the ${u}`))}` : "";
        const mailLine = [typeof unread === "number" ? `${unread} unread mail` : "", unavailPhrase].filter(Boolean).join("; ");
        let changes = "";
        if (events) {
          const ev = itemsOf(events), closed = ev.filter((e: any) => /closed/i.test(String(e.type))).length, other = ev.length - closed;
          const lead = last === undefined ? "In the last 12 hours" : `Since your last briefing ${howLong(windowMs)} ago`;
          changes = ev.length ? `${lead}: ${[closed ? `${plural(closed, "bead")} closed` : "", other ? `${plural(other, "other change")}` : ""].filter(Boolean).join(", ")}.` : `${lead}: nothing has changed.`;
        }
        // fit the cap: drop the least important pieces first, never the needs or the counts
        const build = (b: number, withChanges: boolean, withMail: boolean) => [needs, countLine, busyLine(b), withMail && mailLine ? `${mailLine}.` : "", withChanges ? changes : ""].filter(Boolean);
        let text = "";
        for (const [b, c, m] of [[3, true, true], [2, true, true], [2, false, true], [1, false, true], [1, false, false], [0, false, false]] as const) {
          text = build(b, c, m).join(" ");
          if (text.length <= BRIEFING_MAX) break;
        }
        if (!text) text = `I couldn't read ${andList(unavailable.map((u) => `the ${u}`))}, so I can't tell how things are.`;
        if (text.length > BRIEFING_MAX) text = clip(text.slice(0, BRIEFING_MAX - 1), BRIEFING_MAX).replace(/[\s,;:(]+\S*$/, "") + ".";

        return { text, structured: { counts, attention, busiest: titled, unread_mail: typeof unread === "number" ? unread : undefined, unavailable, since_ms: windowMs,
          first_briefing: last === undefined, recent: events ? { total: itemsOf(events).length } : undefined, untrusted: true } };
      }),
    },
    {
      name: "what_is_working_on", title: "What is an agent working on",
      description: "Use when the person asks what is steve working on, or what an agent is doing right now. One or two sentences. Say a name, or leave it out for their own session.",
      inputShape: { session: z.string().min(1).max(128).optional() }, annotations: RO,
      handler: guard(async (a): Promise<ToolResult> => {
        const f = await target(d, who, a.session);
        if ("say" in f) return f.say;
        const hit = f.hit, name = spokenName(hit);
        let full: any = hit;
        try { full = await gc.get(`/session/${encodeURIComponent(hit.id)}`); } catch { /* the list entry is enough */ }
        let waitingFor = "";
        let isWaiting = false;
        try { isWaiting = itemsOf(await gc.get("/pending")).some((p: any) => String(p.session_id) === hit.id); } catch { /* fall back to the session's own pending */ }
        try {
          const p: any = await gc.get(`/session/${encodeURIComponent(hit.id)}/pending`);
          if (p?.pending && (p.pending.request_id || p.pending.prompt)) { isWaiting = true; waitingFor = spokenTitle(p.pending.prompt ?? "", 120); }
        } catch { /* none or unsupported */ }
        const state = deriveState(full, hit, isWaiting);
        const beadId = String(full?.active_bead ?? hit.active_bead ?? "");
        let title = "";
        if (beadId) { try { title = spokenTitle(((await gc.get(`/bead/${encodeURIComponent(beadId)}`)) as any).title, 80); } catch { /* no title */ } }
        const text = state === "working" ? (title ? `${name} is working on ${title}.` : `${name} is working, but I can't see on what.`)
          : state === "waiting for input" ? `${name} is waiting for you${waitingFor ? `: ${waitingFor}` : "."}`
          : state === "stuck" ? `${name} looks stuck.`
          : state === "not running" ? `${name} is not running.`
          : state === "idle" ? `${name} is idle${title ? `; it last worked on ${title}` : ""}.` : `${name} is active, but I can't tell what it is doing.`;
        return { text: clip(text, 280), structured: { found: true, name, state, working_on: title || undefined, waiting_for: waitingFor || undefined, untrusted: !!(title || waitingFor) } };
      }),
    },
  ];
}
