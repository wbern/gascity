# gc-mcp — Gas City MCP server (voice control)

Fork-owned. Read-only MCP server (Streamable HTTP, stateless) in front of the
loopback Gas City supervisor API, protected by OAuth 2.1. Tools: `city_status`,
`list_sessions`, `peek_session`, `mail_inbox`, `ready_work`, `bead_show`,
`whats_new` — all `readOnlyHint`, spoken-length answers plus structuredContent,
every string passed through `src/redact.ts`.

Safety properties (all tested): GET-only upstream client restricted to
loopback; binds 127.0.0.1 only; OAuth 2.1 auth-code + PKCE S256, dynamic client
registration, audience-bound opaque tokens (hashes at rest), rotating refresh
tokens; consent approved by an owner passphrase typed into a page outside the
model (scrypt, lockout after repeated failures); per-client rate limit; JSONL
audit log with redacted arguments and no credentials. No write tools exist.

    npm ci && npm run gen:types && npm test && npm run build
    node scripts/hash-passphrase.mjs            # prints GC_MCP_OWNER_HASH value
    GC_MCP_OWNER_HASH=... GC_MCP_PORT=47811 node dist/main.js
    DEMO_PASSPHRASE=... node scripts/oauth-demo.mjs http://127.0.0.1:47811

Env uses `GC_MCP_*` names on purpose: agent sessions export `GC_CITY` as a path.
Deployment and phone setup: `docs/runbook.md`. Samples: `docs/oauth-demo.txt`,
`docs/recorded-run.txt` (scrubbed).
