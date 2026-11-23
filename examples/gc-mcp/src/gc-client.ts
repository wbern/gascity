// Loopback supervisor transport. Mutations are a fixed table; no arbitrary verb or URL.
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
  submit: { path: "/session/{id}/submit", operationId: "submit-session" },
  respond: { path: "/session/{id}/respond", operationId: "respond-session" },
  mail_send: { path: "/mail", operationId: "send-mail" },
  mail_reply: { path: "/mail/{id}/reply", operationId: "reply-mail" },
  mail_mark_read: { path: "/mail/{id}/read", operationId: "post-v0-city-by-city-name-mail-by-id-read" },
  bead_create: { path: "/beads", operationId: "create-bead" },
  bead_sling: { path: "/sling", operationId: "post-v0-city-by-city-name-sling" },
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

  /** The only mutation entry point: a fixed operation, validated id, capped body. */
  async write(op: WriteOp, sessionId: string, body: Record<string, unknown>, idempotencyKey: string): Promise<unknown> {
    const def = Object.hasOwn(WRITE_OPS, op) ? WRITE_OPS[op] : undefined;
    if (!def) throw new Error("operation not allowed");
    if (!SESSION_ID.test(sessionId)) throw new Error("invalid session id");
    if (!/^[A-Za-z0-9-]{8,64}$/.test(idempotencyKey)) throw new Error("invalid idempotency key");
    const payload = JSON.stringify(body);
    if (Buffer.byteLength(payload, "utf8") > MAX_WRITE_BYTES) throw new GcError(0, "request too large");
    const url = `${this.o.baseUrl}/v0/city/${encodeURIComponent(this.o.city)}${def.path.replace("{id}", encodeURIComponent(sessionId))}`;
    return this.send("POST", url, payload, { "content-type": "application/json", "x-gc-request": "gc-mcp", "idempotency-key": idempotencyKey });
  }

  async get<T = unknown>(path: string, query: Record<string, string | number | boolean | string[] | undefined> = {}, timeoutMs?: number, maxBytes?: number, signal?: AbortSignal): Promise<T> {
    const u = new URL(`${this.o.baseUrl}/v0/city/${encodeURIComponent(this.o.city)}${path}`);
    for (const [k, v] of Object.entries(query)) if (v !== undefined) {
      if (Array.isArray(v)) for (const item of v) u.searchParams.append(k, item);
      else u.searchParams.set(k, String(v));
    }
    return (await this.send("GET", u.toString(), undefined, {}, timeoutMs, maxBytes, signal)) as T;
  }

  private async send(method: "GET" | "POST", url: string, body?: string, extra: Record<string, string> = {}, timeoutMs?: number, maxBytesOverride?: number, signal?: AbortSignal): Promise<unknown> {
    const f = this.o.fetchImpl ?? fetch;
    const max = maxBytesOverride ?? this.o.maxBytes ?? 2_000_000;
    const ctl = new AbortController();
    const abort = () => ctl.abort();
    signal?.addEventListener("abort", abort, { once: true });
    if (signal?.aborted) ctl.abort();
    const timer = setTimeout(() => ctl.abort(), timeoutMs ?? this.o.timeoutMs ?? 8000);
    try {
      // redirect:"error": a loopback API has no business redirecting us anywhere.
      const res = await f(url, { method, headers: { accept: "application/json", ...extra }, body, signal: ctl.signal, redirect: "error" });
      const declared = Number(res.headers.get("content-length") ?? 0);
      if (declared > max) {
        ctl.abort(); void res.body?.cancel().catch(() => {});
        throw new GcError(0, "response too large", "too_large");
      }
      // Bound actual UTF-8 bytes while reading, including chunked responses. A
      // stalled body must obey the same deadline/cancellation as the fetch.
      let text = "", received = 0;
      const reader = res.body?.getReader();
      if (reader) {
        const decoder = new TextDecoder();
        const cancelBody = () => { void reader.cancel().catch(() => {}); };
        ctl.signal.addEventListener("abort", cancelBody, { once: true });
        if (ctl.signal.aborted) cancelBody();
        try {
          for (;;) {
            if (ctl.signal.aborted) throw new DOMException("aborted", "AbortError");
            const { done, value } = await reader.read();
            if (ctl.signal.aborted) throw new DOMException("aborted", "AbortError");
            if (done) break;
            received += value.byteLength;
            if (received > max) {
              ctl.abort();
              throw new GcError(0, "response too large", "too_large");
            }
            text += decoder.decode(value, { stream: true });
          }
          text += decoder.decode();
        } finally {
          ctl.signal.removeEventListener("abort", cancelBody);
          reader.releaseLock();
        }
      }
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
      signal?.removeEventListener("abort", abort);
    }
  }
}
