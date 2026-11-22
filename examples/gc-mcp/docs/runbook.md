# Runbook: Gas City by voice (Claude + gc-mcp)

Status: DESIGN + HARDENED BUILD. Nothing here has been applied to any host.
Every step that touches DNS, the reverse proxy, the firewall, systemd/launchd or the phone needs
a human go. Production deployment on GC3 is owned by `gas-city-infra`
(`doc/gc-mcp-deployment.md`, role `gc_mcp`); this file describes the service itself.

## 1. What runs where

```
Claude (voice) -> Claude cloud --HTTPS :443--> public hostname (DNS at HostUp)
                                                   |
                                  your LAN nginx (TLS, source allowlist, rate limit)
                                                   |  one private hop, one pinned peer
                                                   v
                                  gc-mcp on ONE private IP of the GC3 host (e.g. 192.168.23.69:47811)
                                                   | loopback, GET-only
                                                   v
                                  Gas City supervisor 127.0.0.1:8372 (never public)
```
One gc-mcp per machine, next to the supervisor it fronts (GC3 first; the Mac city is a
second instance). Default bind is 127.0.0.1. When a reverse proxy on another LAN host must reach
it, bind ONE validated private address instead (section 4). The raw supervisor API has no auth
and must never be public.

## 2. Security model (read before exposing)

Anyone who gets past auth can read agent transcripts, mail subjects and bead text. So:

- **Read-only tools only** (`readOnlyHint`). No write tools exist. Mail bodies are never returned.
- **One static OAuth client, no dynamic registration by default.** The client id is generated
  on first start (`<state>/client-id`, 0600) and pasted into Claude's *Use your own OAuth
  client* field. `/register` does not exist (404). Only the exact Claude callback URLs
  (`https://claude.ai/api/mcp/auth_callback`, `https://claude.com/...`) are accepted.
  `GC_MCP_ALLOW_DCR=1` re-enables registration, still allowlisted and capped (25 clients).
- **Consent needs the owner passphrase**, typed into a page this server serves, never seen
  by the model. scrypt, 5 wrong tries burn a request, per-source and global lockouts.
- Opaque tokens, audience-bound (RFC 8707), hashes at rest (0600). Access 1h, refresh 30d,
  rotating with **reuse detection**: a replayed old refresh token revokes its whole family.
- Free text (transcripts, mail, beads) is redacted BEFORE clipping, links are stripped, and
  it is labelled untrusted, because it can carry prompt-injection. `GC_MCP_PEEK_ALLOW`
  (comma list of aliases/templates) restricts which sessions `peek_session` may read.
- Host header check (421), request size caps, connection/time-out limits, audit log of
  every tool call and every failed auth request (size-rotated, redacted, client id hashed).
- **Owner kill switch, no restart needed:** `touch <state>/DISABLED` stops serving (503);
  `touch <state>/REVOKE_ALL` drops every token (consumed on next request).

### What is NOT solved (residual risk, be honest)
- A public endpoint can be probed. Passphrase guessing is slow (scrypt + lockouts) but possible.
- Per-IP limiting is only meaningful when the real client IP is known. With `GC_MCP_TRUST_PROXY=0`
  (default) there is deliberately NO per-IP limiter (a shared bucket would let any stranger lock
  the owner out). Behind nginx set `GC_MCP_TRUST_PROXY=1` and make nginx OVERWRITE
  `X-Forwarded-For` with `$remote_addr`; verify with a forged header (it must be ignored).
- IP allowlisting (`GC_MCP_ALLOW_CIDRS`, applied to /mcp and /token only, never /authorize)
  needs the same verification. Anthropic documents TWO egress ranges:
  `160.79.104.0/21` and `153.61.208.0/20`; they promise notice before changes, not stability.
- Prompt injection can still lie to you by voice ("all clear"). Read-only limits the damage.
- The unauthenticated supervisor API on the tailnet (`tailscale serve :8372`) is a separate,
  pre-existing exposure; remove it deliberately, after confirming nothing uses it.

## 3. Requirements imposed by Claude (verified against docs 2026-11-21)

- **The server must be on port 443.** Claude does not support `https://host:8443/...`.
  Localhost/private hosts are rejected.
- `GC_MCP_PUBLIC_URL` must equal the exact URL typed into Claude (it is the issuer, the
  resource and the token audience). No redirects (Claude drops Authorization across hosts).
- An unauthenticated `/mcp` must answer 401 with `WWW-Authenticate: ...resource_metadata=`.
- Voice: Anthropic's help page says connected tools work in voice, but a collaborator wrote
  on 2026-05-12 that custom connector tools are not supported in voice, and users report it
  working then breaking. **Treat Android voice + custom connector as unproven until tested.**
  Text chat with the connector is the reliable fallback; a Telegram voice bot is plan B.

## 4. Reach: DNS at HostUp + your own nginx (decided)

No Cloudflare, no Funnel. Port 443 on its own hostname is required by Claude, so gc-mcp gets a
dedicated subdomain whose DNS record (in HostUp) points at the nginx that already fronts other
sites from this LAN (see gas-city-infra `doc/foxtrot-practice.md`, the same pattern).
Deployment files, the nginx vhost generator, the DNS script and the ordered runbook live in
`gas-city-infra` (`doc/gc-mcp-deployment.md`); this section is the app-side contract.

**How nginx reaches the app (three independent locks, all fail-closed):**
1. `GC_MCP_BIND_HOST=<one private IP>`: validated at startup; `0.0.0.0`, `::`, public addresses
   and hostnames are refused.
2. `GC_MCP_PEER_ALLOW=<nginx host IP>`: the app drops every TCP connection from any other peer
   before answering (even `/healthz`). Binding beyond loopback without it, or without
   `GC_MCP_TRUST_PROXY>=1`, refuses to start.
3. The host firewall and the systemd unit (`IPAddressAllow`) admit only the nginx host
   (infra role).

**Real client addresses.** nginx is the public edge, so its `$remote_addr` is the genuine caller. It
must set `X-Forwarded-For` to exactly `$remote_addr` (overwrite, never append), which makes
`GC_MCP_TRUST_PROXY=1` safe: per-IP limits, consent lockouts and the optional app-level source gate
(`GC_MCP_ALLOW_CIDRS`, Anthropic ranges) then work on real addresses. This resolves the shared-IP
problem that a tunnel would have had.

**What nginx must do** (the infra generator produces it): TLS on 443; allow only Anthropic's ranges
`160.79.104.0/21` and `153.61.208.0/20` on `/mcp` and `/token` (plus optionally your own IP); proxy
only the MCP/OAuth paths and 404 everything else (including `/register`, `/healthz`); rate limit;
`proxy_buffering off` for streamable HTTP; small body cap. `/authorize` and `/consent` stay open
(they are opened from your phone) but rate limited.

### 4.1 Public naming (final): one host per server, no path prefix

Host `mcp.gc3.enmansbyra.se` (HostUp record name `mcp.gc3`), MCP endpoint exactly `/mcp`. The **issuer is the host root**
(`https://mcp.gc3.enmansbyra.se`); the **resource and the RFC 8707 token audience are exactly
`https://mcp.gc3.enmansbyra.se/mcp`** (the check is an exact string comparison, so `/`, `/mcp/` and look-alikes are refused).
`GC_MCP_PUBLIC_URL=https://mcp.gc3.enmansbyra.se`; the endpoint path defaults to `/mcp`. A second city later gets its own
host (`mcp.gc2...`): not built. nginx routes only the nine locations in `gas-city-infra` (`/mcp`, `/authorize`, `/token`,
`/revoke`, `/consent`, the two protected-resource documents, the authorization-server document, and `/.well-known/openid-configuration`).

**Why no path prefix.** An independent review found, and I verified, two things. (1) The MCP SDK's all-in-one router builds its
endpoints with `new URL('/authorize', base)`, which drops any path, and serves authorization-server metadata only at the host
root; a path-bearing issuer therefore needs the SDK's handlers assembled by hand (I did that and proved it end to end in an
earlier iteration; the app still supports it) but it leaves the host root unserved, which is the very place Claude falls back to.
(2) Claude has **open, unresolved community reports** on path-based servers: anthropics/claude-ai-mcp **#878** (OAuth completes,
then no authenticated call is ever sent, only for endpoint paths other than exactly `/mcp`; no maintainer response) and **#716**
(when resource metadata 404s Claude falls back to `{issuer}/authorize`, ignoring the advertised endpoint; open, labelled `bug`).
A root issuer with the endpoint at exactly `/mcp` avoids both: the fallback `/authorize` is the real endpoint, and the path is the
one reported to work. These are third-party reports, not Anthropic statements.

**Documents served for robustness** (root-issuer mode): the protected-resource document at both `/.well-known/oauth-protected-resource/mcp`
and the host-root `/.well-known/oauth-protected-resource` (same content), and the authorization-server document at both
`/.well-known/oauth-authorization-server` and `/.well-known/openid-configuration` (same content, so a client probing OIDC discovery
gets the same endpoints).

**Verified** (evidence: `docs/discovery-e2e.txt`, reproduce with `scripts/discovery-e2e.mjs`): the MCP TypeScript SDK's own OAuth
client, given only `https://mcp.gc3.enmansbyra.se:<port>/mcp`, discovers everything through a real nginx container running the
infra generator's config in front of the real app and completes consent, tokens and a tool call; the root aliases answer; every
other path (`/`, `/healthz`, `/register`, `/gc3`, `/gc2`, ...) is 404.

**NOT verified.** Claude's own connector (it needs the live public URL), and the TLS-terminating proxy and Claude's egress behaving
in production as documented. The reference client working is not proof that Claude works.

**Still your call / unknown:** the exact subdomain; the nginx host's address, OS and who runs it;
whether public 80/443 reach it (the Foxtrot site suggests yes); stability of the GC3 LAN address
(DHCP changes would break the pinned bind: fail loudly, not silently).

## 5. Prerequisites (human)

1. Owner passphrase (>= 12 chars, unique, in your password manager):
   `node scripts/hash-passphrase.mjs` (typed without echo, prints only a hash).
2. Env file `0600`, **value single-quoted** (the hash contains `$`; unquoted it is mangled
   when the file is sourced):
   ```
   GC_MCP_OWNER_HASH='scrypt1$...'
   GC_MCP_PUBLIC_URL=https://<hostname-on-443>
   GC_MCP_CITY=<city name from GET /v0/cities>
   GC_MCP_PEEK_ALLOW=mayor,gc-architect      # optional but recommended
   # behind your LAN nginx (section 4):
   GC_MCP_BIND_HOST=<GC3 private IP>
   GC_MCP_PEER_ALLOW=<nginx host IP>
   GC_MCP_TRUST_PROXY=1
   GC_MCP_ALLOW_CIDRS='160.79.104.0/21,153.61.208.0/20'   # defence in depth behind nginx's own allowlist
   ```
3. `npm ci && npm run build`.

## 6. Verify locally before any exposure
```
curl -s localhost:47811/healthz                       # ok
curl -si -XPOST localhost:47811/mcp | head -3         # 401 + resource_metadata
curl -s -o /dev/null -w '%{http_code}\n' -XPOST localhost:47811/register   # 404
DEMO_PASSPHRASE=... DEMO_CLIENT_ID=$(cat ~/.local/state/gc-mcp/client-id) node scripts/oauth-demo.mjs http://127.0.0.1:47811
```
(`docs/oauth-demo.txt` is a recorded run.)

## 7. Phone setup
1. claude.ai (web) -> Settings -> Connectors -> Add custom connector. URL
   `<GC_MCP_PUBLIC_URL>/mcp`. Open *advanced / Use your own OAuth client* and paste the
   client id from `<state>/client-id`. Leave the secret empty (public client + PKCE).
2. Claude opens the consent page served by gc-mcp. Check the redirect shown is
   `claude.ai`, type the passphrase, Approve.
3. The connector then shows on the Android app (add it there if not). Try text chat first,
   then voice. Headset button routing to Claude is unverified.

## 8. Verification checklist after deployment
- [ ] `/.well-known/oauth-protected-resource/mcp` returns JSON with the public URL
- [ ] `POST /mcp` without token -> 401; `/register` -> 404
- [ ] wrong passphrase -> no code; right passphrase -> connector works
- [ ] `audit.jsonl` shows the call and no token, passphrase or raw client id
- [ ] raw supervisor `:8372` is not reachable from the public hostname
- [ ] (if enabled) a request from outside the allowed ranges gets 403 on `/mcp`
- [ ] kill switch tested: `touch DISABLED` -> 503; remove -> works

## 9. Rollback
`touch <state>/DISABLED` (instant), then disable the nginx vhost (or delete the HostUp DNS record) and
stop the service. To invalidate every login: `touch <state>/REVOKE_ALL`. Rotate the passphrase: new
hash + restart.

## 10. Linux and macOS service files
`deploy/gc-mcp-run.sh` + `deploy/gc-mcp.service` are a minimal systemd example. Production
GC3 deployment is the `gc_mcp` Ansible role in gas-city-infra (hardened unit, dedicated
user, SOPS-sourced env). On macOS use a launchd agent running the same wrapper.
