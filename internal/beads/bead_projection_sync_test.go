package beads

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// TestEveryBeadFieldSurvivesJSONRoundTrip is the drift guard for the
// hand-written bead mappings.
//
// beads.Bead is converted to and from wire form by FOUR hand-written mappings —
// internal/beadclient/wire_shared.go, internal/api/decode_convoys.go,
// beadFromNativeIssue here, and internal/beads/exec. Adding a field to the
// struct and regenerating the OpenAPI spec does NOT make it flow: three of the
// four silently dropped await_type/created_by/owner/notes after those fields
// shipped, and every suite stayed green because nothing asserted the round trip.
//
// This cannot reach into the other packages, but it does pin the invariant they
// each have to honor: a populated Bead must survive a JSON round trip with no
// field lost. A new field with no json tag, or one a mapping forgets, shows up
// here as a mismatch.
func TestEveryBeadFieldSurvivesJSONRoundTrip(t *testing.T) {
	in := Bead{
		ID: "gcw-1", Title: "t", Status: "open", Type: "gate",
		Assignee: "a", From: "f", ParentID: "p", Ref: "r",
		Description: "d", Labels: []string{"l"},
		Metadata:  StringMap{"k": "v"},
		AwaitType: "human", CreatedBy: "seeder", Owner: "o", Notes: "n",
	}
	raw, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out Bead
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, f := range []struct{ name, got, want string }{
		{"AwaitType", out.AwaitType, in.AwaitType},
		{"CreatedBy", out.CreatedBy, in.CreatedBy},
		{"Owner", out.Owner, in.Owner},
		{"Notes", out.Notes, in.Notes},
	} {
		if f.got != f.want {
			t.Errorf("%s = %q, want %q", f.name, f.got, f.want)
		}
	}
}

// TestBeadStringFieldsAreAllWireTagged catches a string field added to Bead
// without a json tag, which would be invisible on every wire path at once.
func TestBeadStringFieldsAreAllWireTagged(t *testing.T) {
	rt := reflect.TypeOf(Bead{})
	for i := 0; i < rt.NumField(); i++ {
		f := rt.Field(i)
		if f.PkgPath != "" { // unexported
			continue
		}
		tag := f.Tag.Get("json")
		if tag == "" {
			t.Errorf("Bead.%s has no json tag; it cannot reach any wire path", f.Name)
			continue
		}
		if strings.HasPrefix(tag, ",") {
			t.Errorf("Bead.%s json tag %q has no name", f.Name, tag)
		}
	}
}
