// Shared text helpers for every tool: clip, untrusted-content cleaning, and the guard that maps supervisor errors to speakable text.
import { ReplyStateError } from "./reply-state.js";
import { GcError } from "./gc-client.js";
import { redactDeep, redactText } from "./redact.js";

export interface ToolResult { text: string; structured: Record<string, unknown> }
export const clip = (s: string, n: number) => (s.length > n ? s.slice(0, n - 1).trimEnd() + "…" : s);
// Free text from transcripts/mail/beads is attacker-influenceable: redact FIRST (a clipped
// secret can fall under a pattern threshold), drop links, collapse whitespace, then clip.
export const clean = (s: unknown) => redactText(String(s ?? "")).replace(/https?:\/\/\S+/gi, "[link]").replace(/\s+/g, " ").trim();
export const untrusted = (s: unknown, n: number) => clip(clean(s), n);
export const plural = (n: number, w: string) => `${n} ${w}${n === 1 ? "" : "s"}`;
/** Say a name aloud: "crm/steve" is read "crm slash steve". */
export const speakName = (s: string) => s.replace(/\//g, " slash ");

/** One honest sentence per kind of failure, never "unavailable" for everything. */
export function gcErrorResult(e: GcError): ToolResult {
  const detail = redactText(e.message).slice(0, 120);
  const text = e.kind === "not_found" ? "Gas City has no such thing: it was not found."
    : e.kind === "too_large" ? "That is too large to read in one go, so I did not read it."
    : e.kind === "timeout" ? "Gas City took too long to answer, so I stopped waiting."
    : e.kind === "unavailable" ? `Gas City is not responding right now${e.status ? ` (${e.status})` : ""}.`
    : `Gas City refused that (${e.status}: ${detail}).`;
  return { text, structured: { error: true, kind: e.kind, status: e.status } };
}

// Every handler goes through guard(): redaction + speakable error mapping.
// eslint-disable-next-line @typescript-eslint/no-explicit-any
export function guard(fn: (a: any) => Promise<ToolResult>): (a: any) => Promise<ToolResult> {
  return async (args) => {
    try {
      const r = await fn(args);
      return { text: redactText(r.text), structured: redactDeep(r.structured) };
    } catch (e) {
      if (e instanceof GcError) return gcErrorResult(e);
      if (e instanceof ReplyStateError) return { text: "Reply tracking is unavailable, so automatic continuation stopped. Do not resend automatically.", structured: { error: true, kind: "reply_state_unavailable", automatic_retry: false, completion: "not_proven" } };
      throw e;
    }
  };
}
