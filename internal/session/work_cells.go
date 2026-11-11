package session

import (
	"encoding/json"
	"strings"
)

// The work cells on a session row (PHASE1-2 §6.1): work_started, written only
// by the seat's own `gc hook --claim`, and work_ladder, written only by the v2
// lane's work-ladder step. Each is one JSON value its owner rewrites whole by
// a CAS on the row (UpdateRowFenced), never blind (CASOnly), and outside the
// premise (ClassWork): a v2 effect that loses its CAS to one re-reads and
// re-decides. This file holds the work_started codec and the one field of
// work_ladder the hook reads, its pending release.
// WorkStartedKey is declared with the fork's execution-evidence record in
// claim_execution.go.
const WorkLadderKey = "work_ladder"

// How a claim reached a turn: one the hook minted, or an existing assignment
// it handed back.
const (
	WorkStartedViaClaim = "claim"
	WorkStartedViaAdopt = "adopt"
)

const (
	workCellVersion = 1
	// workStartedCap bounds the cell: the writer drops the oldest entry.
	workStartedCap = 8
)

// EncodeWorkStarted is the cell's value for w.
func EncodeWorkStarted(w WorkStarted) string {
	w.V = workCellVersion
	if w.Claims == nil {
		w.Claims = []WorkStartedEntry{}
	}
	b, _ := json.Marshal(w) // strings, ints and times always marshal
	return string(b)
}

// sameClaim reports whether a claim recorded under (aStore, aBead) is the one
// named by (bStore, bBead). An unknown store on either side matches any.
func sameClaim(aStore, aBead, bStore, bBead string) bool {
	if aBead == "" || aBead != bBead {
		return false
	}
	return aStore == "" || bStore == "" || aStore == bStore
}

// With is w with e recorded first, every entry naming the same claim dropped
// (Names' matching), and the oldest beyond the cap dropped.
func (w WorkStarted) With(e WorkStartedEntry) WorkStarted {
	// Releases and Drain belong to the fork's drain-admission and release
	// reservation fences; a claim write must carry them through untouched.
	out := WorkStarted{V: workCellVersion, Claims: []WorkStartedEntry{e}, Releases: w.Releases, Drain: w.Drain}
	for _, c := range w.Claims {
		if sameClaim(c.Store, c.Bead, e.Store, e.Bead) {
			continue
		}
		if len(out.Claims) == workStartedCap {
			break
		}
		out.Claims = append(out.Claims, c)
	}
	return out
}

// Without is w with every entry naming (store, bead) dropped.
func (w WorkStarted) Without(store, bead string) WorkStarted {
	out := WorkStarted{V: workCellVersion, Releases: w.Releases, Drain: w.Drain}
	for _, c := range w.Claims {
		if !sameClaim(c.Store, c.Bead, store, bead) {
			out.Claims = append(out.Claims, c)
		}
	}
	return out
}

// DecodeWorkLadderRelease reads the one field of a work_ladder cell the hook
// needs: the claim the controller is releasing. ok is false for an absent
// cell, one with no pending release, and one it cannot read: the ladder never
// writes a release into a cell the hook cannot read without first having
// proved, by the same row CAS, that work_started does not name it.
func DecodeWorkLadderRelease(raw string) (store, bead string, ok bool) {
	if strings.TrimSpace(raw) == "" {
		return "", "", false
	}
	var cell struct {
		V       int `json:"v"`
		Release *struct {
			Store string `json:"store"`
			Bead  string `json:"bead"`
		} `json:"release"`
	}
	if err := json.Unmarshal([]byte(raw), &cell); err != nil || cell.V != workCellVersion || cell.Release == nil {
		return "", "", false
	}
	if bead = strings.TrimSpace(cell.Release.Bead); bead == "" {
		return "", "", false
	}
	return strings.TrimSpace(cell.Release.Store), bead, true
}

// WorkLadderReleases reports whether a work_ladder cell's pending release
// names the claim (store, bead).
func WorkLadderReleases(raw, store, bead string) bool {
	rs, rb, ok := DecodeWorkLadderRelease(raw)
	return ok && sameClaim(rs, rb, store, bead)
}
