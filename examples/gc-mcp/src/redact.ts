// Redaction applied to EVERY value leaving the server. Conservative: it is
// better to speak "[redacted]" than to read a credential aloud. Callers must
// redact BEFORE clipping so a truncated secret cannot slip under a threshold.
const R = "[redacted]";

const RULES: [RegExp, string][] = [
  [/-----BEGIN [A-Z ]*PRIVATE KEY-----[\s\S]*?(?:-----END [A-Z ]*PRIVATE KEY-----|$)/g, R],
  [/\b([a-z][a-z0-9+.-]*:\/\/)[^\s/:@]+:[^\s/@]+@/gi, `$1${R}@`],
  [/(x-vercel-protection-bypass=)[^\s&"']+/gi, `$1${R}`],
  [/\bBearer\s+[A-Za-z0-9._~+/=-]{12,}/gi, R],
  [/\bgh[pousr]_[A-Za-z0-9]{20,}/g, R],
  [/\bglpat-[A-Za-z0-9_-]{15,}/g, R],
  [/\bsk-[A-Za-z0-9_-]{16,}/g, R],
  [/\bsk_(?:live|test)_[A-Za-z0-9]{10,}/g, R],
  [/\bxox[abprs]-[A-Za-z0-9-]{10,}/g, R],
  [/\bnpm_[A-Za-z0-9]{30,}/g, R],
  [/\btskey-[A-Za-z0-9-]{10,}/g, R],
  [/\bAIza[0-9A-Za-z_-]{30,}/g, R],
  [/\bAKIA[0-9A-Z]{16}\b/g, R],
  [/\beyJ[A-Za-z0-9_-]{6,}\.[A-Za-z0-9_-]{6,}\.[A-Za-z0-9_-]{6,}/g, R],
  // KEY=value / key: value where the key name smells like a secret.
  [/\b([A-Za-z0-9_.-]*(?:secret|token|passw(?:or)?d|passwd|api[_-]?key|bypass|credential|private[_-]?key)[A-Za-z0-9_.-]*)(\s*[=:]\s*)("[^"]*"|'[^']*'|[^\s,;]+)/gi, `$1$2${R}`],
  // "the password is hunter2" phrasing
  [/\b(passw(?:or)?d|passphrase|secret|token|api key)\s+(?:is|was)\s+\S+/gi, `$1 ${R}`],
  [/\b[0-9a-fA-F]{32,}\b/g, R],
  // long mixed alnum runs (no '/', '.', so file paths and kebab ids are safe)
  [/(?<![\w/.-])(?=[A-Za-z0-9_+-]*[A-Za-z])(?=[A-Za-z0-9_+-]*\d)[A-Za-z0-9_+-]{40,}={0,2}(?![\w/.-])/g, R],
  // AWS-style 40-char secrets that contain '/' or '+' (need upper+lower+digit)
  [/(?<![\w/+=.-])(?=[A-Za-z0-9/+]*[a-z])(?=[A-Za-z0-9/+]*[A-Z])(?=[A-Za-z0-9/+]*\d)(?=[A-Za-z0-9]*[/+])[A-Za-z0-9/+]{40}(?![\w/+=.-])/g, R],
];

const SECRET_KEY = /secret|token|passw|api[_-]?key|bypass|credential|private[_-]?key|authorization/i;

export function redactText(s: string): string {
  let out = s;
  for (const [re, rep] of RULES) out = out.replace(re, rep);
  return out;
}

// Only this module can mint values trusted as slices of an already redacted
// complete document. Wire objects/markers cannot forge WeakMap identity.
const redactedValues = new WeakMap<object, string>();

/** JSON representation used to measure a redacted fragment before guard unwraps it. */
export interface RedactedSlice { toJSON(): string }
/** Immutable complete-document redaction, with context-preserving slices. */
export interface RedactedDocument extends RedactedSlice {
  readonly value: string;
  readonly length: number;
  slice(start: number, end?: number): RedactedSlice;
}
function redactedSlice(value: string): RedactedSlice {
  const slice = Object.freeze({ toJSON: () => value });
  redactedValues.set(slice, value);
  return slice;
}
/** Redact the complete source exactly once; only its immutable slices bypass re-redaction. */
export function redactDocument(source: string): RedactedDocument {
  const value = redactText(source);
  const document = Object.freeze({ value, length: value.length,
    slice: (start: number, end?: number) => redactedSlice(value.slice(start, end)),
    toJSON: () => value });
  redactedValues.set(document, value);
  return document;
}
function isRedactedValue(value: unknown): value is object {
  return !!value && typeof value === "object" && redactedValues.has(value);
}

export function redactDeep<T>(v: T): T {
  if (isRedactedValue(v)) return redactedValues.get(v)! as unknown as T;
  // Never preserve an arbitrary serialization hook that could evade traversal.
  if (typeof v === "function") return undefined as T;
  if (typeof v === "string") return redactText(v) as unknown as T;
  if (Array.isArray(v)) return v.map(redactDeep) as unknown as T;
  if (v && typeof v === "object") {
    const o: Record<string, unknown> = {};
    for (const [k, val] of Object.entries(v as Record<string, unknown>)) {
      o[k] = SECRET_KEY.test(k) && (typeof val === "string" || isRedactedValue(val)) ? R : redactDeep(val);
    }
    return o as T;
  }
  return v;
}
