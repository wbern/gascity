> **North star (William):** "I want to get into the car in the morning, start a Claude voice chat, and converse with my agents on gc3 to see how they are doing; right up there with being able to talk to Slay CRM. I cannot emphasize enough that I want this to feel SIMPLE."

# gc-mcp — Gas City MCP server (voice control)

Fork-owned. MCP server (Streamable HTTP, stateless) in front of the
loopback Gas City supervisor API, protected by OAuth 2.1. Tools: `city_status`,
`list_sessions`, `peek_session`, `mail_inbox`, `ready_work`, `bead_show`,
`whats_new` — all `readOnlyHint`, spoken-length answers plus structuredContent,
every string passed through `src/redact.ts`.
Optional writes (OFF by default): `GC_MCP_WRITES_ENABLED=1` adds seven WRITE tools (session input, mail send/reply/mark-read, bead creation and routing), behind the separate
scope `gc:write` (see "Write control" below). Without it the server is read-only exactly as before.

Safety properties (tested): upstream client restricted to loopback, with GET reads and a fixed POST allowlist;
binds 127.0.0.1; OAuth 2.1 auth-code + PKCE S256 with ONE static owner client (Claude) plus an
OPTIONAL second static client for ChatGPT (off by default, see below) and no
dynamic registration by default; audience-bound opaque tokens (hashes at rest),
rotating refresh tokens with reuse detection; consent approved by an owner passphrase
typed into a page outside the model (scrypt, per-request and global lockouts); output
redacted before clipping and labelled untrusted; Host check, size/time limits, kill-switch
files; per-client rate limit; rotated, redacted audit log. Write tools require explicit enablement and scope.

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
tmux). The client sends only the seven approved operations in the fixed table in `gc-client.ts`; tests exclude stop, kill, reset, close, delete, config and forced reassignment. Rails: tokens hold `gc:read` only unless the client asks for `gc:write` (the consent page lists the scopes); write tools carry no `readOnlyHint` and are listed only to a
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

Faithful to the Gas City API (not a lossy summary): the API's `tail` on a transcript counts compaction SEGMENTS, so the server asks for the last segment only and takes the last messages
from it (a segment can be hundreds of KB: it is read with an 8 MB cap, and anything larger is reported as "too large", never as "Gas City is down"); `wait_for_reply` polls the small
session record and reads the transcript only when something changed, at most three times per call; `latest_reply`, `read_more` and `wait_for_reply` send only the PAGE that was spoken (plus
`total_chars`, `next_offset` and a `reply_id`), and `read_more` is pinned to that `reply_id` so an offset never lands in a different message. Lists are read across pages and the API's
`total`, `partial` and `next_cursor` are passed through ("counted 200 of 900"; "at least 12 decisions"). Errors are told apart: not found, too large, timed out, Gas City not responding, refused.
Derived values say so (`derived`, `source`): agent state, "working", flagged mail and the finished-task filter are heuristics and the briefing's `structuredContent.derived` says how each was worked
out. Nothing names a person or a city in the code: `GC_MCP_OWNER_NAME` (optional) addresses the owner and `GC_MCP_CITY` is read from the environment or, when the supervisor runs exactly one city, from it.

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

### Transparent reads (audit Batch 2)

`gc_api_list_operations` discovers the supported city GET operations by subject,
with path/query parameter schemas and paged catalogue output. `gc_api_read` calls
one operation by its OpenAPI id. It accepts only declared parameters, supports
repeated query values, and optionally selects top-level response fields. No raw
URL, configuration, patches, provider settings, external messages or streams are
exposed. The generated catalogue imports the generated `operations` type; run
`node scripts/gen-read-catalog.mjs` after updating the OpenAPI specification.
Contract tests reject drift.

Read data is labelled untrusted and redacted before paging. Each structured
result is at most 4096 UTF-8 JSON bytes. Join `page` fragments until `done` to
reconstruct the response. The outer `next_cursor` continues a fragment; the
response's own `next_cursor` continues an API list via `query.cursor`. API cursors
are held server-side and represented by client/operation-bound handles so
credential redaction cannot destroy them. Both kinds expire after five minutes,
are bounded in memory, and disappear on restart. Upstream totals and partial
flags remain data, without inferred summaries. With optional session read lists
configured, raw reads fail closed; use the session-aware spoken tools instead.

`docs/recorded-batch2.txt` records actual loopback supervisor reads on clodcod;
it does not claim that this source has been deployed.


### Mail and work actions (audit Batch 3)

The read catalogue includes mail bodies and threads, bead lists, dependencies and
work graphs, convoys, order details/history and run details/steps. Discover the
matching read by subject, then pass its declared parameters. These reads do not
mark mail as read.

With writes enabled and `gc:write` granted, `mail_send`, `mail_reply` and
`mail_mark_read` operate on mail; sends and replies identify the sender as
`human`. `bead_create` requires a named rig. `bead_sling` routes an existing bead
to a named target in a named rig, optionally using a formula. Creation and routing
are separate calls, so a failed route leaves the created bead visible. No force,
reassign, close, archive, delete, configuration or lifecycle action is exposed.
Optional session lists govern session tools; they do not restrict these explicit
mail and work actions.

All five actions enter `WriteGate.run`, share its rate limit and kill switch,
validate strict input schemas, audit attempts without mail/task bodies, and call
the existing confirmation hook. Bodies are capped at 8000 UTF-8 bytes and text
fields at 4000 characters. Responses contain short acknowledgements and selected
identifiers. Simultaneous identical calls share one attempt; an identical retry
within 60 seconds returns the cached result. A lost response is reported as
`outcome_unknown` and cached too. Verify state before repeating: this in-memory
protection does not survive server restart or the replay window, and the API does
not offer durable idempotency for every action. No production mutations were used
in author validation; live deployment and verification remain with crm/steve.

Session-name resolution requires a complete list. If the supervisor reports
partial data, an unread next page, or a total larger than the fetched rows,
spoken session reads and submit/respond return `incomplete_sessions`. A visible
unique name is insufficient: an unseen session may have the same name.
The HTTP regressions cover two DCR clients with the same display name through
PKCE, OAuth and MCP, including cursor and send-marker isolation.
