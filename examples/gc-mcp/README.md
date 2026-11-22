# gc-mcp — Gas City MCP server (voice control)

Fork-owned. Read-only MCP server (Streamable HTTP, stateless) in front of the
loopback Gas City supervisor API, protected by OAuth 2.1. Tools: `city_status`,
`list_sessions`, `peek_session`, `mail_inbox`, `ready_work`, `bead_show`,
`whats_new` — all `readOnlyHint`, spoken-length answers plus structuredContent,
every string passed through `src/redact.ts`.
Phase 4 (optional, OFF by default): `GC_MCP_WRITES_ENABLED=1` adds two WRITE tools, `send_to_session` and `respond_to_session`, behind the separate
scope `gc:write` (see "Write control" below). Without it the server is read-only exactly as before.

Safety properties (all tested): GET-only upstream client restricted to loopback;
binds 127.0.0.1; OAuth 2.1 auth-code + PKCE S256 with ONE static owner client (Claude) plus an
OPTIONAL second static client for ChatGPT (off by default, see below) and no
dynamic registration by default; audience-bound opaque tokens (hashes at rest),
rotating refresh tokens with reuse detection; consent approved by an owner passphrase
typed into a page outside the model (scrypt, per-request and global lockouts); output
redacted before clipping and labelled untrusted; Host check, size/time limits, kill-switch
files; per-client rate limit; rotated, redacted audit log. No write tools exist.

    npm ci && npm run gen:types && npm test && npm run build
    node scripts/hash-passphrase.mjs            # prints GC_MCP_OWNER_HASH value
    GC_MCP_OWNER_HASH=... GC_MCP_PORT=47811 node dist/main.js
    DEMO_PASSPHRASE=... DEMO_CLIENT_ID=... node scripts/oauth-demo.mjs http://127.0.0.1:47811

ChatGPT (optional, text first): `GC_MCP_CHATGPT_ENABLED=1` adds a second STATIC client with its own persisted id (`$GC_MCP_STATE_DIR/chatgpt-client-id`).
Its redirect URIs are per connection (`https://chatgpt.com/connector/oauth/<id>`) and only known after the owner creates the
connector, so set `GC_MCP_CHATGPT_REDIRECTS` (comma-separated EXACT `https://chatgpt.com/...` URIs, anything else stops startup) in a
second step; empty = the client cannot authorize. It is a public PKCE client unless `GC_MCP_CHATGPT_SECRET_HASH` (an `scrypt1$`
verifier made like the owner hash) is set, which makes `client_secret_post` mandatory at /token and /revoke. Tokens are bound to the client
that earned them (a Claude refresh token is useless to ChatGPT and the reverse). The consent page and audit log name the client.
No dynamic registration, no CIMD, no change to the owner passphrase gate.

Write control (Phase 4, `GC_MCP_WRITES_ENABLED=1`): `send_to_session(session, text, intent)` (intent `default|follow_up|interrupt_now`) and
`respond_to_session(session, action, text?, request_id?)`, implemented with the supervisor's `submit-session` and `respond-session` operations (the blessed way, not
tmux). The client can send ONLY those two operations (a fixed table in `gc-client.ts`; tests pin that stop, kill, reset, close, config, beads and any other path cannot be
sent). Rails: tokens hold `gc:read` only unless the client asks for `gc:write` (the consent page lists the scopes); write tools carry no `readOnlyHint` and are listed only to a
`gc:write` token; text <= 4000 plain characters; 20 writes/minute; the session must exist; a default deny list (`*-william`, `claude-william`, `zachris-*`, `elsakert-*`, replaced by
`GC_MCP_WRITE_DENY_SESSIONS`), an optional `GC_MCP_WRITE_ALLOW_SESSIONS`, and sessions with a person attached are refused unless `GC_MCP_WRITE_ALLOW_ATTACHED=1`; every attempt,
sent or refused, is audited (client name, session, intent, length, an 80-character redacted preview, outcome); `WRITES_DISABLED` in the state dir turns writes off for the next
request while reads keep working. `confirmWrite()` in `src/writes.ts` is the single place a later second factor (TOTP/push) slots in; it allows today.

Env uses `GC_MCP_*` names on purpose: agent sessions export `GC_CITY` as a path.
Security model, residual risks, hostname decision and phone setup: `docs/runbook.md`. Samples: `docs/oauth-demo.txt`, `docs/discovery-e2e.txt` (path-prefixed OAuth discovery through a real nginx),
`docs/recorded-run.txt` (scrubbed).
