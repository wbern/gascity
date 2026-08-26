package sling

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

func TestCheckBeadStateRoutedPoolClaimIdempotency(t *testing.T) {
	t.Parallel()

	poolMin, poolMax, singletonMax := 1, 4, 1
	tests := []struct {
		name       string
		agent      config.Agent
		cityName   string
		assignee   string
		others     []config.Agent
		idempotent bool
	}{
		{
			name: "simple pool session",
			agent: config.Agent{
				Name: "smiths", MinActiveSessions: &poolMin, MaxActiveSessions: &poolMax,
			},
			assignee:   "smiths-sess1",
			idempotent: true,
		},
		{
			name: "configured sibling pool claim remains a conflict",
			agent: config.Agent{
				Name: "smiths", MinActiveSessions: &poolMin, MaxActiveSessions: &poolMax,
			},
			assignee: "smiths-ops-sess1",
			others: []config.Agent{{
				Name: "smiths-ops", MinActiveSessions: &poolMin, MaxActiveSessions: &poolMax,
			}},
			idempotent: false,
		},
		{
			name: "rig pool bead-derived session",
			agent: config.Agent{
				Name: "codex", Dir: "gas-city-wbern", MinActiveSessions: &poolMin, MaxActiveSessions: &poolMax,
			},
			cityName:   "gc2",
			assignee:   "codex-gc2-izdtyc",
			idempotent: true,
		},
		{
			name: "sibling pool claim remains a conflict",
			agent: config.Agent{
				Name: "codex", Dir: "gas-city-wbern", MinActiveSessions: &poolMin, MaxActiveSessions: &poolMax,
			},
			cityName:   "gc2",
			assignee:   "codex-ops-gc2-izdtyc",
			idempotent: false,
		},
		{
			name: "singleton prefix-shaped claim remains a conflict",
			agent: config.Agent{
				Name: "mayor", MaxActiveSessions: &singletonMax,
			},
			cityName:   "gc2",
			assignee:   "mayor-gc2-izdtyc",
			idempotent: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			store := beads.NewMemStore()
			convoy, err := store.Create(beads.Bead{Title: "auto convoy", Type: "convoy", Status: "open"})
			if err != nil {
				t.Fatalf("create convoy: %v", err)
			}
			target := tt.agent.QualifiedName()
			bead, err := store.Create(beads.Bead{
				Title:    "routed work",
				Type:     "task",
				Status:   "open",
				Assignee: tt.assignee,
				Metadata: map[string]string{beadmeta.RoutedToMetadataKey: target},
			})
			if err != nil {
				t.Fatalf("create routed work: %v", err)
			}
			if err := store.DepAdd(convoy.ID, bead.ID, "tracks"); err != nil {
				t.Fatalf("track routed work: %v", err)
			}

			agents := append([]config.Agent{tt.agent}, tt.others...)
			got := CheckBeadState(store, bead.ID, tt.agent, SlingDeps{
				CityName: tt.cityName,
				Cfg:      &config.City{Agents: agents},
				Store:    store,
			})
			if got.Idempotent != tt.idempotent {
				t.Fatalf("Idempotent = %v, want %v; result=%+v", got.Idempotent, tt.idempotent, got)
			}
			if !tt.idempotent && len(got.Warnings) == 0 {
				t.Fatal("conflicting claim returned no warning")
			}
		})
	}
}
