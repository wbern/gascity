// Bounded reply continuity metadata. No request/reply bodies or credentials belong here.
import crypto from "node:crypto";
import { z } from "zod";
import { loadPrivateJson, savePrivateJson, ReplyStateError } from "./private-json.js";
export { ReplyStateError } from "./private-json.js";

const record = z.object({
  handle: z.string().regex(/^h[0-9a-f-]{36}$/),
  client: z.string().min(1).max(256),
  session: z.string().regex(/^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$/),
  since: z.number().int().nonnegative(),
  expiresAt: z.number().int().positive(),
  firstReplyId: z.string().regex(/^[a-f0-9]{12}$/).optional(),
  cancelledAt: z.number().int().nonnegative().optional(),
}).strict();
const snapshot = z.object({ version: z.literal(1), handles: z.array(record).max(500) }).strict();
type Entry = z.infer<typeof record>;
export interface SendMarkerOptions { file?: string; now?: () => number; ttlMs?: number; maxHandles?: number }

/** One server process owns this file. Expiry does not renew on reads or candidate pins. */
export class SendMarkers {
  private handles = new Map<string, Entry>();
  private m = new Map<string, { ms: number; expiresAt: number }>();
  private failed = false;
  private cancelListeners = new Map<string, Set<() => void>>();
  private now: () => number;
  private ttlMs: number;
  private maxHandles: number;
  constructor(private o: SendMarkerOptions = {}) {
    this.now = o.now ?? Date.now;
    this.ttlMs = o.ttlMs ?? 86_400_000;
    this.maxHandles = o.maxHandles ?? 500;
    if (!Number.isInteger(this.ttlMs) || this.ttlMs < 1 || this.ttlMs > 7 * 86_400_000 || !Number.isInteger(this.maxHandles) || this.maxHandles < 1 || this.maxHandles > 500) throw new ReplyStateError("Invalid reply state bounds");
    if (!o.file) return;
    const raw = loadPrivateJson(o.file);
    if (raw === undefined) return;
    try {
      const saved = snapshot.parse(raw);
      for (const entry of saved.handles) {
        if (this.handles.has(entry.handle)) throw new Error("duplicate handle");
        this.handles.set(entry.handle, entry);
      }
    } catch { throw new ReplyStateError("Invalid reply state; refusing to discard continuity metadata"); }
    this.prune();
    for (const e of this.handles.values()) {
      const key = `${e.client}|${e.session}`;
      if ((this.m.get(key)?.ms ?? -1) < e.since) this.m.set(key, { ms: e.since, expiresAt: e.expiresAt });
    }
  }
  get persistent(): boolean { return !!this.o.file && !this.failed; }
  private healthy() { if (this.failed) throw new ReplyStateError("Reply state is unavailable; automatic continuation is disabled"); }
  private save(next: Map<string, Entry>) {
    this.healthy();
    if (this.o.file) {
      try { savePrivateJson(this.o.file, { version: 1, handles: [...next.values()] }); }
      catch (e) { this.failed = true; for (const h of [...this.cancelListeners.keys()]) this.notifyCancellation(h); throw e; }
    }
    const previous = this.handles;
    this.handles = next;
    for (const [h, old] of previous) if (!next.has(h) || (old.cancelledAt === undefined && next.get(h)?.cancelledAt !== undefined)) this.notifyCancellation(h);
  }
  private notifyCancellation(handle: string) {
    const listeners = this.cancelListeners.get(handle);
    this.cancelListeners.delete(handle);
    for (const listener of listeners ?? []) { try { listener(); } catch { /* other waits must also be notified */ } }
  }
  watchCancellation(client: string, handle: string, listener: () => void): () => void {
    const entry = this.resolve(client, handle);
    if (!entry || entry.cancelledAt !== undefined) { listener(); return () => {}; }
    const listeners = this.cancelListeners.get(handle) ?? new Set<() => void>();
    if (listeners.size >= 16) throw new ReplyStateError("Too many concurrent waits for this handle");
    listeners.add(listener); this.cancelListeners.set(handle, listeners);
    return () => { listeners.delete(listener); if (!listeners.size && this.cancelListeners.get(handle) === listeners) this.cancelListeners.delete(handle); };
  }
  cancelWait(client: string, handle: string): boolean {
    this.prune();
    const entry = this.handles.get(handle);
    if (!entry || entry.client !== client) return false;
    if (entry.cancelledAt !== undefined) return true;
    const next = new Map(this.handles); next.set(handle, record.parse({ ...entry, cancelledAt: this.now() }));
    this.save(next); return true;
  }
  private prune() {
    this.healthy();
    const now = this.now(), next = new Map(this.handles);
    for (const [h, e] of next) if (e.expiresAt <= now) next.delete(h);
    while (next.size > this.maxHandles) next.delete(next.keys().next().value!);
    for (const [k, e] of this.m) if (e.expiresAt <= now) this.m.delete(k);
    if (next.size !== this.handles.size) this.save(next);
  }
  create(client: string, session: string, since: number): string {
    this.prune();
    const handle = `h${crypto.randomUUID()}`, expiresAt = this.now() + this.ttlMs;
    const entry = record.parse({ handle, client, session, since, expiresAt });
    const next = new Map(this.handles); next.set(handle, entry);
    while (next.size > this.maxHandles) next.delete(next.keys().next().value!);
    this.save(next);
    this.set(client, session, since);
    return handle;
  }
  resolve(client: string, handle: string) {
    this.prune();
    const entry = this.handles.get(handle);
    return entry?.client === client ? { session: entry.session, since: entry.since, firstReplyId: entry.firstReplyId, cancelledAt: entry.cancelledAt } : undefined;
  }
  pinFirstReply(client: string, handle: string, id: string): string | undefined {
    this.prune();
    const entry = this.handles.get(handle);
    if (!entry || entry.client !== client) return undefined;
    if (entry.firstReplyId) return entry.firstReplyId;
    const pinned = record.parse({ ...entry, firstReplyId: id });
    const next = new Map(this.handles); next.set(handle, pinned);
    this.save(next);
    return id;
  }
  set(client: string, session: string, ms: number) {
    this.healthy();
    this.m.set(`${client}|${session}`, { ms, expiresAt: this.now() + this.ttlMs });
    if (this.m.size > 500) this.m.delete(this.m.keys().next().value!);
  }
  get(client: string, session: string): number | undefined {
    this.prune();
    return this.m.get(`${client}|${session}`)?.ms;
  }
}
