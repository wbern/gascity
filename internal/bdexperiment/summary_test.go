package bdexperiment

import (
	"strings"
	"testing"
)

func TestSummarizeRejectsMixedBuilds(t *testing.T) {
	input := strings.NewReader(strings.Join([]string{
		`{"ts":"2026-07-26T00:00:00Z","schema":1,"build":"one","arm":"shim","verb":"list","shape":"list_json","disposition":"controller","exit":0,"stdout_bytes":2,"config_generation":"0","main_ms":2,"dispatcher_ms":1}`,
		`{"ts":"2026-07-26T00:00:01Z","schema":1,"build":"two","arm":"direct","verb":"list","shape":"list_json","disposition":"controller","exit":0,"stdout_bytes":2,"config_generation":"0","main_ms":1,"dispatcher_ms":1}`,
	}, "\n"))
	if _, err := Summarize(input); err == nil {
		t.Fatal("Summarize() accepted mixed builds")
	}
}

func TestSummarizeGroupsArmAndShapeWithPercentiles(t *testing.T) {
	input := strings.NewReader(strings.Join([]string{
		`{"ts":"2026-07-26T00:00:00Z","schema":1,"build":"one","arm":"direct","verb":"list","shape":"list_json","disposition":"controller","exit":0,"stdout_bytes":2,"config_generation":"0","main_ms":1,"dispatcher_ms":3}`,
		`{"ts":"2026-07-26T00:00:01Z","schema":1,"build":"one","arm":"direct","verb":"list","shape":"list_json","disposition":"controller","exit":1,"stdout_bytes":0,"config_generation":"0","main_ms":9,"dispatcher_ms":7}`,
	}, "\n"))
	summary, err := Summarize(input)
	if err != nil {
		t.Fatalf("Summarize(): %v", err)
	}
	if len(summary.Groups) != 1 {
		t.Fatalf("groups = %d, want 1", len(summary.Groups))
	}
	group := summary.Groups[0]
	if group.Count != 2 || group.SuccessRate != .5 || group.P50MainMS != 1 || group.P95DispatcherMS != 3 {
		t.Fatalf("group = %+v", group)
	}
}
