// WHO MAY READ WHAT, decided in ONE place (every tool that reads a session goes through readVerdict), and always answered with the true reason:
// a person asking "read zachris gemini 2" must hear that it belongs to someone else and is on the protected list, never a vague "not allowed".
//
// Order: (1) the READ deny list (default: the same list as the write deny list; GC_MCP_READ_DENY_SESSIONS / gc_mcp_read_deny_sessions overrides it for reads only);
// (2) no peek allow list set, or the session is on it, or it is the owner's default session; (3) a token holding gc:write may read what the write allow list
// permits while writes are active. Otherwise the reason is no_scope, writes_disabled or not_in_allow_list.
import { identifiers, spokenName, type SessionRef } from "./sessions.js";
import { DEFAULT_WRITE_DENY, WRITE_SCOPE, globMatch, type WriteCtx } from "./writes.js";
import type { Deps } from "./tools.js";

export type ReadReason = "denied_session" | "not_in_allow_list" | "no_scope" | "writes_disabled";
export type Verdict = { ok: true } | { ok: false; reason: ReadReason; name: string; text: string; structured: Record<string, unknown> };

const matches = (list: string[], hit: SessionRef) => identifiers(hit).some((n) => list.some((p) => globMatch(p, n)));
const isDefault = (d: Deps, hit: SessionRef) => { const q = (d.defaultSession ?? "").trim().toLowerCase(); return !!q && identifiers(hit).some((x) => x.toLowerCase() === q); };

/** The effective read deny list: explicit, else the write deny list, else the built-in one. */
export const readDenyList = (d: Deps): string[] => d.readDeny ?? d.writes?.denyList ?? DEFAULT_WRITE_DENY;
export const isReadDenied = (d: Deps, hit: SessionRef): boolean => matches(readDenyList(d), hit);

export function readVerdict(d: Deps, who: WriteCtx | undefined, hit: SessionRef): Verdict {
  const name = spokenName(hit);
  const refuse = (reason: ReadReason, text: string): Verdict => ({ ok: false, reason, name, text, structured: { found: true, allowed: false, reason, name } });
  if (isReadDenied(d, hit)) {
    const alsoWrite = matches(d.writes?.denyList ?? DEFAULT_WRITE_DENY, hit);
    return refuse("denied_session", `${name} belongs to someone else, so it is on the protected list and I will not read${alsoWrite ? " or write" : ""} it. Ask the owner to change the list if you want that.`);
  }
  if (!d.peekAllow || d.peekAllow.some((x) => [hit.id, hit.alias, hit.template].includes(x)) || isDefault(d, hit)) return { ok: true };
  const hasWrite = !!who?.scopes.includes(WRITE_SCOPE);
  if (hasWrite && d.writes?.active && d.writes.allowListOk(hit)) return { ok: true };
  if (!hasWrite) return refuse("no_scope", `${name} is not on my read list, and this connection can only read the listed agents. Reconnect with the gc:write scope, or ask the owner to add it.`);
  if (!d.writes?.active) return refuse("writes_disabled", `${name} is not on my read list, and writes are switched off right now, which is what lets a connection read beyond the list. Ask the owner to add it or switch writes back on.`);
  return refuse("not_in_allow_list", `${name} is not on the list of agents I may read or write. Ask the owner to add it.`);
}
