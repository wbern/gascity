// Dynamic client registration (RFC 7591), restricted. The point is convenience (paste the URL, click Connect, type the passphrase) WITHOUT
// letting a stranger register a client that redirects an approval code to their own site. So every redirect URI must belong to a short list of
// exact, canonical shapes: Claude's callbacks, ChatGPT's connector callbacks, loopback for desktop clients, and operator-listed extras.
// Anything else is invalid_redirect_uri. There is a loudly named unsafe switch to allow any clean https URI; it is OFF by default.
export const CLAUDE_CALLBACKS = ["https://claude.ai/api/mcp/auth_callback", "https://claude.com/api/mcp/auth_callback"];
export const CHATGPT_PLATFORM_CALLBACK = "https://chatgpt.com/connector_platform_oauth_redirect";
const CHATGPT_CONNECTOR = /^https:\/\/chatgpt\.com\/connector\/oauth\/[A-Za-z0-9_-]{1,128}$/;
const SAFE_EXACT = new Set([...CLAUDE_CALLBACKS, CHATGPT_PLATFORM_CALLBACK]);
const LOOPBACK_HOSTS = new Set(["127.0.0.1", "localhost"]);
const LOOPBACK_PATH = /^\/[A-Za-z0-9._~/-]*$/;
const MAX_URI = 2048;
export const MAX_REDIRECTS_PER_CLIENT = 5;

export interface RedirectPolicy {
  extra?: string[];   // exact extra URIs (validated at startup by validateExtraRedirects)
  open?: boolean;     // UNSAFE: any clean https URI. Default off.
}

/** Characters that never belong in a redirect URI we are willing to hold: whitespace, control characters, backslash, wildcard, fragment marker. */
const FORBIDDEN = /[\s\u0000-\u001F\u007F\\*#]/;

/** True when `uri` is exactly the canonical string form of a URL we accept (no case tricks, no implicit port, no stray characters). */
function canonical(uri: string): URL | null {
  if (typeof uri !== "string" || uri.length === 0 || uri.length > MAX_URI || FORBIDDEN.test(uri)) return null;
  let u: URL;
  try { u = new URL(uri); } catch { return null; }
  if (u.username || u.password || u.hash) return null;
  const same = uri === u.href || (u.pathname === "/" && !u.search && !uri.endsWith("/") && `${uri}/` === u.href);   // "http://127.0.0.1:8080" for "http://127.0.0.1:8080/"
  return same ? u : null;
}

function isLoopback(u: URL): boolean {
  if (u.protocol !== "http:" || !LOOPBACK_HOSTS.has(u.hostname) || !u.port || u.search) return false;
  const port = Number(u.port);
  return Number.isInteger(port) && port >= 1 && port <= 65535 && LOOPBACK_PATH.test(u.pathname) && !u.pathname.split("/").includes("..");
}

export function isSafeRedirect(uri: string, policy: RedirectPolicy = {}): boolean {
  if (policy.extra?.includes(uri)) return true;
  const u = canonical(uri);
  if (!u) return false;
  if (SAFE_EXACT.has(uri) || CHATGPT_CONNECTOR.test(uri)) return true;
  if (isLoopback(u)) return true;
  if (policy.open && u.protocol === "https:" && u.hostname.includes(".")) return true;
  return false;
}

/** Operator-supplied extras (GC_MCP_DCR_EXTRA_REDIRECTS): exact https URIs or loopback URIs; anything looser stops startup. */
export function validateExtraRedirects(list: string[]): string[] {
  for (const raw of list) {
    const u = canonical(raw);
    if (!u || !(u.protocol === "https:" || isLoopback(u))) throw new Error(`GC_MCP_DCR_EXTRA_REDIRECTS: ${JSON.stringify(raw).slice(0, 80)} must be an exact https URL (no userinfo, fragment, wildcard or whitespace) or a loopback http URL with a port`);
  }
  return list;
}

/** A registered client's display name: control, bidi and zero-width characters removed (they spoof other names), whitespace collapsed, 60 characters at most. */
export function sanitizeClientName(raw: unknown): string {
  const s = String(raw ?? "").replace(/[\u0000-\u001F\u007F-\u009F\u200B-\u200F\u202A-\u202E\u2060-\u2069\uFEFF]/g, "").replace(/\s+/g, " ").trim();
  return s.slice(0, 60).trim() || "client";
}
