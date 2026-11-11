package main

import "maps"

// The fork's divergences from upstream's v2 reconciler, as the timeline
// differential accounts for them (gcw-ti3qt, upstream-intake tranche 3).
//
// forkTimelineExplain adds Explain keys ("@<step> <aspect>") to a timeline
// fixture by name; tlFixture merges it (an empty id deletes an upstream key). Like every Explain key, each must show
// at some tick of its step, so an entry whose divergence goes away fails until
// it is deleted.
var forkTimelineExplain = map[string]map[string]string{
	"a crash 29s after wake": {
		`@crash event:session.continuation_observed s-gc-1 {"boundary":"runtime_start","continuation_epoch":"1","generation":"2","instance_token_fingerprint":"<fingerprint>","old_work_id":"gw-1","outcome":"succeeded","schema_version":"1","session_name":"s-gc-1","source":"session_reconciler","template":"worker"}`: "continuation-observed-start",
	},
	"a crash 5m0s after wake": {
		`@crash event:session.continuation_observed s-gc-1 {"boundary":"runtime_start","continuation_epoch":"1","generation":"2","instance_token_fingerprint":"<fingerprint>","old_work_id":"gw-1","outcome":"succeeded","schema_version":"1","session_name":"s-gc-1","source":"session_reconciler","template":"worker"}`: "continuation-observed-start",
	},
	"a live drift on a seat": {
		`@seed event:session.continuation_observed s-gc-1 {"boundary":"runtime_start","continuation_epoch":"1","generation":"2","instance_token_fingerprint":"<fingerprint>","old_work_id":"gw-1","outcome":"succeeded","schema_version":"1","session_name":"s-gc-1","source":"session_reconciler","template":"worker"}`: "continuation-observed-start",
	},
	"a managed suspend drains, then wake": {
		// Upstream's drain leaves env clears (SetMeta/RemoveMeta) that the fork's
		// immediate stop does not, so legacy and v2 agree here: delete the entries.
		`@suspend provider:RemoveMeta s-gc-1`:      "",
		`@suspend provider:SetMeta s-gc-1`:         "",
		`@past the drain timeout row:gc-1:*uhstop`: "fork-user-hold-immediate-stop",
		`@suspend event:session.drain_acked_with_assigned_work worker {"bead_id":"gw-1","bead_status":"in_progress","reason":"drain_acked_with_assigned_work","session_id":"gc-1","template":"worker"}`: "fork-user-hold-immediate-stop",
		`@suspend event:session.stopped worker {"reason":"drain acknowledged","session_id":"gc-1","template":"worker"}`:                                                                                 "fork-user-hold-immediate-stop",
		`@suspend row:gc-1:*uhstop`:       "fork-user-hold-immediate-stop",
		`@the next pass row:gc-1:*uhstop`: "fork-user-hold-immediate-stop",
		`@wake event:session.continuation_observed s-gc-1 {"boundary":"runtime_start","continuation_epoch":"1","generation":"2","instance_token_fingerprint":"<fingerprint>","old_work_id":"gw-1","outcome":"succeeded","schema_version":"1","session_name":"s-gc-1","source":"session_reconciler","template":"worker"}`: "continuation-observed-start",
		`@wake row:gc-1:*uhstop`: "fork-user-hold-immediate-stop",
	},
	"a manual create read between its two writes": {
		`@its session_name lands event:session.continuation_observed s-gc-1 {"boundary":"runtime_start","continuation_epoch":"1","generation":"2","instance_token_fingerprint":"<fingerprint>","outcome":"succeeded","schema_version":"1","session_name":"s-gc-1","source":"session_reconciler","template":"worker"}`: "continuation-observed-start",
	},
	"a provider swap stops the runtime": {
		`@swap event:session.continuation_observed s-gc-1 {"boundary":"runtime_start","continuation_epoch":"1","generation":"2","instance_token_fingerprint":"<fingerprint>","old_work_id":"gw-1","outcome":"succeeded","schema_version":"1","session_name":"s-gc-1","source":"session_reconciler","template":"worker"}`: "continuation-observed-start",
	},
	"a quarantine, then a kill fence, expires": {
		`@the kill fence expires event:session.continuation_observed s-gc-1 {"boundary":"runtime_start","continuation_epoch":"1","generation":"2","instance_token_fingerprint":"<fingerprint>","old_work_id":"gw-1","outcome":"succeeded","schema_version":"1","session_name":"s-gc-1","source":"session_reconciler","template":"worker"}`: "continuation-observed-start",
	},
	"a waiter sleeps": {
		`@the wait turns ready event:session.continuation_observed s-gc-1 {"boundary":"runtime_start","continuation_epoch":"1","generation":"2","instance_token_fingerprint":"<fingerprint>","old_work_id":"gw-1","outcome":"succeeded","schema_version":"1","session_name":"s-gc-1","source":"session_reconciler","template":"worker"}`: "continuation-observed-start",
	},
	"an idle named session holding assigned work defers until its streak is exhausted": {
		`@idle past idle_timeout event:session.continuation_observed test-city--chat {"boundary":"runtime_start","continuation_epoch":"1","generation":"2","instance_token_fingerprint":"<fingerprint>","old_work_id":"gw-1","outcome":"succeeded","schema_version":"1","session_name":"test-city--chat","source":"session_reconciler","template":"chat"}`: "continuation-observed-start",
	},
	"drift deferrals through time": {
		`@seed event:session.continuation_observed s-gc-1 {"boundary":"runtime_start","continuation_epoch":"1","generation":"2","instance_token_fingerprint":"<fingerprint>","old_work_id":"gw-1","outcome":"succeeded","schema_version":"1","session_name":"s-gc-1","source":"session_reconciler","template":"worker"}`: "continuation-observed-start",
	},
	"idle, then sleep, then the next pass": {
		`@the sleep policy changes event:session.continuation_observed test-city--chat {"boundary":"runtime_start","continuation_epoch":"1","generation":"2","instance_token_fingerprint":"<fingerprint>","outcome":"succeeded","schema_version":"1","session_name":"test-city--chat","source":"session_reconciler","template":"chat"}`: "continuation-observed-start",
	},
	"kill a seat holding a ready claim": {
		`@past the kill grace event:session.continuation_observed s-gc-1 {"boundary":"runtime_start","continuation_epoch":"1","generation":"2","instance_token_fingerprint":"<fingerprint>","old_work_id":"gw-1","outcome":"succeeded","schema_version":"1","session_name":"s-gc-1","source":"session_reconciler","template":"worker"}`: "continuation-observed-start",
	},
	"suspend, then wake": {
		`@wake event:session.continuation_observed s-gc-1 {"boundary":"runtime_start","continuation_epoch":"1","generation":"2","instance_token_fingerprint":"<fingerprint>","old_work_id":"gw-1","outcome":"succeeded","schema_version":"1","session_name":"s-gc-1","source":"session_reconciler","template":"worker"}`: "continuation-observed-start",
	},
}

// forkTimelineGoldenRuling admits the sections where the fork's legacy
// reconciler differs from upstream's: core hash v8 with CodexSessionFlags (v6
// upstream), gc.trigger_bead_store_ref stamped on rows, the runtime metadata a
// start sets and clears, the diagnostic session.continuation_observed events,
// and the immediate stop of a user-held runtime (decision D7, provisional).
// Every one is a difference of legacy from upstream's legacy, not of legacy from v2.
const forkTimelineGoldenRuling = "fork keep (gcw-ti3qt): the fork's legacy differs from upstream's (T3, provisional; digests re-recorded in T5 under fingerprint v9)"

func init() {
	timelineGoldenRulings = append(timelineGoldenRulings,
		goldenRuling{"a crash 29s after wake", forkTimelineGoldenRuling, "5fe27bd5b8b8"},
		goldenRuling{"a crash 31s after wake", forkTimelineGoldenRuling, "97a3b1c623fb"},
		goldenRuling{"a crash 5m0s after wake", forkTimelineGoldenRuling, "53c267fab544"},
		goldenRuling{"a cutover finds v5 hashes on a live seat", forkTimelineGoldenRuling, "41b6374e0028"},
		goldenRuling{"a live drift on a seat", forkTimelineGoldenRuling, "6eb5d26b7357"},
		goldenRuling{"a live quarantined row 29s after wake", forkTimelineGoldenRuling, "54bf033dac33"},
		goldenRuling{"a live quarantined row 31s after wake", forkTimelineGoldenRuling, "060ad5819e45"},
		goldenRuling{"a managed suspend drains, then wake", forkTimelineGoldenRuling, "f122f1da9826"},
		goldenRuling{"a manual create read between its two writes", forkTimelineGoldenRuling, "3ddd8df558cd"},
		goldenRuling{"a provider swap stops the runtime", forkTimelineGoldenRuling, "2f71232d9a73"},
		goldenRuling{"a quarantine, then a kill fence, expires", forkTimelineGoldenRuling, "c2b73b4377b6"},
		goldenRuling{"a waiter sleeps", forkTimelineGoldenRuling, "5297093fa7ad"},
		goldenRuling{"an idle named session holding assigned work defers until its streak is exhausted", forkTimelineGoldenRuling, "200391b7e36d"},
		goldenRuling{"an idle on-demand named session sleeps", forkTimelineGoldenRuling, "4014ebf25083"},
		goldenRuling{"an unwanted row inside, then past, the wake grace", forkTimelineGoldenRuling, "3da158e19b9e"},
		goldenRuling{"an unwanted row of a suspended agent", forkTimelineGoldenRuling, "b806b255f900"},
		goldenRuling{"an unwanted row woken by a clock ten minutes ahead", forkTimelineGoldenRuling, "eb7762e58461"},
		goldenRuling{"an unwanted row woken by a clock two minutes ahead", forkTimelineGoldenRuling, "f5e77ec3f5c4"},
		goldenRuling{"drift deferrals through time", forkTimelineGoldenRuling, "994f2d3c95cc"},
		goldenRuling{"idle, then sleep, then the next pass", forkTimelineGoldenRuling, "37f1c7935cc0"},
		goldenRuling{"kill a seat holding a dep-blocked claim", forkTimelineGoldenRuling, "052357a07a3a"},
		goldenRuling{"kill a seat holding a ready claim", forkTimelineGoldenRuling, "7f7d6e923ec9"},
		goldenRuling{"suspend, then wake", forkTimelineGoldenRuling, "6a850a4ae243"},
	)
	// A user hold is the operator's request to free the runtime now, so the
	// fork's reconciler stops a held, live runtime at once instead of draining
	// it (cmd/gc session_reconciler.go, intent == "user-hold"). PROVISIONAL
	// decision D7, awaiting the owner's review before merge. v2 drains it.
	parityFindings["fork-user-hold-immediate-stop"] = parityEntry{
		"fork keep (gcw-ti3qt D7, provisional)",
		"legacy stops a user-held session's live runtime at once, heals the row asleep over the missing runtime and records no drain; v2 begins a user-hold drain and completes it with the drain's own sleep",
		[]string{"state", "state_reason", "sleep_reason", "slept_at", "drain_at", "last_woke_at", "churn_count"},
	}
}

// forkParityExplain adds Explain keys to a differential fixture by name, where
// the fork's legacy differs from upstream's: the fork's diagnostic
// session.continuation_observed event on every start (one key set with and one
// without old_work_id, and one with error_code), and the immediate stop of a
// user-held runtime (decision D7, provisional). An empty id deletes an upstream
// key the fork's behavior makes agree. Like every Explain key, each must show.
var forkParityExplain = map[string]map[string]string{
	"a dead phantom of a configured name closes and keeps the identity's work": {
		`event:session.continuation_observed chat [boundary continuation_epoch generation instance_token_fingerprint old_work_id outcome schema_version session_name source template]`: "continuation-observed-start",
		`event:session.continuation_observed chat [boundary continuation_epoch generation instance_token_fingerprint outcome schema_version session_name source template]`:             "",
	},
	"a drifted floor seat drains for config drift": {
		`event:session.continuation_observed worker-gc-2 [boundary continuation_epoch generation instance_token_fingerprint outcome schema_version session_name source template]`: "continuation-observed-start",
	},
	"a heartbeat-held seat whose runtime died respawns for its work": {
		`event:session.continuation_observed s-gc-1 [boundary continuation_epoch generation instance_token_fingerprint old_work_id outcome schema_version session_name source template]`: "continuation-observed-start",
		`event:session.continuation_observed s-gc-1 [boundary continuation_epoch generation instance_token_fingerprint outcome schema_version session_name source template]`:             "",
	},
	"a named row's drift restarts it in place": {
		`event:session.continuation_observed chat [boundary continuation_epoch generation instance_token_fingerprint outcome schema_version session_name source template]`: "continuation-observed-start",
	},
	"a pending create whose start fails rolls back": {
		`event:session.continuation_observed s-gc-1 [boundary continuation_epoch error_code generation instance_token_fingerprint outcome schema_version session_name source template]`: "continuation-observed-start",
	},
	"a reset-pending seat whose agent stalled past its startup timeout is replaced": {
		`event:session.continuation_observed s-gc-1 [boundary continuation_epoch generation instance_token_fingerprint old_work_id outcome schema_version session_name source template]`: "continuation-observed-start",
	},
	"a seat whose user hold expired resumes for its work": {
		`event:session.continuation_observed s-gc-1 [boundary continuation_epoch generation instance_token_fingerprint old_work_id outcome schema_version session_name source template]`: "continuation-observed-start",
		`event:session.continuation_observed s-gc-1 [boundary continuation_epoch generation instance_token_fingerprint outcome schema_version session_name source template]`:             "",
	},
	"a stale terminal mark does not hold a named row's crashed zombie": {
		`event:session.continuation_observed chat [boundary continuation_epoch generation instance_token_fingerprint outcome schema_version session_name source template]`: "continuation-observed-start",
	},
	"a stale terminal mark does not park a named row": {
		`event:session.continuation_observed chat [boundary continuation_epoch generation instance_token_fingerprint outcome schema_version session_name source template]`: "continuation-observed-start",
	},
	"a stop-pending named row ends drained": {
		`event:session.continuation_observed chat [boundary continuation_epoch generation instance_token_fingerprint outcome schema_version session_name source template]`: "continuation-observed-start",
	},
	"a stop-pending pool seat with work sleeps idle": {
		`event:session.continuation_observed s-gc-1 [boundary continuation_epoch generation instance_token_fingerprint old_work_id outcome schema_version session_name source template]`: "continuation-observed-start",
	},
	"a wanted asleep named row's drift is repaired and started": {
		`event:session.continuation_observed chat [boundary continuation_epoch generation instance_token_fingerprint outcome schema_version session_name source template]`: "continuation-observed-start",
	},
	"a wanted asleep named row's v5 hash is rebaselined and started": {
		`event:session.continuation_observed chat [boundary continuation_epoch generation instance_token_fingerprint outcome schema_version session_name source template]`: "continuation-observed-start",
	},
	"a wanted row whose provider health is unknown starts": {
		`event:session.continuation_observed s-gc-1 [boundary continuation_epoch generation instance_token_fingerprint old_work_id outcome schema_version session_name source template]`: "continuation-observed-start",
		`event:session.continuation_observed s-gc-1 [boundary continuation_epoch generation instance_token_fingerprint outcome schema_version session_name source template]`:             "",
	},
	"a wanted seat whose zombie shows a terminal provider error is parked": {
		`event:session.continuation_observed s-gc-1 [boundary continuation_epoch generation instance_token_fingerprint old_work_id outcome schema_version session_name source template]`: "continuation-observed-start",
	},
	"a wanted seat's own crashed zombie is recycled once": {
		`event:session.continuation_observed s-gc-1 [boundary continuation_epoch generation instance_token_fingerprint old_work_id outcome schema_version session_name source template]`: "continuation-observed-start",
	},
	"an asleep named row's drift is repaired": {
		`event:session.continuation_observed chat [boundary continuation_epoch generation instance_token_fingerprint outcome schema_version session_name source template]`: "continuation-observed-start",
	},
	"an explicit wake starts a parked named row": {
		`event:session.continuation_observed chat [boundary continuation_epoch generation instance_token_fingerprint outcome schema_version session_name source template]`: "continuation-observed-start",
	},
	"an idle pool row awake only for unclaimed work": {
		`event:session.continuation_observed worker-gc-2 [boundary continuation_epoch generation instance_token_fingerprint outcome schema_version session_name source template]`: "continuation-observed-start",
	},
	"an operator's suspend stops a seat that holds work, and the hold sticks": {
		`event:session.drain_acked_with_assigned_work worker [bead_id bead_status reason session_id template]`: "fork-user-hold-immediate-stop",
		`event:session.stopped worker [reason session_id template]`:                                            "fork-user-hold-immediate-stop",
		`provider:RemoveMeta s-gc-1`: "",
		`provider:SetMeta s-gc-1`:    "",
		`row:gc-1:*uhstop`:           "fork-user-hold-immediate-stop",
	},
}

// withForkParityExplain returns f with the fork's Explain keys applied.
func withForkParityExplain(f parityFixture) parityFixture {
	extra := forkParityExplain[f.Name]
	if len(extra) == 0 {
		return f
	}
	explain := maps.Clone(f.Explain)
	if explain == nil {
		explain = map[string]string{}
	}
	for k, v := range extra {
		if v == "" {
			delete(explain, k)
			continue
		}
		explain[k] = v
	}
	f.Explain = explain
	return f
}
