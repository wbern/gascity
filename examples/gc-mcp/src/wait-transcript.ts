import {GcClient, GcError, TRANSCRIPT_MAX_BYTES} from './gc-client.js';
export interface TranscriptTurn { id?: string; role?: string; text?: string; timestamp?: string }

/** Per-call optimization only: never share cursors or transcript text between principals.
 * Keep the anchor BEFORE the earliest relevant output, and retain one preceding entry
 * so an in-progress tail can be re-read. Never advance it on polling: that would skip
 * edits and change lost-response replay semantics. This is not a lossless stream. */
export function waitTranscript(gc: GcClient, id: string, since: number) {
 let anchor: string | undefined;
 return async (timeoutMs: number, signal?: AbortSignal): Promise<TranscriptTurn[]> => {
  const page: any = await gc.get(`/session/${encodeURIComponent(id)}/transcript`,
   {format:'conversation',tail:1,after:anchor},timeoutMs,TRANSCRIPT_MAX_BYTES,signal);
  const turns: TranscriptTurn[] = Array.isArray(page.turns) ? page.turns : [];
  if (anchor && (page.id !== id || page.pagination?.has_newer_messages === true || turns.some(t => t.id === anchor)))
   throw new GcError(409,'Transcript cursor continuity is unknown. Do not resend.');
  if (!anchor && page.id === id && page.pagination?.has_newer_messages !== true && turns.length >= 2 &&
      turns.every(t => typeof t.id === 'string' && t.id.length > 0 && t.id.length <= 2048) &&
      new Set(turns.map(t => t.id)).size === turns.length) {
   const first = turns.findIndex(t => Date.parse(String(t.timestamp ?? '')) > since);
   const before = (first < 0 ? turns.length : first) - 2;
   if (before >= 0) anchor = turns[before].id;
  }
  return turns.filter(t => String(t.text ?? '').trim() !== '' && !/^\s*\[[A-Za-z_ -]{1,24}\]\s*$/.test(String(t.text ?? '')));
 };
}
