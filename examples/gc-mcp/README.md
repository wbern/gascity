> **North star (William):** "I want to get into the car in the morning, start a Claude voice chat, and converse with my agents on gc3 to see how they are doing; right up there with being able to talk to Slay CRM. I cannot emphasize enough that I want this to feel SIMPLE."

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
`gc:write` token; text <= 4000 plain characters; 20 writes/minute; the session must exist; NO session is denied by default (gc3 is the owner's machine: `gc:write` sends to any session, `gc:read` reads any session, and a session a person is attached to is treated like any other). The lists are OPTIONAL operator settings, empty by default: `GC_MCP_WRITE_DENY_SESSIONS`, `GC_MCP_WRITE_ALLOW_SESSIONS`, `GC_MCP_READ_DENY_SESSIONS`, `GC_MCP_PEEK_ALLOW`, and `GC_MCP_WRITE_ALLOW_ATTACHED=0` to refuse attached sessions; every attempt,
sent or refused, is audited (client name, session, intent, length, an 80-character redacted preview, outcome); `WRITES_DISABLED` in the state dir turns writes off for the next
request while reads keep working. `confirmWrite()` in `src/writes.ts` is the single place a later second factor (TOTP/push) slots in; it allows today.

Voice-first read experience: `briefing` answers "how are my agents doing?" in ONE call as a calm morning briefing, at most 400 characters: what needs the owner FIRST (open human
decisions described by their question, never a raw gate title, newest first, with "many look stale, the oldest is N days old" past ten; agents waiting for input; stuck work; mail from the
mayor or marked for the owner), then who is working and on what (an agent in a turn, or holding a bead, is working; "nobody is working right now" when so), then what finished (real tasks by
title, never automation), then mail only if useful (a pile of notices is "nothing flagged for you", never a count). Ids, hashes and agent paths are never spoken. A source the supervisor
cannot give is named in one clause, never guessed. `what_is_working_on(session?)` answers "what is steve working on?" in a sentence or two. The server sends `instructions` in the
initialize result written for a spoken assistant, and every tool's title and description is plain spoken intent ("Ask how the agents are doing", "Say something to an agent").

Reading is by scope only: `gc:read` reads ANY session, and nothing is protected by default. If an operator DOES configure a list and it refuses a read, the answer says why in one speakable
sentence with `{found:true, allowed:false, reason, name}` in structuredContent (`src/access.ts`): `denied_session` ("zachris gemini 2 is on the protected list, so I will not read or write it. Ask the
owner to change the list if you want that.") or `not_in_allow_list` ("... is not on the list of agents I may read. Ask the owner to add it."); it never blames "voice". `GC_MCP_READ_DENY_SESSIONS`
defaults to the write deny list; set it (even to empty) to differ for reads. The briefing hides no agent or task unless an operator protected that session.

Voice loop (vibe coding by voice): `GC_MCP_DEFAULT_SESSION` (for example `claude-william`) is what an omitted `session` means on EVERY tool, because nobody says session ids aloud.
A spoken name is resolved against the live session list (`src/sessions.ts`): exact match first, then a unique case-insensitive prefix, suffix or contains match
(`steve` is `crm/steve`; `crm slash steve` works); if more than one fits, the tool asks ("I found crm slash steve and crm slash bob, which one?") and sends nothing.
Read tools for the loop (all spoken text <= 280 characters, full text in structuredContent, replies redacted, links dropped and labelled untrusted): `session_state`
(working | idle | waiting for input | not running), `latest_reply`, `read_more(offset)` for the rest of a long reply, and `wait_for_reply(max_seconds <= 20, default 15)`, which polls the
transcript for the first reply after our own send (the send acknowledgement carries a marker) and always returns inside 20 seconds ("still working" or the reply so far).
A repeated identical send within 60 seconds from the same client returns the first acknowledgement instead of sending twice (voice clients retry); each real send has its own
idempotency key. Any `gc:read` token reads any session's replies (the default session only fills in an omitted name).

Simple connect (optional, `GC_MCP_ALLOW_DCR=1`): the server accepts dynamic client registration (RFC 7591), so Claude or ChatGPT needs only the server URL, then you type the owner
passphrase: no client id, no redirect URI to copy. A registration is accepted ONLY if every redirect URI is an exact, canonical Claude callback, ChatGPT connector callback
(`https://chatgpt.com/connector/oauth/<id>` or `https://chatgpt.com/connector_platform_oauth_redirect`), a loopback `http://127.0.0.1|localhost:<port>/...` URL, or listed in
`GC_MCP_DCR_EXTRA_REDIRECTS`; anything else is `invalid_redirect_uri`. Registered clients are PUBLIC (a requested secret method is ignored, no secret is created or stored), PKCE-only.
Limits: registration rate limit 10 an hour for everyone (one shared address behind the NAT; only successful registrations count), 25 clients (the oldest never-used is pruned), unused
for 90 days = forgotten, 8 kB request cap, names sanitised. The consent page shows the name as UNVERIFIED, the redirect host in bold, first time or known, read-only or READ AND WRITE.
`REVOKE_ALL` also removes every registered client. `GC_MCP_DCR_OPEN_REDIRECTS=1` (UNSAFE, off) lets any https redirect register; registered clients are then read-only. The static Claude and
ChatGPT clients remain as a fallback.

Env uses `GC_MCP_*` names on purpose: agent sessions export `GC_CITY` as a path.
Security model, residual risks, hostname decision and phone setup: `docs/runbook.md`. Samples: `docs/oauth-demo.txt`, `docs/discovery-e2e.txt` (path-prefixed OAuth discovery through a real nginx),
`docs/recorded-run.txt` (scrubbed).
