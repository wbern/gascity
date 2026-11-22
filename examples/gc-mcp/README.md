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
transcript for candidate output after our own send (use its handle, or the legacy marker). Output never proves completion; repeat bounded checks within the agreed wait budget.
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

### Operational orientation (overnight Batch A)

Initialization carries compact first-contact operating rules. `city_guide`
provides bounded deeper guidance for overview, roles, work, communication,
operations and capabilities. The trusted curated rules have a version and
public source-file SHA-256 provenance from the core boot/claim/mail/dispatch
standards and this connector contract. Run `npm run gen:guide` after reviewing
guidance against a changed source; drift tests require matching provenance.
Runtime tools never open those files or arbitrary paths, and never project raw
private prompts, environment variables or credentials.

Roles can read the installed agent directory and optionally resolve one name.
Only names, reported pool membership, running/state and explicit unknown-purpose
markers are projected. Names do not establish role powers or persistence.
Incomplete directory reads do not authorize a guessed match. Capabilities can
probe one named/default session and its turn endpoint; missing installed routes
and missing completion signals stay explicit. Curated standards and untrusted
runtime observations are separate fields, with observation time and a bounded
4096-byte structured response. No write is added or gated by a new approval.

Mail stored, submit accepted, receipt, execution, reply and completed work are
separate claims. First-contact guidance forbids inferring completion from active
state, unchanged text or absent status. The owner can request a longer check-back
budget across bounded calls without a repeated permission question; timing out
never authorizes resending the original message.

Send acknowledgements include a connection-bound `handle`. Pass it alone to
`wait_for_reply` to retain that send's target and timestamp boundary even after
another send. Unknown/other-client handles fail closed; old `marker`/`since`
clients remain supported. After receiving a candidate, pass its `reply_id` as
`after_reply_id` on the next wait to suppress that unchanged candidate and wait
boundedly for different output. Keep arguments unchanged when retrying a lost
response: reading never consumes or advances a server-side acknowledgement.
A single deadline starts before session lookup and is carried through every
lookup page, status read, transcript read and sleep. Each transport read aborts
at its remaining budget; `waited_ms` includes lookup. No background read races
continue after a timeout.
Handles pin their first available candidate fingerprint after the send boundary;
a handle-only retry returns that candidate or `reply_candidate_unavailable`
if it was removed, never silently substitutes a later segment's output.
`after_reply_id` selects the first candidate after that fingerprint. Retries do
not skip it just because newer candidates arrived. If compaction or editing
removes the fingerprint, `reply_cursor_unavailable` reports unknown continuity;
check the latest output before explicitly resetting the cursor. This is not a
causal completion signal or a guarantee of history beyond the available segment. Handles currently live in bounded process memory
(maximum 500); restart or eviction returns unknown, never permission to resend.
`read_more(reply_id, offset)` continues that selected reply anywhere in the
available segment, even when newer replies exist. Without `reply_id` it retains
the legacy newest-reply default. Missing/edited/compacted selected replies return
`reply_unavailable`; offsets are never applied to another reply, and missing
history is never reported as the end of a reply.
Transcript output is a **temporal candidate**, not causal attribution or task
completion: `complete:false`, `completion:not_proven`, and `reply_available`
are explicit. Active/idle state and unchanged text never prove completion.
Concurrent identical sends share one backend attempt; an unconfirmed attempt is
cached for the configured replay window (default 60 seconds) to avoid retrying an
uncertain side effect. Outside that window, check before sending again.
The first-contact guide names that limitation explicitly.

### Verified role standards snapshots

`GC_MCP_GUIDE_PROFILE=gc3-2026-11-22` explicitly enables the included standards
snapshot for clodcod's `gc2` city. It is off by default; an unknown profile or a
wrong host/city stops startup. No write authority is added or restricted.
`city_guide` roles then returns a verified default-role purpose and bounded
standards for matched configured identities: CRM developer, CRM architecture,
infra operations, CRM code-review pool and the registered implementation profile. A familiar name alone is insufficient;
configuration-derived identity and expected pool/pack metadata must match. A pool
query describes the configured target without choosing a member for a send.

The trusted curated `role_contract` is separate from untrusted observations.
It includes immutable tracked-source revisions/digests, installed-template digest,
verification time, host/city and the precise verification scope. Three installed
named-agent templates differ from their tracked source: only selected clauses
verified in both are summarized, explicitly not the full effective instructions.
The reviewer template matched exactly. Current assignment, later human authority,
private additions and lifecycle policy are not inferred from a default contract.

The author/operator verifies this snapshot with fixed declared inputs only:

    npm run gen:role-contracts -- --infra-root <existing-infra-task-tree> --city-root <city>
    npm run gen:role-contracts -- --infra-root <existing-infra-task-tree> --city-root <city> --check

The verifier reads only selected public fields from `gc config explain`, checks
complete installed agent identities and verifies curated clauses against known
tracked/installed templates. It emits summaries and hashes, never raw prompts,
verification anchors, environment fields or private additions. Recheck immediately
before deploying an enabled profile; drift requires review and regeneration.
The runtime reads only the compiled snapshot and public agent directory, with no
local prompt/config file access. Add new contracts through the same verified,
reviewed path; unmatched identities remain explicitly unknown.

With the profile enabled, `city_guide` roles without a name first returns a
bounded catalogue of verified role purposes and configured targets, so names need
not be known beforehand. Query one target to read its standards/provenance. Only
contracts matching currently observed allowed identities enter that catalogue.
