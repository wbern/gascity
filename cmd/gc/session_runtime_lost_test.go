package main

import (
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/events"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// The city records the session deaths it PERFORMS (session.stopped,
// session.idle_killed, session.suspended) and, before this event, recorded
// nothing for the ones it merely DISCOVERS. On 2026-08-15 every session in a
// city died at once when the shared tmux server was replaced; the event log
// carried seven session.woke entries and not a single death, so reconstructing
// what happened needed a socket mtime and process start times — both of which
// the next occurrence overwrites. session.runtime_lost is the missing half.
func TestEmitSessionRuntimeLost_EmitsForDiscoveredDeath(t *testing.T) {
	rec := &capturingRecorder{}
	now := time.Date(2026, 8, 15, 21, 14, 2, 0, time.UTC)
	info := sessionpkg.Info{
		ID:          "gc2-j8f1tn",
		State:       sessionpkg.StateActive,
		SessionName: "slay-chat-v3-8a1c4ff",
	}

	emitSessionRuntimeLost(info, "claude-docker", rec, &clock.Fake{Time: now})

	lost := runtimeLostEvents(rec)
	if len(lost) != 1 {
		t.Fatalf("session.runtime_lost events = %d, want 1; events: %+v", len(lost), rec.events)
	}
	e := lost[0]
	if e.SessionID != info.ID {
		t.Errorf("SessionID = %q, want %q", e.SessionID, info.ID)
	}
	if e.Actor != "gc" {
		t.Errorf("Actor = %q, want %q", e.Actor, "gc")
	}
	if !e.Ts.Equal(now) {
		t.Errorf("Ts = %v, want %v", e.Ts, now)
	}
	// The message has to name the session and the template: an operator
	// reading the log after the fact is asking "which sessions died and what
	// were they", and the answer must not require joining to another store.
	if !strings.Contains(e.Message, info.SessionName) {
		t.Errorf("Message = %q, want it to name session %q", e.Message, info.SessionName)
	}
	if !strings.Contains(e.Message, "claude-docker") {
		t.Errorf("Message = %q, want it to name the template", e.Message)
	}
}

// A session gc stopped on purpose is not a discovered death. Drain, suspend and
// idle-kill each emit their own event and leave the bead in a non-active state;
// firing runtime_lost for those too would turn "something killed this and
// nothing in gc knows what" into a routine line nobody reads, which is the
// failure mode that makes an audit trail worthless.
func TestEmitSessionRuntimeLost_SilentForDeliberateStop(t *testing.T) {
	for _, state := range []sessionpkg.State{
		sessionpkg.StateSuspended,
		sessionpkg.StateStartPending,
	} {
		rec := &capturingRecorder{}
		info := sessionpkg.Info{ID: "gc2-lwo0s5", State: state, SessionName: "polecat-1"}

		emitSessionRuntimeLost(info, "claude-sonnet-polecat", rec, &clock.Fake{Time: time.Now()})

		if got := len(runtimeLostEvents(rec)); got != 0 {
			t.Errorf("state %q: session.runtime_lost events = %d, want 0", state, got)
		}
	}
}

// A nil recorder is the norm rather than the exception on this path — every
// non-supervisor caller of the reconciler passes events.Discard or nothing at
// all — so the emitter must tolerate it rather than panic a controller tick.
func TestEmitSessionRuntimeLost_NilRecorderIsSafe(_ *testing.T) {
	// No assertion: the test is that this call returns rather than panicking,
	// which a panic would report as a package-level failure.
	emitSessionRuntimeLost(
		sessionpkg.Info{ID: "gc2-x", State: sessionpkg.StateActive},
		"claude-docker",
		nil,
		&clock.Fake{Time: time.Now()},
	)
}

// Every event type must have a registered payload; the SSE projection's
// conformance test treats a missing registration as a build error rather than a
// runtime surprise.
func TestSessionRuntimeLost_HasRegisteredPayload(t *testing.T) {
	if _, ok := events.LookupPayload(events.SessionRuntimeLost); !ok {
		t.Fatal("no payload registered for session.runtime_lost")
	}
}

func runtimeLostEvents(c *capturingRecorder) []events.Event {
	out := make([]events.Event, 0, len(c.events))
	for _, e := range c.events {
		if e.Type == events.SessionRuntimeLost {
			out = append(out, e)
		}
	}
	return out
}
