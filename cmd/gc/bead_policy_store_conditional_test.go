package main

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/rollout/gate"
)

type metadataPatchCapableStore struct{ beads.Store }

func (s *metadataPatchCapableStore) CompareAndSetMetadataPatch(string, beads.Bead, map[string]string) (bool, error) {
	return true, nil
}

func TestBeadPolicyStoreExposesMetadataPatchCapability(t *testing.T) {
	backing := &metadataPatchCapableStore{Store: beads.NewMemStore()}
	wrapped := wrapStoreWithBeadPolicies(backing, nil)
	writer, ok := beads.MetadataPatchCASWriterFor(wrapped)
	if !ok || writer != backing {
		t.Fatalf("MetadataPatchCASWriterFor(policy wrapper) = (%T, %v), want backing capability", writer, ok)
	}
}

func TestCachingStoreDoesNotInventMetadataPatchCapability(t *testing.T) {
	file, err := beads.OpenFileStore(fsys.OSFS{}, filepath.Join(t.TempDir(), "beads.json"))
	if err != nil {
		t.Fatal(err)
	}
	for name, backing := range map[string]beads.Store{"mem": beads.NewMemStore(), "file": file} {
		cache := beads.NewCachingStoreForTest(backing, nil)
		if writer, ok := beads.MetadataPatchCASWriterFor(cache); ok || writer != nil {
			t.Fatalf("MetadataPatchCASWriterFor(cache over %s) = (%T, %v), want unavailable", name, writer, ok)
		}
	}
}

// TestBeadPolicyStoreResolvesConditionalWritesThroughWrapper pins the stage-3
// wiring hazard: every factory store is policy-wrapped, and interface
// embedding hides the factory's conditional-writes stamp — without the
// wrapper's declared resolution target, a require deployment would silently
// resolve unset→legacy through the wrapper on every consumer.
func TestBeadPolicyStoreResolvesConditionalWritesThroughWrapper(t *testing.T) {
	result, err := beads.OpenStoreAtForCity(context.Background(), beads.StoreOpenOptions{
		ScopeRoot:         t.TempDir(),
		Provider:          "file",
		ConditionalWrites: gate.Require,
		OpenFileStore:     func() (beads.Store, error) { return beads.NewMemStore(), nil },
	})
	if err != nil {
		t.Fatalf("OpenStoreAtForCity: %v", err)
	}

	wrapped := wrapStoreWithBeadPolicies(result.Store, nil)
	if _, _, ok := unwrapBeadPolicyStore(wrapped); !ok {
		t.Fatalf("test premise: store %T is not policy-wrapped", wrapped)
	}

	writer, diag, resolveErr := beads.ResolveConditionalWriter(wrapped)
	if resolveErr != nil || diag != nil {
		t.Fatalf("resolve through policy wrapper = diag %v err %v, want the stamped store's writer", diag, resolveErr)
	}
	if writer == nil {
		t.Fatal("resolve through policy wrapper returned no writer: the require stamp was hidden by interface embedding")
	}
}
