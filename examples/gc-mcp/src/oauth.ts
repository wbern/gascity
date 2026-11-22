// OAuth 2.1 authorization server for the MCP server: auth-code + PKCE (S256),
// audience-bound tokens (RFC 8707). Consent is approved by the OWNER typing a
// passphrase into a page served by this process, so the model never sees it.
//
// Client model (security review fix): by default there is ONE static client whose
// id only the owner holds ("Use your own OAuth client" in Claude) and NO /register
// endpoint. Dynamic registration exists only behind an explicit flag and is
// restricted to an exact redirect-URI allowlist with hard caps.
import crypto from "node:crypto";
import fs from "node:fs";
import path from "node:path";
import type { Response } from "express";
import { InvalidGrantError, InvalidTokenError, InvalidTargetError, InvalidClientMetadataError } from "@modelcontextprotocol/sdk/server/auth/errors.js";
import type { OAuthServerProvider, AuthorizationParams } from "@modelcontextprotocol/sdk/server/auth/provider.js";
import type { OAuthRegisteredClientsStore } from "@modelcontextprotocol/sdk/server/auth/clients.js";
import type { AuthInfo } from "@modelcontextprotocol/sdk/server/auth/types.js";
import type { OAuthClientInformationFull, OAuthTokens } from "@modelcontextprotocol/sdk/shared/auth.js";

export const SCOPE = "gc:read";
export const CLAUDE_CALLBACKS = ["https://claude.ai/api/mcp/auth_callback", "https://claude.com/api/mcp/auth_callback"];
export const CLAUDE_NAME = "Claude (owner client)";
export const CHATGPT_NAME = "ChatGPT (owner client)";

/** A second STATIC client for ChatGPT (no dynamic registration). Its redirect URI is per connection
 *  (https://chatgpt.com/connector/oauth/<id>) and only known after the owner creates the connector, so the
 *  list is configuration: an empty list means this client cannot authorize at all (fail closed). */
export interface ChatgptClientSpec {
  clientId: string;
  redirectUris: string[];        // exact https://chatgpt.com/... URIs (validateChatgptRedirects)
  secretHash?: string;           // optional scrypt1$ verifier: when set the client is confidential (client_secret_post)
}

/** Throws unless every entry is an exact https://chatgpt.com/<path> URI: no other host or port, no userinfo,
 *  no fragment, no wildcard or whitespace, and a real path. Returns the list unchanged. */
export function validateChatgptRedirects(uris: string[]): string[] {
  for (const raw of uris) {
    let u: URL;
    try { u = new URL(raw); } catch { throw new Error(`ChatGPT redirect URI is not a URL: ${JSON.stringify(raw).slice(0, 80)}`); }
    const bad = !raw.startsWith("https://chatgpt.com/") || u.protocol !== "https:" || u.hostname !== "chatgpt.com" || u.port !== ""
      || u.username !== "" || u.password !== "" || u.hash !== "" || /[\s*\\]/.test(raw) || u.pathname.length < 2;
    if (bad) throw new Error(`ChatGPT redirect URI must be an exact https://chatgpt.com/<path> URL: ${JSON.stringify(raw).slice(0, 80)}`);
  }
  return uris;
}
const ACCESS_TTL_S = 3600;
const REFRESH_TTL_S = 30 * 24 * 3600;
const CODE_TTL_MS = 5 * 60_000;
const PENDING_TTL_MS = 10 * 60_000;
const MAX_FAILS_PER_RID = 5;

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
  hit(key: string): boolean {
    const t = this.now();
    const arr = (this.hits.get(key) ?? []).filter((x) => t - x < this.windowMs);
    if (arr.length >= this.max) { this.hits.set(key, arr); return false; }
    arr.push(t);
    this.hits.set(key, arr);
    if (this.hits.size > 10_000) for (const [k, v] of this.hits) if (!v.some((x) => t - x < this.windowMs)) this.hits.delete(k);
    return true;
  }
  blocked(key: string): boolean {
    const t = this.now();
    return (this.hits.get(key) ?? []).filter((x) => t - x < this.windowMs).length >= this.max;
  }
}

// ---- persistent state: client registry + refresh-token hashes only ----------
interface Persisted {
  clients: Record<string, OAuthClientInformationFull>;
  refresh: Record<string, { clientId: string; scopes: string[]; exp: number; fam: string }>;
  used: Record<string, { fam: string; exp: number }>; // rotated-out refresh hashes (reuse detection)
}
interface PendingAuth { client: OAuthClientInformationFull; params: AuthorizationParams; exp: number; fails: number }
interface CodeRec { clientId: string; challenge: string; redirectUri: string; scopes: string[]; exp: number }
interface AccessRec { clientId: string; scopes: string[]; exp: number; fam: string }

export interface ProviderOpts {
  resource: URL;                 // canonical audience of this MCP server
  ownerHash: string;             // scrypt1$... from hashPassphrase
  staticClientId?: string;       // the owner's own OAuth client (recommended)
  chatgpt?: ChatgptClientSpec;   // optional second static client for ChatGPT; absent = disabled
  consentPath?: string;          // where the consent form posts (default /consent; follows the issuer path)
  allowDcr?: boolean;            // OFF by default
  redirectAllow?: string[];      // exact redirect URIs accepted (default: Claude callbacks)
  maxClients?: number;
  stateFile?: string;            // omit for memory-only (tests)
  now?: () => number;
  onEvent?: (note: string, ok: boolean, client?: string) => void;   // client = the OAuth client's display name
}

export class OwnerApprovalProvider implements OAuthServerProvider {
  private st: Persisted = { clients: {}, refresh: {}, used: {} };
  private pending = new Map<string, PendingAuth>();
  private codes = new Map<string, CodeRec>();
  private access = new Map<string, AccessRec>();
  private now: () => number;
  private allow: string[];
  readonly failedBySource: RateLimiter;
  readonly failedGlobal: RateLimiter;

  constructor(private o: ProviderOpts) {
    this.now = o.now ?? Date.now;
    this.allow = o.redirectAllow ?? CLAUDE_CALLBACKS;
    if (o.chatgpt) {
      if (!o.chatgpt.clientId || o.chatgpt.clientId === o.staticClientId) throw new Error("the ChatGPT client id must be set and different from the Claude client id");
      if (o.chatgpt.secretHash !== undefined && !o.chatgpt.secretHash.startsWith("scrypt1$")) throw new Error("the ChatGPT secret hash must be a scrypt1$ verifier");
      validateChatgptRedirects(o.chatgpt.redirectUris);
    }
    this.failedBySource = new RateLimiter(5, 5 * 60_000, this.now);
    this.failedGlobal = new RateLimiter(30, 15 * 60_000, this.now);
    if (o.stateFile && fs.existsSync(o.stateFile)) {
      try { this.st = { clients: {}, refresh: {}, used: {}, ...JSON.parse(fs.readFileSync(o.stateFile, "utf8")) }; } catch { /* start clean */ }
    }
  }

  private emit(note: string, ok = true, clientId?: string) { this.o.onEvent?.(note, ok, clientId ? this.clientName(clientId) : undefined); }

  /** Display name of a client id for the consent page and the audit log. */
  clientName(clientId: string): string | undefined {
    if (clientId === this.o.staticClientId) return CLAUDE_NAME;
    if (this.o.chatgpt && clientId === this.o.chatgpt.clientId) return CHATGPT_NAME;
    return this.st.clients[clientId]?.client_name;
  }

  /** True when this client must present a secret at /token and /revoke (ChatGPT confidential mode only). */
  clientNeedsSecret(clientId: string): boolean {
    return !!this.o.chatgpt?.secretHash && clientId === this.o.chatgpt.clientId;
  }

  /** Verifies the client secret in constant time against the scrypt verifier. True for clients that need none. */
  verifyClientSecret(clientId: string, secret: string | undefined): boolean {
    if (!this.clientNeedsSecret(clientId)) return true;
    return typeof secret === "string" && secret.length > 0 && secret.length <= 512 && verifyPassphrase(secret, this.o.chatgpt!.secretHash!);
  }

  private chatgptClient(): OAuthClientInformationFull | undefined {
    const c = this.o.chatgpt;
    if (!c) return undefined;
    // NOT the SDK's client_secret field: the SDK compares that as plain text. The secret is verified by verifyClientSecret().
    return { client_id: c.clientId, redirect_uris: c.redirectUris, token_endpoint_auth_method: c.secretHash ? "client_secret_post" : "none",
      grant_types: ["authorization_code", "refresh_token"], response_types: ["code"], client_name: CHATGPT_NAME, scope: SCOPE } as OAuthClientInformationFull;
  }

  private save() {
    if (!this.o.stateFile) return;
    fs.mkdirSync(path.dirname(this.o.stateFile), { recursive: true, mode: 0o700 });
    const tmp = `${this.o.stateFile}.${process.pid}.tmp`;
    fs.writeFileSync(tmp, JSON.stringify(this.st), { mode: 0o600 });
    fs.renameSync(tmp, this.o.stateFile);
  }

  private staticClient(): OAuthClientInformationFull | undefined {
    if (!this.o.staticClientId) return undefined;
    return { client_id: this.o.staticClientId, redirect_uris: this.allow, token_endpoint_auth_method: "none",
      grant_types: ["authorization_code", "refresh_token"], response_types: ["code"], client_name: CLAUDE_NAME, scope: SCOPE } as OAuthClientInformationFull;
  }

  get clientsStore(): OAuthRegisteredClientsStore {
    const store: OAuthRegisteredClientsStore = {
      getClient: (id) => (this.o.staticClientId && id === this.o.staticClientId ? this.staticClient()
        : this.o.chatgpt && id === this.o.chatgpt.clientId ? this.chatgptClient() : this.st.clients[id]),
    };
    if (this.o.allowDcr) {
      store.registerClient = (c) => {
        const uris = (c.redirect_uris ?? []).map(String);
        if (!uris.length || uris.length > 2 || uris.some((u) => !this.allow.includes(u))) {
          this.emit("dcr rejected: redirect_uri not allowlisted", false);
          throw new InvalidClientMetadataError("redirect_uris must be exact Claude callback URLs");
        }
        if (Object.keys(this.st.clients).length >= (this.o.maxClients ?? 25)) {
          this.emit("dcr rejected: client cap reached", false);
          throw new InvalidClientMetadataError("too many registered clients");
        }
        const full = { ...c, redirect_uris: uris, client_name: String(c.client_name ?? "client").slice(0, 60), token_endpoint_auth_method: "none",
          client_id: rand(18), client_id_issued_at: Math.floor(this.now() / 1000) } as OAuthClientInformationFull;
        this.st.clients[full.client_id] = full;
        this.save();
        this.emit("dcr registered client");
        return full;
      };
    }
    return store;
  }

  private checkResource(r?: URL) {
    if (r && r.href !== this.o.resource.href) throw new InvalidTargetError("unknown resource");
  }

  // Step 1: render the owner-consent page. Nothing is granted yet.
  async authorize(client: OAuthClientInformationFull, params: AuthorizationParams, res: Response): Promise<void> {
    try { this.checkResource(params.resource); } catch { res.status(400).type("text/plain").send("invalid_target"); return; }
    for (const [k, v] of this.pending) if (v.exp < this.now()) this.pending.delete(k);
    if (this.pending.size >= 50) { res.status(503).type("text/plain").send("busy"); return; }
    const rid = rand(24);
    this.pending.set(rid, { client, params, exp: this.now() + PENDING_TTL_MS, fails: 0 });
    this.emit("authorize page served", true, client.client_id);
    const redirect = esc(params.redirectUri);
    // form-action must include the callback origins: Chrome applies it to the post-submit redirect too.
    // Per client: Claude's page allows only Claude's origins, ChatGPT's page only chatgpt.com.
    const origins = [...new Set(client.redirect_uris.map((u) => new URL(String(u)).origin))];
    const name = client.client_name ?? "unnamed";
    res.status(200)
      .set({ "content-type": "text/html; charset=utf-8", "cache-control": "no-store", "x-frame-options": "DENY",
        "content-security-policy": `default-src 'none'; style-src 'unsafe-inline'; form-action 'self' ${origins.join(" ")}; frame-ancestors 'none'` })
      .send(`<!doctype html><meta name=viewport content="width=device-width"><title>Approve Gas City access</title>
<body style="font:16px system-ui;max-width:28em;margin:2em auto;padding:0 1em">
<h2>Approve Gas City access?</h2>
<p>An application (<b>${esc(name)}</b>, unverified name) wants <b>read-only</b> access to Gas City status, sessions, mail subjects and beads.</p>
<p>It will be sent back to:<br><code>${redirect}</code><br>Only approve if you just started this from <b>${esc(name.replace(/ \(owner client\)$/, ""))}</b>.</p>
<form method=post action="${esc(this.o.consentPath ?? "/consent")}">
<input type=hidden name=rid value="${esc(rid)}">
<p><label>Owner passphrase<br><input type=password name=passphrase autocomplete=current-password autofocus style="width:100%;font-size:1em"></label></p>
<button name=decision value=approve>Approve</button> <button name=decision value=deny>Deny</button>
</form></body>`);
  }

  // Step 2: owner submits the passphrase. Returns the redirect target.
  consent(rid: string, passphrase: string, decision: string, source: string): { status: number; redirect?: string; message?: string } {
    if (this.failedGlobal.blocked("all") || this.failedBySource.blocked(source)) return { status: 429, message: "Too many failed attempts. Try again later." };
    const p = this.pending.get(rid);
    if (!p || p.exp < this.now()) return { status: 400, message: "This approval request expired. Start again from the app." };
    const back = (q: Record<string, string>) => {
      const u = new URL(p.params.redirectUri);
      for (const [k, v] of Object.entries(q)) u.searchParams.set(k, v);
      if (p.params.state) u.searchParams.set("state", p.params.state);
      return u.toString();
    };
    if (decision !== "approve") { this.pending.delete(rid); this.emit("consent denied", true, p.client.client_id); return { status: 302, redirect: back({ error: "access_denied" }) }; }
    if (!verifyPassphrase(passphrase ?? "", this.o.ownerHash)) {
      this.failedBySource.hit(source);
      this.failedGlobal.hit("all");
      p.fails += 1;
      if (p.fails >= MAX_FAILS_PER_RID) { this.pending.delete(rid); this.emit("consent request burned after repeated wrong passphrase", false, p.client.client_id); }
      else this.emit("consent wrong passphrase", false, p.client.client_id);
      return { status: 401, message: "Wrong passphrase." };
    }
    this.pending.delete(rid); // single use
    const code = rand(32);
    this.codes.set(sha(code), { clientId: p.client.client_id, challenge: p.params.codeChallenge, redirectUri: p.params.redirectUri, scopes: [SCOPE], exp: this.now() + CODE_TTL_MS });
    this.emit("consent approved, code issued", true, p.client.client_id);
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
    this.emit("token issued from code", true, client.client_id);
    return this.issue(client.client_id, rec.scopes, rand(12));
  }

  async exchangeRefreshToken(client: OAuthClientInformationFull, refreshToken: string, _scopes?: string[], resource?: URL): Promise<OAuthTokens> {
    const key = sha(refreshToken);
    const reused = this.st.used[key];
    if (reused) { // a rotated-out token came back: assume theft, kill the whole family
      this.revokeFamily(reused.fam);
      this.emit("refresh token REUSE detected, family revoked", false, client.client_id);
      throw new InvalidGrantError("invalid or expired grant");
    }
    const rec = this.st.refresh[key];
    if (!rec || rec.clientId !== client.client_id || rec.exp < this.now()) throw new InvalidGrantError("invalid or expired grant");
    delete this.st.refresh[key];
    this.st.used[key] = { fam: rec.fam, exp: rec.exp };
    this.checkResource(resource);
    this.emit("token issued from refresh", true, client.client_id);
    return this.issue(client.client_id, rec.scopes, rec.fam);
  }

  private revokeFamily(fam: string) {
    for (const [k, v] of Object.entries(this.st.refresh)) if (v.fam === fam) delete this.st.refresh[k];
    for (const [k, v] of this.access) if (v.fam === fam) this.access.delete(k);
    this.save();
  }

  private issue(clientId: string, scopes: string[], fam: string): OAuthTokens {
    const at = rand(32), rt = rand(32);
    this.access.set(sha(at), { clientId, scopes, exp: this.now() + ACCESS_TTL_S * 1000, fam });
    this.st.refresh[sha(rt)] = { clientId, scopes, exp: this.now() + REFRESH_TTL_S * 1000, fam };
    for (const [k, v] of Object.entries(this.st.refresh)) if (v.exp < this.now()) delete this.st.refresh[k];
    for (const [k, v] of Object.entries(this.st.used)) if (v.exp < this.now()) delete this.st.used[k];
    this.save();
    return { access_token: at, token_type: "Bearer", expires_in: ACCESS_TTL_S, refresh_token: rt, scope: scopes.join(" ") };
  }

  async verifyAccessToken(token: string): Promise<AuthInfo> {
    const rec = this.access.get(sha(token));
    if (!rec || rec.exp < this.now()) throw new InvalidTokenError("invalid or expired token");
    return { token, clientId: rec.clientId, scopes: rec.scopes, expiresAt: Math.floor(rec.exp / 1000), resource: this.o.resource };
  }

  async revokeToken(client: OAuthClientInformationFull, req: { token: string }): Promise<void> {
    const key = sha(req.token);
    const a = this.access.get(key), r = this.st.refresh[key];
    const rec = a ?? r;
    if (!rec || rec.clientId !== client.client_id) return; // not yours: silently ignore (RFC 7009)
    this.revokeFamily(rec.fam); // revoking either token kills the whole family
    this.emit("token family revoked by client", true, client.client_id);
  }

  /** Owner kill switch: forget every token and pending grant (clients/passphrase stay). */
  revokeAll(): void {
    this.st.refresh = {}; this.st.used = {};
    this.access.clear(); this.codes.clear(); this.pending.clear();
    this.save();
    this.emit("ALL tokens revoked");
  }
}
