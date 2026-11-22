// WHO MAY READ WHAT. gc3 is the owner's machine: reading is by scope only, so gc:read reads ANY session. There are no default lists and no opinion about
// whose agent is whose. An operator MAY configure lists; when one refuses a read, the answer says so in one speakable sentence with a machine reason.
//
// Order: (1) the optional READ deny list (default: the write deny list, which is empty by default); (2) the optional peek allow list (default: everything).
import { identifiers, spokenName, type SessionRef } from "./sessions.js";
import { DEFAULT_WRITE_DENY, globMatch, type WriteCtx } from "./writes.js";
import type { Deps } from "./tools.js";

export type ReadReason = "denied_session" | "not_in_allow_list";
export type Verdict = { ok: true } | { ok: false; reason: ReadReason; name: string; text: string; structured: Record<string, unknown> };

const matches = (list: string[], hit: SessionRef) => identifiers(hit).some((n) => list.some((p) => globMatch(p, n)));

/** The effective read deny list: explicit, else the write deny list, else none. */
export const readDenyList = (d: Deps): string[] => d.readDeny ?? d.writes?.denyList ?? DEFAULT_WRITE_DENY;
export const isReadDenied = (d: Deps, hit: SessionRef): boolean => matches(readDenyList(d), hit);

export function readVerdict(d: Deps, _who: WriteCtx | undefined, hit: SessionRef): Verdict {
  const name = spokenName(hit);
  const refuse = (reason: ReadReason, text: string): Verdict => ({ ok: false, reason, name, text, structured: { found: true, allowed: false, reason, name } });
  if (isReadDenied(d, hit)) {
    const alsoWrite = matches(d.writes?.denyList ?? DEFAULT_WRITE_DENY, hit);
    return refuse("denied_session", `${name} is on the protected list, so I will not read${alsoWrite ? " or write" : ""} it. Ask the owner to change the list if you want that.`);
  }
  if (d.peekAllow?.length && !d.peekAllow.some((x) => [hit.id, hit.alias, hit.template].includes(x))) {
    return refuse("not_in_allow_list", `${name} is not on the list of agents I may read. Ask the owner to add it.`);
  }
  return { ok: true };
}
