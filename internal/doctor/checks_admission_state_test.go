package doctor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAdmissionStateCheck_NoFiles(t *testing.T) {
	cityDir := t.TempDir()
	check := NewAdmissionStateCheck(cityDir)
	res := check.Run(&CheckContext{CityPath: cityDir})
	if res.Status != StatusOK {
		t.Fatalf("expected StatusOK, got %v: %s", res.Status, res.Message)
	}
	if !strings.Contains(res.Message, "no blocked admission gates") {
		t.Errorf("unexpected message: %s", res.Message)
	}
}

func TestAdmissionStateCheck_HealthyFile(t *testing.T) {
	cityDir := t.TempDir()
	runtimeDir := filepath.Join(cityDir, ".gc", "runtime")
	if err := os.MkdirAll(runtimeDir, 0o755); err != nil {
		t.Fatalf("mkdir runtime: %v", err)
	}

	content := `{"blocked": false, "last_reason": "load normal"}`
	if err := os.WriteFile(filepath.Join(runtimeDir, "city-admission.json"), []byte(content), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	check := NewAdmissionStateCheck(cityDir)
	res := check.Run(&CheckContext{CityPath: cityDir})
	if res.Status != StatusOK {
		t.Fatalf("expected StatusOK, got %v: %s", res.Status, res.Message)
	}
}

func TestAdmissionStateCheck_BlockedTopLevel(t *testing.T) {
	cityDir := t.TempDir()
	runtimeDir := filepath.Join(cityDir, ".gc", "runtime")
	if err := os.MkdirAll(runtimeDir, 0o755); err != nil {
		t.Fatalf("mkdir runtime: %v", err)
	}

	content := `{"blocked": true, "last_reason": "load5=15.0_exceeds_12.0"}`
	if err := os.WriteFile(filepath.Join(runtimeDir, "system-admission.json"), []byte(content), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	check := NewAdmissionStateCheck(cityDir)
	res := check.Run(&CheckContext{CityPath: cityDir})
	if res.Status != StatusWarning {
		t.Fatalf("expected StatusWarning, got %v: %s", res.Status, res.Message)
	}
	if !strings.Contains(res.Message, "system") || !strings.Contains(res.Message, "load5=15.0_exceeds_12.0") {
		t.Errorf("message %q does not contain gate name and reason", res.Message)
	}
}

func TestAdmissionStateCheck_BlockedRecoveryPolicy(t *testing.T) {
	cityDir := t.TempDir()
	runtimeDir := filepath.Join(cityDir, ".gc", "runtime")
	if err := os.MkdirAll(runtimeDir, 0o755); err != nil {
		t.Fatalf("mkdir runtime: %v", err)
	}

	content := `{
		"recovery_policies": {
			"memory_pressure": {
				"blocked": true,
				"last_reason": "memory_psi_avg10=1.50_exceeds_1.00"
			}
		}
	}`
	if err := os.WriteFile(filepath.Join(runtimeDir, "worker-pool-admission.json"), []byte(content), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	check := NewAdmissionStateCheck(cityDir)
	res := check.Run(&CheckContext{CityPath: cityDir})
	if res.Status != StatusWarning {
		t.Fatalf("expected StatusWarning, got %v: %s", res.Status, res.Message)
	}
	if !strings.Contains(res.Message, "worker-pool") || !strings.Contains(res.Message, "memory_pressure") || !strings.Contains(res.Message, "memory_psi_avg10=1.50_exceeds_1.00") {
		t.Errorf("message %q does not contain expected details", res.Message)
	}
}
