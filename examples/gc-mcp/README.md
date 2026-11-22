# gc-mcp — Gas City MCP server (voice control)

Fork-owned. Read-only MCP server (Streamable HTTP, stateless) in front of the
loopback Gas City supervisor API, protected by OAuth 2.1. Tools: `city_status`,
`list_sessions`, `peek_session`, `mail_inbox`, `ready_work`, `bead_show`,
`whats_new` — all `readOnlyHint`, spoken-length answers plus structuredContent,
every string passed through `src/redact.ts`.

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

Env uses `GC_MCP_*` names on purpose: agent sessions export `GC_CITY` as a path.
Security model, residual risks, hostname decision and phone setup: `docs/runbook.md`. Samples: `docs/oauth-demo.txt`, `docs/discovery-e2e.txt` (path-prefixed OAuth discovery through a real nginx),
`docs/recorded-run.txt` (scrubbed).
