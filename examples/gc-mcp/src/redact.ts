// Redaction applied to EVERY value leaving the server. Conservative: it is
// better to speak "[redacted]" than to read a credential aloud.
const R = "[redacted]";

const PATTERNS: RegExp[] = [
  /\bBearer\s+[A-Za-z0-9._~+/=-]{12,}/gi,
  /\bgh[pousr]_[A-Za-z0-9]{20,}/g,
  /\bsk-[A-Za-z0-9_-]{16,}/g,
  /\bxox[abprs]-[A-Za-z0-9-]{10,}/g,
  /\bAKIA[0-9A-Z]{16}\b/g,
  /\beyJ[A-Za-z0-9_-]{6,}\.[A-Za-z0-9_-]{6,}\.[A-Za-z0-9_-]{6,}/g,
  /(x-vercel-protection-bypass=)[^\s&"']+/gi,
  /\b[0-9a-fA-F]{32,}\b/g,
  /(?<![\w/.-])(?=[A-Za-z0-9_+-]*[A-Za-z])(?=[A-Za-z0-9_+-]*\d)[A-Za-z0-9_+-]{40,}={0,2}(?![\w/.-])/g,
];

// KEY=value / key: value where the key name smells like a secret.
const KV =
  /\b([A-Za-z0-9_.-]*(?:secret|token|passw(?:or)?d|passwd|api[_-]?key|bypass|credential|private[_-]?key)[A-Za-z0-9_.-]*)(\s*[=:]\s*)("[^"]*"|'[^']*'|[^\s,;]+)/gi;

const SECRET_KEY = /secret|token|passw|api[_-]?key|bypass|credential|private[_-]?key|authorization/i;

export function redactText(s: string): string {
  let out = s.replace(KV, (_m, k, sep) => `${k}${sep}${R}`);
  for (const p of PATTERNS) {
    out = out.replace(p, (m, g1) => (typeof g1 === "string" && /=$/.test(g1) ? g1 + R : R));
  }
  return out;
}

export function redactDeep<T>(v: T): T {
  if (typeof v === "string") return redactText(v) as unknown as T;
  if (Array.isArray(v)) return v.map(redactDeep) as unknown as T;
  if (v && typeof v === "object") {
    const o: Record<string, unknown> = {};
    for (const [k, val] of Object.entries(v as Record<string, unknown>)) {
      o[k] = SECRET_KEY.test(k) && typeof val === "string" ? R : redactDeep(val);
    }
    return o as T;
  }
  return v;
}
