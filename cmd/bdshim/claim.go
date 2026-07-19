package main

import (
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/bddispatch"
	"github.com/gastownhall/gascity/internal/beads"
)

// dispatchClaim routes an atomic claim to the controller and reproduces the
// `bd update <id> --claim --json` output contract its caller (BdStore.Claim)
// parses: on success it prints the claimed bead JSON (exit 0); on a lost race it
// prints an "already claimed by <holder>" message (exit 1); on a backend that
// cannot claim on behalf of an actor it returns handled=false so the caller
// falls back to the real bd. Mirrors cmd/gc's dispatchBdShimClaim.
func dispatchClaim(client *api.Client, id, actor string, stdout, stderr io.Writer) (code int, handled bool) {
	bead, claimed, err := client.ClaimBead(id, actor)
	if err != nil {
		if errors.Is(err, api.ErrClaimRouteUnsupported) {
			return 0, false
		}
		fmt.Fprintf(stderr, "bdproxy: claiming %q via API: %v\n", id, err) //nolint:errcheck // best-effort stderr
		return 1, true
	}
	if !claimed {
		holder := strings.TrimSpace(bead.Assignee)
		if holder == "" {
			holder = "another agent"
		}
		fmt.Fprintf(stderr, "bdproxy: bead %s already claimed by %s\n", id, holder) //nolint:errcheck // best-effort stderr
		return 1, true
	}
	return bddispatch.WriteReadyJSON([]beads.Bead{bead}, stdout, stderr), true
}
