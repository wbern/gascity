package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/splittest"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/reconcilekey"
	"github.com/gastownhall/gascity/internal/rollout/gate"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/runtime/registry"
	"github.com/gastownhall/gascity/internal/session"
)

func TestS1DrainAckRetainsCurrentExecution(t *testing.T) {
	for _, key := range []string{beadmeta.CurrentClaimBeadIDMetadataKey, session.CurrentBeadIDKey} {
		t.Run(key, func(t *testing.T) {
			work := splittest.NewWorkStore(t, "gc")
			held := mustCreateDrainAckBead(t, work, beads.Bead{
				Title: "started work must survive drain acknowledgement", Type: "task",
			}, "in_progress", "worker-1")
			seat := drainAckSessionBead()
			seat.Metadata[key] = held.ID

			var stderr bytes.Buffer
			var retained *startedClaimHeldError
			if err := releaseUnexecutedClaimsOnDrainAck("", nil, work, nil, seat, drainAckReleaseBudget, &stderr); !errors.As(err, &retained) || retained.Claim.ID != held.ID {
				t.Fatalf("retained execution error = %v, want held %s", err, held.ID)
			}

			status, assignee := drainAckBeadStatus(t, work, held.ID)
			if status != "in_progress" || assignee != "worker-1" {
				t.Fatalf("started claim released: status=%q assignee=%q; want in_progress/worker-1; stderr=%s", status, assignee, stderr.String())
			}
		})
	}
}

func TestS1DrainAckRetainsWorkAndDoesNotPokeOrAcknowledge(t *testing.T) {
	oldRelease, oldPoke := drainAckReleaseHeldClaims, drainAckPokeController
	t.Cleanup(func() { drainAckReleaseHeldClaims, drainAckPokeController = oldRelease, oldPoke })
	held := &startedClaimHeldError{Claim: beads.Bead{ID: "task-1", Status: "in_progress", Assignee: "worker-1"}}
	drainAckReleaseHeldClaims = func(string, string, io.Writer) error { return held }
	poked := false
	drainAckPokeController = func(string, reconcilekey.Key) error { poked = true; return nil }
	dops := newFakeDrainOps()
	var stdout, stderr bytes.Buffer
	code, err := doRuntimeDrainAckResult(dops, "", "worker-1", "worker-1", "", false, &stdout, &stderr)
	if code == 0 || !errors.Is(err, held) || poked || dops.acked["worker-1"] {
		t.Fatalf("retained work acknowledged: code=%d err=%v poked=%v ack=%v", code, err, poked, dops.acked)
	}
	stdout.Reset()
	code = writeHookClaimDrain(hookClaimLabel, hookClaimReasonNoWork, true, true, func(io.Writer) error { return held }, &stdout, &stderr)
	if code == 0 || bytes.Contains(stdout.Bytes(), []byte(`"action":"work"`)) || bytes.Contains(stdout.Bytes(), []byte(`"drain_acknowledged":true`)) {
		t.Fatalf("generic drain bypassed qualified admission: code=%d stdout=%s", code, stdout.String())
	}
}

type s1ReacquireDuringReleaseStore struct {
	*beads.MemStore
	workID string
	raced  bool
}

func (s *s1ReacquireDuringReleaseStore) Update(id string, opts beads.UpdateOpts) error {
	if err := s.reacquire(id, opts); err != nil {
		return err
	}
	return s.MemStore.Update(id, opts)
}

func (s *s1ReacquireDuringReleaseStore) reacquire(id string, opts beads.UpdateOpts) error {
	if id == s.workID && !s.raced && opts.Status != nil && *opts.Status == "open" {
		s.raced = true
		if _, err := s.ReleaseIfCurrent(id, "worker-1"); err != nil {
			return err
		}
		if _, ok, err := s.ClaimAs(id, "worker-1"); err != nil || !ok {
			return errors.New("same-owner reacquisition failed")
		}
	}
	return nil
}

func (s *s1ReacquireDuringReleaseStore) UpdateIfMatch(id string, revision int64, opts beads.UpdateOpts) error {
	if err := s.reacquire(id, opts); err != nil {
		return err
	}
	return s.MemStore.UpdateIfMatch(id, revision, opts)
}

var _ beads.ConditionalWriter = (*s1ReacquireDuringReleaseStore)(nil)

func TestS1DrainAckRefusesSameOwnerReacquisition(t *testing.T) {
	mem := beads.NewMemStore()
	if err := beads.StampOpenedStore(mem, "memory", gate.Require, nil, nil); err != nil {
		t.Fatal(err)
	}
	racing := &s1ReacquireDuringReleaseStore{MemStore: mem}
	work := mustCreateDrainAckBead(t, racing, beads.Bead{Title: "unexecuted assignment", Type: "task"}, "in_progress", "worker-1")
	racing.workID = work.ID
	seat, err := racing.Create(beads.Bead{Title: "worker-1", Type: session.BeadType, Metadata: map[string]string{
		"session_name": "worker-1", "instance_token": "incarnation-one", "generation": "7",
	}})
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.City{Beads: config.BeadsConfig{ConditionalWrites: "require"}}
	var stderr bytes.Buffer
	err = releaseUnexecutedClaimsOnDrainAck("", cfg, racing, nil, seat, drainAckReleaseBudget, &stderr)
	status, owner := drainAckBeadStatus(t, racing, work.ID)
	if status != "in_progress" || owner != "worker-1" {
		t.Fatalf("replacement acquisition lost: status=%q owner=%q", status, owner)
	}
	if err == nil || !racing.raced {
		t.Fatalf("same-owner race was not refused: raced=%v err=%v stderr=%s", racing.raced, err, stderr.String())
	}
}

func TestS1DrainAckUsesOpaqueNonzeroWorkRevisions(t *testing.T) {
	for _, revision := range []int64{-71, 0, 71} {
		mem := beads.NewMemStoreFrom(2, []beads.Bead{
			{ID: "gc-1", Title: "unstarted assignment", Type: "task", Status: "in_progress", Assignee: "worker-1", Revision: revision},
			{
				ID: "gc-2", Title: "worker-1", Type: session.BeadType, Status: "open", Revision: 11,
				Metadata: map[string]string{"session_name": "worker-1", "instance_token": "incarnation-one", "generation": "7"},
			},
		}, nil)
		if err := beads.StampOpenedStore(mem, "memory", gate.Require, nil, nil); err != nil {
			t.Fatal(err)
		}
		seat, err := mem.Get("gc-2")
		if err != nil {
			t.Fatal(err)
		}
		var stderr bytes.Buffer
		err = releaseUnexecutedClaimsOnDrainAck("", &config.City{Beads: config.BeadsConfig{ConditionalWrites: "require"}}, mem, nil, seat, drainAckReleaseBudget, &stderr)
		status, owner := drainAckBeadStatus(t, mem, "gc-1")
		if revision == 0 {
			if err == nil || status != "in_progress" || owner != "worker-1" {
				t.Fatalf("zero revision did not refuse unchanged: status=%q owner=%q err=%v", status, owner, err)
			}
		} else if err != nil || status != "open" || owner != "" {
			t.Fatalf("valid signed revision %d did not release: status=%q owner=%q err=%v stderr=%s", revision, status, owner, err, stderr.String())
		}
	}
}

func TestS1DrainAckNeverDowngradesPersistedExecutionEvidence(t *testing.T) {
	mem := beads.NewMemStore()
	work := mustCreateDrainAckBead(t, mem, beads.Bead{Title: "unstarted assignment", Type: "task"}, "in_progress", "worker-1")
	seat, err := mem.Create(beads.Bead{Title: "worker-1", Type: session.BeadType, Metadata: map[string]string{
		"session_name": "worker-1", "instance_token": "incarnation-one", "generation": "7", session.WorkStartedKey: `{"v":1,"claims":[]}`,
	}})
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	err = releaseUnexecutedClaimsOnDrainAck("", &config.City{Beads: config.BeadsConfig{ConditionalWrites: "off"}}, mem, nil, seat, drainAckReleaseBudget, &stderr)
	status, owner := drainAckBeadStatus(t, mem, work.ID)
	if err == nil || status != "in_progress" || owner != "worker-1" {
		t.Fatalf("persisted cell downgraded: status=%s owner=%s err=%v", status, owner, err)
	}
}

type s1AdoptDuringReleaseStore struct {
	*beads.MemStore
	workID, sessionID string
	attempted         bool
	startErr          error
}

func (s *s1AdoptDuringReleaseStore) UpdateIfMatch(id string, revision int64, opts beads.UpdateOpts) error {
	if id == s.workID && opts.Status != nil && *opts.Status == "open" {
		s.attempted = true
		front := session.NewStore(beads.SessionStore{Store: s})
		s.startErr = front.RecordStartedClaim(s.sessionID, session.ExecutionFence{InstanceToken: "incarnation-one", Generation: "7"}, session.WorkStartedEntry{Bead: id})
	}
	return s.MemStore.UpdateIfMatch(id, revision, opts)
}

func TestS1DrainReservationExcludesConcurrentAdoption(t *testing.T) {
	mem := beads.NewMemStore()
	if err := beads.StampOpenedStore(mem, "memory", gate.Require, nil, nil); err != nil {
		t.Fatal(err)
	}
	racing := &s1AdoptDuringReleaseStore{MemStore: mem}
	work := mustCreateDrainAckBead(t, racing, beads.Bead{Title: "not yet started", Type: "task"}, "in_progress", "worker-1")
	seat, err := racing.Create(beads.Bead{Title: "worker-1", Type: session.BeadType, Metadata: map[string]string{"session_name": "worker-1", "instance_token": "incarnation-one", "generation": "7"}})
	if err != nil {
		t.Fatal(err)
	}
	racing.workID, racing.sessionID = work.ID, seat.ID
	var stderr bytes.Buffer
	err = releaseUnexecutedClaimsOnDrainAck("", &config.City{Beads: config.BeadsConfig{ConditionalWrites: "require"}}, racing, nil, seat, drainAckReleaseBudget, &stderr)
	if err != nil || !racing.attempted || !errors.Is(racing.startErr, session.ErrClaimReleasePending) {
		t.Fatalf("concurrent adoption crossed reserved release: release=%v attempted=%v start=%v stderr=%s", err, racing.attempted, racing.startErr, stderr.String())
	}
	status, owner := drainAckBeadStatus(t, racing, work.ID)
	if status != "open" || owner != "" {
		t.Fatalf("unexecuted release incomplete: %s/%s", status, owner)
	}
}

type s1UncertainReleaseStore struct {
	*beads.MemStore
	workID         string
	writeThenError bool
	attempts       int
}

func (s *s1UncertainReleaseStore) UpdateIfMatch(id string, revision int64, opts beads.UpdateOpts) error {
	if id == s.workID && opts.Status != nil && *opts.Status == "open" {
		s.attempts++
		if s.writeThenError {
			if err := s.MemStore.UpdateIfMatch(id, revision, opts); err != nil {
				return err
			}
		}
		return errors.New("synthetic uncertain backend outcome")
	}
	return s.MemStore.UpdateIfMatch(id, revision, opts)
}

func TestS1UncertainReleaseKeepsAdoptionExcluded(t *testing.T) {
	for _, writeThenError := range []bool{false, true} {
		mem := beads.NewMemStore()
		if err := beads.StampOpenedStore(mem, "memory", gate.Require, nil, nil); err != nil {
			t.Fatal(err)
		}
		backend := &s1UncertainReleaseStore{MemStore: mem, writeThenError: writeThenError}
		work := mustCreateDrainAckBead(t, backend, beads.Bead{Title: "not delivered", Type: "task"}, "in_progress", "worker-1")
		seat, err := backend.Create(beads.Bead{Title: "worker-1", Type: session.BeadType, Metadata: map[string]string{"session_name": "worker-1", "instance_token": "incarnation-one", "generation": "7"}})
		if err != nil {
			t.Fatal(err)
		}
		backend.workID = work.ID
		var stderr bytes.Buffer
		err = releaseUnexecutedClaimsOnDrainAck("", &config.City{Beads: config.BeadsConfig{ConditionalWrites: "require"}}, backend, nil, seat, drainAckReleaseBudget, &stderr)
		if err == nil || backend.attempts != 1 {
			t.Fatalf("uncertain release not exercised: writeThenError=%v attempts=%d err=%v", writeThenError, backend.attempts, err)
		}
		front := session.NewStore(beads.SessionStore{Store: backend})
		err = front.RecordStartedClaim(seat.ID, session.ExecutionFence{InstanceToken: "incarnation-one", Generation: "7"}, session.WorkStartedEntry{Bead: work.ID})
		if !errors.Is(err, session.ErrClaimReleasePending) {
			t.Fatalf("uncertain release allowed adoption: writeThenError=%v err=%v", writeThenError, err)
		}
	}
}

func TestS1RetainedWorkNeverBypassesInvocationOrSuspensionFences(t *testing.T) {
	for _, reason := range []string{
		hookClaimReasonClaimsErrored, hookClaimReasonStaleSession, hookClaimReasonNonTurnContext,
		hookClaimReasonMissingSessionRegistration, hookClaimReasonCitySuspended,
		hookClaimReasonAgentSuspended, hookClaimReasonRigSuspended,
	} {
		t.Run(reason, func(t *testing.T) {
			held := &startedClaimHeldError{Claim: beads.Bead{ID: "task-1", Status: "in_progress", Assignee: "worker-1"}}
			var stdout, stderr bytes.Buffer
			code := writeHookClaimDrain(hookClaimLabel, reason, true, true, func(io.Writer) error { return held }, &stdout, &stderr)
			if code == 0 || bytes.Contains(stdout.Bytes(), []byte(`"action":"work"`)) || bytes.Contains(stdout.Bytes(), []byte(`"drain_acknowledged":true`)) {
				t.Fatalf("retained work bypassed %s fence: code=%d stdout=%s stderr=%s", reason, code, stdout.String(), stderr.String())
			}
		})
	}
}

func TestS1RetainedWorkRecordsExecutionBeforeHandback(t *testing.T) {
	for _, recordError := range []error{nil, session.ErrExecutionIncarnationChanged} {
		fixture := newS1FileAdmissionFixture(t, "in_progress", "worker-1")
		held := &startedClaimHeldError{Claim: fixture.work}
		var stdout, stderr bytes.Buffer
		recorded := false
		opts := hookClaimOptions{Assignee: "worker-1", SessionID: fixture.seat.ID, StartedFence: &fixture.fence, JSON: true, DrainAck: true}
		ops := hookClaimOps{
			DrainAck: func(io.Writer) error { return held },
			RecordStartedClaim: func(id string, fence session.ExecutionFence, entry session.WorkStartedEntry) error {
				recorded = true
				if stdout.Len() != 0 || id != opts.SessionID || fence != *opts.StartedFence || entry.Bead != held.Claim.ID {
					t.Fatalf("incorrect execution record or work delivered first: id=%s entry=%+v stdout=%s", id, entry, stdout.String())
				}
				return recordError
			},
		}
		ops.StampWorkMeta = func(context.Context, string, []string, string, string, map[string]string) error { return nil }
		ops.StampSessionClaim = func(string, string) error { return nil }
		ops.ReadWorkMeta = func(_ context.Context, _ string, _ []string, id, _ string) (beads.Bead, error) {
			return fixture.store.Get(id)
		}
		ops.ResolveWorkBranch = func(hookClaimWorkTree) string { return "" }
		ops.ResolveSessionWorkDir = func(string) string { return "" }
		ops.applyDefaults()
		code := writeHookClaimNoWork(opts, ops, false, fixture.city, &stdout, &stderr)
		if !recorded {
			t.Fatalf("retained work delivered without recording: code=%d stdout=%s", code, stdout.String())
		}
		if recordError != nil {
			if code == 0 || bytes.Contains(stdout.Bytes(), []byte(`"action":"work"`)) {
				t.Fatalf("failed record admitted work: code=%d stdout=%s", code, stdout.String())
			}
		} else if code != 0 || !bytes.Contains(stdout.Bytes(), []byte(`"action":"work"`)) {
			t.Fatalf("recorded work not returned: code=%d stdout=%s stderr=%s", code, stdout.String(), stderr.String())
		}
	}
}

type s1FileAdmissionFixture struct {
	city       string
	cfg        *config.City
	store      beads.Store
	work, seat beads.Bead
	fence      session.ExecutionFence
}

func newS1FileAdmissionFixture(t *testing.T, status, owner string) s1FileAdmissionFixture {
	t.Helper()
	clearGCEnv(t)
	disableManagedDoltRecoveryForTest(t)
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_BEADS_CONDITIONAL_WRITES", "require")
	city := writeFenceTestCity(t)
	cfg, err := loadCityConfigWithoutBuiltinPackRefresh(city, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	store, err := openCityStoreAt(city)
	if err != nil {
		t.Fatal(err)
	}
	work := mustCreateDrainAckBead(t, store, beads.Bead{Title: "admission boundary proof", Type: "task"}, status, owner)
	id := newFenceSessionBeadWithMetadata(t, city, session.StateActive, "runtime-capability", map[string]string{"generation": "7"})
	setFenceClaimEnv(t, city, id, "runtime-capability")
	t.Setenv("GC_RUNTIME_EPOCH", "7")
	store, err = openCityStoreAt(city)
	if err != nil {
		t.Fatal(err)
	}
	seat, err := store.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	work, err = store.Get(work.ID)
	if err != nil {
		t.Fatal(err)
	}
	return s1FileAdmissionFixture{city: city, cfg: cfg, store: store, work: work, seat: seat, fence: session.ExecutionFence{InstanceToken: "runtime-capability", Generation: "7"}}
}

func s1AdmissionResult(t *testing.T, fixture s1FileAdmissionFixture, snapshot beads.Bead, record func(string, session.ExecutionFence, session.WorkStartedEntry) error) (int, string) {
	t.Helper()
	opts := hookClaimOptions{Assignee: "worker-1", SessionID: fixture.seat.ID, StartedFence: &fixture.fence, JSON: true}
	ops := hookClaimOps{
		RecordStartedClaim: record,
		StampWorkMeta:      func(context.Context, string, []string, string, string, map[string]string) error { return nil },
		StampSessionClaim:  func(string, string) error { return nil },
		ReadWorkMeta: func(_ context.Context, _ string, _ []string, id, _ string) (beads.Bead, error) {
			return fixture.store.Get(id)
		},
		ResolveWorkBranch:     func(hookClaimWorkTree) string { return "" },
		ResolveSessionWorkDir: func(string) string { return "" },
	}
	ops.applyDefaults()
	result := hookClaimJSONResult{SchemaVersion: "1", OK: true, Command: "hook", Action: "work", Reason: "existing_assignment", BeadID: snapshot.ID, Assignee: "worker-1"}
	var stdout, stderr bytes.Buffer
	code := writeHookClaimWorkResultForBead(result, snapshot, opts, ops, fixture.city, false, &stdout, &stderr)
	return code, stdout.String()
}

func TestS1CompletedDrainRejectsStaleAdoption(t *testing.T) {
	for _, nextOwner := range []string{"unchanged", "", "worker-2"} {
		t.Run(nextOwner, func(t *testing.T) {
			fixture := newS1FileAdmissionFixture(t, "in_progress", "worker-1")
			staged := false
			record := func(id string, fence session.ExecutionFence, entry session.WorkStartedEntry) error {
				staged = true
				if nextOwner != "unchanged" {
					var stderr bytes.Buffer
					if err := releaseUnexecutedClaimsOnDrainAck(fixture.city, fixture.cfg, fixture.store, nil, fixture.seat, drainAckReleaseBudget, &stderr); err != nil {
						t.Fatalf("release stage failed: %v stderr=%s", err, stderr.String())
					}
					fresh, err := fixture.store.Get(fixture.work.ID)
					if err != nil {
						t.Fatal(err)
					}
					if fresh.Status != "open" || fresh.Assignee != "" {
						t.Fatalf("release did not complete: %+v", fresh)
					}
					if nextOwner != "" {
						writer, _, err := beads.ResolveConditionalWriter(fixture.store)
						if err != nil || writer == nil {
							t.Fatalf("writer: %v", err)
						}
						status := "in_progress"
						if err := writer.UpdateIfMatch(fresh.ID, fresh.Revision, beads.UpdateOpts{Status: &status, Assignee: &nextOwner}); err != nil {
							t.Fatal(err)
						}
					}
				}
				return hookRecordStartedClaim(id, fence, entry)
			}
			code, output := s1AdmissionResult(t, fixture, fixture.work, record)
			if !staged {
				t.Fatal("hook did not reach the staged recording boundary")
			}
			workReturned := bytes.Contains([]byte(output), []byte(`"action":"work"`))
			if nextOwner == "unchanged" {
				if code != 0 || !workReturned {
					t.Fatalf("valid ownership control refused: code=%d stdout=%s", code, output)
				}
			} else if code == 0 || workReturned {
				t.Fatalf("stale adoption emitted work after completed release: nextOwner=%q code=%d stdout=%s", nextOwner, code, output)
			}
		})
	}
}

func TestS1CompletedReleaseAllowsFreshSameOwnerReacquisition(t *testing.T) {
	fixture := newS1FileAdmissionFixture(t, "in_progress", "worker-1")
	front, err := sessionCurrentClaimFrontDoor()
	if err != nil {
		t.Fatal(err)
	}
	staged := false
	record := func(id string, fence session.ExecutionFence, entry session.WorkStartedEntry) error {
		staged = true
		reservation, err := front.ReserveClaimRelease(id, fence, session.ClaimRelease{Store: "work", Bead: fixture.work.ID, Revision: fixture.work.Revision})
		if err != nil {
			t.Fatal(err)
		}
		writer, _, err := beads.ResolveConditionalWriter(fixture.store)
		if err != nil || writer == nil {
			t.Fatalf("writer: %v", err)
		}
		open, empty := "open", ""
		if err := writer.UpdateIfMatch(fixture.work.ID, reservation.Revision, beads.UpdateOpts{Status: &open, Assignee: &empty}); err != nil {
			t.Fatal(err)
		}
		if err := front.CompleteClaimRelease(id, fence, reservation); err != nil {
			t.Fatal(err)
		}
		fresh, err := fixture.store.Get(fixture.work.ID)
		if err != nil {
			t.Fatal(err)
		}
		progress, owner := "in_progress", "worker-1"
		if err := writer.UpdateIfMatch(fresh.ID, fresh.Revision, beads.UpdateOpts{Status: &progress, Assignee: &owner}); err != nil {
			t.Fatal(err)
		}
		return hookRecordStartedClaim(id, fence, entry)
	}
	code, output := s1AdmissionResult(t, fixture, fixture.work, record)
	if !staged {
		t.Fatal("completed-release stage was not exercised")
	}
	if code == 0 || bytes.Contains([]byte(output), []byte(`"action":"work"`)) {
		t.Fatalf("old snapshot delivered replacement acquisition: code=%d stdout=%s", code, output)
	}
	fresh, err := fixture.store.Get(fixture.work.ID)
	if err != nil {
		t.Fatal(err)
	}
	code, output = s1AdmissionResult(t, fixture, fresh, hookRecordStartedClaim)
	if code != 0 || !bytes.Contains([]byte(output), []byte(`"action":"work"`)) {
		t.Fatalf("fresh same-owner acquisition refused: code=%d stdout=%s", code, output)
	}
}

type s1DuringAckOps struct {
	*fakeDrainOps
	beforeAck func() error
}

func (o *s1DuringAckOps) setDrainAck(name string) error {
	if o.beforeAck != nil {
		if err := o.beforeAck(); err != nil {
			return err
		}
	}
	return o.fakeDrainOps.setDrainAck(name)
}

func TestS1CensusToAckExcludesInFlightAdmission(t *testing.T) {
	for _, race := range []bool{false, true} {
		t.Run(map[bool]string{false: "empty-control", true: "late-claim"}[race], func(t *testing.T) {
			fixture := newS1FileAdmissionFixture(t, "open", "")
			oldRelease, oldPoke := drainAckReleaseHeldClaims, drainAckPokeController
			t.Cleanup(func() { drainAckReleaseHeldClaims = oldRelease; drainAckPokeController = oldPoke })
			censused := false
			drainAckReleaseHeldClaims = func(string, string, io.Writer) error {
				var stderr bytes.Buffer
				err := releaseUnexecutedClaimsOnDrainAck(fixture.city, fixture.cfg, fixture.store, nil, fixture.seat, drainAckReleaseBudget, &stderr)
				censused = err == nil
				return err
			}
			drainAckPokeController = func(string, reconcilekey.Key) error { return nil }
			var stageError, startError error
			attempted := false
			ops := &s1DuringAckOps{fakeDrainOps: newFakeDrainOps()}
			if race {
				ops.beforeAck = func() error {
					attempted = true
					if !censused {
						stageError = errors.New("ack stage ran before successful census")
						return stageError
					}
					fresh, err := fixture.store.Get(fixture.work.ID)
					if err != nil {
						stageError = err
						return err
					}
					writer, _, err := beads.ResolveConditionalWriter(fixture.store)
					if err != nil || writer == nil {
						stageError = errors.New("qualified fixture writer unavailable")
						return stageError
					}
					progress, owner := "in_progress", "worker-1"
					if err := writer.UpdateIfMatch(fresh.ID, fresh.Revision, beads.UpdateOpts{Status: &progress, Assignee: &owner}); err != nil {
						stageError = err
						return err
					}
					startError = hookRecordStartedClaim(fixture.seat.ID, fixture.fence, session.WorkStartedEntry{Bead: fresh.ID, Via: "claim"})
					return nil
				}
			}
			var stdout, stderr bytes.Buffer
			code, err := doRuntimeDrainAckResult(ops, fixture.city, "worker-1", "worker-1", fixture.seat.ID, false, &stdout, &stderr)
			if !race {
				if code != 0 || err != nil || !ops.acked["worker-1"] {
					t.Fatalf("empty ack control failed: code=%d err=%v stderr=%s", code, err, stderr.String())
				}
			} else {
				if !attempted || stageError != nil {
					t.Fatalf("late-claim stage not exercised: attempted=%v setup=%v code=%d err=%v", attempted, stageError, code, err)
				}
				if startError == nil && ops.acked["worker-1"] {
					t.Fatalf("claim absent from census started before acknowledged stop: code=%d err=%v", code, err)
				}
			}
		})
	}
}

// Admission must use the residence selected by the canonical claim router,
// including a migrated class binding and the work-first co-resident contract.
func TestS1AdmissionUsesCanonicalClassResidence(t *testing.T) {
	for _, coResident := range []bool{false, true} {
		t.Run(map[bool]string{false: "class-only", true: "work-before-class"}[coResident], func(t *testing.T) {
			fixture := newS1FileAdmissionFixture(t, "in_progress", "worker-1")
			class := newClaimRouteClassStore(t)
			if err := beads.StampOpenedStore(class, "class", gate.Require, nil, nil); err != nil {
				t.Fatal(err)
			}
			id := "gcg-admission"
			if coResident {
				id = fixture.work.ID
			}
			created, err := class.Create(beads.Bead{ID: id, Title: "class-resident admission", Type: "task"})
			if err != nil {
				t.Fatal(err)
			}
			status, owner := "in_progress", "worker-1"
			if err := class.Update(created.ID, beads.UpdateOpts{Status: &status, Assignee: &owner}); err != nil {
				t.Fatal(err)
			}
			candidate, err := class.Get(created.ID)
			if err != nil {
				t.Fatal(err)
			}
			if coResident {
				candidate = fixture.work
			}
			route := newClaimRouteFor(t, class)
			ops := classRoutedHookClaimOps(hookClaimOps{
				RecordStartedClaim: hookRecordStartedClaim,
				ReadWorkMeta: func(_ context.Context, _ string, _ []string, id, _ string) (beads.Bead, error) {
					return fixture.store.Get(id)
				},
				StampWorkMeta:         func(context.Context, string, []string, string, string, map[string]string) error { return nil },
				StampSessionClaim:     func(string, string) error { return nil },
				ResolveWorkBranch:     func(hookClaimWorkTree) string { return "" },
				ResolveSessionWorkDir: func(string) string { return "" },
			}, route)
			opts := hookClaimOptions{Assignee: "worker-1", SessionID: fixture.seat.ID, StartedFence: &fixture.fence, JSON: true}
			result := hookClaimJSONResult{SchemaVersion: "1", OK: true, Command: "hook", Action: "work", Reason: "existing_assignment", BeadID: candidate.ID, Assignee: owner}
			var stdout, stderr bytes.Buffer
			code := writeHookClaimWorkResultForBead(result, candidate, opts, ops, fixture.city, false, &stdout, &stderr)
			if code != 0 || !bytes.Contains(stdout.Bytes(), []byte(`"action":"work"`)) {
				t.Fatalf("canonical resident refused: code=%d stdout=%s stderr=%s", code, stdout.String(), stderr.String())
			}
			if route.knownResident(candidate.ID) == coResident {
				t.Fatalf("wrong canonical residence selected: class=%v coResident=%v", route.knownResident(candidate.ID), coResident)
			}
		})
	}
}

type s1DelayedAckOps struct {
	drainOps
	release <-chan struct{}
	done    chan struct{}
}

func (o s1DelayedAckOps) setDrainAck(name string) error {
	<-o.release
	err := o.drainOps.setDrainAck(name)
	close(o.done)
	return err
}

// A timed-out provider operation can still land later. The hook must not claim
// acknowledgement merely because the legacy CLI returned a deferred success.
func TestS1HookDoesNotReportAckAfterUncertainProviderTimeout(t *testing.T) {
	oldTimeout, oldRelease, oldPoke := drainAckMutationTimeout, drainAckReleaseHeldClaims, drainAckPokeController
	t.Cleanup(func() {
		drainAckMutationTimeout, drainAckReleaseHeldClaims, drainAckPokeController = oldTimeout, oldRelease, oldPoke
	})
	drainAckMutationTimeout = 20 * time.Millisecond
	drainAckReleaseHeldClaims = func(string, string, io.Writer) error { return nil }
	drainAckPokeController = func(string, reconcilekey.Key) error { return nil }
	base := newFakeDrainOps()
	release, done := make(chan struct{}), make(chan struct{})
	ops := s1DelayedAckOps{drainOps: base, release: release, done: done}
	var stdout, stderr bytes.Buffer
	ack := func(io.Writer) error {
		code, err := doRuntimeDrainAckResult(ops, "", "worker-1", "worker-1", "", false, io.Discard, &stderr)
		if err != nil {
			return err
		}
		if code != 0 {
			return errors.New("ACK command failed")
		}
		return nil
	}
	code := writeHookClaimDrain(hookClaimLabel, hookClaimReasonNoWork, true, true, ack, &stdout, &stderr)
	if code == 0 || bytes.Contains(stdout.Bytes(), []byte(`"drain_acknowledged":true`)) {
		t.Errorf("uncertain ACK reported as acknowledged: code=%d stdout=%s", code, stdout.String())
	}
	close(release)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("delayed ACK did not finish")
	}
	if acked, err := base.isDrainAcked("worker-1"); err != nil || !acked {
		t.Fatalf("uncertain-after-write control did not land: acked=%v err=%v", acked, err)
	}
}

func TestS1NoArgumentDrainAckPublishesQualifiedTerminalAck(t *testing.T) {
	for _, staleEnv := range []bool{false, true} {
		t.Run(fmt.Sprintf("stale-env-%v", staleEnv), func(t *testing.T) {
			f := newS1FileAdmissionFixture(t, "open", "")
			t.Setenv("GC_SESSION", "fake")
			if staleEnv {
				t.Setenv("GC_SESSION_ID", "stale-unrelated-session")
			}
			sp := runtime.NewFake()
			if err := sp.Start(context.Background(), "worker-1", runtime.Config{}); err != nil {
				t.Fatal(err)
			}
			for k, v := range map[string]string{"GC_SESSION_ID": f.seat.ID, "GC_INSTANCE_TOKEN": f.fence.InstanceToken} {
				if err := sp.SetMeta("worker-1", k, v); err != nil {
					t.Fatal(err)
				}
			}
			previous := runtimeRegistry
			runtimeRegistry = registry.New()
			t.Cleanup(func() { runtimeRegistry = previous })
			if err := runtimeRegistry.Register("fake", func(string, config.SessionConfig, string, string) (runtime.Provider, error) { return sp, nil }); err != nil {
				t.Fatal(err)
			}
			oldPoke := drainAckPokeController
			drainAckPokeController = func(string, reconcilekey.Key) error { return nil }
			t.Cleanup(func() { drainAckPokeController = oldPoke })
			var stdout, stderr bytes.Buffer
			code, err := cmdRuntimeDrainAckResult(nil, true, &stdout, &stderr)
			if code != 0 || err != nil {
				t.Fatalf("production ACK failed: code=%d err=%v stderr=%s", code, err, stderr.String())
			}
			front := sessionFrontDoor(f.store)
			info, cell, enrolled, err := front.ExecutionEvidence(f.seat.ID)
			if err != nil || !enrolled || cell.Drain == nil || !cell.Drain.Committed {
				t.Fatalf("missing committed census: enrolled=%v cell=%+v err=%v", enrolled, cell, err)
			}
			nonce, err := sp.GetMeta("worker-1", drainAdmissionAttemptKey)
			if err != nil || nonce != cell.Drain.Attempt {
				t.Fatalf("actual no-args ACK skipped binding: nonce=%q census=%q err=%v", nonce, cell.Drain.Attempt, err)
			}
			if !qualifiedDrainAck(info, front, sp, "worker-1", false) {
				t.Fatal("actual no-args ACK cannot qualify")
			}
			// A retry republishes the committed census B. Work assigned after
			// that census is a sentinel: sweeping again would release it.
			late := mustCreateDrainAckBead(t, f.store, beads.Bead{Title: "assigned after committed census", Type: "task"}, "in_progress", "worker-1")
			committedAttempt := cell.Drain.Attempt
			if code, err := cmdRuntimeDrainAckResult(nil, true, &stdout, &stderr); code != 0 || err != nil {
				t.Fatalf("actual ACK retry failed: code=%d err=%v stderr=%s", code, err, stderr.String())
			}
			_, retried, _, err := front.ExecutionEvidence(f.seat.ID)
			if err != nil || retried.Drain == nil || retried.Drain.Attempt != committedAttempt {
				t.Fatalf("retry replaced committed B: cell=%+v err=%v", retried, err)
			}
			if status, owner := drainAckBeadStatus(t, f.store, late.ID); status != "in_progress" || owner != "worker-1" {
				t.Fatalf("retry swept late assignment: status=%s owner=%s", status, owner)
			}
			env := newReconcilerTestEnv()
			env.store = f.store
			env.sp = sp
			env.cfg = f.cfg
			env.addDesired("worker-1", "worker", true)
			seat, err := f.store.Get(f.seat.ID)
			if err != nil {
				t.Fatal(err)
			}
			reconcileS1StopCompletion(t, env, []beads.Bead{seat}, map[string]int{"worker": 1}, newDrainOps(sp))
			if sp.IsRunning("worker-1") {
				t.Fatalf("production no-args ACK did not complete stop: %s", env.stderr.String())
			}
		})
	}
}

type s1PausedAckProvider struct {
	runtime.Provider
	paused  chan struct{}
	release chan struct{}
}

func (p *s1PausedAckProvider) SetMeta(name, key, value string) error {
	if key == "GC_DRAIN_ACK" && value == "1" {
		close(p.paused)
		<-p.release
	}
	return p.Provider.SetMeta(name, key, value)
}

func TestS1NoArgumentDelayedAckCannotSurviveCanonicalCancellation(t *testing.T) {
	f := newS1FileAdmissionFixture(t, "open", "")
	t.Setenv("GC_SESSION", "fake")
	t.Setenv("GC_SESSION_ID", "stale-env-id")
	sp := runtime.NewFake()
	if err := sp.Start(context.Background(), "worker-1", runtime.Config{}); err != nil {
		t.Fatal(err)
	}
	for k, v := range map[string]string{"GC_SESSION_ID": f.seat.ID, "GC_INSTANCE_TOKEN": f.fence.InstanceToken} {
		if err := sp.SetMeta("worker-1", k, v); err != nil {
			t.Fatal(err)
		}
	}
	paused := &s1PausedAckProvider{Provider: sp, paused: make(chan struct{}), release: make(chan struct{})}
	t.Cleanup(func() {
		select {
		case <-paused.release:
		default:
			close(paused.release)
		}
	})
	previous := runtimeRegistry
	runtimeRegistry = registry.New()
	t.Cleanup(func() { runtimeRegistry = previous })
	if err := runtimeRegistry.Register("fake", func(string, config.SessionConfig, string, string) (runtime.Provider, error) { return paused, nil }); err != nil {
		t.Fatal(err)
	}
	oldPoke := drainAckPokeController
	drainAckPokeController = func(string, reconcilekey.Key) error { return nil }
	t.Cleanup(func() { drainAckPokeController = oldPoke })
	type outcome struct {
		code int
		err  error
	}
	done := make(chan outcome, 1)
	var stdout, stderr bytes.Buffer
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		code, err := cmdRuntimeDrainAckResult(nil, true, &stdout, &stderr)
		done <- outcome{code, err}
	}()
	t.Cleanup(func() {
		select {
		case <-paused.release:
		default:
			close(paused.release)
		}
		select {
		case <-finished:
		case <-time.After(5 * time.Second):
			t.Error("ACK goroutine did not finish before fixture cleanup")
		}
	})
	select {
	case <-paused.paused:
	case out := <-done:
		t.Fatalf("ACK did not reach barrier: %+v stderr=%s", out, stderr.String())
	case <-time.After(5 * time.Second):
		t.Fatal("ACK publication barrier not reached")
	}
	front := sessionFrontDoor(f.store)
	_, cell, _, err := front.ExecutionEvidence(f.seat.ID)
	if err != nil || cell.Drain == nil {
		t.Fatalf("census unavailable: %v", err)
	}
	oldAttempt := cell.Drain.Attempt
	env := newReconcilerTestEnv()
	env.store = f.store
	env.sp = sp
	env.cfg = f.cfg
	env.addDesired("worker-1", "worker", false)
	now := env.clk.Now()
	env.dt.set(f.seat.ID, &drainState{reason: "wait-hold", generation: 7, admissionAttempt: oldAttempt, startedAt: now.Add(-time.Second), deadline: now.Add(time.Minute)})
	advanceSessionDrainsWithSessionsTraced(env.dt, sp, f.store, infoLookupFromBeadLookup(func(id string) *beads.Bead {
		row, err := f.store.Get(id)
		if err != nil {
			return nil
		}
		return &row
	}), map[string]wakeEvaluation{f.seat.ID: {Reasons: []WakeReason{WakeWait}}}, f.cfg, env.clk, nil)
	if env.dt.get(f.seat.ID) != nil {
		t.Fatal("canonical cancellation did not complete")
	}
	status, owner := "in_progress", "worker-1"
	if err := f.store.Update(f.work.ID, beads.UpdateOpts{Status: &status, Assignee: &owner}); err != nil {
		t.Fatal(err)
	}
	work, err := f.store.Get(f.work.ID)
	if err != nil {
		t.Fatal(err)
	}
	code, result := s1AdmissionResult(t, f, work, front.RecordStartedClaim)
	if code != 0 || !strings.Contains(result, `"action":"work"`) {
		t.Fatalf("canceled session did not resume work: code=%d result=%s", code, result)
	}
	close(paused.release)
	var out outcome
	select {
	case out = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("delayed ACK did not finish")
	}
	nonce, err := sp.GetMeta("worker-1", drainAdmissionAttemptKey)
	if err != nil || nonce != oldAttempt {
		t.Fatalf("delayed old-publication control not observed: nonce=%q err=%v", nonce, err)
	}
	if out.code == 0 || out.err == nil {
		t.Errorf("canceled publication reported qualified success: %+v stdout=%s", out, stdout.String())
	}
	seat, err := f.store.Get(f.seat.ID)
	if err != nil {
		t.Fatal(err)
	}
	reconcileS1StopCompletion(t, env, []beads.Bead{seat}, map[string]int{"worker": 1}, newDrainOps(sp))
	if !sp.IsRunning("worker-1") || env.sessionInfo(f.seat.ID).StateReason == session.DrainAckStopPendingReason {
		t.Fatal("delayed canceled production ACK stopped resumed work")
	}
}

type s1AfterAckFlagProvider struct {
	runtime.Provider
	published chan struct{}
	resume    chan struct{}
}

func (p *s1AfterAckFlagProvider) SetMeta(name, key, value string) error {
	if err := p.Provider.SetMeta(name, key, value); err != nil {
		return err
	}
	if key == "GC_DRAIN_ACK" && value == "1" {
		close(p.published)
		<-p.resume
	}
	return nil
}

func TestS1AckFlagBeforeFrameCannotBeCleanedAsStale(t *testing.T) {
	f := newS1FileAdmissionFixture(t, "open", "")
	t.Setenv("GC_SESSION", "fake")
	sp := runtime.NewFake()
	if err := sp.Start(context.Background(), "worker-1", runtime.Config{}); err != nil {
		t.Fatal(err)
	}
	for k, v := range map[string]string{"GC_SESSION_ID": f.seat.ID, "GC_INSTANCE_TOKEN": f.fence.InstanceToken} {
		if err := sp.SetMeta("worker-1", k, v); err != nil {
			t.Fatal(err)
		}
	}
	paused := &s1AfterAckFlagProvider{Provider: sp, published: make(chan struct{}), resume: make(chan struct{})}
	previous := runtimeRegistry
	runtimeRegistry = registry.New()
	t.Cleanup(func() { runtimeRegistry = previous })
	if err := runtimeRegistry.Register("fake", func(string, config.SessionConfig, string, string) (runtime.Provider, error) { return paused, nil }); err != nil {
		t.Fatal(err)
	}
	oldPoke := drainAckPokeController
	drainAckPokeController = func(string, reconcilekey.Key) error { return nil }
	t.Cleanup(func() { drainAckPokeController = oldPoke })
	type outcome struct {
		code int
		err  error
	}
	done := make(chan outcome, 1)
	finished := make(chan struct{})
	var stdout, stderr bytes.Buffer
	go func() {
		defer close(finished)
		code, err := cmdRuntimeDrainAckResult(nil, true, &stdout, &stderr)
		done <- outcome{code, err}
	}()
	t.Cleanup(func() {
		select {
		case <-paused.resume:
		default:
			close(paused.resume)
		}
		select {
		case <-finished:
		case <-time.After(5 * time.Second):
			t.Error("publisher did not finish before fixture cleanup")
		}
	})
	select {
	case <-paused.published:
	case out := <-done:
		t.Fatalf("publisher never reached ACK-before-frame: %+v %s", out, stderr.String())
	case <-time.After(5 * time.Second):
		t.Fatal("publisher ACK-before-frame barrier not reached")
	}
	front := sessionFrontDoor(f.store)
	info, cell, _, err := front.ExecutionEvidence(f.seat.ID)
	if err != nil || cell.Drain == nil || !cell.Drain.Committed {
		t.Fatalf("native committed census missing: %v", err)
	}
	// This is the real supervisor reader while the agent process is publishing.
	_, _, _ = currentDrainAck(info, front, sp, newDrainOps(sp), "worker-1")
	ack, err := sp.GetMeta("worker-1", "GC_DRAIN_ACK")
	if err != nil {
		t.Fatal(err)
	}
	close(paused.resume)
	var out outcome
	select {
	case out = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("publisher did not finish")
	}
	if ack != "1" {
		t.Fatalf("publication-in-progress was cleared as stale: ack=%q publisher=%+v", ack, out)
	}
	if out.code != 0 || out.err != nil {
		t.Fatalf("publisher failed: %+v stderr=%s", out, stderr.String())
	}
	env := newReconcilerTestEnv()
	env.store = f.store
	env.sp = sp
	env.cfg = f.cfg
	env.addDesired("worker-1", "worker", true)
	seat, err := f.store.Get(f.seat.ID)
	if err != nil {
		t.Fatal(err)
	}
	reconcileS1StopCompletion(t, env, []beads.Bead{seat}, map[string]int{"worker": 1}, newDrainOps(sp))
	if sp.IsRunning("worker-1") {
		t.Fatalf("completed real ACK did not terminally stop: %s", env.stderr.String())
	}
}

func TestS1ActualUndrainCancelsNativeAttemptBeforeProviderCleanup(t *testing.T) {
	for _, scenario := range []string{"controller", "agent", "accepted", "corrupt", "wrong-runtime", "reused-runtime-name", "provider-read-failure", "native-request", "enrolled-off", "cleanup-retry", "replacement-during-retry", "legacy"} {
		t.Run(scenario, func(t *testing.T) {
			f := newS1FileAdmissionFixture(t, "open", "")
			t.Setenv("GC_SESSION", "fake")
			front := sessionFrontDoor(f.store)
			attempt := ""
			if scenario != "legacy" && scenario != "native-request" && scenario != "enrolled-off" {
				var err error
				if scenario == "agent" {
					attempt, err = front.BeginDrainAdmission(f.seat.ID, f.fence)
					if err == nil {
						err = front.FinishDrainAdmission(f.seat.ID, f.fence, attempt, true)
					}
				} else {
					attempt, err = front.BeginControllerDrainAdmission(f.seat.ID, f.fence)
				}
				if err != nil {
					t.Fatal(err)
				}
			} else if scenario == "legacy" {
				t.Setenv("GC_BEADS_CONDITIONAL_WRITES", "off")
			}
			sp := runtime.NewFake()
			if err := sp.Start(context.Background(), "worker-1", runtime.Config{}); err != nil {
				t.Fatal(err)
			}
			for k, v := range map[string]string{"GC_SESSION_ID": f.seat.ID, "GC_INSTANCE_TOKEN": f.fence.InstanceToken, "GC_DRAIN": "1", "GC_DRAIN_ACK": "1"} {
				if err := sp.SetMeta("worker-1", k, v); err != nil {
					t.Fatal(err)
				}
			}
			if scenario != "legacy" {
				source := reconcilerDrainAckSourceValue
				reason := "wait-hold"
				if scenario == "agent" {
					source = drainAckSourceAgentValue
					reason = ""
				}
				if err := publishDrainAckFrame(sp, "worker-1", session.Info{ID: f.seat.ID, SessionNameMetadata: "worker-1", Generation: f.fence.Generation, InstanceToken: f.fence.InstanceToken}, source, reason, attempt); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "native-request" || scenario == "enrolled-off" {
				if err := front.RecordStartedClaim(f.seat.ID, f.fence, session.WorkStartedEntry{Bead: "running-before-request"}); err != nil {
					t.Fatal(err)
				}
				_, cell, enrolled, err := front.ExecutionEvidence(f.seat.ID)
				if err != nil || !enrolled || cell.Drain != nil {
					t.Fatalf("native request must start enrolled without a drain gate: enrolled=%v cell=%+v err=%v", enrolled, cell, err)
				}
				opened, err := openCityStoreAt(f.city)
				if err != nil {
					t.Fatal(err)
				}
				writer, _, err := beads.ResolveConditionalWriter(opened)
				if err != nil || writer == nil || beads.InspectConditionalWrites(opened).Mode != gate.Require {
					t.Fatalf("reopened native store must support required CAS: writer=%v err=%v", writer, err)
				}
				if scenario == "enrolled-off" {
					t.Setenv("GC_BEADS_CONDITIONAL_WRITES", "off")
					reopened, err := openCityStoreAt(f.city)
					if err != nil {
						t.Fatal(err)
					}
					writer, _, err := beads.ResolveConditionalWriter(reopened)
					if err != nil || writer != nil || beads.InspectConditionalWrites(reopened).Mode != gate.Off {
						t.Fatalf("OFF control did not reopen without CAS: writer=%v err=%v", writer, err)
					}
				}
			}
			if scenario == "accepted" {
				if err := front.AcceptControllerDrainStop(f.seat.ID, f.fence, attempt); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "corrupt" {
				if err := f.store.SetMetadata(f.seat.ID, session.WorkStartedKey, "{ corrupt"); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "wrong-runtime" {
				if err := sp.SetMeta("worker-1", "GC_INSTANCE_TOKEN", "different-runtime"); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "reused-runtime-name" {
				// The explicit old ID must not follow a runtime name now owned by B,
				// even if the provider retained the old token metadata.
				if err := sp.SetMeta("worker-1", "GC_SESSION_ID", "replacement-session-B"); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "provider-read-failure" {
				sp.GetMetaErrors["worker-1"] = map[string]error{"GC_SESSION_ID": errors.New("runtime identity unavailable")}
			}
			before, err := f.store.Get(f.seat.ID)
			if err != nil {
				t.Fatal(err)
			}
			beforeFrame, err := sp.GetMeta("worker-1", drainAckFrameKey)
			if err != nil {
				t.Fatal(err)
			}
			var provider runtime.Provider = sp
			previous := runtimeRegistry
			runtimeRegistry = registry.New()
			t.Cleanup(func() { runtimeRegistry = previous })
			if err := runtimeRegistry.Register("fake", func(string, config.SessionConfig, string, string) (runtime.Provider, error) { return provider, nil }); err != nil {
				t.Fatal(err)
			}
			// The operator targets a canonical ID, not its own caller environment.
			t.Setenv("GC_SESSION_ID", "unrelated-caller")
			t.Setenv("GC_INSTANCE_TOKEN", "unrelated-caller-token")
			var stdout, stderr bytes.Buffer
			if scenario == "cleanup-retry" || scenario == "replacement-during-retry" {
				sp.RemoveMetaErrors["worker-1"] = map[string]error{"GC_DRAIN_ACK": errors.New("cleanup unavailable")}
				if code := cmdRuntimeUndrain([]string{f.seat.ID}, true, &stdout, &stderr); code == 0 {
					t.Fatal("cleanup failure reported success")
				}
				_, cell, _, err := front.ExecutionEvidence(f.seat.ID)
				if err != nil || cell.Drain != nil {
					t.Fatalf("native cancellation did not precede failed cleanup: %+v %v", cell, err)
				}
				delete(sp.RemoveMetaErrors, "worker-1")
				for k, v := range map[string]string{"GC_DRAIN": "2", "GC_DRAIN_ACK": "1"} {
					if err := sp.SetMeta("worker-1", k, v); err != nil {
						t.Fatal(err)
					}
				}
				if scenario == "replacement-during-retry" {
					interleaved := false
					provider = &s1UndrainIdentityReadProvider{Provider: sp, once: func() {
						interleaved = true
						if _, err := front.BeginControllerDrainAdmission(f.seat.ID, f.fence); err != nil {
							t.Fatal(err)
						}
					}}
					before, err := f.store.Get(f.seat.ID)
					if err != nil {
						t.Fatal(err)
					}
					if code := cmdRuntimeUndrain([]string{f.seat.ID}, true, &stdout, &stderr); code == 0 || !interleaved {
						t.Fatalf("replacement retry not refused: code=%d interleaved=%v", code, interleaved)
					}
					ack, _ := sp.GetMeta("worker-1", "GC_DRAIN_ACK")
					drain, _ := sp.GetMeta("worker-1", "GC_DRAIN")
					after, err := f.store.Get(f.seat.ID)
					if err != nil {
						t.Fatal(err)
					}
					if ack != "1" || drain != "2" || after.Metadata[session.WorkStartedKey] == before.Metadata[session.WorkStartedKey] {
						t.Fatal("replacement intent lost or interleaving did not change native state")
					}
					return
				}
			}
			code := cmdRuntimeUndrain([]string{f.seat.ID}, true, &stdout, &stderr)
			if scenario == "enrolled-off" && !strings.Contains(stderr.String(), beads.ErrConditionalWriteUnsupported.Error()) {
				t.Fatalf("OFF refusal was not the required unsupported-CAS guard: %s", stderr.String())
			}
			ack, _ := sp.GetMeta("worker-1", "GC_DRAIN_ACK")
			draining, _ := sp.GetMeta("worker-1", "GC_DRAIN")
			frame, _ := sp.GetMeta("worker-1", drainAckFrameKey)
			saved, err := f.store.Get(f.seat.ID)
			if err != nil {
				t.Fatal(err)
			}
			if scenario == "accepted" || scenario == "corrupt" || scenario == "wrong-runtime" || scenario == "reused-runtime-name" || scenario == "provider-read-failure" || scenario == "enrolled-off" {
				if code == 0 || ack != "1" || draining != "1" || frame != beforeFrame || saved.Metadata[session.WorkStartedKey] != before.Metadata[session.WorkStartedKey] {
					t.Fatalf("undrain reported/cleared unproved native cancellation: code=%d ack=%q drain=%q stderr=%s", code, ack, draining, stderr.String())
				}
				return
			}
			if code != 0 || ack != "" || draining != "" {
				t.Fatalf("actual undrain failed: code=%d ack=%q drain=%q stderr=%s", code, ack, draining, stderr.String())
			}
			if scenario == "legacy" {
				if saved.Metadata[session.WorkStartedKey] != "" {
					t.Fatal("legacy undrain enrolled session")
				}
				return
			}
			if err := front.RecordStartedClaim(f.seat.ID, f.fence, session.WorkStartedEntry{Bead: "resumed-after-operator-undrain"}); err != nil {
				t.Fatalf("Undrained success left native admission closed: %v", err)
			}
		})
	}
}

func TestS1OverwrittenControllerFrameRepublishesCapturedAttempt(t *testing.T) {
	f := newS1FileAdmissionFixture(t, "open", "")
	front := sessionFrontDoor(f.store)
	a, err := front.BeginControllerDrainAdmission(f.seat.ID, f.fence)
	if err != nil {
		t.Fatal(err)
	}
	if err := front.CancelDrainAdmission(f.seat.ID, f.fence, a); err != nil {
		t.Fatal(err)
	}
	b, err := front.BeginControllerDrainAdmission(f.seat.ID, f.fence)
	if err != nil {
		t.Fatal(err)
	}
	info := frontInfoForSoftReloadTest(t, front, f.seat.ID)
	sp := runtime.NewFake()
	if err := sp.Start(context.Background(), "worker-1", runtime.Config{}); err != nil {
		t.Fatal(err)
	}
	for k, v := range map[string]string{"GC_SESSION_ID": info.ID, "GC_INSTANCE_TOKEN": info.InstanceToken} {
		if err := sp.SetMeta("worker-1", k, v); err != nil {
			t.Fatal(err)
		}
	}
	gen, err := strconv.Atoi(info.Generation)
	if err != nil {
		t.Fatal(err)
	}
	ds := &drainState{reason: "orphaned", generation: gen, admissionAttempt: b, ackSet: true}
	if err := setReconcilerDrainAckMetadata(sp, "worker-1", ds, front); err != nil {
		t.Fatal(err)
	}
	// Delayed A lands only after B's complete publication.
	if err := publishDrainAckFrame(sp, "worker-1", info, reconcilerDrainAckSourceValue, "config-drift", a); err != nil {
		t.Fatal(err)
	}
	if qualifiedDrainAck(info, front, sp, "worker-1", true) {
		t.Fatal("stale A accepted B for stop")
	}
	if _, err := front.InspectDrainAdmission(info.ID, f.fence, b); err != nil {
		t.Fatalf("stale A changed B: %v", err)
	}
	dt := newDrainTracker()
	dt.set(info.ID, ds)
	env := newReconcilerTestEnv()
	ds.deadline = env.clk.Now().Add(time.Hour)
	advanceSessionDrainsWithSessionsTraced(dt, sp, f.store, func(id string) (session.Info, bool) { return info, id == info.ID }, map[string]wakeEvaluation{}, f.cfg, env.clk, nil)
	frame, err := readDrainAckFrame(info, sp, "worker-1")
	if err != nil || frame.Attempt != b || frame.Reason != ds.reason || !ds.ackSet || ds.admissionAttempt != b {
		t.Fatalf("captured B not republished: frame=%+v state=%+v err=%v", frame, ds, err)
	}
	if !sp.IsRunning("worker-1") {
		t.Fatal("republication stopped runtime")
	}
}

type s1UndrainIdentityReadProvider struct {
	runtime.Provider
	once func()
}

func (p *s1UndrainIdentityReadProvider) GetMeta(name, key string) (string, error) {
	if key == "GC_INSTANCE_TOKEN" && p.once != nil {
		callback := p.once
		p.once = nil
		callback()
	}
	return p.Provider.GetMeta(name, key)
}

func TestS1RetainedFallbackUsesSecondWorkRoot(t *testing.T) {
	for _, inverse := range []bool{false, true} {
		topology := "rig-held"
		if inverse {
			topology = "city-held"
		}
		t.Run(topology, func(t *testing.T) {
			for _, scenario := range []string{"success", "read-failure", "revision-change", "missing-environment", "scoped-query"} {
				t.Run(scenario, func(t *testing.T) { testS1RetainedFallbackSecondWorkRoot(t, scenario, inverse) })
			}
		})
	}
}

func testS1RetainedFallbackSecondWorkRoot(t *testing.T, scenario string, inverse bool) {
	clearGCEnv(t)
	disableManagedDoltRecoveryForTest(t)
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_BEADS_CONDITIONAL_WRITES", "require")
	city := writeFenceTestCity(t)
	configPath := filepath.Join(city, "city.toml")
	text, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	text = []byte(strings.Replace(string(text), "name = \"worker\"", "name = \"worker\"\nscope = \"city\"", 1) + "\n[[rigs]]\nname = \"later\"\npath = \"later\"\nprefix = \"wb\"\n[beads]\nconditional_writes = \"require\"\n")
	if err := os.WriteFile(configPath, text, 0o644); err != nil {
		t.Fatal(err)
	}
	later := filepath.Join(city, "later")
	for _, root := range []string{city, later} {
		if err := os.MkdirAll(filepath.Join(root, ".gc"), 0o755); err != nil {
			t.Fatal(err)
		}
		store, err := openScopeLocalFileStoreForCity(root, city)
		if err != nil {
			t.Fatal(err)
		}
		seed, err := store.Create(beads.Bead{Title: "initialize disposable physical root"})
		if err != nil {
			t.Fatal(err)
		}
		if err := store.Delete(seed.ID); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(fileStoreLayoutMarkerPath(city), []byte(fileStoreLayoutScopedV1), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadCityConfigWithoutBuiltinPackRefresh(city, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	primaryRoot, retainedRoot := city, later
	if inverse {
		primaryRoot, retainedRoot = later, city
	}
	primary, err := openStoreAtForCityWithConfig(primaryRoot, city, cfg)
	if err != nil {
		t.Fatal(err)
	}
	secondary, err := openStoreAtForCityWithConfig(retainedRoot, city, cfg)
	if err != nil {
		t.Fatal(err)
	}
	binding := newClaimRouteClassStore(t)
	if err := beads.StampOpenedStore(binding, "sqlite", gate.Require, nil, nil); err != nil {
		t.Fatal(err)
	}
	seedSplitRoutes(t, city, binding)
	work := mustCreateDrainAckBead(t, secondary, beads.Bead{Title: "retained only in work root B", Type: "task"}, "in_progress", "worker-1")
	work, err = secondary.Get(work.ID)
	if err != nil || work.Revision == 0 {
		t.Fatalf("native B fixture: work=%+v err=%v", work, err)
	}
	seat, err := binding.Create(beads.Bead{Title: "worker-1", Type: session.BeadType, Metadata: map[string]string{"session_name": "worker-1", "template": "worker", "state": "active", "instance_token": "retained-runtime", "generation": "7"}})
	if err != nil {
		t.Fatal(err)
	}
	// Runtime initialization follows row creation and gives the native row its
	// first revision before an execution fence can admit work.
	if err := binding.Update(seat.ID, beads.UpdateOpts{Metadata: map[string]string{"instance_token": "retained-runtime", "generation": "7"}}); err != nil {
		t.Fatal(err)
	}
	fence := session.ExecutionFence{InstanceToken: "retained-runtime", Generation: "7"}
	front := sessionFrontDoor(binding)
	if err := front.RecordStartedClaim(seat.ID, fence, session.WorkStartedEntry{Bead: work.ID, Via: "claim"}); err != nil {
		t.Fatal(err)
	}
	seat, err = binding.Get(seat.ID)
	if err != nil {
		t.Fatal(err)
	}
	setFenceClaimEnv(t, city, seat.ID, "retained-runtime")
	t.Setenv("GC_RUNTIME_EPOCH", "7")
	route, err := newHookClaimClassRoute(binding)
	if err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	recorded, drainCalls := 0, 0
	opts := hookClaimOptions{Assignee: "worker-1", SessionID: seat.ID, StartedFence: &fence, JSON: true, DrainAck: true}
	ops := hookClaimOps{
		DrainAck: func(w io.Writer) error {
			drainCalls++
			cityStore, rigStore := primary, secondary
			if inverse {
				cityStore, rigStore = secondary, primary
			}
			return releaseUnexecutedClaimsOnDrainAck(city, cfg, cityStore, map[string]beads.Store{"later": rigStore}, seat, drainAckReleaseBudget, w)
		},
		RecordStartedClaim: func(id string, got session.ExecutionFence, entry session.WorkStartedEntry) error {
			recorded++
			if stdout.Len() != 0 || id != seat.ID || got != fence || entry.Bead != work.ID {
				t.Fatalf("incorrect execution ordering: id=%s entry=%+v stdout=%s", id, entry, stdout.String())
			}
			if err := front.RecordStartedClaim(id, got, entry); err != nil {
				return err
			}
			if scenario == "revision-change" {
				writer, _, err := beads.ResolveConditionalWriter(secondary)
				if err != nil {
					return err
				}
				return writer.UpdateIfMatch(work.ID, work.Revision, beads.UpdateOpts{Metadata: map[string]string{"revision-control": "changed"}})
			}
			return nil
		},
		ReadWorkMeta: func(_ context.Context, dir string, env []string, id, _ string) (beads.Bead, error) {
			if drainCalls > 0 && (dir != retainedRoot || !slices.Contains(env, "GC_RIG_ROOT="+retainedRoot)) {
				t.Errorf("read lost B context: dir=%q env=%v", dir, env)
			}
			if drainCalls > 0 && scenario == "read-failure" {
				return beads.Bead{}, errors.New("retained B read unavailable")
			}
			store, err := openStoreAtForCityWithConfig(dir, city, cfg)
			if err != nil {
				return beads.Bead{}, err
			}
			return store.Get(id)
		},
		StampWorkMeta: func(_ context.Context, dir string, env []string, _, _ string, _ map[string]string) error {
			if !slices.Contains(env, "GC_RIG_ROOT="+retainedRoot) {
				t.Errorf("stamp lost B environment: %v", env)
			}
			if filepath.Clean(dir) != filepath.Clean(retainedRoot) {
				t.Errorf("work stamp used %q, want physical B %q", dir, retainedRoot)
			}
			return nil
		},
		StampSessionClaim:     func(string, string) error { return nil },
		ResolveWorkBranch:     func(hookClaimWorkTree) string { return "" },
		ResolveSessionWorkDir: func(string) string { return "" },
	}
	ops = classRoutedHookClaimOps(ops, route)
	primaryBefore, err := os.ReadFile(filepath.Join(primaryRoot, ".gc", "beads.json"))
	if err != nil {
		t.Fatal(err)
	}
	stores := []hookStore{{dir: primaryRoot, env: []string{"GC_CITY=" + city, "GC_RIG_ROOT=" + primaryRoot}}, {dir: retainedRoot, env: []string{"GC_CITY=" + city, "GC_RIG_ROOT=" + retainedRoot}}}
	if scenario == "missing-environment" {
		stores[1].env = nil
	}
	wantQueries := 2
	if scenario == "scoped-query" {
		ops.WorkStores = stores
		stores = scopeFederatedHookStores(stores, "federated-ready", "single-store-ready")
		if len(stores) != 1 {
			t.Fatal("production query scoping did not remove secondary leg")
		}
		wantQueries = 1
	}
	queried := 0
	code := claimHookWorkWithRunner("printf '[]'", primaryRoot, stores[0].env, stores, opts, ops,
		func(_, _ string, _ []string) (string, error) { queried++; return "[]", nil },
		func(command string, err error) { t.Errorf("unexpected query failure: %s: %v", command, err) }, &stdout, &stderr)
	if queried != wantQueries {
		t.Fatalf("no-work query did not visit both physical roots: got %d", queried)
	}
	primaryAfter, err := os.ReadFile(filepath.Join(primaryRoot, ".gc", "beads.json"))
	if err != nil || !bytes.Equal(primaryBefore, primaryAfter) {
		t.Fatalf("primary A changed: %v", err)
	}
	if scenario != "success" && scenario != "scoped-query" {
		wantError, wantRecorded := "retained B read unavailable", 0
		if scenario == "revision-change" {
			wantError, wantRecorded = "refusing execution after claim revision changed", 1
		}
		if scenario == "missing-environment" {
			wantError = "has no captured query environment"
		}
		if code != 1 || recorded != wantRecorded || drainCalls != 1 || stdout.Len() != 0 || !strings.Contains(stderr.String(), wantError) {
			t.Fatalf("control failed closed incorrectly: code=%d recorded=%d drains=%d stdout=%s stderr=%s", code, recorded, drainCalls, stdout.String(), stderr.String())
		}
	} else if code != 0 || !bytes.Contains(stdout.Bytes(), []byte(`"action":"work"`)) || recorded != 1 || drainCalls != 1 {
		t.Fatalf("retained B not served once after execution recording: code=%d recorded=%d drains=%d stdout=%s stderr=%s", code, recorded, drainCalls, stdout.String(), stderr.String())
	}
	if bytes.Contains(stdout.Bytes(), []byte(`"drain_acknowledged":true`)) {
		t.Fatalf("retained claim acknowledged drain: %s", stdout.String())
	}
	if _, err := primary.Get(work.ID); !errors.Is(err, beads.ErrNotFound) {
		t.Fatalf("B claim leaked into primary A: %v", err)
	}
	current, err := secondary.Get(work.ID)
	if err != nil || current.Status != "in_progress" || current.Assignee != "worker-1" {
		t.Fatalf("B ownership changed: %+v err=%v", current, err)
	}
}

// reconcileS1StopCompletion observes completion of the actual queued stop,
// including a failed stop, before the test checks custody or retries it.
func reconcileS1StopCompletion(t *testing.T, env *reconcilerTestEnv, rows []beads.Bead, desired map[string]int, dops drainOps) {
	t.Helper()
	tracker := &asyncStartTracker{}
	previous := env.startOptions
	env.startOptions = append(slices.Clone(previous), withAsyncDrainAckStopTracker(tracker))
	defer func() { env.startOptions = previous }()
	env.reconcileWithPoolDesiredAndDrainOps(rows, desired, dops)
	if !tracker.wait(2 * time.Second) {
		t.Fatal("queued S1 stop did not complete")
	}
}

// An unreadable native ACK must hold an expired drain even after its desired
// agent disappears; the next healthy tick may resume the same stop.
func TestS1NotDesiredUnreadableAckDefersExpiredDrain(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{Agents: []config.Agent{{Name: "worker"}}, Beads: config.BeadsConfig{ConditionalWrites: "require"}}
	env.addDesired("worker", "worker", true)
	seat := env.createSessionBead("worker", "worker")
	env.markSessionActive(&seat)
	if err := beads.StampOpenedStore(env.store, "memory", gate.Require, nil, nil); err != nil {
		t.Fatal(err)
	}
	for key, value := range map[string]string{"GC_SESSION_ID": seat.ID, "GC_INSTANCE_TOKEN": "test-token"} {
		if err := env.sp.SetMeta("worker", key, value); err != nil {
			t.Fatal(err)
		}
	}
	front := sessionFrontDoor(env.store)
	info := env.sessionInfo(seat.ID)
	if ok, err := beginSessionDrainInfoResult(info, env.sp, env.dt, "orphaned", env.clk, time.Second, front); err != nil || !ok {
		t.Fatalf("begin drain = %v, %v", ok, err)
	}
	pending := env.dt.get(seat.ID)
	if err := setReconcilerDrainAckMetadata(env.sp, "worker", pending, front); err != nil {
		t.Fatal(err)
	}
	pending.ackSet = true
	delete(env.desiredState, "worker")
	env.cfg.Agents = nil
	env.clk.Time = env.clk.Now().Add(2 * time.Second)
	sentinel := errors.New("native ACK frame unavailable")
	env.sp.GetMetaErrors["worker"] = map[string]error{drainAckFrameKey: sentinel}
	seat, err := env.store.Get(seat.ID)
	if err != nil {
		t.Fatal(err)
	}
	reconcileS1StopCompletion(t, env, []beads.Bead{seat}, map[string]int{}, newDrainOps(env.sp))
	if !env.sp.IsRunning("worker") || env.dt.get(seat.ID) != pending {
		t.Fatalf("unreadable ACK stopped runtime or discarded drain: running=%v drain=%+v", env.sp.IsRunning("worker"), env.dt.get(seat.ID))
	}
	if !strings.Contains(env.stderr.String(), sentinel.Error()) {
		t.Fatalf("ACK fault not diagnosed: %s", env.stderr.String())
	}
	_, cell, enrolled, err := front.ExecutionEvidence(seat.ID)
	if err != nil || !enrolled || cell.Drain == nil || cell.Drain.Attempt != pending.admissionAttempt || cell.Drain.Accepted {
		t.Fatalf("native attempt changed under unreadable ACK: cell=%+v enrolled=%v err=%v", cell, enrolled, err)
	}
	delete(env.sp.GetMetaErrors["worker"], drainAckFrameKey)
	seat, err = env.store.Get(seat.ID)
	if err != nil {
		t.Fatal(err)
	}
	reconcileS1StopCompletion(t, env, []beads.Bead{seat}, map[string]int{}, newDrainOps(env.sp))
	if env.sp.IsRunning("worker") {
		t.Fatalf("healthy next tick failed to stop: %s", env.stderr.String())
	}
}

func TestS1RetainedWorkDoesNotMaskJoinedCensusFailure(t *testing.T) {
	fixture := newS1FileAdmissionFixture(t, "in_progress", "worker-1")
	held := &startedClaimHeldError{Claim: fixture.work}
	censusErr := errors.New("secondary census store unavailable")
	var stdout, stderr bytes.Buffer
	recorded := false
	opts := hookClaimOptions{Assignee: "worker-1", SessionID: fixture.seat.ID, StartedFence: &fixture.fence, JSON: true, DrainAck: true}
	ops := hookClaimOps{
		DrainAck:           func(io.Writer) error { return errors.Join(held, censusErr) },
		RecordStartedClaim: func(string, session.ExecutionFence, session.WorkStartedEntry) error { recorded = true; return nil },
		StampWorkMeta:      func(context.Context, string, []string, string, string, map[string]string) error { return nil },
		StampSessionClaim:  func(string, string) error { return nil },
		ReadWorkMeta: func(_ context.Context, _ string, _ []string, id, _ string) (beads.Bead, error) {
			return fixture.store.Get(id)
		},
		ResolveWorkBranch:     func(hookClaimWorkTree) string { return "" },
		ResolveSessionWorkDir: func(string) string { return "" },
	}
	ops.applyDefaults()
	code := writeHookClaimNoWork(opts, ops, false, fixture.city, &stdout, &stderr)
	if code == 0 || recorded || bytes.Contains(stdout.Bytes(), []byte(`"action":"work"`)) || bytes.Contains(stdout.Bytes(), []byte(`"drain_acknowledged":true`)) || !strings.Contains(stderr.String(), censusErr.Error()) {
		t.Fatalf("joined census failure bypassed: code=%d recorded=%v stdout=%s stderr=%s", code, recorded, stdout.String(), stderr.String())
	}
}
