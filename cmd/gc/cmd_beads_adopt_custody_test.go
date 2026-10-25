package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/worker"
)

func TestCustodyCommandRegistered(t *testing.T) {
	root := newBeadsCmd(io.Discard, io.Discard)
	command, _, err := root.Find([]string{"adopt-custody"})
	if err != nil || command.Name() != "adopt-custody" {
		t.Fatalf("exact-source custody command absent: command=%s error=%v", command.Name(), err)
	}
}

func custodyFixture(t *testing.T) (*beads.MemStore, custodyOperation, custodyChecks) {
	t.Helper()
	city := custodyTestDir(t)
	now := time.Date(2026, 10, 25, 3, 0, 0, 0, time.UTC)
	empty := ""
	o := custodyOperation{Schema: "existing-source-custody.v1", Purpose: "adopt-existing-source-custody", City: city, StoreRef: "rig:product", Source: "work-1", Status: "open", Assignee: &empty, PriorSession: "session-old", PriorOwner: "original-instance", ClaimedAt: "2026-10-23T07:45:48Z", Branch: "existing/branch", Base: strings.Repeat("1", 40), Head: strings.Repeat("2", 40), Workspace: filepath.Join(city, "tasks", "work-1"), Successor: custodySession{ID: "session-new", Name: "runtime-new", Agent: "session-new", Claim: "session-new"}, Dependencies: []beads.Dep{}, Admission: custodyAdmission{Custodian: "operator-evidence", PredecessorDead: true, SoleWriter: true, Scheduling: "custody-only-no-entry", NotBefore: now.Add(-time.Minute), ExpiresAt: now.Add(time.Minute)}}
	s := beads.NewMemStore()
	s.HonorExplicitIDs = true
	b, e := s.Create(beads.Bead{ID: o.Source, Title: "Existing source", Status: "open", Labels: []string{"hold:external"}, Metadata: beads.StringMap{"gc.session_id": o.PriorSession, "gc.workspace_owner": o.PriorOwner, "gc.claimed_at": o.ClaimedAt, "branch": o.Branch, "gc.work_branch": o.Branch, "gc.base_sha": o.Base, "gc.task_worktree": o.Workspace, "gc.workspace_mode": "task", "preserve": "yes"}})
	if e != nil {
		t.Fatal(e)
	}
	o.Revision = b.Revision
	c := custodyChecks{now: func() time.Time { return now }, physical: func(custodyOperation) error { return nil }, runtime: func(custodyOperation) error { return nil }}
	return s, o, c
}

func TestCustodyExactTransitionPreservesHold(t *testing.T) {
	s, o, c := custodyFixture(t)
	_, e := s.Create(beads.Bead{ID: "prerequisite", Title: "Still blocked", Status: "in_progress"})
	if e != nil {
		t.Fatal(e)
	}
	if e = s.DepAdd(o.Source, "prerequisite", "blocks"); e != nil {
		t.Fatal(e)
	}
	o.Dependencies, e = s.DepList(o.Source, "down")
	if e != nil {
		t.Fatal(e)
	}
	b, _ := s.Get(o.Source)
	o.Revision = b.Revision
	r, e := applyCustodyOperation(s, o, "signed-hash", c)
	if e != nil {
		t.Fatal(e)
	}
	if !r.Committed || r.WorkerEntry || r.Outcome != "custody-recorded-no-entry" {
		t.Fatalf("bad result %+v", r)
	}
	after, _ := s.Get(o.Source)
	if after.Metadata["gc.claimed_at"] != o.ClaimedAt || after.Metadata["preserve"] != "yes" || after.Assignee != o.Successor.Claim || after.Metadata["gc.workspace_owner"] != o.Successor.Agent || !reflect.DeepEqual(after.Labels, []string{"hold:external"}) {
		t.Fatalf("custody or preservation failed %+v", after)
	}
	deps, _ := s.DepList(o.Source, "down")
	if custodyDeps(deps) != custodyDeps(o.Dependencies) {
		t.Fatal("prerequisite changed")
	}
	r, e = applyCustodyOperation(s, o, "signed-hash", c)
	if e == nil || r.Committed {
		t.Fatal("replay admitted")
	}
}

func TestCustodyRefusesPrewriteAndPostwriteDrift(t *testing.T) {
	for _, phase := range []int{1, 2, 3} {
		t.Run(fmt.Sprint(phase), func(t *testing.T) {
			s, o, c := custodyFixture(t)
			calls := 0
			c.physical = func(custodyOperation) error {
				calls++
				if calls == phase {
					return errors.New("physical drift")
				}
				return nil
			}
			r, e := applyCustodyOperation(s, o, "hash", c)
			if e == nil || !strings.Contains(e.Error(), "physical drift") || r.WorkerEntry {
				t.Fatalf("drift not refused %+v %v", r, e)
			}
			if r.Committed != (phase == 3) {
				t.Fatalf("wrong commit outcome %+v", r)
			}
			b, _ := s.Get(o.Source)
			if phase == 3 {
				if b.Assignee != o.Successor.Claim || r.Outcome != "committed-unadmitted" {
					t.Fatal("postwrite auto rollback/entry")
				}
			} else if b.Assignee != "" {
				t.Fatal("prewrite mutated")
			}
		})
	}
}

func TestCustodyMandatoryBindingNegatives(t *testing.T) {
	cases := map[string]func(*custodyOperation){"purpose": func(o *custodyOperation) { o.Purpose = "source-implementation" }, "city": func(o *custodyOperation) { o.City = "/elsewhere" }, "revision": func(o *custodyOperation) { o.Revision++ }, "owner": func(o *custodyOperation) { o.PriorOwner = "forged" }, "session": func(o *custodyOperation) { o.PriorSession = "forged" }, "head prefix": func(o *custodyOperation) { o.Head = o.Head[:8] }, "base": func(o *custodyOperation) { o.Base = strings.Repeat("3", 40) }, "workspace": func(o *custodyOperation) { o.Workspace = "/other" }, "branch": func(o *custodyOperation) { o.Branch = "other" }, "claim": func(o *custodyOperation) { o.ClaimedAt = "2026-10-24T00:00:00Z" }, "missing assignee": func(o *custodyOperation) { o.Assignee = nil }, "not sole": func(o *custodyOperation) { o.Admission.SoleWriter = false }, "predecessor live": func(o *custodyOperation) { o.Admission.PredecessorDead = false }, "missing admission": func(o *custodyOperation) { o.Admission.Custodian = "" }, "entry allowed": func(o *custodyOperation) { o.Admission.Scheduling = "start-now" }, "expired": func(o *custodyOperation) { o.Admission.ExpiresAt = o.Admission.NotBefore }, "dependencies missing": func(o *custodyOperation) { o.Dependencies = nil }}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			s, o, c := custodyFixture(t)
			mutate(&o)
			if name == "city" {
				c.runtime = func(custodyOperation) error { return errors.New("wrong target runtime") }
			}
			r, e := applyCustodyOperation(s, o, "hash", c)
			if e == nil || r.Committed {
				t.Fatalf("invalid binding accepted %+v", r)
			}
		})
	}
}

func TestCustodyRuntimeAndExpiryRechecked(t *testing.T) {
	for _, post := range []bool{false, true} {
		t.Run(fmt.Sprint(post), func(t *testing.T) {
			s, o, c := custodyFixture(t)
			calls := 0
			c.runtime = func(custodyOperation) error {
				calls++
				if (!post && calls == 2) || (post && calls == 3) {
					return errors.New("successor gone")
				}
				return nil
			}
			r, e := applyCustodyOperation(s, o, "hash", c)
			if e == nil || r.Committed != post || r.WorkerEntry {
				t.Fatalf("runtime drift ignored %+v %v", r, e)
			}
		})
	}
	s, o, c := custodyFixture(t)
	calls := 0
	now := c.now()
	c.now = func() time.Time {
		calls++
		if calls >= 3 {
			return o.Admission.ExpiresAt
		}
		return now
	}
	r, e := applyCustodyOperation(s, o, "hash", c)
	if e == nil || r.WorkerEntry || r.Committed {
		t.Fatalf("expiry before CAS must not write %+v %v", r, e)
	}
}

func TestCustodyDuplicateJSONRefused(t *testing.T) {
	for _, data := range []string{`{"custody_operation":{"source_id":"a","source_id":"b"}}`, `{} {}`, `{"approval":{},"approval":{}}`} {
		var v any
		if custodyJSON([]byte(data), &v) == nil {
			t.Fatal("ambiguous JSON accepted", data)
		}
	}
}

type custodyRaceStore struct {
	*beads.MemStore
	beforeCAS func()
	afterCAS  func()
}

func (s *custodyRaceStore) UpdateIfMatch(id string, rev int64, o beads.UpdateOpts) error {
	if s.beforeCAS != nil {
		s.beforeCAS()
	}
	err := s.MemStore.UpdateIfMatch(id, rev, o)
	if err == nil && s.afterCAS != nil {
		s.afterCAS()
	}
	return err
}

func TestCustodyNativeCASRace(t *testing.T) {
	s, o, c := custodyFixture(t)
	race := &custodyRaceStore{MemStore: s}
	race.beforeCAS = func() {
		note := "concurrent change"
		if e := s.Update(o.Source, beads.UpdateOpts{Description: &note}); e != nil {
			t.Fatal(e)
		}
	}
	r, e := applyCustodyOperation(race, o, "hash", c)
	if e == nil || !beads.IsPreconditionFailed(e) || r.Committed {
		t.Fatalf("lost revision race admitted %+v %v", r, e)
	}
	b, _ := s.Get(o.Source)
	if b.Assignee != "" {
		t.Fatal("race lost but custody changed")
	}
}

func TestCustodyPostwriteNativeDrift(t *testing.T) {
	s, o, c := custodyFixture(t)
	race := &custodyRaceStore{MemStore: s}
	race.afterCAS = func() {
		if e := s.Update(o.Source, beads.UpdateOpts{Metadata: map[string]string{"gc.claimed_at": "forged"}}); e != nil {
			t.Fatal(e)
		}
	}
	r, e := applyCustodyOperation(race, o, "hash", c)
	if e == nil || !strings.Contains(e.Error(), "postcommit native readback differs") || !r.Committed || r.WorkerEntry || r.Outcome != "committed-unadmitted" {
		t.Fatalf("native drift not fenced %+v %v", r, e)
	}
}

func TestCustodyDependencyDrift(t *testing.T) {
	s, o, c := custodyFixture(t)
	_, e := s.Create(beads.Bead{ID: "new-prerequisite", Title: "external"})
	if e != nil {
		t.Fatal(e)
	}
	calls := 0
	c.physical = func(custodyOperation) error {
		calls++
		if calls == 2 {
			if e := s.DepAdd(o.Source, "new-prerequisite", "blocks"); e != nil {
				t.Fatal(e)
			}
		}
		return nil
	}
	r, e := applyCustodyOperation(s, o, "hash", c)
	if e == nil || !strings.Contains(e.Error(), "dependency snapshot differs") || r.Committed {
		t.Fatalf("new dependency ignored %+v %v", r, e)
	}
}

func TestCustodySourceGrantCannotAdopt(t *testing.T) {
	for _, key := range []string{"source_only", "no_adoption_execution", "no_runtime_apply"} {
		t.Run(key, func(t *testing.T) {
			p := filepath.Join(custodyTestDir(t), "source.json")
			data := `{"approval":{"mode":"direct_chat_preapproval"},"execution_constraints":{"custody_adoption_authorized":true,"` + key + `":true}}`
			if e := os.WriteFile(p, []byte(data), 0o600); e != nil {
				t.Fatal(e)
			}
			_, _, e := verifyCustodyPacket("/city", p, "/absent-verifier")
			if e == nil || !strings.Contains(e.Error(), "source-only authority cannot adopt custody") {
				t.Fatalf("source grant not rejected before verifier: %v", e)
			}
		})
	}
}

func TestCustodyMalformedInputs(t *testing.T) {
	p := filepath.Join(custodyTestDir(t), "manifest.json")
	if e := os.WriteFile(p, []byte(`{"approval":{},"approval":{}}`), 0o600); e != nil {
		t.Fatal(e)
	}
	_, _, e := verifyCustodyPacket("/city", p, "/absent-verifier")
	if e == nil || !strings.Contains(e.Error(), "duplicate JSON") {
		t.Fatal(e)
	}
	if _, e = readCustodyFile(p, 2); e == nil {
		t.Fatal("size bound ignored")
	}
}

func TestCustodyRegisteredRuntimeQualification(t *testing.T) {
	_, o, _ := custodyFixture(t)
	live := func(string) bool { return true }
	good := worker.SessionInfo{ID: o.Successor.ID, SessionName: o.Successor.Name, WorkDir: o.Workspace, State: "active"}
	if e := checkCustodyRuntime([]worker.SessionInfo{good}, live, o); e != nil {
		t.Fatal(e)
	}
	for _, name := range []string{"absent", "asleep", "wrong-name", "wrong-workspace", "closed", "spoofed-claim", "not-running", "predecessor", "competing-writer"} {
		t.Run(name, func(t *testing.T) {
			s := good
			infos := []worker.SessionInfo{s}
			running := live
			switch name {
			case "absent":
				infos = nil
			case "asleep":
				infos[0].State = "asleep"
			case "wrong-name":
				infos[0].SessionName = "other"
			case "wrong-workspace":
				infos[0].WorkDir = "/other"
			case "closed":
				infos[0].Closed = true
			case "spoofed-claim":
				infos[0].Alias = "other"
			case "not-running":
				running = func(string) bool { return false }
			case "predecessor":
				infos = append(infos, worker.SessionInfo{ID: o.PriorSession, SessionName: "old"})
			case "competing-writer":
				infos = append(infos, worker.SessionInfo{ID: "another", SessionName: "another", WorkDir: o.Workspace})
			}
			if checkCustodyRuntime(infos, running, o) == nil {
				t.Fatal("invalid runtime accepted")
			}
		})
	}
}

func TestCustodyTargetCityBinding(t *testing.T) {
	_, o, c := custodyFixture(t)
	o.City = "/different-city"
	if validateCustodyOperation(o, "/city", c.now()) == nil {
		t.Fatal("wrong actual target city admitted")
	}
}

func TestCustodyUntrustedVerifierRefused(t *testing.T) {
	_, o, _ := custodyFixture(t)
	o.Admission.NotBefore = time.Now().Add(-time.Minute)
	o.Admission.ExpiresAt = time.Now().Add(time.Minute)
	dir := custodyTestDir(t)
	packet := filepath.Join(dir, "packet.json")
	verifier := filepath.Join(dir, "helper.py")
	envelope := struct {
		Approval    map[string]string `json:"approval"`
		Constraints map[string]bool   `json:"execution_constraints"`
		Operation   custodyOperation  `json:"custody_operation"`
	}{map[string]string{"mode": "direct_chat_preapproval"}, map[string]bool{"custody_adoption_authorized": true}, o}
	data, e := json.Marshal(envelope)
	if e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(packet, data, 0o600); e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(verifier, []byte("raise Exception('must never execute')"), 0o600); e != nil {
		t.Fatal(e)
	}
	_, _, e = verifyCustodyPacket(o.City, packet, verifier)
	if e == nil || !strings.Contains(e.Error(), "accepted verifier content differs") {
		t.Fatalf("untrusted verifier reached execution: %v", e)
	}
}

func TestCustodyCompetingRuntimeWorkspaceAlias(t *testing.T) {
	_, o, _ := custodyFixture(t)
	dir := custodyTestDir(t)
	actual := filepath.Join(dir, "source")
	if e := os.Mkdir(actual, 0o700); e != nil {
		t.Fatal(e)
	}
	resolved, e := filepath.EvalSymlinks(actual)
	if e != nil {
		t.Fatal(e)
	}
	alias := filepath.Join(dir, "alias")
	if e = os.Symlink(actual, alias); e != nil {
		t.Fatal(e)
	}
	o.Workspace = resolved
	good := worker.SessionInfo{ID: o.Successor.ID, SessionName: o.Successor.Name, WorkDir: resolved, State: "active"}
	other := worker.SessionInfo{ID: "competitor", SessionName: "other", WorkDir: alias, State: "active"}
	if e = checkCustodyRuntime([]worker.SessionInfo{good, other}, func(string) bool { return true }, o); e == nil || !strings.Contains(e.Error(), "another runtime occupies") {
		t.Fatalf("aliased writer not refused: %v", e)
	}
}

func custodyTestDir(t *testing.T) string {
	t.Helper()
	path, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return path
}

func TestCustodyPostcommitExpiryUnadmitted(t *testing.T) {
	s, o, c := custodyFixture(t)
	now := c.now()
	calls := 0
	c.now = func() time.Time {
		calls++
		if calls >= 4 {
			return o.Admission.ExpiresAt
		}
		return now
	}
	r, e := applyCustodyOperation(s, o, "hash", c)
	if e == nil || !r.Committed || r.WorkerEntry || r.Outcome != "committed-unadmitted" {
		t.Fatalf("postcommit expiry admitted or rolled back %+v %v", r, e)
	}
}

func TestCustodyCanonicalRuntimeClaim(t *testing.T) {
	for _, variant := range []string{"alias", "trimmed-alias", "configured-name", "stored-runtime-name", "pool-id"} {
		t.Run(variant, func(t *testing.T) {
			_, o, _ := custodyFixture(t)
			s := worker.SessionInfo{ID: o.Successor.ID, SessionName: o.Successor.Name, WorkDir: o.Workspace, State: "active"}
			claim := ""
			switch variant {
			case "alias":
				s.Alias = "public-instance"
				claim = s.Alias
			case "trimmed-alias":
				s.Alias = " public-instance "
				claim = "public-instance"
			case "configured-name":
				s.ConfiguredNamedIdentity = "configured-instance"
				claim = s.ConfiguredNamedIdentity
			case "stored-runtime-name":
				s.SessionNameMetadata = s.SessionName
				claim = s.SessionName
			case "pool-id":
				s.PoolManaged = true
				claim = s.ID
			}
			o.Successor.Claim = claim
			o.Successor.Agent = claim
			if e := checkCustodyRuntime([]worker.SessionInfo{s}, func(string) bool { return true }, o); e != nil {
				t.Fatalf("canonical runtime claim refused: %v", e)
			}
			if claim != s.ID {
				o.Successor.Claim = s.ID
				o.Successor.Agent = s.ID
				if checkCustodyRuntime([]worker.SessionInfo{s}, func(string) bool { return true }, o) == nil {
					t.Fatal("noncanonical claimant accepted")
				}
			}
		})
	}
}
