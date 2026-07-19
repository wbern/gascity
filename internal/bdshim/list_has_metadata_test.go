package bdshim
import "testing"
func TestListHasMetadataPredicate(t *testing.T) {
	yes := [][]string{{"--status","open","--metadata-field","pr_number=5","--json"},{"--has-metadata-key=branch","--json"}}
	no := [][]string{{"--status","open","--json"},{"--limit","1"}}
	for _, a := range yes { if !ListHasMetadataPredicate(a) { t.Errorf("want true for %v", a) } }
	for _, a := range no { if ListHasMetadataPredicate(a) { t.Errorf("want false for %v", a) } }
}
