package main

import (
	"fmt"
	"strings"

	"github.com/gastownhall/gascity/internal/bdflags"
	"github.com/gastownhall/gascity/internal/bdshim"
	"github.com/gastownhall/gascity/internal/config"
)

// REFUSE A CROSS-STORE `bd dep add` RATHER THAN LET IT SUCCEED INTO NOTHING.
//
// MEASURED 2026-08-16 on live gc2 (gci-cxfpt), with a same-store control so
// that "cross-store" was the only variable:
//
//	same store  gc bd dep add gci-A gci-B
//	            -> "Added dependency: gci-A (title) depends on gci-B (title)"  PERSISTS
//	cross store gc bd dep add crm-C gci-A
//	            -> "Added dependency: crm-C (title) depends on gci-A"          exit 0,
//	               and the edge exists on NEITHER side.
//
// Each rig and the city HQ own a SEPARATE bead database. bd resolves a
// depends-on ID within the store it was pointed at, so an ID belonging to
// another store resolves to nothing — and the write is reported as a success
// anyway. The operator gets an affirmative checkmark for a constraint that does
// not exist, and finds out when a worker claims a bead that should have been
// blocked.
//
// The success line already contains the evidence and nobody would ever see it:
// the same-store form prints the depends-on bead's TITLE, the cross-store form
// prints a bare ID. A missing parenthetical is the only tell.
//
// WHY REFUSE RATHER THAN ENFORCE. Enforcing a cross-store blocker means
// resolving it during readiness computation in every store that could be
// blocked. bd has a mechanism for that shape — `external:<project>:<capability>`
// refs, "resolved at query time using the external_projects config" — but it
// lives in bd (upstream) and external_projects is not configured on this city.
// Until that exists, a refusal that names both stores is strictly better than a
// false success, and it is fork-local. This does not close the enforcement
// question; it closes the SILENT one.
//
// WHY ONLY WHEN BOTH SIDES RESOLVE. The guard fires only when both prefixes map
// to KNOWN and DIFFERENT stores. An unknown prefix is left alone deliberately:
// external: refs, and any ID shape this build does not know about, must keep
// working. That makes a false refusal impossible at the cost of not catching a
// typo'd prefix — the right trade for a guard that sits in front of every
// `gc bd dep add`.

// bdBeadStore identifies the store a bead ID's prefix belongs to.
type bdBeadStore struct {
	// key is the comparison identity: rig name, or "" for the city HQ store.
	key string
	// label is how the store is named to a human in the refusal.
	label string
}

// bdStoreForBeadID maps a bead ID to the store its prefix belongs to. ok is
// false when the prefix matches no configured rig or the city HQ store, or
// when the ID is not a plain local ID (an external: ref is not ours to judge).
func bdStoreForBeadID(cfg *config.City, id string) (bdBeadStore, bool) {
	id = strings.TrimSpace(id)
	if id == "" || cfg == nil || strings.Contains(id, ":") {
		return bdBeadStore{}, false
	}
	prefix, _, found := strings.Cut(id, "-")
	if !found || prefix == "" {
		return bdBeadStore{}, false
	}
	for i := range cfg.Rigs {
		rig := &cfg.Rigs[i]
		if rig.EffectivePrefix() == prefix {
			return bdBeadStore{key: "rig:" + rig.Name, label: fmt.Sprintf("rig %q", rig.Name)}, true
		}
	}
	if config.EffectiveHQPrefix(cfg) == prefix {
		return bdBeadStore{key: "city", label: "the city (HQ) store"}, true
	}
	return bdBeadStore{}, false
}

// bdCrossRigDepRefusal reports whether a `bd dep add` names two beads that live
// in different stores, and the message to show if so.
func bdCrossRigDepRefusal(cfg *config.City, args []string) (string, bool) {
	// Resolve the subcommand through SplitGlobalFlags rather than reading
	// args[0]: `bd --actor bob dep add ...` would otherwise present "--actor"
	// as the verb and walk straight past this guard.
	sub, rest := bdflags.SplitGlobalFlags(args)
	dep, ok := bdshim.ParseDepAddArgs(append([]string{sub}, rest...))
	if !ok {
		return "", false
	}
	from, fromOK := bdStoreForBeadID(cfg, dep.FromID)
	to, toOK := bdStoreForBeadID(cfg, dep.ToID)
	if !fromOK || !toOK || from.key == to.key {
		return "", false
	}
	return fmt.Sprintf(
		"refusing a cross-store dependency: %s lives in %s and %s lives in %s.\n"+
			"  Each rig and the city keep SEPARATE bead databases, so bd cannot resolve a\n"+
			"  blocker in another store — it would report success and record nothing, and the\n"+
			"  blocked bead would stay in its rig's ready set (gci-cxfpt).\n"+
			"  Instead: gc bd update %s --status blocked   (and clear it when %s closes),\n"+
			"  or keep both beads in one rig if the dependency must be enforced by the graph.",
		dep.FromID, from.label, dep.ToID, to.label,
		dep.FromID, dep.ToID,
	), true
}
