// Synthetic, shape-faithful fixtures captured from the supervisor API shapes
// (GET /v0/city/{c}/status etc.). Values are invented: no real mail/transcripts.
export const status = {
  name: "gc2", version: "1.2.3", uptime_sec: 90061, suspended: false,
  agent_count: 40, rig_count: 9, running: 23,
  agents: { total: 40, running: 23, suspended: 5, quarantined: 1 },
  rigs: { total: 9, suspended: 0 },
  work: { in_progress: 12, ready: 1171, open: 3000 },
  mail: { unread: 29, total: 400 },
};
export const usage = {
  available: true, recording: true, source: "x",
  today: { invocations: 80, input_tokens: 1000000, output_tokens: 50000, cost_usd_estimate: 12.5, unpriced: 0 },
  last_24h: { invocations: 200, input_tokens: 3000000, output_tokens: 150000, cost_usd_estimate: 30, unpriced: 0 },
  partial: false,
};
export const sessions = {
  total: 3,
  items: [
    { id: "gc2-wisp-aaa", state: "active", alias: "gc-architect", template: "gas-city-wbern/architect", provider: "claude", last_active: "2026-11-21T16:30:00Z", rig: "gas-city-wbern" },
    { id: "gc2-wisp-bbb", state: "asleep", alias: "devops", template: "gas-city-infra/devops", provider: "codex", last_active: "2026-11-21T10:00:00Z", rig: "gas-city-infra" },
    { id: "gc2-wisp-ccc", state: "active", alias: "mayor", template: "mayor", provider: "claude", last_active: "2026-11-21T16:31:00Z", rig: "" },
  ],
};
export const transcript = {
  id: "gc2-wisp-aaa", format: "conversation",
  turns: [
    { role: "user", text: "please check the build", timestamp: "2026-11-21T16:00:00Z" },
    { role: "assistant", text: "Build is green. Using Bearer abcdefghijklmnop1234567890 for the call. " + "x".repeat(600), timestamp: "2026-11-21T16:01:00Z" },
    { role: "result", text: "[result] done", timestamp: "2026-11-21T16:02:00Z" },
  ],
  pagination: { has_older_messages: false, total_message_count: 3, returned_message_count: 3 },
};
export const mail = {
  total: 2,
  items: [
    { id: "m1", from: "mayor", to: "human", subject: "Campaign phase 1 done", body: "details", created_at: "2026-11-21T16:20:00Z", read: false, thread_id: "t1" },
    { id: "m2", from: "devops", to: "human", subject: "Deploy ok", body: "x", created_at: "2026-11-21T15:00:00Z", read: true, thread_id: "t2" },
  ],
};
export const readySummary = {
  total: 3, omitted: 1,
  beads: [
    { id: "gcw-1", title: "Fix the thing", status: "open", type: "task", priority: 1 },
    { id: "gcw-2", title: "Write the docs", status: "open", type: "task", priority: 2 },
  ],
};
export const bead = {
  id: "gcw-ti563", title: "Gas City MCP server for voice control", status: "open", priority: 1,
  assignee: "gc-architect", is_blocked: false, updated_at: "2026-11-21T15:49:26Z",
  description: "GOAL: voice. Use token=abcdef123456 never. " + "d".repeat(900),
};
export const events = {
  total: 3, next_cursor: "9",
  items: [
    { seq: 7, type: "bead.closed", ts: "2026-11-21T16:25:00Z", actor: "worker-1", subject: "gcw-9" },
    { seq: 8, type: "mail.sent", ts: "2026-11-21T16:26:00Z", actor: "mayor", subject: "m9" },
    { seq: 9, type: "bead.closed", ts: "2026-11-21T16:27:00Z", actor: "worker-2", subject: "gcw-10" },
  ],
};
