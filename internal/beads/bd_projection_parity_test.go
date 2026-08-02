package beads

import (
	"reflect"
	"sort"
	"strings"
	"testing"

	beadslib "github.com/steveyegge/beads"
)

// bd's --json output is not formatted by hand anywhere in bd: it is
// json.Marshal of an exported type. `bd list`/`bd ready` emit
// beadslib.IssueWithCounts; `bd show` emits beadslib.IssueDetails. That makes
// bd's own struct tags the authoritative statement of what a bead looks like on
// the wire, and it means the question "does our projection match bd's" has a
// mechanical answer instead of a human one.
//
// gc reproduces that shape from its own beads.Bead. Nothing connected the two,
// so a field bd emits and Bead does not model was invisible: it did not fail a
// build, a test, or a read — the key was simply absent from routed output.
// That is how created_by, owner, notes and await_type went missing on 240 of
// 240 live beads until someone diffed key sets by hand.
//
// This test makes bd's type the source of truth. Every json key bd emits must
// either be representable by beads.Bead or be named in knownProjectionGaps with
// a reason. A NEW gap fails; an existing one is documented rather than silent.

// knownProjectionGaps is the FROZEN BASELINE of bd projection keys beads.Bead
// does not carry today. Its purpose is not to excuse them — it is to make the
// set closed, so that adding a field to bd (or dropping one from Bead) fails
// here instead of silently shrinking routed output.
//
// The size of this list is the finding. bd's Issue carries ~47 json fields;
// beads.Bead models roughly a third of them. Every key below is a value a
// caller of routed `bd list`/`bd ready` does not receive when the underlying
// bead has it set. Most are empty on most beads, which is exactly why the gap
// stayed invisible: omitempty hides an unmodeled field until the one bead that
// uses it goes through the routed path.
//
// Three groups, by why they are outstanding:
//
//	COMPUTED     the controller does not yet plumb them. beadslib exposes them
//	             directly via Storage.GetReadyWorkWithCounts and
//	             SearchIssuesWithCounts, so this is plumbing, not reimplementation.
//	UNMODELLED   bd tracks the field and gc's domain has no equivalent. Carrying
//	             one means deciding it belongs in gc's model, not just copying it.
//	MOLECULE     bd's molecule/wisp/event vocabulary, which gc represents through
//	             its own formula and event primitives rather than on Bead.
var knownProjectionGaps = map[string]string{
	// COMPUTED
	"dependency_count": "COMPUTED",
	"dependent_count":  "COMPUTED",
	"comment_count":    "COMPUTED",
	"comments":         "COMPUTED",

	// UNMODELLED
	"acceptance_criteria": "UNMODELLED",
	"design":              "UNMODELLED",
	"due_at":              "UNMODELLED",
	"estimated_minutes":   "UNMODELLED",
	"external_ref":        "UNMODELLED",
	"is_template":         "UNMODELLED",
	"pinned":              "UNMODELLED",
	"spec_id":             "UNMODELLED",
	"started_at":          "UNMODELLED",
	"closed_at":           "UNMODELLED",
	"close_reason":        "UNMODELLED",
	"closed_by_session":   "UNMODELLED",
	"work_type":           "UNMODELLED",
	"source_formula":      "UNMODELLED",
	"source_location":     "UNMODELLED",
	"source_system":       "UNMODELLED",
	"timeout":             "UNMODELLED",

	// MOLECULE / event vocabulary
	"actor":               "MOLECULE",
	"target":              "MOLECULE",
	"event_kind":          "MOLECULE",
	"payload":             "MOLECULE",
	"sender":              "MOLECULE",
	"waiters":             "MOLECULE",
	"await_id":            "MOLECULE",
	"bonded_from":         "MOLECULE",
	"mol_type":            "MOLECULE",
	"wisp_type":           "MOLECULE",
	"original_size":       "MOLECULE",
	"compacted_at":        "MOLECULE",
	"compacted_at_commit": "MOLECULE",
	"compaction_level":    "MOLECULE",
}

// TestBeadCanRepresentEveryBdListProjectionField pins the list/ready shape.
func TestBeadCanRepresentEveryBdListProjectionField(t *testing.T) {
	assertProjectionCovered(t, "bd list/ready (IssueWithCounts)", jsonKeysOf(reflect.TypeOf(beadslib.IssueWithCounts{})))
}

// The `bd show` shape (types.IssueDetails) is NOT guarded here, because beads
// does not export it: the root package aliases Issue, IssueWithCounts,
// DependencyCounts, IssueFilter, WorkFilter and friends, but not IssueDetails.
// So the list/ready projection can be checked mechanically and the show
// projection cannot. That asymmetry is worth knowing before anyone plans routed
// `show`: its output contract is only reachable by observing real bd output,
// which is exactly the fragile position this guard removes for list/ready.

// TestKnownProjectionGapsAreStillGaps keeps the allowlist honest. An entry that
// has since been implemented must be removed, or the list quietly grants
// permission for a regression to reintroduce it.
func TestKnownProjectionGapsAreStillGaps(t *testing.T) {
	beadKeys := jsonKeysOf(reflect.TypeOf(Bead{}))
	for key, reason := range knownProjectionGaps {
		if reason == "" {
			t.Errorf("knownProjectionGaps[%q] has no reason; every gap must say why it is outstanding", key)
		}
		if beadKeys[key] {
			t.Errorf("knownProjectionGaps[%q] is listed as a gap but beads.Bead now carries it; remove the entry so a regression cannot hide behind it", key)
		}
	}
}

func assertProjectionCovered(t *testing.T, shape string, bdKeys map[string]bool) {
	t.Helper()
	if len(bdKeys) == 0 {
		t.Fatalf("%s: derived no json keys from bd's type; the guard is not watching anything", shape)
	}
	beadKeys := jsonKeysOf(reflect.TypeOf(Bead{}))
	var missing []string
	for key := range bdKeys {
		if beadKeys[key] {
			continue
		}
		if _, known := knownProjectionGaps[key]; known {
			continue
		}
		missing = append(missing, key)
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("%s emits %d key(s) beads.Bead cannot represent: %s\n"+
			"A routed read silently omits each of these. Either add the field to beads.Bead "+
			"(and to the four hand-written mappings), or record it in knownProjectionGaps with a reason.",
			shape, len(missing), strings.Join(missing, ", "))
	}
}

// jsonKeysOf returns the marshaled json key set of a struct type, following
// embedded structs and embedded struct POINTERS (beadslib.IssueWithCounts
// embeds *Issue, so a walker that only handles value embedding would silently
// see four keys instead of fifteen and pass vacuously).
func jsonKeysOf(rt reflect.Type) map[string]bool {
	keys := make(map[string]bool)
	collectJSONKeys(rt, keys)
	return keys
}

func collectJSONKeys(rt reflect.Type, keys map[string]bool) {
	for rt.Kind() == reflect.Pointer {
		rt = rt.Elem()
	}
	if rt.Kind() != reflect.Struct {
		return
	}
	for i := range rt.NumField() {
		f := rt.Field(i)
		if f.PkgPath != "" && !f.Anonymous {
			continue // unexported
		}
		tag := f.Tag.Get("json")
		name, _, _ := strings.Cut(tag, ",")
		if f.Anonymous && name == "" {
			collectJSONKeys(f.Type, keys)
			continue
		}
		if name == "-" {
			continue
		}
		if name == "" {
			name = f.Name
		}
		keys[name] = true
	}
}
