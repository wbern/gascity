package main

import (
	"io"

	"github.com/gastownhall/gascity/internal/bddispatch"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// graphStoreSQLiteEnabled reports whether the city runs the embedded SQLite
// graph store (the "split phase"). This fork does not yet carry the SQLite
// graph-store backend, so it is always false: the bd shim's split-phase
// refuse-vs-passthrough gating collapses to the identity phase (one Dolt-backed
// work store), where routed verbs still route to the controller and every
// graph-touching-but-unrouted verb passes through to the real bd byte-identically.
// When the graph-store backend lands on this fork, replace the body with the
// upstream form: cfg != nil && strings.EqualFold(strings.TrimSpace(cfg.Beads.GraphStore), "sqlite").
func graphStoreSQLiteEnabled(cfg *config.City) bool {
	_ = cfg
	return false
}

// writeReadyJSON encodes the ready beads as a JSON array; see
// bddispatch.WriteReadyJSON.
func writeReadyJSON(out []beads.Bead, stdout, stderr io.Writer) int {
	return bddispatch.WriteReadyJSON(out, stdout, stderr)
}
