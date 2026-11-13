package main

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/worker"
)

type sessionTargetHandleResolver func(string, beads.Store, runtime.Provider, *config.City, string) (worker.Handle, error)

var liveSubagentsForKill = func(ctx context.Context, handle worker.Handle) ([]worker.InFlightSubagent, error) {
	return worker.InFlightBackgroundSubagents(ctx, handle)
}

// refuseKillForLiveSubagents prints the exact work a kill would destroy. Any
// transcript lookup failure is intentionally fail-open: an operator must still
// be able to kill a damaged or history-less session.
func refuseKillForLiveSubagents(command string, resolve sessionTargetHandleResolver, cityPath string, store beads.Store, provider runtime.Provider, cfg *config.City, target string, out io.Writer) bool {
	handle, err := resolve(cityPath, store, provider, cfg, target)
	if err != nil {
		return false
	}
	live, err := liveSubagentsForKill(context.Background(), handle)
	if err != nil || len(live) == 0 {
		return false
	}
	printLiveSubagentRefusal(command, live, time.Now(), out)
	return true
}

// observeAutonomousKillLiveSubagents records the background work an
// autonomous session destruction will terminate. Observation failures are
// intentionally fail-open so controller recovery can still proceed.
func observeAutonomousKillLiveSubagents(source string, resolve sessionTargetHandleResolver, cityPath string, store beads.Store, provider runtime.Provider, cfg *config.City, target string, out io.Writer) {
	handle, err := resolve(cityPath, store, provider, cfg, target)
	if err != nil {
		return
	}
	observeAutonomousKillLiveSubagentsForHandle(source, target, handle, out)
}

func observeAutonomousKillLiveSubagentsForHandle(source, target string, handle worker.Handle, out io.Writer) {
	live, err := liveSubagentsForKill(context.Background(), handle)
	if err != nil || len(live) == 0 {
		return
	}
	for _, subagent := range live {
		description := strings.TrimSpace(subagent.Description)
		if description == "" {
			description = "(no description)"
		}
		fmt.Fprintf(out, "gc: autonomous session destruction source=%q target=%q live_subagent=%q description=%q\n", source, target, subagent.AgentID, description) //nolint:errcheck
	}
}

// printLiveSubagentRefusal prints command's refusal to destroy the live
// background subagents, naming each with its age at now.
func printLiveSubagentRefusal(command string, live []worker.InFlightSubagent, now time.Time, out io.Writer) {
	fmt.Fprintf(out, "%s: refusing to destroy %d live background subagent(s); retry with --force:\n", command, len(live)) //nolint:errcheck
	for _, subagent := range live {
		fmt.Fprintf(out, "  - %s\n", subagent.Summary(now)) //nolint:errcheck
	}
}
