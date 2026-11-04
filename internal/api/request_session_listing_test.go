package api

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
)

type listingCostProvider struct {
	*runtime.Fake
	lists            []string
	probes, metadata int
	complete         bool
	listErr          error
	afterList        func()
}

func (p *listingCostProvider) ListRunning(prefix string) ([]string, error) {
	p.lists = append(p.lists, prefix)
	if prefix == "" && p.listErr != nil {
		return nil, p.listErr
	}
	names, err := p.Fake.ListRunning(prefix)
	if p.afterList != nil {
		p.afterList()
	}
	return names, err
}

func (p *listingCostProvider) ListRunningComplete() bool { return p.complete }

func (p *listingCostProvider) ObserveLiveness(name string, _ []string) runtime.Liveness {
	p.probes++
	running := p.IsRunning(name)
	return runtime.Liveness{Running: running, Alive: running}
}

func (p *listingCostProvider) GetMeta(name, key string) (string, error) {
	p.metadata++
	return p.Fake.GetMeta(name, key)
}

type listingCostState struct {
	*fakeState
	provider runtime.Provider
}

func (s listingCostState) SessionProvider() runtime.Provider { return s.provider }

func TestRigListSharesListingAndSkipsProvenAbsent(t *testing.T) {
	f := newFakeState(t)
	f.cfg.Agents = []config.Agent{
		{Name: "one", Dir: "myrig", MaxActiveSessions: intPtr(-1)},
		{Name: "two", Dir: "myrig", MaxActiveSessions: intPtr(-1)},
		{Name: "stopped", Dir: "myrig", MaxActiveSessions: intPtr(1)},
	}
	p := &listingCostProvider{Fake: f.sp, complete: true}
	for _, name := range []string{"myrig--one-1", "myrig--two-1"} {
		if err := p.Start(context.Background(), name, runtime.Config{}); err != nil {
			t.Fatal(err)
		}
	}
	s := New(listingCostState{fakeState: f, provider: p})
	out, err := s.humaHandleRigList(context.Background(), &RigListInput{})
	if err != nil {
		t.Fatal(err)
	}
	if out.Body.Total != 1 || out.Body.Items[0].AgentCount != 3 || out.Body.Items[0].RunningCount != 2 {
		t.Fatalf("rig rows = %+v", out.Body)
	}
	if !reflect.DeepEqual(p.lists, []string{""}) {
		t.Fatalf("list calls = %q, want one full listing", p.lists)
	}
	if p.probes != 3 {
		t.Fatalf("probes = %d, want two live rows plus first suspension check", p.probes)
	}
}

func TestRigListDoesNotInferAbsenceFromUnattestedOrFailedListing(t *testing.T) {
	for _, tc := range []struct {
		name     string
		complete bool
		err      error
	}{
		{name: "unattested"},
		{name: "failed", complete: true, err: errors.New("partial listing")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeState(t)
			p := &listingCostProvider{Fake: f.sp, complete: tc.complete, listErr: tc.err}
			s := New(listingCostState{fakeState: f, provider: p})
			if _, err := s.humaHandleRigList(context.Background(), &RigListInput{}); err != nil {
				t.Fatal(err)
			}
			if p.probes == 0 {
				t.Fatal("unproved absence skipped liveness probe")
			}
		})
	}
}

func TestStoppedProviderObservationDoesNotReadMetadata(t *testing.T) {
	p := &listingCostProvider{Fake: runtime.NewFake()}
	obs := observeProviderSession(p, "stopped", nil)
	if obs.Running || obs.Suspended || obs.RuntimeSessionID != "" {
		t.Fatalf("observation = %+v", obs)
	}
	if p.metadata != 0 {
		t.Fatalf("metadata calls = %d, want none for stopped session", p.metadata)
	}
}

func TestRequestListingAttestationMustCoverRead(t *testing.T) {
	for _, tc := range []struct {
		name                  string
		before, after, absent bool
	}{
		{"complete", true, true, true},
		{"lost during read", true, false, false},
		{"gained during read", false, true, false},
		{"unattested", false, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := &listingCostProvider{Fake: runtime.NewFake(), complete: tc.before}
			p.afterList = func() { p.complete = tc.after }
			listing := newRequestSessionListing(p)
			if _, err := listing.ListRunning(""); err != nil {
				t.Fatal(err)
			}
			if got := listedAbsent(listing, "missing"); got != tc.absent {
				t.Fatalf("proved absence = %v, want %v", got, tc.absent)
			}
		})
	}
}

func TestRequestListingFallsBackPerPrefixOnPartialFullRead(t *testing.T) {
	p := &listingCostProvider{Fake: runtime.NewFake(), complete: true, listErr: errors.New("partial full listing")}
	for _, name := range []string{"a-1", "b-1"} {
		if err := p.Start(context.Background(), name, runtime.Config{}); err != nil {
			t.Fatal(err)
		}
	}
	listing := newRequestSessionListing(p)
	for _, prefix := range []string{"a-", "b-"} {
		got, err := listing.ListRunning(prefix)
		if err != nil || !reflect.DeepEqual(got, []string{prefix + "1"}) {
			t.Fatalf("ListRunning(%q) = %v, %v", prefix, got, err)
		}
	}
	if !reflect.DeepEqual(p.lists, []string{"", "a-", "b-"}) {
		t.Fatalf("list calls = %q", p.lists)
	}
	if listedAbsent(listing, "missing") {
		t.Fatal("partial full read proved absence")
	}
}
