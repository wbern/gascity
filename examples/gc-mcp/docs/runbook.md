# Runbook: Gas City by voice (Claude Android + gc-mcp)

Status: DESIGN ONLY. Nothing here has been applied. Every step that touches the
Mac, Tailscale, a tunnel, launchd or the phone needs a human or devops go.

## 1. What runs where

```
Claude Android (voice) ──► Claude cloud (160.79.104.0/21) ──HTTPS──► public URL
                                                                        │
                       Funnel / Cloudflare Tunnel: ONLY this one port   ▼
                                              gc-mcp  127.0.0.1:47811  (OAuth 2.1, read-only tools)
                                                                        │ loopback, GET-only
                                                                        ▼
                                              Gas City supervisor 127.0.0.1:8372  (stays loopback)
```

- One gc-mcp process per machine, next to the supervisor it fronts. The Mac city
  gets its own instance on the Mac; GC3 gets its own on clodcod. Same artifact.
- gc-mcp binds 127.0.0.1 only. The tunnel is the only thing that reaches it.
- The raw supervisor API (:8372) must never be public. It has no auth.

## 2. Prerequisites (human)

1. Choose an owner passphrase (>= 12 chars, unique, not reused). On the host:
   `node scripts/hash-passphrase.mjs` (typed without echo, prints only a hash).
2. Put the hash in a 0600 env file, e.g. `~/.config/gc-mcp/env`:
   ```
   GC_MCP_OWNER_HASH=scrypt1$...
   GC_MCP_PUBLIC_URL=https://<public-host>[:port]
   GC_MCP_CITY=<city name from GET /v0/cities>
   GC_MCP_MAIL_AGENT=human
   ```
   `GC_MCP_PUBLIC_URL` must equal the exact final URL Claude will call (no
   redirects: Claude drops Authorization across hosts). It is also the OAuth
   issuer and the token audience (RFC 8707), so changing it invalidates tokens.
3. `npm ci && npm run build` in `examples/gc-mcp` (Node >= 20).

## 3. Reach: pick ONE (recommended: A)

### A. Tailscale Funnel (no new account)
Funnel may only use ports 443, 8443 or 10000.
```
tailscale funnel --bg --https=8443 http://127.0.0.1:47811
tailscale funnel status          # confirm ONLY 8443 -> 47811 was added
```
Public URL: `https://<machine>.<tailnet>.ts.net:8443`. Set `GC_MCP_PUBLIC_URL` to it.
Check first with `tailscale serve status` which of 443/8443/10000 are already
taken on that machine; do not displace an existing mapping.

### B. Cloudflare Tunnel (needs a Cloudflare zone)
`~/.cloudflared/config.yml`:
```
tunnel: <TUNNEL_ID>
credentials-file: /Users/<user>/.cloudflared/<TUNNEL_ID>.json
ingress:
  - hostname: gc-mcp.<your-domain>
    service: http://127.0.0.1:47811
  - service: http_status:404
```
`cloudflared tunnel run gc-mcp`. The catch-all 404 rule is what keeps everything
except this one service unreachable. `GC_MCP_PUBLIC_URL=https://gc-mcp.<your-domain>`.

### Known limitation to decide before exposing
Behind a tunnel every request arrives from 127.0.0.1, so the per-IP limiters
(consent lockout, unauthenticated cap) see ONE source. Effect: fail-closed, an
attacker spamming wrong passphrases can lock the owner out of approving for 5
minutes; they cannot get in. Fix when the tunnel's forwarded-for header is
confirmed: set express `trust proxy` accordingly. Tool-call limits are per OAuth
client and unaffected.

## 4. Mac side (launchd) — for devops/William

1. Wrapper `~/bin/gc-mcp-run` (chmod 700):
   ```
   #!/bin/sh
   set -a; . "$HOME/.config/gc-mcp/env"; set +a
   exec nice -n 10 /usr/local/bin/node "$HOME/gc2/rigs/gas-city-wbern/examples/gc-mcp/dist/main.js"
   ```
2. `~/Library/LaunchAgents/com.gascity.gc-mcp.plist`: `ProgramArguments` = the
   wrapper, `RunAtLoad` + `KeepAlive` true, `StandardErrorPath` under
   `~/Library/Logs/gc-mcp.log`. Load: `launchctl bootstrap gui/$(id -u) <plist>`.
3. Verify locally before exposing: `curl -s localhost:47811/healthz` -> `ok`,
   and `curl -si -XPOST localhost:47811/mcp` -> 401 with `resource_metadata`.
4. State/audit live in `~/.local/state/gc-mcp/` (`oauth-state.json`, `audit.jsonl`,
   both 0600). Review `audit.jsonl` after the first sessions.
5. Close the existing hole: `tailscale serve status` shows the raw supervisor
   on `:8372` is exposed to the whole tailnet unauthenticated. Remove that
   mapping (`tailscale serve --https=8372 off`) once gc-mcp is verified.
   This is the one change that REDUCES exposure; do it after step 3.

## 5. Phone setup

1. On claude.ai (web): Settings -> Connectors -> Add custom connector.
   Name `Gas City`, URL `<GC_MCP_PUBLIC_URL>/mcp`. Leave OAuth client id/secret
   empty (dynamic registration handles it).
2. Claude opens the consent page served by gc-mcp. Type the owner passphrase and
   Approve. (This page is the only place approval happens; the model never sees it.)
3. The connector then appears in the Android app (unverified: if it does not,
   add it from the app's Settings -> Connectors). Enable it for voice chats.
4. Try: "What's the state of Gas City?", "What did gc-architect say last?",
   "Do I have unread mail?", "What's ready to work on?".
5. Headset button (UNVERIFIED): set Claude as the Android default digital
   assistant so a long-press/double-press triggers it. If the headset sends a
   key event that is not routed to the assistant, map it with Key Mapper or
   Tasker to launch Claude's voice mode.

## 6. Verification checklist (after deployment)

- [ ] `https://<public>/.well-known/oauth-protected-resource/mcp` returns JSON
- [ ] `POST /mcp` without a token -> 401
- [ ] Wrong passphrase -> no redirect, no code
- [ ] A voice question returns a short spoken answer
- [ ] `audit.jsonl` shows the call, with no token or passphrase in it
- [ ] `curl https://<public>:8372/...` (the raw API) is unreachable from outside
- [ ] `tailscale funnel status` / tunnel ingress lists only gc-mcp

## 7. Rollback

`tailscale funnel --https=8443 off` (or stop cloudflared) cuts public access
instantly; `launchctl bootout gui/$(id -u)/com.gascity.gc-mcp` stops the server.
To revoke every client, delete `oauth-state.json` and restart. Rotating the
passphrase = new hash in the env file + restart.

## 8. Threat model in one paragraph

Everything the assistant reads aloud (transcripts, mail, bead text) is
attacker-influenceable text that could carry instructions. So phase 1-3 expose
READ-ONLY tools only (`readOnlyHint`), outputs are clipped and secret-redacted,
mail bodies are never returned, and consent is approved by a passphrase typed
into a page outside the model. Phase 4 writes (send to session, send mail) must
add a propose/confirm step approved the same out-of-band way; stop, kill, reset,
delete, close, config and hold-release are never exposed.
