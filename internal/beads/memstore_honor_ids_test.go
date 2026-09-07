package beads

import (
	"strings"
	"testing"
)

// TestMemStoreDefaultClobbersExplicitID pins the default: every existing caller
// keeps minting over whatever ID it passed, so adding HonorExplicitIDs cannot
// change behavior for a store that does not opt in.
func TestMemStoreDefaultClobbersExplicitID(t *testing.T) {
	m := NewMemStore()

	got, err := m.Create(Bead{ID: "pinned-1", Title: "t"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if got.ID != "gc-1" {
		t.Fatalf("default store honored an explicit id: got %q, want %q", got.ID, "gc-1")
	}
}

// TestMemStoreHonorExplicitIDs is the fork-side pin for the HonorExplicitIDs
// contract. Upstream pins the same behavior against SQLiteStore; this fork has
// no SQLiteStore, so the contract is asserted directly.
func TestMemStoreHonorExplicitIDs(t *testing.T) {
	t.Run("honors a caller-supplied id verbatim", func(t *testing.T) {
		m := NewMemStore()
		m.HonorExplicitIDs = true

		got, err := m.Create(Bead{ID: "gc-wisp-abc", Title: "t"})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		if got.ID != "gc-wisp-abc" {
			t.Fatalf("explicit id not honored: got %q, want %q", got.ID, "gc-wisp-abc")
		}
	})

	t.Run("still mints when the caller supplies no id", func(t *testing.T) {
		m := NewMemStore()
		m.HonorExplicitIDs = true

		got, err := m.Create(Bead{Title: "t"})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		if got.ID != "gc-1" {
			t.Fatalf("empty id did not mint: got %q, want %q", got.ID, "gc-1")
		}
	})

	t.Run("rejects a duplicate id instead of silently renaming", func(t *testing.T) {
		m := NewMemStore()
		m.HonorExplicitIDs = true

		if _, err := m.Create(Bead{ID: "gc-dup", Title: "first"}); err != nil {
			t.Fatalf("first Create: %v", err)
		}
		_, err := m.Create(Bead{ID: "gc-dup", Title: "second"})
		if err == nil {
			t.Fatal("duplicate id was accepted; want a hard error")
		}
		if !strings.Contains(err.Error(), "duplicate id") {
			t.Fatalf("error does not name the cause: %v", err)
		}
		// A rejected Create must not leave a second row behind.
		all, listErr := m.List(ListQuery{AllowScan: true, IncludeClosed: true})
		if listErr != nil {
			t.Fatalf("List: %v", listErr)
		}
		if len(all) != 1 {
			t.Fatalf("rejected Create still stored a bead: got %d beads, want 1", len(all))
		}
	})

	t.Run("a pinned numeric suffix consumes that sequence slot", func(t *testing.T) {
		m := NewMemStore()
		m.HonorExplicitIDs = true

		if _, err := m.Create(Bead{ID: "gc-5", Title: "pinned"}); err != nil {
			t.Fatalf("pinned Create: %v", err)
		}
		got, err := m.Create(Bead{Title: "minted"})
		if err != nil {
			t.Fatalf("minted Create: %v", err)
		}
		if got.ID == "gc-5" {
			t.Fatal("mint re-issued the pinned id gc-5")
		}
		if got.ID != "gc-6" {
			t.Fatalf("mint did not advance past the pinned suffix: got %q, want %q", got.ID, "gc-6")
		}
	})

	t.Run("mint skips an id already taken by a non-numeric pin", func(t *testing.T) {
		m := NewMemStore()
		m.HonorExplicitIDs = true

		// "gc-1" is pinned directly; the sequence starts at 0, so the next mint
		// would collide were it not for the free-id re-check in mintIDLocked.
		if _, err := m.Create(Bead{ID: "gc-1", Title: "pinned"}); err != nil {
			t.Fatalf("pinned Create: %v", err)
		}
		got, err := m.Create(Bead{Title: "minted"})
		if err != nil {
			t.Fatalf("minted Create: %v", err)
		}
		if got.ID == "gc-1" {
			t.Fatal("mint aliased the pinned id gc-1")
		}
	})
}

// TestMemStoreIDPrefix pins that IDPrefix changes what the store MINTS without
// affecting what it accepts, so two MemStores can model two bead databases that
// mint under different prefixes.
func TestMemStoreIDPrefix(t *testing.T) {
	m := NewMemStore()
	m.IDPrefix = "rig"

	got, err := m.Create(Bead{Title: "t"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if got.ID != "rig-1" {
		t.Fatalf("IDPrefix not applied: got %q, want %q", got.ID, "rig-1")
	}

	def := NewMemStore()
	defGot, err := def.Create(Bead{Title: "t"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if defGot.ID != "gc-1" {
		t.Fatalf("empty IDPrefix changed the default mint: got %q, want %q", defGot.ID, "gc-1")
	}
}

// TestNumericIDSuffix covers the parser that decides whether a pinned id
// consumes a sequence slot, including the ids that must NOT consume one.
func TestNumericIDSuffix(t *testing.T) {
	for _, tc := range []struct {
		id   string
		want int
	}{
		{"gc-42", 42},
		{"gc-0", 0},
		{"42", 42},
		{"gc-wisp-abc", 0},
		{"", 0},
		{"gc-", 0},
		{"gc-12x", 0},
	} {
		if got := numericIDSuffix(tc.id); got != tc.want {
			t.Errorf("numericIDSuffix(%q) = %d, want %d", tc.id, got, tc.want)
		}
	}
}
