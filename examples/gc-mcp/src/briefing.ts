// The voice-first read experience: ONE call that answers "how are my agents doing?" as a few short spoken sentences, and "what is steve working on?".
// Only data the supervisor already provides (status, sessions, pending interactions, human gates, work titles, recent events); a source that cannot be
// read is named in one clause and never guessed. Nothing here reads out an id: names are spoken the way people say them, titles are scrubbed.
import { z } from "zod";
import { SHA, UUID, WISP, ID_TOKEN, spokenName, type SessionRef } from "./sessions.js";
export { spokenName };
import { clean, clip, guard, type ToolResult } from "./text.js";
import { deriveState, realClock, target } from "./voice.js";
import { isReadDenied } from "./access.js";
import type { Deps, ToolDef } from "./tools.js";
import type { WriteCtx } from "./writes.js";

export const BRIEFING_MAX = 400;
const FIRST_WINDOW_MS = 12 * 3_600_000;
const MAX_WINDOW_MS = 7 * 86_400_000;
const RO = { readOnlyHint: true, destructiveHint: false, openWorldHint: false } as const;

/** Spoken form of a title from beads, mail or agents: untrusted text, redacted, with ids and links removed. */
export function spokenTitle(raw: unknown, max: number): string {
  let s = clean(raw).replace(UUID, "").replace(WISP, "").replace(ID_TOKEN, "").replace(SHA, "").replace(/\([^)]*\)|\[[^\]]*\]/g, "").replace(/#(\d+)/g, "$1").replace(/\s@\s?/g, " ").replace(/[_]+/g, " ").replace(/\s+/g, " ").trim();
  if (s.length > max) { const cut = s.slice(0, max + 1); s = cut.slice(0, Math.max(cut.lastIndexOf(" "), Math.floor(max * 0.6))); }   // a word boundary, never a mid-word "…" (it is read aloud)
  return s.replace(/[\s,;:(-]+$/g, "").replace(/(?:\s+(?:for|on|of|to|and|with|in|at|by|the|a|an|is|are))+$/i, "").trim();
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

/** What a decision is ABOUT, in words a person can hear: never the raw gate title ("Gate: human" means nothing). */
const GENERIC_GATE = /^(human|timer|manual|decision|gate)?$|^[a-z]+:[a-z0-9-]+$/i;
const firstSentence = (t: string) => t.split(/\n|\.\s|;\s|\s[—-]\s/)[0] ?? t;
export function decisionQuestion(b: any): string {
  const desc = String(b.description ?? "");
  const decision = /(?:^|\n)\s*Decision needed:\s*(.+)/i.exec(desc)?.[1];
  const reason = /(?:^|\n)\s*Reason:\s*(.+)/i.exec(desc)?.[1];
  const title = String(b.title ?? "").replace(/^gate:\s*/i, "").trim();
  const chosen = decision ?? (GENERIC_GATE.test(title) ? reason : title) ?? reason ?? "";
  return spokenTitle(firstSentence(chosen).replace(/^William('s)?\s+/i, "your "), 56).replace(/[/_-]+/g, " ");   // scrub ids FIRST, then make it speakable (hyphens and slashes become spaces)
}
const blockedId = (b: any): string | undefined => /blocking\s+([A-Za-z0-9][A-Za-z0-9._-]{2,40})/i.exec(String(b.description ?? ""))?.[1];
const OWNER_MAIL = /\b(decision needed|needs? (you|william|the owner)|for (william|the owner)|urgent|action required)\b/i;
const AUTOMATED_MAIL = /^\s*(reminder\b|orphan-sweep|NOTICE:|auto)/i;
/** Mail worth interrupting for: from the mayor or the owner, or marked for the owner; never the system's own reminders. */
export function isFlaggedMail(m: any): boolean {
  const from = String(m.from ?? ""), subject = String(m.subject ?? "");
  if (AUTOMATED_MAIL.test(subject)) return false;
  return /^(mayor|william|owner)\b/i.test(from) || OWNER_MAIL.test(subject);
}
const AUTOMATED_TITLE = /^(order:|mol-|orphan-sweep|sling-|gate\b)/i;   // automation and gates are not "tasks"; a single token (an id or an agent identity such as rig/agent-2) is not a task title either
const TASK_TYPES = new Set(["task", "bug", "feature", "story", "epic", "chore"]);
const days = (ms: number) => { const d = Math.floor(ms / 86_400_000); return d >= 1 ? plural(d, "day") : plural(Math.max(1, Math.floor(ms / 3_600_000)), "hour"); };
const holds = (s: SessionRef, assignees: Map<string, string>): string | undefined => {
  for (const n of [s.id, s.alias, s.session_name, s.template, s.agent_name]) { const t = n ? assignees.get(String(n)) : undefined; if (t) return t; }
  return undefined;
};

export function buildBriefingTools(d: Deps, who?: WriteCtx): ToolDef[] {
  const clock = d.clock ?? realClock;
  const memory = d.briefings ?? new Map<string, number>();
  const gc = d.gc;
  const title = async (id: string, n: number) => { try { return spokenTitle(((await gc.get(`/bead/${encodeURIComponent(id)}`)) as any).title, n); } catch { return ""; } };

  return [
    {
      name: "briefing", title: "Ask how the agents are doing",
      description: "Use when the person asks how are my agents doing, how are things, or is anything going on. One call gives a few calm sentences: what needs them first, who is working on what, what finished since last time, and mail only if it matters.",
      inputShape: {}, annotations: RO,
      handler: guard(async () => {
        const now = clock.now();
        const client = who?.client ?? "";
        const last = memory.get(client);
        const windowMs = Math.min(MAX_WINDOW_MS, last === undefined ? FIRST_WINDOW_MS : Math.max(60_000, now - last));
        const since = last === undefined ? "12h" : `${Math.max(1, Math.round(windowMs / 60_000))}m`;
        const unavailable: string[] = [];
        const read = async <T>(name: string, f: () => Promise<T>): Promise<T | undefined> => { try { return await f(); } catch { unavailable.push(name); return undefined; } };
        const [status, sess, pend, gates, events, mail, inprog] = await Promise.all([
          read("city status", () => gc.get("/status") as Promise<any>),
          read("sessions", () => gc.get("/sessions", { limit: 200 }) as Promise<any>),
          read("agents waiting for input", () => gc.get("/pending") as Promise<any>),
          read("decisions", () => gc.get("/beads", { type: "gate", status: "open", limit: 200 }) as Promise<any>),
          read("recent changes", () => gc.get("/events", { since, type: "bead.closed", limit: 100 }) as Promise<any>),
          read("mail", () => gc.get("/mail", { agent: d.mailAgent, status: "unread", limit: 50 }) as Promise<any>),
          read("work in progress", () => gc.get("/beads", { status: "in_progress", limit: 200 }) as Promise<any>),
        ]);
        memory.set(client, now);
        const ORDER = ["city status", "sessions", "agents waiting for input", "decisions", "recent changes", "mail", "work in progress"];
        unavailable.sort((a, b) => ORDER.indexOf(a) - ORDER.indexOf(b));   // a stable order, whatever finished first

        // ---- who holds what: in-progress beads by assignee (newest first)
        const assignees = new Map<string, string>();
        for (const b of [...itemsOf(inprog)].sort((x: any, y: any) => String(y.updated_at).localeCompare(String(x.updated_at)))) {
          const a = String(b.assignee ?? ""); if (a && !assignees.has(a)) assignees.set(a, spokenTitle(b.title, 45));
        }
        // ---- classify sessions (waiting > stuck > not running > working > idle). An agent in a turn, or holding a bead, is working, never idle.
        const sessions: SessionRef[] = sess ? itemsOf(sess) : [];
        const waitingIds = new Set<string>(itemsOf(pend).map((p: any) => String(p.session_id)));
        const counts = { working: 0, idle: 0, waiting: 0, stuck: 0, not_running: 0 };
        const working: { s: SessionRef; title: string }[] = [], waiting: SessionRef[] = [], stuck: SessionRef[] = [];
        for (const s of sessions) {
          const st = deriveState(s, s, waitingIds.has(s.id));
          const heldTitle = holds(s, assignees);
          if (st === "waiting for input") { counts.waiting++; waiting.push(s); }
          else if (st === "stuck") { counts.stuck++; stuck.push(s); }
          else if (st === "not running") counts.not_running++;
          else if (st === "working" || s.active_bead || heldTitle) { counts.working++; working.push({ s, title: isReadDenied(d, s) ? "" : (heldTitle ?? "") }); }   // a protected session may be named as busy, but what it works on is not ours to say
          else counts.idle++;
        }
        for (const id of waitingIds) if (!sessions.some((s) => s.id === id)) { counts.waiting++; waiting.push({ id }); }
        working.sort((a, b) => String(b.s.last_active).localeCompare(String(a.s.last_active)));
        await Promise.all(working.slice(0, 8).map(async (w) => { if (!w.title && w.s.active_bead && !isReadDenied(d, w.s)) w.title = await title(String(w.s.active_bead), 45); }));
        // name the ones whose work we can SAY first (a name alone tells nothing), most recently active first within each group
        const shown = [...working].sort((a, b) => Number(!!b.title) - Number(!!a.title)).slice(0, 3);

        // ---- decisions: what each one is ABOUT, deduplicated, newest first (the oldest of a long list are usually stale)
        const humanGates = itemsOf(gates).filter((b: any) => String(b.await_type) === "human" && String(b.status ?? "open") !== "closed")
          .sort((x: any, y: any) => String(y.created_at).localeCompare(String(x.created_at)));
        const seen = new Set<string>(), asks: string[] = [];
        for (const b of humanGates) {
          if (asks.length >= 2) break;
          let q = decisionQuestion(b);
          if (!q) { const id = blockedId(b); const t = id ? await title(id, 45) : ""; q = t ? `the work on ${t}` : ""; }
          const key = q.toLowerCase().replace(/[^a-z0-9]+/g, " ").trim();
          if (!q || seen.has(key)) continue;
          seen.add(key); asks.push(q);
        }
        const oldestMs = humanGates.length ? now - Math.min(...humanGates.map((b: any) => Date.parse(String(b.created_at)) || now)) : 0;
        const stale = humanGates.length > 10;

        // ---- mail: only what is for the owner; a big pile of notices is not news
        const mailItems = itemsOf(mail);
        const flagged = mailItems.filter(isFlaggedMail);
        const unread = typeof mail?.total === "number" ? mail.total : mailItems.length;

        // ---- what finished (tasks, not automation), by title
        const closed = itemsOf(events).filter((e: any) => /bead\.closed/i.test(String(e.type)));
        const real = closed.filter((e: any) => { const b = e?.payload?.bead ?? {}; const t = String(b.title ?? "").trim(); return /\s/.test(t) && !AUTOMATED_TITLE.test(t) && (!b.issue_type || TASK_TYPES.has(String(b.issue_type))); });
        const finishedTitles = [...new Set(real.map((e: any) => spokenTitle(String(e?.payload?.bead?.title ?? "").replace(/[/]+/g, " "), 40)).filter(Boolean))];

        // ---- attention, in this order: decisions, agents waiting for input, failed or stuck work, mail for the owner
        const quarantined = Number(status?.agents?.quarantined ?? 0);
        const attention: { kind: string; count: number; items: string[] }[] = [];
        if (humanGates.length) attention.push({ kind: "decision", count: humanGates.length, items: asks });
        if (waiting.length) attention.push({ kind: "waiting_for_input", count: waiting.length, items: waiting.map(spokenName) });
        const stuckCount = Math.max(stuck.length, quarantined);
        if (stuckCount) attention.push({ kind: "stuck", count: stuckCount, items: stuck.map(spokenName) });
        if (flagged.length) attention.push({ kind: "mail", count: flagged.length, items: flagged.slice(0, 2).map((m: any) => `${spokenTitle(m.subject, 40)} from ${spokenName({ id: "", alias: String(m.from ?? "") })}`) });

        // ---- sentences, in order: needs you, who is working, what finished, mail (only if useful)
        const needsFor = (nAsks: number) => attention.map((a) => {
          if (a.kind === "decision") {
            const list = a.items.slice(0, nAsks).join("; ");
            return stale ? `${a.count} open decisions, but many look stale: the oldest is ${days(oldestMs)} old${list ? `. Newest: ${list}` : ""}`
              : `${plural(a.count, "decision")}${list ? `, newest first: ${list}` : ""}`;
          }
          if (a.kind === "waiting_for_input") return `${plural(a.count, "agent")} waiting for input${a.items.length ? ` (${[...new Set(a.items)].slice(0, 2).join(", ")})` : ""}`;
          if (a.kind === "stuck") return `${a.count} stuck${a.items.length ? ` (${[...new Set(a.items)].slice(0, 2).join(", ")})` : ""}`;
          return `mail about ${a.items[0]}`;
        });
        const needs = (n: number) => attention.length ? `Needs you: ${needsFor(n).join("; ")}.` : (gates && pend && sess ? "Nothing needs you right now." : "");
        const workLine = (n: number) => {
          if (!sess) return "";
          if (!working.length) return counts.idle ? "Nobody is working right now." : "";
          const xs = shown.slice(0, n).map((w) => (w.title ? `${spokenName(w.s)} on ${w.title}` : spokenName(w.s)));
          const rest = working.length - xs.length;
          return `${working.length === 1 ? "1 agent is" : `${working.length} agents are`} working: ${rest > 0 ? `${xs.join(", ")} and ${rest} more` : andList(xs)}.`;
        };
        const lead = last === undefined ? "In the last 12 hours" : `Since your last briefing ${howLong(windowMs)} ago`;
        const doneLine = (n: number) => {
          if (!events) return "";
          if (!real.length) return `${lead}, nothing finished.`;
          const names = finishedTitles.slice(0, n);
          return `${lead}, ${plural(real.length, "task")} finished${names.length ? `, including ${andList(names)}` : ""}.`;
        };
        const unavailPhrase = unavailable.length ? `I couldn't read ${andList(unavailable.map((u) => `the ${u}`))}` : "";
        const mailLine = [mail && !flagged.length && unread > 0 ? (unread > 50 ? "A lot of unread notices, nothing flagged for you" : `${plural(unread, "unread mail", "unread mail")}`) : "", unavailPhrase].filter(Boolean).join("; ");
        const build = (nAsks: number, nWork: number, nDone: number, withMail: boolean, withDone: boolean) =>
          [needs(nAsks), workLine(nWork), withDone ? doneLine(nDone) : "", withMail && mailLine ? `${mailLine}.` : ""].filter(Boolean);
        let text = "";
        for (const [a2, w, f, m, dn] of [[2, 3, 2, true, true], [2, 3, 2, false, true], [2, 2, 1, false, true], [1, 2, 0, false, true], [1, 1, 0, false, true], [1, 1, 0, false, false], [0, 1, 0, false, false]] as const) {
          text = build(a2, w, f, m, dn).join(" ");
          if (text.length <= BRIEFING_MAX) break;
        }
        if (!text) text = `I couldn't read ${andList(unavailable.map((u) => `the ${u}`))}, so I can't tell how things are.`;
        if (text.length > BRIEFING_MAX) text = clip(text.slice(0, BRIEFING_MAX - 1), BRIEFING_MAX).replace(/[\s,;:(]+\S*$/, "") + ".";

        return { text, structured: { counts, attention, busiest: shown.map((w) => ({ name: spokenName(w.s), title: w.title })), finished: { count: real.length, titles: finishedTitles.slice(0, 5), capped: closed.length >= 100 },
          mail: { unread: mail ? unread : undefined, flagged: flagged.length }, decisions: { open: humanGates.length, oldest_ms: oldestMs, stale }, unavailable, since_ms: windowMs,
          first_briefing: last === undefined, untrusted: true } };
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
