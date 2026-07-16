package session

import (
	"context"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/runtime"
)

// Tests for Fix A: alias-derived session_name.
//
// When a session is created (started or bead-only) with a non-empty Alias and
// an empty ExplicitName, the resulting session_name must be
// SanitizeQualifiedNameForSession(alias), not "s-<beadID>". An empty alias
// still falls back to the s-<id> form. A non-empty ExplicitName always wins
// over alias derivation.

func TestCreateAliasedNamed_DerivesSessionNameFromAlias(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	mgr := NewManagerWithOptions(store, sp)

	info, err := mgr.CreateSession(context.Background(), CreateOptions{
		Alias:    "kenneth",
		Template: "helper",
		Title:    "my chat",
		Command:  "claude",
		WorkDir:  "/tmp",
		Provider: "claude",
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if info.SessionName != "kenneth" {
		t.Errorf("SessionName = %q, want %q", info.SessionName, "kenneth")
	}
	if !sp.IsRunning("kenneth") {
		t.Error("expected runtime session named \"kenneth\" to be running")
	}
	b, err := store.Get(info.ID)
	if err != nil {
		t.Fatalf("store.Get: %v", err)
	}
	if b.Metadata["session_name"] != "kenneth" {
		t.Errorf("bead session_name = %q, want %q", b.Metadata["session_name"], "kenneth")
	}
}

func TestCreateAliasedNamed_QualifiedAliasGetsDoubleDash(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	mgr := NewManagerWithOptions(store, sp)

	info, err := mgr.CreateSession(context.Background(), CreateOptions{
		Alias:    "gas-city-infra/devops",
		Template: "helper",
		Title:    "devops chat",
		Command:  "claude",
		WorkDir:  "/tmp",
		Provider: "claude",
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	const want = "gas-city-infra--devops"
	if info.SessionName != want {
		t.Errorf("SessionName = %q, want %q", info.SessionName, want)
	}
	if !sp.IsRunning(want) {
		t.Errorf("expected runtime session named %q to be running", want)
	}
}

func TestCreateAliasedNamed_EmptyAliasFallsBackToBeadID(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	mgr := NewManagerWithOptions(store, sp)

	info, err := mgr.CreateSession(context.Background(), CreateOptions{
		Template: "helper",
		Title:    "no alias",
		Command:  "claude",
		WorkDir:  "/tmp",
		Provider: "claude",
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if !strings.HasPrefix(info.SessionName, "s-") {
		t.Errorf("SessionName = %q, want prefix %q", info.SessionName, "s-")
	}
}

func TestCreateAliasedNamed_ExplicitNameTakesPrecedenceOverAlias(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	mgr := NewManagerWithOptions(store, sp)

	info, err := mgr.CreateSession(context.Background(), CreateOptions{
		Alias:        "kenneth",
		ExplicitName: "explicit-sky",
		Template:     "helper",
		Title:        "my chat",
		Command:      "claude",
		WorkDir:      "/tmp",
		Provider:     "claude",
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if info.SessionName != "explicit-sky" {
		t.Errorf("SessionName = %q, want %q", info.SessionName, "explicit-sky")
	}
}

func TestCreateAliasedBeadOnlyNamed_DerivesSessionNameFromAlias(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	mgr := NewManagerWithOptions(store, sp)

	info, err := mgr.CreateSession(context.Background(), CreateOptions{
		BeadOnly: true,
		Alias:    "kenneth",
		Template: "helper",
		Title:    "my chat",
		Command:  "claude",
		WorkDir:  "/tmp",
		Provider: "claude",
	})
	if err != nil {
		t.Fatalf("CreateSession(BeadOnly): %v", err)
	}
	b, err := store.Get(info.ID)
	if err != nil {
		t.Fatalf("store.Get: %v", err)
	}
	if b.Metadata["session_name"] != "kenneth" {
		t.Errorf("bead session_name = %q, want %q", b.Metadata["session_name"], "kenneth")
	}
	if info.SessionName != "kenneth" {
		t.Errorf("info.SessionName = %q, want %q", info.SessionName, "kenneth")
	}
}

func TestCreateAliasedBeadOnlyNamed_QualifiedAliasGetsDoubleDash(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	mgr := NewManagerWithOptions(store, sp)

	info, err := mgr.CreateSession(context.Background(), CreateOptions{
		BeadOnly: true,
		Alias:    "gas-city-infra/devops",
		Template: "helper",
		Title:    "devops",
		Command:  "claude",
		WorkDir:  "/tmp",
		Provider: "claude",
	})
	if err != nil {
		t.Fatalf("CreateSession(BeadOnly): %v", err)
	}
	b, err := store.Get(info.ID)
	if err != nil {
		t.Fatalf("store.Get: %v", err)
	}
	const want = "gas-city-infra--devops"
	if b.Metadata["session_name"] != want {
		t.Errorf("bead session_name = %q, want %q", b.Metadata["session_name"], want)
	}
	if info.SessionName != want {
		t.Errorf("info.SessionName = %q, want %q", info.SessionName, want)
	}
}

func TestCreateAliasedBeadOnlyNamed_EmptyAliasFallsBackToBeadID(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	mgr := NewManagerWithOptions(store, sp)

	info, err := mgr.CreateSession(context.Background(), CreateOptions{
		BeadOnly: true,
		Template: "helper",
		Title:    "no alias",
		Command:  "claude",
		WorkDir:  "/tmp",
		Provider: "claude",
	})
	if err != nil {
		t.Fatalf("CreateSession(BeadOnly): %v", err)
	}
	b, err := store.Get(info.ID)
	if err != nil {
		t.Fatalf("store.Get: %v", err)
	}
	if !strings.HasPrefix(b.Metadata["session_name"], "s-") {
		t.Errorf("bead session_name = %q, want prefix %q", b.Metadata["session_name"], "s-")
	}
}
