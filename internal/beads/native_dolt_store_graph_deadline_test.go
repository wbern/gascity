package beads

import (
	"testing"
	"time"
)

func TestNativeGraphApplyDeadlineScalesWithPlanSize(t *testing.T) {
	if got := nativeGraphApplyDeadline(nil); got != bdCommandTimeout {
		t.Fatalf("nil plan deadline = %v, want %v", got, bdCommandTimeout)
	}

	small := &GraphApplyPlan{Nodes: make([]GraphApplyNode, 1)}
	if got := nativeGraphApplyDeadline(small); got <= bdCommandTimeout {
		t.Fatalf("small plan deadline = %v, want > %v", got, bdCommandTimeout)
	}

	big := &GraphApplyPlan{
		Nodes: make([]GraphApplyNode, 67),
		Edges: make([]GraphApplyEdge, 100),
	}
	if got, want := nativeGraphApplyDeadline(big), bdCommandTimeout+167*2*time.Second; got != want {
		t.Fatalf("67-node/100-edge plan deadline = %v, want %v", got, want)
	}
}
