import { describe, expect, it } from 'vitest';
import type { Bead } from 'gas-city-dashboard-shared/gc-supervisor';
import {
  inProgressCardNote,
  selectBeadsNeedingAttention,
  type BeadAttentionSession,
} from './beadsNeedingAttention';

const NOW = Date.parse('2026-06-07T12:00:00.000Z');

function bead(overrides: Partial<Bead>): Bead {
  return {
    created_at: '2026-06-07T11:00:00.000Z',
    id: 'B-0',
    issue_type: 'task',
    status: 'open',
    title: 'Bead',
    ...overrides,
  };
}

function session(overrides: Partial<BeadAttentionSession>): BeadAttentionSession {
  return {
    id: 'ci-1',
    session_name: 'worker-ci-1',
    state: 'active',
    last_active: '2026-06-07T11:58:00.000Z',
    ...overrides,
  };
}

function select(
  inputs: {
    beads?: readonly Bead[];
    escalations?: readonly Bead[];
    sessions?: readonly BeadAttentionSession[];
  },
  now = NOW,
) {
  return selectBeadsNeedingAttention(
    {
      beads: inputs.beads ?? [],
      escalations: inputs.escalations ?? [],
      ...(inputs.sessions === undefined ? {} : { sessions: inputs.sessions }),
    },
    now,
  );
}

describe('selectBeadsNeedingAttention (gascity-dashboard-2j8e.3)', () => {
  it('includes a ready-unclaimed bead once it has aged past the watch window', () => {
    const rows = select({
      beads: [bead({ id: 'B-ready', status: 'open', created_at: '2026-06-05T11:00:00.000Z' })],
    });
    expect(rows).toEqual([
      expect.objectContaining({ beadId: 'B-ready', reason: 'ready-unclaimed', severity: 'watch' }),
    ]);
  });

  it('escalates a long-stale ready-unclaimed bead to attention', () => {
    const rows = select({
      beads: [bead({ id: 'B-stale', status: 'open', created_at: '2026-06-01T11:00:00.000Z' })],
    });
    expect(rows[0]).toEqual(
      expect.objectContaining({ reason: 'ready-unclaimed', severity: 'attention' }),
    );
  });

  it('does not surface a freshly-filed open bead as noise', () => {
    const rows = select({
      beads: [bead({ id: 'B-fresh', status: 'open', created_at: '2026-06-07T11:30:00.000Z' })],
    });
    expect(rows).toEqual([]);
  });

  it('does not surface an assigned open bead as ready-unclaimed', () => {
    const rows = select({
      beads: [
        bead({
          id: 'B-assigned',
          status: 'open',
          assignee: 'worker-1',
          created_at: '2026-06-01T11:00:00.000Z',
        }),
      ],
    });
    expect(rows).toEqual([]);
  });

  it('includes an abnormally-blocked (escalated) bead immediately, regardless of age', () => {
    const rows = select({
      escalations: [
        bead({
          id: 'B-esc',
          status: 'blocked',
          labels: ['gc:escalation'],
          created_at: '2026-06-07T11:55:00.000Z',
        }),
      ],
    });
    expect(rows).toEqual([
      expect.objectContaining({ beadId: 'B-esc', reason: 'escalated', severity: 'attention' }),
    ]);
  });

  it('excludes a plain dependency-blocked bead (working-as-intended queuing)', () => {
    const rows = select({
      beads: [bead({ id: 'B-dep', status: 'blocked', created_at: '2026-06-01T11:00:00.000Z' })],
    });
    expect(rows).toEqual([]);
  });

  it('excludes a closed (resolved) escalation', () => {
    const rows = select({
      escalations: [bead({ id: 'B-done', status: 'closed', labels: ['gc:escalation'] })],
    });
    expect(rows).toEqual([]);
  });

  it('does not count a P1 high-priority open bead just for its priority', () => {
    const rows = select({
      beads: [
        bead({
          id: 'B-p1',
          status: 'open',
          priority: 1,
          assignee: 'worker-1',
          created_at: '2026-06-07T11:55:00.000Z',
        }),
      ],
    });
    expect(rows).toEqual([]);
  });

  it('combines ready-unclaimed and escalated across both inputs', () => {
    const rows = select({
      beads: [bead({ id: 'B-ready', status: 'open', created_at: '2026-06-05T11:00:00.000Z' })],
      escalations: [bead({ id: 'B-esc', status: 'blocked', labels: ['gc:escalation'] })],
    });
    expect(rows.map((row) => `${row.beadId}:${row.reason}`)).toEqual([
      'B-esc:escalated',
      'B-ready:ready-unclaimed',
    ]);
  });
});

describe('stalled in-progress detection (gp-6xd)', () => {
  const inProgress = (overrides: Partial<Bead> = {}) =>
    bead({
      id: 'B-doing',
      status: 'in_progress',
      assignee: 'worker-ci-1',
      updated_at: '2026-06-07T10:00:00.000Z',
      ...overrides,
    });

  it('marks an in-progress bead with no assignee as stalled', () => {
    const rows = select({
      beads: [
        bead({ id: 'B-doing', status: 'in_progress', updated_at: '2026-06-07T10:00:00.000Z' }),
      ],
    });
    expect(rows).toEqual([
      expect.objectContaining({
        beadId: 'B-doing',
        reason: 'stalled',
        severity: 'attention',
        summary: expect.stringContaining('no assignee'),
      }),
    ]);
  });

  it('marks an in-progress bead as stalled when no session resolves to the assignee', () => {
    const rows = select({
      beads: [inProgress()],
      sessions: [session({ session_name: 'someone-else' })],
    });
    expect(rows[0]).toEqual(
      expect.objectContaining({
        reason: 'stalled',
        summary: expect.stringContaining('no live session for worker-ci-1'),
      }),
    );
  });

  it('marks an in-progress bead as stalled when its session is not in a live state', () => {
    const rows = select({
      beads: [inProgress()],
      sessions: [session({ state: 'dead' })],
    });
    expect(rows[0]).toEqual(
      expect.objectContaining({
        reason: 'stalled',
        summary: expect.stringContaining('session dead'),
      }),
    );
  });

  // A closed session decodes to the empty state ("closed beads have no runtime
  // state", internal/session/info_codec.go), which must not render as the
  // dangling "stalled 3h — session ".
  it('names an ended session rather than emitting a dangling empty state', () => {
    const rows = select({
      beads: [inProgress()],
      sessions: [session({ state: '' })],
    });
    expect(rows[0]).toEqual(
      expect.objectContaining({
        reason: 'stalled',
        summary: expect.stringContaining('session ended'),
      }),
    );
    expect(rows[0]?.summary).not.toMatch(/session\s*$/);
  });

  // `state` is free-form with no enum upstream, and the sibling reader
  // (shared/src/agents/needsYou.ts) matches it case-insensitively — a differently
  // cased spelling must not paint a healthy worker stalled.
  it('treats a live state as live regardless of case', () => {
    const rows = select({ beads: [inProgress()], sessions: [session({ state: 'Active' })] });
    expect(rows).toEqual([]);
  });

  it('marks an in-progress bead as stalled when activity is older than an hour', () => {
    const rows = select({
      beads: [inProgress()],
      sessions: [session({ last_active: '2026-06-07T10:00:00.000Z' })],
    });
    expect(rows[0]).toEqual(
      expect.objectContaining({
        reason: 'stalled',
        summary: expect.stringContaining('no activity'),
      }),
    );
  });

  it('does not mark a bead with a live, recently-active session', () => {
    const rows = select({ beads: [inProgress()], sessions: [session({})] });
    expect(rows).toEqual([]);
  });

  it('a fresh bead heartbeat keeps a quiet session from reading as stalled', () => {
    const rows = select({
      beads: [inProgress({ metadata: { 'gc.last_heartbeat_at': '2026-06-07T11:59:00.000Z' } })],
      sessions: [session({ last_active: '2026-06-07T09:00:00.000Z' })],
    });
    expect(rows).toEqual([]);
  });

  it('resolves the session by gc.session_id when the assignee name does not match', () => {
    const rows = select({
      beads: [inProgress({ metadata: { 'gc.session_id': 'ci-9' } })],
      sessions: [session({ id: 'ci-9', session_name: 'renamed' })],
    });
    expect(rows).toEqual([]);
  });

  it('skips session-dependent checks when the session read failed (sessions omitted)', () => {
    const rows = select({ beads: [inProgress()] });
    expect(rows).toEqual([]);
  });

  it('does not guess stalled from a stale heartbeat alone when the session read failed', () => {
    // The worker may be alive but not heartbeating (the common pre-fix state);
    // only session data can distinguish that from a dead worker — do not guess.
    const rows = select({
      beads: [inProgress({ metadata: { 'gc.last_heartbeat_at': '2026-06-07T09:00:00.000Z' } })],
    });
    expect(rows).toEqual([]);
  });

  it('prefers a live session over a dead one carrying the same recycled name', () => {
    const rows = select({
      beads: [inProgress()],
      sessions: [session({ id: 'ci-old', state: 'dead' }), session({ id: 'ci-new' })],
    });
    expect(rows).toEqual([]);
  });

  it('resolves an assignee that is a bare session id', () => {
    const rows = select({
      beads: [inProgress({ assignee: 'ci-1' })],
      sessions: [session({ session_name: 'unrelated-name' })],
    });
    expect(rows).toEqual([]);
  });
});

// FORK-LOCAL. Upstream's stalled rule reads ownership off `bead.assignee`.
// This fork never stamps `assignee` on in-progress work: the dispatcher pins
// the worker in bead metadata instead (`gc.session_id` / `gc.session_name`,
// with `gc.execution_routed_to` / `gc.routed_to` naming the agent route). So
// every in-progress bead fell into upstream's ownerless branch and rendered
// "stalled Nd — no assignee".
describe('stalled detection from the metadata ownership pin (fork-local)', () => {
  const pinned = (metadata: Record<string, string>, overrides: Partial<Bead> = {}) =>
    bead({
      id: 'B-doing',
      status: 'in_progress',
      updated_at: '2026-06-07T10:00:00.000Z',
      metadata,
      ...overrides,
    });

  it('does not mark a gc.session_id-pinned bead whose session is live and active', () => {
    const rows = select({
      beads: [pinned({ 'gc.session_id': 'ci-9' })],
      sessions: [session({ id: 'ci-9', session_name: 'gc2-worker' })],
    });
    expect(rows).toEqual([]);
  });

  it('does not mark a gc.session_name-pinned bead whose session is live and active', () => {
    const rows = select({
      beads: [pinned({ 'gc.session_name': 'codex-polecat-gc2-e05h6m' })],
      sessions: [session({ id: 'ci-9', session_name: 'codex-polecat-gc2-e05h6m' })],
    });
    expect(rows).toEqual([]);
  });

  it('names the pinned session when it is absent from the session listing', () => {
    const rows = select({
      beads: [pinned({ 'gc.session_id': 'gc2-cdwv0s' })],
      sessions: [session({ id: 'ci-1' })],
    });
    expect(rows[0]).toEqual(
      expect.objectContaining({
        reason: 'stalled',
        summary: expect.stringContaining('no live session for gc2-cdwv0s'),
      }),
    );
    expect(rows[0]?.summary).not.toContain('no assignee');
  });

  it('names an ended pinned session rather than reporting it ownerless', () => {
    const rows = select({
      beads: [pinned({ 'gc.session_id': 'ci-9' })],
      sessions: [session({ id: 'ci-9', state: '' })],
    });
    expect(rows[0]?.summary).toContain('session ended');
  });

  it('names a non-live pinned session state', () => {
    const rows = select({
      beads: [pinned({ 'gc.session_id': 'ci-9' })],
      sessions: [session({ id: 'ci-9', state: 'asleep' })],
    });
    expect(rows[0]?.summary).toContain('session asleep');
  });

  it('reports inactivity for a live pinned session that has gone quiet', () => {
    const rows = select({
      beads: [pinned({ 'gc.session_id': 'ci-9' })],
      sessions: [session({ id: 'ci-9', last_active: '2026-06-07T10:00:00.000Z' })],
    });
    expect(rows[0]?.summary).toContain('no activity for');
  });

  // The routed-to keys name an agent SLOT ("gas-city-infra/codex-polecat"),
  // not a session: live aliases carry a pool index ("…/codex-polecat-3"), so a
  // route never resolves to a session identity. It is still proof the bead is
  // owned, so "no assignee" is wrong — and with no resolvable session there is
  // no liveness evidence, so upstream's don't-guess rule applies. In practice
  // these are formula root beads that stay in_progress while their children run.
  it('does not call a routed-to bead ownerless, and does not guess it stalled', () => {
    const rows = select({
      beads: [pinned({ 'gc.execution_routed_to': 'gas-city-infra/codex-polecat' })],
      sessions: [session({ alias: 'gas-city-infra/codex-polecat-3' })],
    });
    expect(rows).toEqual([]);
  });

  it('keeps upstream "no assignee" for a bead with no assignee and no pin at all', () => {
    const rows = select({
      beads: [pinned({})],
      sessions: [session({})],
    });
    expect(rows[0]).toEqual(
      expect.objectContaining({
        reason: 'stalled',
        summary: expect.stringContaining('no assignee'),
      }),
    );
  });

  // Honest degrade. A genuinely ownerless bead is ownerless whether or not the
  // session read succeeded — that verdict needs no session data, so it still
  // fires (upstream's behaviour, unchanged). A PINNED bead is the opposite: the
  // only thing that could condemn it is session data we do not have, so it is
  // skipped rather than guessed. A sessions outage therefore cannot convert the
  // fork's pinned in-progress work into a board full of false stalls.
  it('still reports a genuinely ownerless bead when the sessions read failed', () => {
    const rows = select({ beads: [pinned({})] });
    expect(rows[0]).toEqual(
      expect.objectContaining({
        reason: 'stalled',
        summary: expect.stringContaining('no assignee'),
      }),
    );
  });

  it('skips a pinned bead entirely when the sessions read failed', () => {
    const rows = select({ beads: [pinned({ 'gc.session_id': 'gc2-cdwv0s' })] });
    expect(rows).toEqual([]);
  });

  it('prefers the assignee over the metadata pin when both are present', () => {
    const rows = select({
      beads: [pinned({ 'gc.session_id': 'gc2-gone' }, { assignee: 'worker-ci-1' })],
      sessions: [session({ session_name: 'someone-else' })],
    });
    expect(rows[0]?.summary).toContain('no live session for worker-ci-1');
  });
});

describe('waiting-on-human detection (gp-6xd)', () => {
  const held = (overrides: Partial<Bead> = {}) =>
    bead({
      id: 'B-held',
      status: 'in_progress',
      assignee: 'worker-ci-1',
      updated_at: '2026-06-07T11:30:00.000Z',
      metadata: { 'gc.checkpoint_hold': 'founder design review (Taylor+Afik)' },
      ...overrides,
    });

  it('surfaces a checkpoint-held bead as watch, naming who it waits on', () => {
    const rows = select({ beads: [held()] });
    expect(rows).toEqual([
      expect.objectContaining({
        beadId: 'B-held',
        reason: 'waiting-human',
        severity: 'watch',
        summary: expect.stringContaining('waiting on Taylor+Afik'),
      }),
    ]);
  });

  it('escalates to attention once the wait passes two hours', () => {
    const rows = select({ beads: [held({ updated_at: '2026-06-07T09:00:00.000Z' })] });
    expect(rows[0]).toEqual(expect.objectContaining({ severity: 'attention' }));
  });

  it('prefers the stamped gc.waiting_on name over the hold-text parenthetical', () => {
    const rows = select({
      beads: [
        held({
          metadata: {
            'gc.checkpoint_hold': 'review (Taylor+Afik)',
            'gc.waiting_on': 'Afik',
          },
        }),
      ],
    });
    expect(rows[0]?.summary).toContain('waiting on Afik');
  });

  it('falls back to "human" when the hold names nobody', () => {
    const rows = select({
      beads: [held({ metadata: { 'gc.founder_gate': 'design signoff' } })],
    });
    expect(rows[0]?.summary).toContain('waiting on human');
  });

  it('recognizes a hold: label as a waiting marker', () => {
    const rows = select({
      beads: [
        bead({
          id: 'B-held',
          status: 'in_progress',
          assignee: 'worker-ci-1',
          updated_at: '2026-06-07T11:30:00.000Z',
          labels: ['hold:founder (Taylor)'],
        }),
      ],
    });
    expect(rows[0]).toEqual(
      expect.objectContaining({
        reason: 'waiting-human',
        summary: expect.stringContaining('waiting on Taylor'),
      }),
    );
  });

  // The two canonical hold values (engdocs/contributors/hold-label-conventions.md,
  // internal/beadmeta/hold_labels.go). Neither carries a parenthetical, so
  // without an explicit mapping both would render the actorless "waiting on
  // human" on the most common real input.
  const heldByLabel = (label: string, overrides: Partial<Bead> = {}) =>
    bead({
      id: 'B-held',
      status: 'in_progress',
      assignee: 'worker-ci-1',
      updated_at: '2026-06-07T11:30:00.000Z',
      labels: [label],
      ...overrides,
    });

  it('names the mayor for the canonical hold:mayor label', () => {
    const rows = select({ beads: [heldByLabel('hold:mayor')] });
    expect(rows[0]).toEqual(
      expect.objectContaining({
        reason: 'waiting-human',
        summary: expect.stringContaining('waiting on mayor'),
      }),
    );
  });

  it('names external for the canonical hold:external label', () => {
    const rows = select({ beads: [heldByLabel('hold:external')] });
    expect(rows[0]?.summary).toContain('waiting on external');
  });

  it('never escalates hold:external — the next actor is outside this bd instance', () => {
    const rows = select({
      beads: [heldByLabel('hold:external', { updated_at: '2026-06-07T09:00:00.000Z' })],
    });
    expect(rows[0]).toEqual(expect.objectContaining({ severity: 'watch' }));
  });

  it('still escalates hold:mayor past two hours — the mayor is here to answer', () => {
    const rows = select({
      beads: [heldByLabel('hold:mayor', { updated_at: '2026-06-07T09:00:00.000Z' })],
    });
    expect(rows[0]).toEqual(expect.objectContaining({ severity: 'attention' }));
  });

  it('takes precedence over stalled — a parked worker is not lost, it is waiting', () => {
    const rows = select({
      beads: [held()],
      sessions: [session({ state: 'dead' })],
    });
    expect(rows.map((row) => row.reason)).toEqual(['waiting-human']);
  });

  it('takes precedence over ready-unclaimed — an open unassigned gate bead is waiting, not claimable', () => {
    const rows = select({
      beads: [
        bead({
          id: 'B-gate',
          status: 'open',
          created_at: '2026-06-01T11:00:00.000Z',
          updated_at: '2026-06-07T11:30:00.000Z',
          metadata: { 'gc.founder_gate': 'design signoff (Taylor)' },
        }),
      ],
    });
    expect(rows).toEqual([
      expect.objectContaining({
        beadId: 'B-gate',
        reason: 'waiting-human',
        summary: expect.stringContaining('waiting on Taylor'),
      }),
    ]);
  });
});

describe('inProgressCardNote (gp-6xd)', () => {
  it('shows assignee and activity age for a healthy in-progress bead', () => {
    const note = inProgressCardNote(
      bead({ status: 'in_progress', assignee: 'worker-ci-1' }),
      [session({ last_active: '2026-06-07T11:52:00.000Z' })],
      NOW,
    );
    expect(note).toBe('worker-ci-1 · active 8m ago');
  });

  it('shows the stalled reason when the session is gone', () => {
    const note = inProgressCardNote(
      bead({
        status: 'in_progress',
        assignee: 'worker-ci-1',
        updated_at: '2026-06-06T12:00:00.000Z',
      }),
      [],
      NOW,
    );
    expect(note).toContain('stalled');
    expect(note).toContain('no live session');
  });

  it('shows the waiting reason for a checkpoint-held bead', () => {
    const note = inProgressCardNote(
      bead({
        status: 'in_progress',
        assignee: 'worker-ci-1',
        updated_at: '2026-06-07T11:30:00.000Z',
        metadata: { 'gc.checkpoint_hold': 'gate (Afik)' },
      }),
      [session({})],
      NOW,
    );
    expect(note).toContain('waiting on Afik');
  });

  it('returns null for beads that are not in progress', () => {
    expect(inProgressCardNote(bead({ status: 'open' }), [], NOW)).toBeNull();
  });

  // FORK-LOCAL: with no `assignee` stamped, the liveness line rendered a bare
  // "active 8m ago" with nobody's name on it for every in-progress bead.
  it('names the pinned session on the liveness line when there is no assignee', () => {
    const note = inProgressCardNote(
      bead({ status: 'in_progress', metadata: { 'gc.session_id': 'ci-1' } }),
      [session({ last_active: '2026-06-07T11:52:00.000Z' })],
      NOW,
    );
    expect(note).toBe('ci-1 · active 8m ago');
  });

  it('names the routed-to agent when that is the only ownership signal', () => {
    const note = inProgressCardNote(
      bead({
        status: 'in_progress',
        metadata: { 'gc.execution_routed_to': 'gas-city-infra/codex-polecat' },
      }),
      [],
      NOW,
    );
    expect(note).toBe('gas-city-infra/codex-polecat');
  });
});
