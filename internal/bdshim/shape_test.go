package bdshim

import (
	"strings"
	"testing"
)

func TestCommandShapeCanonicalizesEquivalentListSyntax(t *testing.T) {
	long := CommandShape([]string{"--status=open", "--assignee", "gas-city-wbern/architect", "--limit", "50", "--json"})
	short := CommandShape([]string{"-s", "open", "-a=gas-city-wbern/architect", "-n", "50", "--json"})
	const want = "flags=--assignee,--json,--limit,--status"
	if long != want {
		t.Fatalf("long shape = %q, want %q", long, want)
	}
	if short != want {
		t.Fatalf("short shape = %q, want %q", short, want)
	}
}

func TestCommandShapeNeverRecordsValuesOrPositionals(t *testing.T) {
	secretValues := []string{
		"gcw-super-secret-id",
		"/Users/willi/private/path",
		"wbern:confidential-label",
		"sensitive free text",
		"token-value-should-not-appear",
		"api-key-value-should-not-appear",
		"bearer-value-should-not-appear",
		"--value-that-looks-like-a-flag",
		"--json-as-an-unknown-option-value",
	}
	shape := CommandShape([]string{
		"gcw-super-secret-id",
		"--description", "sensitive free text",
		"--label=wbern:confidential-label",
		"--set-metadata", "private.key=token-value-should-not-appear",
		"--assignee", "--value-that-looks-like-a-flag",
		"--token=token-value-should-not-appear",
		"--api-key", "api-key-value-should-not-appear",
		"--bearer=bearer-value-should-not-appear",
		"--future-private-flag", "--json-as-an-unknown-option-value",
		"--future-private-flag=/Users/willi/private/path",
	})
	for _, secret := range secretValues {
		if strings.Contains(shape, secret) {
			t.Fatalf("shape leaked %q: %q", secret, shape)
		}
	}
	const want = "flags=--api-key,--assignee,--bearer,--description,--label,--set-metadata,--token,unknown"
	if shape != want {
		t.Fatalf("shape = %q, want %q", shape, want)
	}
}

func TestCommandShapeUnknownOptionCannotExposeFlagLookingValue(t *testing.T) {
	shape := CommandShape([]string{"--future-private-option", "--json", "--status=open"})
	if strings.Contains(shape, "--json") {
		t.Fatalf("shape exposed an unknown option value that looks like a flag: %q", shape)
	}
	const want = "flags=--status,unknown"
	if shape != want {
		t.Fatalf("shape = %q, want %q", shape, want)
	}
}

func TestCommandShapeIsStableAndBoundsUnknownFlagCardinality(t *testing.T) {
	first := CommandShape([]string{"--offset", "10", "--json", "--mystery-flag=one"})
	second := CommandShape([]string{"--mystery-flag=two", "--json", "--offset=20"})
	const want = "flags=--json,--offset,unknown"
	if first != want {
		t.Fatalf("first shape = %q, want %q", first, want)
	}
	if second != want {
		t.Fatalf("second shape = %q, want %q", second, want)
	}
}

func TestCommandShapeNoFlags(t *testing.T) {
	if got := CommandShape([]string{"gcw-private-id"}); got != "flags=none" {
		t.Fatalf("shape = %q, want flags=none", got)
	}
}

func TestCommandVerbUsesFixedVocabulary(t *testing.T) {
	for _, verb := range []string{"list", "context", "release-if-current"} {
		if got := CommandVerb(verb); got != verb {
			t.Fatalf("CommandVerb(%q) = %q, want %q", verb, got, verb)
		}
	}
	for _, arbitrary := range []string{"gcw-private-id", "/Users/willi/private", "sensitive free text", ""} {
		if got := CommandVerb(arbitrary); got != unknownCommandVerb {
			t.Fatalf("CommandVerb(%q) = %q, want %q", arbitrary, got, unknownCommandVerb)
		}
	}
}

func TestCommandShapeKeepsListRoutingPolicyFlagsObservable(t *testing.T) {
	for name, flags := range map[string]map[string]bool{
		"create": CreateRoutableFlags,
		"update": UpdateRoutableFlags,
		"ready":  ReadyRoutableFlags,
		"list":   ListRoutableFlags,
	} {
		for flag := range flags {
			t.Run(name+"/"+flag, func(t *testing.T) {
				shape := CommandShape([]string{flag})
				if strings.Contains(shape, unknownCommandShapeFlag) {
					t.Fatalf("shape for routable %s flag %q = %q, want named safe flag", name, flag, shape)
				}
			})
		}
	}

	for _, flag := range []string{"--metadata-field", "--has-metadata-key", "--exclude-type", "--offset", "--sort", "--no-assignee"} {
		t.Run(flag, func(t *testing.T) {
			shape := CommandShape([]string{flag})
			if strings.Contains(shape, unknownCommandShapeFlag) {
				t.Fatalf("shape for known unroutable list flag %q = %q, want named safe flag", flag, shape)
			}
		})
	}
}
