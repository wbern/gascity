package config

import (
	"testing"
	"time"
)

func TestAdmissionConfigDefaults(t *testing.T) {
	var empty AdmissionConfig
	if d := empty.TimeoutDuration(); d != DefaultAdmissionTimeout {
		t.Errorf("TimeoutDuration() = %v, want %v", d, DefaultAdmissionTimeout)
	}
	if d := empty.IntervalDuration(); d != DefaultAdmissionInterval {
		t.Errorf("IntervalDuration() = %v, want %v", d, DefaultAdmissionInterval)
	}
	if m := empty.OnErrorMode(); m != AdmissionOnErrorAllow {
		t.Errorf("OnErrorMode() = %q, want %q", m, AdmissionOnErrorAllow)
	}

	custom := AdmissionConfig{
		Timeout:  "5s",
		Interval: "1m",
		OnError:  "deny",
	}
	if d := custom.TimeoutDuration(); d != 5*time.Second {
		t.Errorf("TimeoutDuration() = %v, want 5s", d)
	}
	if d := custom.IntervalDuration(); d != 1*time.Minute {
		t.Errorf("IntervalDuration() = %v, want 1m", d)
	}
	if m := custom.OnErrorMode(); m != AdmissionOnErrorDeny {
		t.Errorf("OnErrorMode() = %q, want deny", m)
	}

	invalid := AdmissionConfig{
		Timeout:  "bad-timeout",
		Interval: "-10s",
		OnError:  "something-else",
	}
	if d := invalid.TimeoutDuration(); d != DefaultAdmissionTimeout {
		t.Errorf("TimeoutDuration(invalid) = %v, want default %v", d, DefaultAdmissionTimeout)
	}
	if d := invalid.IntervalDuration(); d != DefaultAdmissionInterval {
		t.Errorf("IntervalDuration(invalid) = %v, want default %v", d, DefaultAdmissionInterval)
	}
	if m := invalid.OnErrorMode(); m != AdmissionOnErrorAllow {
		t.Errorf("OnErrorMode(invalid) = %q, want fallback allow", m)
	}
}

func TestAdmissionConfigValidate(t *testing.T) {
	tests := []struct {
		name    string
		cfg     *AdmissionConfig
		wantErr bool
	}{
		{"nil", nil, false},
		{"empty", &AdmissionConfig{}, false},
		{"allow", &AdmissionConfig{OnError: "allow"}, false},
		{"deny", &AdmissionConfig{OnError: "deny"}, false},
		{"ALLOW_case_insensitive", &AdmissionConfig{OnError: "ALLOW"}, false},
		{"invalid", &AdmissionConfig{OnError: "halt"}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.cfg.Validate("test")
			if (err != nil) != tc.wantErr {
				t.Errorf("Validate() error = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}

func TestAgentEffectiveAdmission(t *testing.T) {
	cityAdmission := AdmissionConfig{
		Check:    "/path/to/host-pressure-check.sh",
		Timeout:  "15s",
		Interval: "45s",
		OnError:  "allow",
	}

	// Case 1: Agent has no admission block -> inherits city settings
	agent1 := &Agent{Name: "worker1"}
	eff1, enabled1 := agent1.EffectiveAdmission(cityAdmission)
	if !enabled1 {
		t.Errorf("agent1: expected admission to be enabled")
	}
	if eff1.Check != cityAdmission.Check || eff1.Timeout != "15s" || eff1.Interval != "45s" || eff1.OnError != "allow" {
		t.Errorf("agent1: eff = %+v, want %+v", eff1, cityAdmission)
	}

	// Case 2: Agent has partial override (overrides check and on_error)
	agent2 := &Agent{
		Name: "worker2",
		Admission: &AdmissionConfig{
			Check:   "/custom/agent-check.sh",
			OnError: "deny",
		},
	}
	eff2, enabled2 := agent2.EffectiveAdmission(cityAdmission)
	if !enabled2 {
		t.Errorf("agent2: expected admission to be enabled")
	}
	if eff2.Check != "/custom/agent-check.sh" {
		t.Errorf("agent2 Check = %q, want /custom/agent-check.sh", eff2.Check)
	}
	if eff2.Timeout != "15s" { // inherited from city
		t.Errorf("agent2 Timeout = %q, want 15s", eff2.Timeout)
	}
	if eff2.Interval != "45s" { // inherited from city
		t.Errorf("agent2 Interval = %q, want 45s", eff2.Interval)
	}
	if eff2.OnError != "deny" {
		t.Errorf("agent2 OnError = %q, want deny", eff2.OnError)
	}

	// Case 3: Agent disables admission explicitly via check="off"
	agent3 := &Agent{
		Name: "worker3",
		Admission: &AdmissionConfig{
			Check: "off",
		},
	}
	_, enabled3 := agent3.EffectiveAdmission(cityAdmission)
	if enabled3 {
		t.Errorf("agent3: expected check='off' to disable admission")
	}

	// Case 4: City admission has empty check, agent has no override -> disabled
	emptyCityAdmission := AdmissionConfig{}
	agent4 := &Agent{Name: "worker4"}
	_, enabled4 := agent4.EffectiveAdmission(emptyCityAdmission)
	if enabled4 {
		t.Errorf("agent4: expected admission to be disabled when check is empty")
	}

	// Case 5: City admission has empty check, agent explicitly sets check -> enabled
	agent5 := &Agent{
		Name: "worker5",
		Admission: &AdmissionConfig{
			Check: "/agent/only/check.sh",
		},
	}
	eff5, enabled5 := agent5.EffectiveAdmission(emptyCityAdmission)
	if !enabled5 {
		t.Errorf("agent5: expected admission to be enabled via agent override")
	}
	if eff5.Check != "/agent/only/check.sh" {
		t.Errorf("agent5: Check = %q, want /agent/only/check.sh", eff5.Check)
	}
}
