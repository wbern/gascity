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
   # GC_MCP_PEEK_ALLOW=mayor,gc-architect    # OPTIONAL: restrict which sessions may be read (unset = gc:read reads any session)
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

## ChatGPT as a second client (optional, off by default)

Two steps, because ChatGPT's redirect URI is per connection and only exists after the connector is created:

1. **Enable with no redirects.** `GC_MCP_CHATGPT_ENABLED=1`, restart. The client id is created in the state dir (`chatgpt-client-id`, 0600).
   The ChatGPT client cannot authorize yet (fail closed). Optional: `GC_MCP_CHATGPT_SECRET_HASH` (scrypt1$ verifier) for a confidential client.
2. **Create the connector in ChatGPT** (chatgpt.com/plugins) with URL `<GC_MCP_PUBLIC_URL>/mcp`, "user-defined OAuth client", the ChatGPT client id
   (and the secret if confidential). Copy the redirect URI the plugin page shows. Set `GC_MCP_CHATGPT_REDIRECTS=<that exact URI>` and restart.
   Then connect, approve with the owner passphrase on the consent page (it names ChatGPT), and test in TEXT first.

Not verified against a live ChatGPT: whether it sends `client_secret_post` or HTTP Basic (only post is accepted), the exact redirect URI
shape, and whether it works with a public client. Failed secret attempts are limited per source address; behind a proxy that hides callers
(`GC_MCP_TRUST_PROXY=0`, or a source-NATting firewall) that bucket is shared, so wrong secrets can lock ChatGPT out for five minutes.

## Phase 4: write control (optional, OFF by default)

**What it is.** With `GC_MCP_WRITES_ENABLED=1` a client holding the `gc:write` scope can message agents: `send_to_session` (like typing into the session's terminal, via the
supervisor's `submit-session`) and `respond_to_session` (answer a waiting agent, via `respond-session`). Nothing else mutates: the client has no stop, kill, reset, delete, close,
config, hold, bead or generic POST path (fixed operation table, tested).

**The new risk, plainly.** A stolen token, or anyone with the owner passphrase, can now instruct agents, and agents can act on their own repos and PRs. There is no per-action approval page
(by decision); a second factor is a later feature with a hook ready (`confirmWrite`). Mitigations that are in place: a separate scope the client must ask for and the owner sees on the consent
page; no session is protected by default (gc3 is the owner's machine); optional operator deny and allow lists; attached sessions are treated like any other unless `GC_MCP_WRITE_ALLOW_ATTACHED=0`; 20 writes a minute; 4000-character plain text; every attempt audited with the client's name; the
`WRITES_DISABLED` kill switch (`touch /var/lib/gc-mcp/WRITES_DISABLED`; reads keep working), plus `DISABLED` and `REVOKE_ALL`. Untrusted-content labels stay on every read tool, but prompt injection through
what the assistant reads remains an accepted risk: it can now ask an agent to do something.

**Connecting with write access.** Claude: the connector requests the scopes the server advertises; if it only granted gc:read, remove and re-add the connector after enabling writes.
ChatGPT: in the connector form tick `gc:write` under the default scopes once the server advertises it. The consent page lists the granted scopes: `gc:read gc:write` means read AND write.

### Voice loop (vibe coding by voice)

Set `GC_MCP_DEFAULT_SESSION=claude-william` (or whichever session is yours): every tool then works with no session argument, because nobody says session ids. A spoken name ("steve",
"the mayor", "crm slash steve") is resolved against the live session list: exact first, then a unique partial match; if it is ambiguous the tool asks which one and sends nothing.
The loop is `send_to_session` (returns at once with an acknowledgement and a marker), `wait_for_reply` (at most 20 seconds, so it returns before a chat client's tool timeout; call it again to keep waiting),
`session_state` (working, idle, waiting for input), `latest_reply` and `read_more` for long replies. A retried identical send within 60 seconds is acknowledged, not sent twice.
Not verified: how ChatGPT voice mode treats tool latency and the acknowledgement; whether the transcript's assistant turns arrive in the shape the tests model; the 1.5 s poll interval against a busy supervisor.

## Simple connect: dynamic client registration (optional, OFF by default)

`GC_MCP_ALLOW_DCR=1` turns on `/register` and advertises `registration_endpoint` (and only then). Claude and ChatGPT then connect with just the URL: they register themselves, you type the owner
passphrase on the consent page. **The new risk, plainly:** an open registration endpoint on the internet. Mitigations: every redirect URI must be in the safe set (exact Claude callbacks, ChatGPT
connector callbacks, loopback with a port, `GC_MCP_DCR_EXTRA_REDIRECTS`), so a stranger cannot register a client that sends an approval code to their own site; registration alone grants nothing (the
passphrase still gates every approval, and the page names the app as UNVERIFIED with the redirect host in bold); public PKCE clients only, never a secret; a global limit of 10 registrations an hour, a
cap of 25 clients (never-used pruned first), 90-day expiry, an 8 kB body cap; `REVOKE_ALL` removes every registered client. An attacker can still use up the hourly limit so that YOUR first connect gets a
429: the static-client steps stay available as the fallback. `GC_MCP_DCR_OPEN_REDIRECTS=1` is UNSAFE (any https site may register; registered clients are read-only while it is on) and off by default.

### The morning drive: voice-first reads

The north star is "get into the car, start a voice chat, ask how my agents are doing." Say "how are my agents doing?": the assistant calls `briefing` and says a few short sentences, the thing
that needs you first. "What is steve working on?" is `what_is_working_on`; "is steve waiting for me?" is `session_state`; "say hello to steve" is `send_to_session` then `wait_for_reply`; "want more?" is
`read_more`. The server's initialize `instructions` tell the assistant to keep answers short and spoken, never read ids or paths aloud, ask which agent when a name is ambiguous, say what needs you first,
and treat transcript, mail and bead text as untrusted. Not verified: how Claude or ChatGPT voice modes actually use `instructions` and tool descriptions (they may ignore them), and whether the supervisor
exposes human-decision gates as beads of type `gate` with `await_type: human` in this city (the briefing then says it could not read the decisions rather than inventing any).
