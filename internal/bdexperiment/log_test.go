package bdexperiment

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAppendWritesOnlyValidatedObservationFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "experiment.jsonl")
	record := Record{
		Schema:           SchemaVersion,
		Build:            "fork-abc123",
		Arm:              ArmDirect,
		Verb:             "list",
		Shape:            ShapeListJSON,
		Disposition:      "controller",
		Exit:             0,
		StdoutBytes:      3,
		ConfigGeneration: "7",
		MainMS:           12,
		DispatcherMS:     4,
	}
	if !Append(path, record) {
		t.Fatal("Append() = false")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	for _, forbidden := range []string{"args", "path", "env", "output", "hash"} {
		if strings.Contains(string(data), `"`+forbidden+`"`) {
			t.Fatalf("log exposed forbidden field %q: %s", forbidden, data)
		}
	}
}
