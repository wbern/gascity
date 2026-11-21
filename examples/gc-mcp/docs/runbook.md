# Runbook: Gas City by voice (Claude + gc-mcp)

Status: DESIGN + HARDENED BUILD. Nothing here has been applied to any host.
Every step that touches Tailscale, a tunnel, systemd/launchd or the phone needs
a human go. Production deployment on GC3 is owned by `gas-city-infra`
(`doc/gc-mcp-deployment.md`, role `gc_mcp`); this file describes the service itself.

## 1. What runs where

```
Claude (voice) ─► Claude cloud ──HTTPS :443──► public hostname ──► gc-mcp 127.0.0.1:47811
                                                                       │ loopback, GET-only
                                                                       ▼
                                                   Gas City supervisor 127.0.0.1:8372 (never public)
```
One gc-mcp per machine, next to the supervisor it fronts (GC3 first; the Mac city is a
second instance). It binds 127.0.0.1 only. The raw supervisor API has no auth and must
never be public.

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
- Behind a tunnel the real client IP is only known if the proxy supplies it. With
  `GC_MCP_TRUST_PROXY=0` (default) there is deliberately NO per-IP limiter (a shared bucket
  would let any stranger lock the owner out). Set `GC_MCP_TRUST_PROXY=1` only after verifying
  the proxy's forwarded-for header cannot be forged by the caller.
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

## 4. The hostname problem (decision needed)

Port 443 on `clodcod.<tailnet>.ts.net` already carries the Slack webhook Funnel at `/`.
Claude discovers OAuth metadata at host-root well-known paths, so serving gc-mcp under a
sub-path (`--set-path=/gc-mcp`) breaks discovery. Therefore gc-mcp needs **its own hostname
on 443**:

| Option | Real client IP | Edge allowlist/rate-limit | Cost / catch |
|---|---|---|---|
| **A. Cloudflare Tunnel** on a dedicated hostname (recommended) | yes (`CF-Connecting-IP`, XFF) | yes (WAF: allow only the two Anthropic ranges on `/mcp` and `/token`) | needs a domain on Cloudflare + `cloudflared` |
| B. Dedicated Tailscale node/hostname with Funnel on 443 | unverified | no | a second tailscaled (container/VM); Funnel may not forward the client IP |
| C. Sub-path on the existing Funnel | n/a | n/a | **does not work** (discovery) |

Do not choose by convenience: A is the only option that gives both a real client IP and an
edge allowlist.

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
`touch <state>/DISABLED` (instant), then stop the service and remove the tunnel/Funnel
mapping (repeat the add command with `off`; never `tailscale serve reset`). To invalidate
every login: `touch <state>/REVOKE_ALL`. Rotate the passphrase: new hash + restart.

## 10. Linux and macOS service files
`deploy/gc-mcp-run.sh` + `deploy/gc-mcp.service` are a minimal systemd example. Production
GC3 deployment is the `gc_mcp` Ansible role in gas-city-infra (hardened unit, dedicated
user, SOPS-sourced env). On macOS use a launchd agent running the same wrapper.
