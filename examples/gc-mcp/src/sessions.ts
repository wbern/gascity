// Resolving what a person SAID ("steve", "the mayor", "crm slash steve") to one live session, and refusing to guess.
// Exact match first, then a unique case-insensitive prefix, suffix or contains match against every identifier of every session.
// More than one candidate at the deciding tier, or none: the caller gets a short speakable question and must NOT act.
import type { GcClient } from "./gc-client.js";
import { clip, speakName } from "./text.js";

export interface SessionRef {
  id: string; alias?: string; session_name?: string; template?: string; agent_name?: string;
  state?: string; running?: boolean; attached?: boolean; [k: string]: unknown;
}
const QUERY = /^[\p{L}\p{N} .:/_-]{1,128}$/u;
const MIN_PARTIAL = 3;   // a 1-2 letter fragment is never a partial match

/** Spoken and typed forms compare equal: case, "slash", and every separator collapse to one hyphen. */
export function normalize(s: string): string {
  return s.toLowerCase().trim().replace(/\s+slash\s+/g, "/").replace(/^(the|my)\s+/, "").replace(/\s+(session|agent)$/, "").replace(/[\s/_.:-]+/g, "-").replace(/^-|-$/g, "");
}
export const identifiers = (s: SessionRef): string[] => [s.id, s.alias, s.session_name, s.template, s.agent_name].filter((x): x is string => !!x).map(String);
export const labelOf = (s: SessionRef): string => String(s.alias || s.template || s.id);

export type Resolution =
  | { kind: "one"; hit: SessionRef }
  | { kind: "many"; hits: SessionRef[] }
  | { kind: "none"; query: string }
  | { kind: "bad"; query: string }
  | { kind: "nodefault" }
  | { kind: "incomplete"; query: string };

export function resolveIn(items: SessionRef[], rawQuery: string): Resolution {
  const q = rawQuery.trim();
  if (!QUERY.test(q)) return { kind: "bad", query: q.slice(0, 40) };
  const key = normalize(q);
  if (!key) return { kind: "none", query: q };
  const keys = items.map((s) => ({ s, ks: identifiers(s).map(normalize).filter(Boolean) }));
  const tiers: ((k: string) => boolean)[] = [(k) => k === key];
  if (key.length >= MIN_PARTIAL) tiers.push((k) => k.startsWith(key), (k) => k.endsWith(key), (k) => k.includes(key));
  for (const test of tiers) {
    const hits = keys.filter((x) => x.ks.some(test)).map((x) => x.s);
    const uniq = [...new Map(hits.map((h) => [h.id, h])).values()];
    if (uniq.length === 1) return { kind: "one", hit: uniq[0] };
    if (uniq.length > 1) return { kind: "many", hits: uniq };
  }
  return { kind: "none", query: q };
}

/** A list read across pages, with what the API said about completeness passed through (never a silently truncated count). */
export interface Page<T> { items: T[]; total: number; partial: boolean; partial_errors: string[]; next_cursor?: string; truncated: boolean }
export async function listPages<T = any>(gc: GcClient, path: string, query: Record<string, string | number | undefined> = {}, maxPages = 5, perPage = 200, readBudgetMs?: () => number): Promise<Page<T>> {
  const items: T[] = [];
  let cursor: string | undefined, total: number | undefined, partial = false;
  const errors: string[] = [];
  for (let i = 0; i < maxPages; i++) {
    const r: any = await gc.get(path, { ...query, limit: perPage, cursor }, readBudgetMs?.());
    items.push(...((r?.items ?? r?.beads ?? []) as T[]));
    if (typeof r?.total === "number") total = r.total;
    if (r?.partial) { partial = true; errors.push(...((r.partial_errors ?? []) as string[]).map(String)); }
    cursor = r?.next_cursor || undefined;
    if (!cursor) break;
  }
  const t = total ?? items.length;
  return { items, total: t, partial, partial_errors: errors.slice(0, 5), next_cursor: cursor, truncated: !!cursor || t > items.length };
}

/** Resolve against the live session list. An empty query means the configured default session. */
export async function resolveSession(gc: GcClient, query: string | undefined, defaultSession?: string, readBudgetMs?: () => number): Promise<Resolution> {
  const q = (query ?? "").trim() || (defaultSession ?? "").trim();
  if (!q) return { kind: "nodefault" };
  if (!QUERY.test(q)) return { kind: "bad", query: q.slice(0, 40) };   // before any call: an unsafe name never leaves the process
  const r = await listPages<SessionRef>(gc, "/sessions", {}, 5, 200, readBudgetMs);          // every page (up to 1000 sessions), not just the first 200
  if (r.partial || r.truncated) return { kind: "incomplete", query: q };
  return resolveIn(r.items, q);
}

/** The short question asked when more than one session fits: "I found crm slash steve and crm slash bob, which one?" */
export function disambiguation(hits: SessionRef[]): string {
  const names = hits.slice(0, 4).map((h) => speakName(clip(labelOf(h), 40)));
  const more = hits.length > 4 ? ` and ${hits.length - 4} more` : "";
  const list = names.length > 1 ? `${names.slice(0, -1).join(", ")} and ${names[names.length - 1]}` : names[0];
  return `I found ${list}${more}, which one?`;
}

// ---- speaking a session: never by id
export const UUID = /\b[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\b/gi;
export const WISP = /\b[a-z0-9]+-wisp-[a-z0-9]+\b/gi;
export const SHA = /\b(?=[0-9a-f]*\d)(?=[0-9a-f]*[a-f])[0-9a-f]{7,40}\b/gi;
export const ID_TOKEN = /\b(?:gc[a-z0-9]{0,3}|[a-z]{2,4})-(?=[a-z0-9]*\d)[a-z0-9]{4,12}\b/gi;
const looksLikeId = (s: string) => { UUID.lastIndex = WISP.lastIndex = ID_TOKEN.lastIndex = 0; return UUID.test(s) || WISP.test(s) || ID_TOKEN.test(s); };

/** "crm/steve" is said "crm steve"; a session with no name of its own is never announced by its id. */
export function spokenName(s: SessionRef): string {
  const label = String(s.alias || s.template || s.agent_name || "").replace(/[0-9a-f]{10,}/gi, "").trim();
  if (!label || looksLikeId(label)) return "an unnamed agent";
  return clip(label.replace(/[/_.-]+/g, " ").replace(/\s+/g, " ").trim(), 40);
}
