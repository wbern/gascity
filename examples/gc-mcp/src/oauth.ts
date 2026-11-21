// OAuth 2.1 authorization server for the MCP server: auth-code + PKCE (S256),
// dynamic client registration (RFC 7591), audience-bound tokens (RFC 8707).
//
// Consent is approved by the OWNER typing a passphrase into a page served by
// this process. The passphrase never touches the model, so a prompt-injected
// transcript cannot approve anything. Tokens are opaque random values; only
// SHA-256 hashes are stored.
import crypto from "node:crypto";
import fs from "node:fs";
import path from "node:path";
import type { Response } from "express";
import { InvalidGrantError, InvalidTokenError, InvalidTargetError } from "@modelcontextprotocol/sdk/server/auth/errors.js";
import type { OAuthServerProvider, AuthorizationParams } from "@modelcontextprotocol/sdk/server/auth/provider.js";
import type { OAuthRegisteredClientsStore } from "@modelcontextprotocol/sdk/server/auth/clients.js";
import type { AuthInfo } from "@modelcontextprotocol/sdk/server/auth/types.js";
import type { OAuthClientInformationFull, OAuthTokens } from "@modelcontextprotocol/sdk/shared/auth.js";

export const SCOPE = "gc:read";
const ACCESS_TTL_S = 3600;
const REFRESH_TTL_S = 30 * 24 * 3600;
const CODE_TTL_MS = 5 * 60_000;
const PENDING_TTL_MS = 10 * 60_000;

const sha = (s: string) => crypto.createHash("sha256").update(s).digest("hex");
const rand = (n = 32) => crypto.randomBytes(n).toString("base64url");
const esc = (s: string) => s.replace(/[&<>"']/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" })[c]!);

// ---- passphrase hashing ----------------------------------------------------
export function hashPassphrase(pass: string): string {
  const salt = crypto.randomBytes(16);
  const h = crypto.scryptSync(pass, salt, 32, { N: 16384, r: 8, p: 1 });
  return `scrypt1$${salt.toString("base64url")}$${h.toString("base64url")}`;
}
export function verifyPassphrase(pass: string, stored: string): boolean {
  const [tag, s, h] = stored.split("$");
  if (tag !== "scrypt1" || !s || !h) return false;
  const want = Buffer.from(h, "base64url");
  const got = crypto.scryptSync(pass, Buffer.from(s, "base64url"), want.length, { N: 16384, r: 8, p: 1 });
  return got.length === want.length && crypto.timingSafeEqual(got, want);
}

// ---- tiny sliding-window limiter -------------------------------------------
export class RateLimiter {
  private hits = new Map<string, number[]>();
  constructor(private max: number, private windowMs: number, private now: () => number = Date.now) {}
  /** records a hit; returns false when the key is over its limit */
  hit(key: string): boolean {
    const t = this.now();
    const arr = (this.hits.get(key) ?? []).filter((x) => t - x < this.windowMs);
    if (arr.length >= this.max) { this.hits.set(key, arr); return false; }
    arr.push(t);
    this.hits.set(key, arr);
    return true;
  }
  /** over the limit without recording a hit */
  blocked(key: string): boolean {
    const t = this.now();
    return (this.hits.get(key) ?? []).filter((x) => t - x < this.windowMs).length >= this.max;
  }
}

// ---- persistent state (clients + refresh-token hashes only) ----------------
interface Persisted {
  clients: Record<string, OAuthClientInformationFull>;
  refresh: Record<string, { clientId: string; scopes: string[]; exp: number }>;
}
interface PendingAuth { client: OAuthClientInformationFull; params: AuthorizationParams; exp: number }
interface CodeRec { clientId: string; challenge: string; redirectUri: string; scopes: string[]; exp: number }
interface AccessRec { clientId: string; scopes: string[]; exp: number }

export interface ProviderOpts {
  resource: URL;                 // canonical audience of this MCP server
  ownerHash: string;             // scrypt1$... from hashPassphrase
  stateFile?: string;            // omit for memory-only (tests)
  now?: () => number;
  failedConsent?: RateLimiter;   // limits wrong-passphrase attempts per source
}

export class OwnerApprovalProvider implements OAuthServerProvider {
  private st: Persisted = { clients: {}, refresh: {} };
  private pending = new Map<string, PendingAuth>();
  private codes = new Map<string, CodeRec>();
  private access = new Map<string, AccessRec>();
  private now: () => number;
  readonly failedConsent: RateLimiter;

  constructor(private o: ProviderOpts) {
    this.now = o.now ?? Date.now;
    this.failedConsent = o.failedConsent ?? new RateLimiter(5, 5 * 60_000, this.now);
    if (o.stateFile && fs.existsSync(o.stateFile)) {
      try { this.st = { clients: {}, refresh: {}, ...JSON.parse(fs.readFileSync(o.stateFile, "utf8")) }; } catch { /* start clean */ }
    }
  }

  private save() {
    if (!this.o.stateFile) return;
    fs.mkdirSync(path.dirname(this.o.stateFile), { recursive: true, mode: 0o700 });
    const tmp = `${this.o.stateFile}.${process.pid}.tmp`;
    fs.writeFileSync(tmp, JSON.stringify(this.st), { mode: 0o600 });
    fs.renameSync(tmp, this.o.stateFile);
  }

  get clientsStore(): OAuthRegisteredClientsStore {
    return {
      getClient: (id) => this.st.clients[id],
      registerClient: (c) => {
        const full = { ...c, client_id: rand(18), client_id_issued_at: Math.floor(this.now() / 1000) } as OAuthClientInformationFull;
        this.st.clients[full.client_id] = full;
        this.save();
        return full;
      },
    };
  }

  private checkResource(r?: URL) {
    if (r && r.href !== this.o.resource.href) throw new InvalidTargetError("unknown resource");
  }

  // Step 1: render the owner-consent page. Nothing is granted yet.
  async authorize(client: OAuthClientInformationFull, params: AuthorizationParams, res: Response): Promise<void> {
    try { this.checkResource(params.resource); } catch { res.status(400).type("text/plain").send("invalid_target"); return; }
    for (const [k, v] of this.pending) if (v.exp < this.now()) this.pending.delete(k);
    const rid = rand(24);
    this.pending.set(rid, { client, params, exp: this.now() + PENDING_TTL_MS });
    const host = new URL(params.redirectUri).host;
    res.status(200)
      .set({ "content-type": "text/html; charset=utf-8", "cache-control": "no-store", "x-frame-options": "DENY", "content-security-policy": "default-src 'none'; style-src 'unsafe-inline'; form-action 'self'; frame-ancestors 'none'" })
      .send(`<!doctype html><meta name=viewport content="width=device-width"><title>Approve Gas City access</title>
<body style="font:16px system-ui;max-width:28em;margin:2em auto;padding:0 1em">
<h2>Approve Gas City access?</h2>
<p><b>${esc(client.client_name ?? "An application")}</b> (returns to <code>${esc(host)}</code>) wants <b>read-only</b> access to Gas City status, sessions, mail subjects and beads.</p>
<form method=post action="/consent">
<input type=hidden name=rid value="${esc(rid)}">
<p><label>Owner passphrase<br><input type=password name=passphrase autocomplete=current-password autofocus style="width:100%;font-size:1em"></label></p>
<button name=decision value=approve>Approve</button> <button name=decision value=deny>Deny</button>
</form></body>`);
  }

  // Step 2: owner submits the passphrase. Returns the redirect target.
  consent(rid: string, passphrase: string, decision: string, source: string): { status: number; redirect?: string; message?: string } {
    if (this.failedConsent.blocked(source)) return { status: 429, message: "Too many attempts. Try again later." };
    const p = this.pending.get(rid);
    if (!p || p.exp < this.now()) return { status: 400, message: "This approval request expired. Start again from the app." };
    const back = (q: Record<string, string>) => {
      const u = new URL(p.params.redirectUri);
      for (const [k, v] of Object.entries(q)) u.searchParams.set(k, v);
      if (p.params.state) u.searchParams.set("state", p.params.state);
      return u.toString();
    };
    if (decision !== "approve") { this.pending.delete(rid); return { status: 302, redirect: back({ error: "access_denied" }) }; }
    if (!verifyPassphrase(passphrase ?? "", this.o.ownerHash)) {
      this.failedConsent.hit(source);
      return { status: 401, message: "Wrong passphrase." };
    }
    this.pending.delete(rid); // single use
    const code = rand(32);
    this.codes.set(sha(code), { clientId: p.client.client_id, challenge: p.params.codeChallenge, redirectUri: p.params.redirectUri, scopes: [SCOPE], exp: this.now() + CODE_TTL_MS });
    return { status: 302, redirect: back({ code }) };
  }

  async challengeForAuthorizationCode(client: OAuthClientInformationFull, code: string): Promise<string> {
    const rec = this.codes.get(sha(code));
    if (!rec || rec.clientId !== client.client_id || rec.exp < this.now()) throw new InvalidGrantError("invalid or expired grant");
    return rec.challenge;
  }

  async exchangeAuthorizationCode(client: OAuthClientInformationFull, code: string, _verifier?: string, redirectUri?: string, resource?: URL): Promise<OAuthTokens> {
    const key = sha(code);
    const rec = this.codes.get(key);
    this.codes.delete(key); // single use even on failure
    if (!rec || rec.clientId !== client.client_id || rec.exp < this.now()) throw new InvalidGrantError("invalid or expired grant");
    if (redirectUri && redirectUri !== rec.redirectUri) throw new InvalidGrantError("redirect_uri mismatch");
    this.checkResource(resource);
    return this.issue(client.client_id, rec.scopes);
  }

  async exchangeRefreshToken(client: OAuthClientInformationFull, refreshToken: string, _scopes?: string[], resource?: URL): Promise<OAuthTokens> {
    const key = sha(refreshToken);
    const rec = this.st.refresh[key];
    delete this.st.refresh[key]; // rotate: old token is dead either way
    if (!rec || rec.clientId !== client.client_id || rec.exp < this.now()) { this.save(); throw new InvalidGrantError("invalid or expired grant"); }
    this.checkResource(resource);
    return this.issue(client.client_id, rec.scopes);
  }

  private issue(clientId: string, scopes: string[]): OAuthTokens {
    const at = rand(32), rt = rand(32);
    this.access.set(sha(at), { clientId, scopes, exp: this.now() + ACCESS_TTL_S * 1000 });
    this.st.refresh[sha(rt)] = { clientId, scopes, exp: this.now() + REFRESH_TTL_S * 1000 };
    for (const [k, v] of Object.entries(this.st.refresh)) if (v.exp < this.now()) delete this.st.refresh[k];
    this.save();
    return { access_token: at, token_type: "Bearer", expires_in: ACCESS_TTL_S, refresh_token: rt, scope: scopes.join(" ") };
  }

  async verifyAccessToken(token: string): Promise<AuthInfo> {
    const rec = this.access.get(sha(token));
    if (!rec || rec.exp < this.now()) throw new InvalidTokenError("invalid or expired token");
    // Audience is fixed to this server: tokens are only ever minted for it.
    return { token, clientId: rec.clientId, scopes: rec.scopes, expiresAt: Math.floor(rec.exp / 1000), resource: this.o.resource };
  }

  async revokeToken(_c: OAuthClientInformationFull, req: { token: string }): Promise<void> {
    this.access.delete(sha(req.token));
    delete this.st.refresh[sha(req.token)];
    this.save();
  }
}
