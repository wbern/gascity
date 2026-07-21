// Command gc-bead-search is a deliberately narrow, controller-only metadata
// lookup for high-frequency automation. Unlike `bd list`, it has one typed,
// bounded contract: metadata equality, optional post-match exclusions, and
// created-at ascending (then priority/ID) selection. It never falls back to direct bd/Dolt, since
// a silent fallback would both defeat the purpose and change the result model.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/gastownhall/gascity/internal/beadclient"
	"github.com/gastownhall/gascity/internal/controllerendpoint"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("gc-bead-search", flag.ContinueOnError)
	flags.SetOutput(stderr)
	var metadata repeatedString
	var excludeTypes repeatedString
	flags.Var(&metadata, "metadata", "metadata key=value pair to match (repeatable; required)")
	flags.Var(&excludeTypes, "exclude-type", "bead type to omit after matching (repeatable)")
	city := flags.String("city", "", "controller city name (defaults from managed session environment)")
	rig := flags.String("rig", "", "exact rig filter")
	status := flags.String("status", "", "exact status filter")
	assignee := flags.String("assignee", "", "exact assignee filter")
	limit := flags.Int("limit", 1, "maximum matches to return (1..1000)")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 0 {
		fmt.Fprintln(stderr, "gc-bead-search: positional arguments are not supported") //nolint:errcheck // best-effort stderr
		return 2
	}
	filters, err := parseMetadata(metadata)
	if err != nil {
		fmt.Fprintf(stderr, "gc-bead-search: %v\n", err) //nolint:errcheck // best-effort stderr
		return 2
	}
	cityName := controllerendpoint.CityName(*city)
	if cityName == "" {
		fmt.Fprintln(stderr, "gc-bead-search: no city scope; set GC_CITY_PATH or pass --city") //nolint:errcheck // best-effort stderr
		return 2
	}
	result, err := beadclient.NewCityScopedClient(controllerendpoint.BaseURL(), cityName).MetadataSearch(beadclient.MetadataSearchOpts{
		Metadata:     filters,
		ExcludeTypes: excludeTypes,
		Status:       *status,
		Assignee:     *assignee,
		Rig:          *rig,
		Limit:        *limit,
	})
	if err != nil {
		fmt.Fprintf(stderr, "gc-bead-search: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	if result.Body.Partial {
		fmt.Fprintf(stderr, "gc-bead-search: refusing partial controller result: %s\n", strings.Join(result.Body.PartialErrors, "; ")) //nolint:errcheck // best-effort stderr
		return 1
	}
	if err := json.NewEncoder(stdout).Encode(result.Body.Items); err != nil {
		fmt.Fprintf(stderr, "gc-bead-search: encoding result: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	return 0
}

type repeatedString []string

func (r *repeatedString) String() string { return strings.Join(*r, ",") }

func (r *repeatedString) Set(value string) error {
	*r = append(*r, value)
	return nil
}

func parseMetadata(values []string) (map[string]string, error) {
	if len(values) == 0 {
		return nil, fmt.Errorf("at least one --metadata key=value is required")
	}
	filters := make(map[string]string, len(values))
	for _, value := range values {
		key, val, ok := strings.Cut(value, "=")
		if key == "" || !ok {
			return nil, fmt.Errorf("invalid --metadata %q; expected key=value", value)
		}
		if _, exists := filters[key]; exists {
			return nil, fmt.Errorf("duplicate --metadata key %q", key)
		}
		filters[key] = val
	}
	return filters, nil
}
