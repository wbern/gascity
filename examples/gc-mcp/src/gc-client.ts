// Minimal client for the Gas City supervisor API (loopback only).
// Reads are GET-only. The ONLY mutations it can ever send are the two operations in WRITE_OPS below
// (submit a message to a session, respond to a pending interaction): a fixed table of method + path,
// a validated session id, a size limit, no redirects. There is no generic post/put/delete and no way to
// reach stop, kill, reset, close, config, holds, beads or anything else; tests pin this.
/** What went wrong, split so a person is told the truth: "not found" is not "too large" is not "Gas City is down". */
export type GcErrorKind = "not_found" | "too_large" | "timeout" | "unavailable" | "refused";
export class GcError extends Error {
  readonly kind: GcErrorKind;
  constructor(public status: number, message: string, kind?: GcErrorKind) {
    super(message);
    this.kind = kind ?? (status === 404 ? "not_found" : status === 413 ? "too_large" : status === 0 || status >= 500 ? "unavailable" : "refused");
  }
}

export interface GcClientOpts {
  baseUrl: string; // e.g. http://127.0.0.1:8372
  city: string;
  timeoutMs?: number;
  maxBytes?: number;
  fetchImpl?: typeof fetch; // tests inject this; the host check still applies
}

/** The complete list of mutating operations this client can send. Anything not here cannot be sent. */
export const WRITE_OPS = {
  submit: { suffix: "submit", operationId: "submit-session" },
  respond: { suffix: "respond", operationId: "respond-session" },
} as const;
export type WriteOp = keyof typeof WRITE_OPS;
/** A transcript segment can be large; it is read with a higher cap than other calls, and a bigger one is reported as "too large" (never as "Gas City is down"). */
export const TRANSCRIPT_MAX_BYTES = 8_000_000;
export const MAX_WRITE_BYTES = 8_000;                      // request body cap (text is further capped at 4000 chars by the caller)
const SESSION_ID = /^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$/;  // the id the API returned for the session; never free-form input
const LOOPBACK = new Set(["127.0.0.1", "localhost", "::1", "[::1]"]);

export class GcClient {
  constructor(private o: GcClientOpts) {
    const host = new URL(o.baseUrl).hostname;
    if (!LOOPBACK.has(host)) throw new Error(`gc-mcp only talks to a loopback supervisor, refusing ${host}`);
  }

  /** The only mutation entry point: a fixed operation on one session. The id is validated, the body is capped. */
  async write(op: WriteOp, sessionId: string, body: Record<string, unknown>, idempotencyKey: string): Promise<unknown> {
    const def = WRITE_OPS[op];
    if (!def) throw new Error("operation not allowed");
    if (!SESSION_ID.test(sessionId)) throw new Error("invalid session id");
    if (!/^[A-Za-z0-9-]{8,64}$/.test(idempotencyKey)) throw new Error("invalid idempotency key");
    const payload = JSON.stringify(body);
    if (payload.length > MAX_WRITE_BYTES) throw new GcError(0, "request too large");
    const url = `${this.o.baseUrl}/v0/city/${encodeURIComponent(this.o.city)}/session/${encodeURIComponent(sessionId)}/${def.suffix}`;
    return this.send("POST", url, payload, { "content-type": "application/json", "x-gc-request": "gc-mcp", "idempotency-key": idempotencyKey });
  }

  async get<T = unknown>(path: string, query: Record<string, string | number | boolean | string[] | undefined> = {}, timeoutMs?: number, maxBytes?: number): Promise<T> {
    const u = new URL(`${this.o.baseUrl}/v0/city/${encodeURIComponent(this.o.city)}${path}`);
    for (const [k, v] of Object.entries(query)) if (v !== undefined) {
      if (Array.isArray(v)) for (const item of v) u.searchParams.append(k, item);
      else u.searchParams.set(k, String(v));
    }
    return (await this.send("GET", u.toString(), undefined, {}, timeoutMs, maxBytes)) as T;
  }

  private async send(method: "GET" | "POST", url: string, body?: string, extra: Record<string, string> = {}, timeoutMs?: number, maxBytesOverride?: number): Promise<unknown> {
    const f = this.o.fetchImpl ?? fetch;
    const max = maxBytesOverride ?? this.o.maxBytes ?? 2_000_000;
    const ctl = new AbortController();
    const timer = setTimeout(() => ctl.abort(), timeoutMs ?? this.o.timeoutMs ?? 8000);
    try {
      // redirect:"error": a loopback API has no business redirecting us anywhere.
      const res = await f(url, { method, headers: { accept: "application/json", ...extra }, body, signal: ctl.signal, redirect: "error" });
      const declared = Number(res.headers.get("content-length") ?? 0);
      if (declared > max) throw new GcError(0, "response too large", "too_large");
      const text = await res.text();
      if (text.length > max) throw new GcError(0, "response too large", "too_large");
      if (!res.ok) {
        let detail = res.statusText;
        try { detail = (JSON.parse(text) as { detail?: string }).detail ?? detail; } catch { /* keep */ }
        throw new GcError(res.status, detail);
      }
      return JSON.parse(text);
    } catch (e) {
      if (e instanceof GcError) throw e;
      throw (e as Error).name === "AbortError" ? new GcError(0, "timed out", "timeout") : new GcError(0, (e as Error).message, "unavailable");
    } finally {
      clearTimeout(timer);
    }
  }
}
