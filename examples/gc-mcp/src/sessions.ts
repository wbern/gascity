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
  | { kind: "nodefault" };

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

/** Resolve against the live session list. An empty query means the configured default session. */
export async function resolveSession(gc: GcClient, query: string | undefined, defaultSession?: string): Promise<Resolution> {
  const q = (query ?? "").trim() || (defaultSession ?? "").trim();
  if (!q) return { kind: "nodefault" };
  if (!QUERY.test(q)) return { kind: "bad", query: q.slice(0, 40) };   // before any call: an unsafe name never leaves the process
  const r = (await gc.get("/sessions", { limit: 200 })) as { items?: SessionRef[] };
  return resolveIn(r.items ?? [], q);
}

/** The short question asked when more than one session fits: "I found crm slash steve and crm slash bob, which one?" */
export function disambiguation(hits: SessionRef[]): string {
  const names = hits.slice(0, 4).map((h) => speakName(clip(labelOf(h), 40)));
  const more = hits.length > 4 ? ` and ${hits.length - 4} more` : "";
  const list = names.length > 1 ? `${names.slice(0, -1).join(", ")} and ${names[names.length - 1]}` : names[0];
  return `I found ${list}${more}, which one?`;
}
