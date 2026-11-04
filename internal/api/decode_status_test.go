package api

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/gastownhall/gascity/internal/api/genclient"
)

func TestStatusViewFromGen_NilReturnsZero(t *testing.T) {
	got := statusViewFromGen(nil)
	if got.CityName != "" || got.CityPath != "" {
		t.Fatalf("statusViewFromGen(nil) = %+v, want zero value", got)
	}
}

func TestStatusViewFromGen_ValidResponse(t *testing.T) {
	version := "v0.1.0"
	sessionName := "city--mayor"
	groupName := "mayor"
	scaleLabel := "scaled (min=0, max=3)"
	expanded := true
	preflightGate := "metadata_backend"
	preflightReason := "metadata backend=file; native store requires dolt"
	body := &genclient.StatusBody{
		Name:      "bright-lights",
		Path:      "/home/u/bright-lights",
		Version:   &version,
		UptimeSec: 42,
		Suspended: false,
		Agents:    genclient.StatusAgentCounts{Total: 2, Running: 1},
		Rigs:      genclient.StatusRigCounts{Total: 1, Suspended: 0},
		Work:      genclient.StatusWorkCounts{},
		Mail:      genclient.StatusMailCounts{},
		AgentDetails: &[]genclient.StatusAgentDetail{
			{
				Name:          "mayor",
				QualifiedName: "mayor",
				Scope:         "city",
				Running:       true,
				Suspended:     false,
				SessionName:   &sessionName,
				GroupName:     &groupName,
				ScaleLabel:    &scaleLabel,
				Expanded:      &expanded,
			},
		},
		RigDetails: &[]genclient.StatusRigDetail{
			{Name: "myrig", Path: "/home/u/myrig", Suspended: false},
		},
		NamedSessionDetails: &[]genclient.StatusNamedSessionDetail{
			{Identity: "myrig/worker", Status: "materialized", Mode: "always"},
		},
		SessionCountsDetail: &genclient.StatusSessionCountsDetail{Active: 3, Suspended: 1},
		Beads: &genclient.BeadsDiagnostic{
			BeadsStore:          "BdStore",
			NativeStoreEligible: false,
			PreflightGate:       &preflightGate,
			PreflightReason:     &preflightReason,
		},
	}

	got := statusViewFromGen(body)

	if got.CityName != "bright-lights" {
		t.Errorf("CityName = %q, want %q", got.CityName, "bright-lights")
	}
	if got.CityPath != "/home/u/bright-lights" {
		t.Errorf("CityPath = %q", got.CityPath)
	}
	if got.Version != "v0.1.0" {
		t.Errorf("Version = %q", got.Version)
	}
	if got.UptimeSec != 42 {
		t.Errorf("UptimeSec = %d", got.UptimeSec)
	}
	if got.Summary.TotalAgents != 2 || got.Summary.RunningAgents != 1 {
		t.Errorf("Summary = %+v", got.Summary)
	}
	if len(got.Agents) != 1 || got.Agents[0].QualifiedName != "mayor" {
		t.Errorf("Agents = %+v", got.Agents)
	}
	if got.Agents[0].SessionName != "city--mayor" {
		t.Errorf("agent[0].SessionName = %q", got.Agents[0].SessionName)
	}
	if got.Agents[0].ScaleLabel != "scaled (min=0, max=3)" {
		t.Errorf("agent[0].ScaleLabel = %q", got.Agents[0].ScaleLabel)
	}
	if !got.Agents[0].Expanded {
		t.Errorf("agent[0].Expanded should be true")
	}
	if len(got.Rigs) != 1 || got.Rigs[0].Name != "myrig" {
		t.Errorf("Rigs = %+v", got.Rigs)
	}
	if len(got.NamedSessions) != 1 || got.NamedSessions[0].Status != "materialized" {
		t.Errorf("NamedSessions = %+v", got.NamedSessions)
	}
	if got.SessionCounts.Active != 3 || got.SessionCounts.Suspended != 1 {
		t.Errorf("SessionCounts = %+v", got.SessionCounts)
	}
	if got.Beads == nil {
		t.Fatal("Beads = nil, want diagnostic")
	}
	if got.Beads.Store != "BdStore" {
		t.Errorf("beads_store = %q, want BdStore", got.Beads.Store)
	}
	if got.Beads.PreflightGate != "metadata_backend" || got.Beads.PreflightReason == "" {
		t.Errorf("Beads diagnostic = %+v, want preflight metadata diagnostic", got.Beads)
	}
}

func TestStatusViewFromGen_EmptyLists(t *testing.T) {
	// Body without any detail slices: views should be non-nil empty slices.
	body := &genclient.StatusBody{
		Name:   "empty-city",
		Path:   "/tmp/empty",
		Agents: genclient.StatusAgentCounts{},
		Rigs:   genclient.StatusRigCounts{},
		Work:   genclient.StatusWorkCounts{},
		Mail:   genclient.StatusMailCounts{},
	}
	got := statusViewFromGen(body)
	if got.Agents == nil {
		t.Error("Agents should be non-nil empty slice, got nil")
	}
	if len(got.Agents) != 0 {
		t.Errorf("Agents length = %d, want 0", len(got.Agents))
	}
	if got.Rigs == nil {
		t.Error("Rigs should be non-nil empty slice, got nil")
	}
	if got.NamedSessions == nil {
		t.Error("NamedSessions should be non-nil empty slice, got nil")
	}
}

func TestStatusViewFromGen_PartialMissingField(t *testing.T) {
	// AgentDetails with minimal fields — optional strings unset.
	body := &genclient.StatusBody{
		Name: "city",
		Path: "/tmp",
		AgentDetails: &[]genclient.StatusAgentDetail{
			{
				Name:          "worker",
				QualifiedName: "myrig/worker",
				Scope:         "rig",
				Running:       false,
				Suspended:     true,
			},
		},
	}
	got := statusViewFromGen(body)
	if len(got.Agents) != 1 {
		t.Fatalf("Agents length = %d, want 1", len(got.Agents))
	}
	a := got.Agents[0]
	if a.QualifiedName != "myrig/worker" {
		t.Errorf("QualifiedName = %q", a.QualifiedName)
	}
	if !a.Suspended {
		t.Error("Suspended should be true")
	}
	if a.SessionName != "" || a.GroupName != "" || a.ScaleLabel != "" {
		t.Errorf("optional fields should be empty: SessionName=%q GroupName=%q ScaleLabel=%q", a.SessionName, a.GroupName, a.ScaleLabel)
	}
	if a.Expanded {
		t.Error("Expanded should default to false")
	}
}

func TestStatusViewFromGenPreservesRigJSONFields(t *testing.T) {
	var body genclient.StatusBody
	raw := []byte(`{"agent_details":[{"name":"worker","qualified_name":"alpha/worker","scope":"rig","running":true,"session_name":"alpha--worker","session_id":"sess-123"}],"rig_details":[{"name":"alpha","path":"/work/alpha","prefix":"al","suspended":false,"default_sling_target":"alpha/worker","default_sling_targets":["alpha/worker","beta/worker"]}]}`)
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("decode status body: %v", err)
	}
	view := statusViewFromGen(&body)
	if len(view.Agents) != 1 {
		t.Fatalf("status agent rows = %d, want 1", len(view.Agents))
	}
	assertStringField := func(label string, value any, field, want string) {
		t.Helper()
		got := reflect.ValueOf(value).FieldByName(field)
		if !got.IsValid() {
			t.Fatalf("%s has no %s field", label, field)
		}
		if got.String() != want {
			t.Errorf("%s.%s = %q, want %q", label, field, got.String(), want)
		}
	}
	assertStringField("status agent", view.Agents[0], "SessionID", "sess-123")
	if len(view.Rigs) != 1 {
		t.Fatalf("status rig rows = %d, want 1", len(view.Rigs))
	}
	assertStringField("status rig", view.Rigs[0], "Prefix", "al")
	assertStringField("status rig", view.Rigs[0], "DefaultSlingTarget", "alpha/worker")
	gotTargets := reflect.ValueOf(view.Rigs[0]).FieldByName("DefaultSlingTargets")
	if !gotTargets.IsValid() {
		t.Fatal("status rig has no DefaultSlingTargets field")
	}
	wantTargets := []string{"alpha/worker", "beta/worker"}
	if got, ok := gotTargets.Interface().([]string); !ok || !reflect.DeepEqual(got, wantTargets) {
		t.Errorf("status rig DefaultSlingTargets = %#v, want %#v", gotTargets.Interface(), wantTargets)
	}
}
