// Minimal read-only client for the Gas City supervisor API (loopback only).
// GET-only by construction: there is no method that can mutate city state.
export class GcError extends Error {
  constructor(public status: number, message: string) {
    super(message);
  }
}

export interface GcClientOpts {
  baseUrl: string; // e.g. http://127.0.0.1:8372
  city: string;
  timeoutMs?: number;
  fetchImpl?: typeof fetch;
}

export class GcClient {
  constructor(private o: GcClientOpts) {
    const host = new URL(o.baseUrl).hostname;
    if (!["127.0.0.1", "localhost", "::1", "[::1]", "x"].includes(host)) {
      throw new Error(`gc-mcp only talks to a loopback supervisor, refusing ${host}`);
    }
  }

  async get<T = unknown>(path: string, query: Record<string, string | number | undefined> = {}): Promise<T> {
    const u = new URL(`${this.o.baseUrl}/v0/city/${encodeURIComponent(this.o.city)}${path}`);
    for (const [k, v] of Object.entries(query)) if (v !== undefined) u.searchParams.set(k, String(v));
    const f = this.o.fetchImpl ?? fetch;
    const ctl = new AbortController();
    const timer = setTimeout(() => ctl.abort(), this.o.timeoutMs ?? 8000);
    try {
      const res = await f(u.toString(), { method: "GET", headers: { accept: "application/json" }, signal: ctl.signal });
      const text = await res.text();
      if (!res.ok) {
        let detail = res.statusText;
        try { detail = (JSON.parse(text) as { detail?: string }).detail ?? detail; } catch { /* keep */ }
        throw new GcError(res.status, detail);
      }
      return JSON.parse(text) as T;
    } catch (e) {
      if (e instanceof GcError) throw e;
      throw new GcError(0, (e as Error).name === "AbortError" ? "timed out" : (e as Error).message);
    } finally {
      clearTimeout(timer);
    }
  }
}
