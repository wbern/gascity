package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"

	"github.com/gastownhall/gascity/internal/api"
)

// newRunCmd constructs the `gc run` parent command. `cancel` is the only
// subcommand today: it is the safe, non-destructive alternative to
// `gc convoy delete --force` for tearing down a stranded graphv2 run,
// routed exclusively through the supervisor API's
// POST /v0/city/{cityName}/runs/{run_id}/cancel endpoint. There is no local
// fallback — the teardown logic (sourceworkflow.CloseWorkflowSubtreeAs) lives
// server-side only, and this command must not reimplement it.
func newRunCmd(stdout, stderr io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "run",
		Short: "Inspect and manage graphv2 runs",
		Args:  cobra.ArbitraryArgs,
		RunE: func(_ *cobra.Command, args []string) error {
			if len(args) == 0 {
				fmt.Fprintln(stderr, "gc run: missing subcommand (cancel)") //nolint:errcheck // best-effort stderr
			} else {
				fmt.Fprintf(stderr, "gc run: unknown subcommand %q\n", args[0]) //nolint:errcheck // best-effort stderr
			}
			return errExit
		},
	}
	cmd.AddCommand(newRunCancelCmd(stdout, stderr))
	return cmd
}

func newRunCancelCmd(stdout, stderr io.Writer) *cobra.Command {
	var jsonOut bool
	cmd := &cobra.Command{
		Use:          "cancel <run-id>",
		Short:        "Cancel a run, closing its root and every open step",
		SilenceUsage: true,
		Long: `Cancel a run via the same endpoint the API uses internally
(POST /v0/city/{cityName}/runs/{run_id}/cancel). Closes the run's root and
every open step bead — control and plain alike — with a canceled outcome, so
the dispatcher finds no more ready work and the run starves.

This is the non-destructive alternative to "gc convoy delete --force" for a
stranded workflow: it tears down the same subtree without deleting bead
history. Requires a live controller or supervisor; there is no local
fallback because the teardown logic lives server-side only.`,
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			return exitForCode(cmdRunCancel(args[0], jsonOut, stdout, stderr))
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "emit machine-readable JSON")
	return cmd
}

func cmdRunCancel(runID string, jsonOut bool, stdout, stderr io.Writer) int {
	cityPath, err := resolveCity()
	if err != nil {
		fmt.Fprintf(stderr, "gc run cancel: %v\n", err) //nolint:errcheck // best-effort stderr
		return 2
	}
	c, reason := runAPIClient(cityPath)
	return routeRunCancel(c, reason, runID, jsonOut, stdout, stderr)
}

// runAPIClient resolves the supervisor API client for `gc run` subcommands,
// or returns (nil, reason) when routing isn't available. There is no local
// fallback: run cancel's teardown is server-side only (see cancelRun in
// internal/api/huma_handlers_runs.go), so a fallback here would mean
// reimplementing domain logic in the CLI, which AGENTS.md forbids.
var runAPIClient = func(cityPath string) (*api.Client, string) {
	if c := apiClient(cityPath); c != nil {
		return c, ""
	}
	// A supervisor-managed city omits a standalone [api] port, so apiClient
	// returns nil even though the controller socket is alive. Route to the
	// supervisor-managed client directly, mirroring maintenanceAPIClient.
	if disabled, _ := classifyGCNoAPI(os.Getenv("GC_NO_API")); !disabled {
		if apiRouteControllerAliveHook(cityPath) != 0 {
			if c := apiRouteSupervisorClientHook(cityPath); c != nil {
				return c, ""
			}
		}
	}
	return nil, apiClientFallbackReason(cityPath)
}

// routeRunCancel dispatches `gc run cancel` to the supervisor API. Exit
// codes: 0 on success, 2 when the supervisor is unreachable, 1 on any API
// error (run not found, already terminal, or otherwise).
func routeRunCancel(c *api.Client, nilReason, runID string, jsonOut bool, stdout, stderr io.Writer) int {
	const cmdName = "run cancel"
	if c == nil {
		logRoute(stderr, cmdName, "fallback", nilReason)
		fmt.Fprintf(stderr, "gc run cancel: supervisor not running (%s)\n", nilReason) //nolint:errcheck // best-effort stderr
		return 2
	}
	result, err := c.CancelRun(runID)
	if err != nil {
		logRoute(stderr, cmdName, "api", "error")
		fmt.Fprintf(stderr, "gc run cancel: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	logRoute(stderr, cmdName, "api", "")
	return renderRunCancel(result, jsonOut, stdout)
}

// runCancelResult is the JSONL shape `gc run cancel --json` emits.
type runCancelResult struct {
	SchemaVersion string `json:"schema_version"`
	OK            bool   `json:"ok"`
	Command       string `json:"command"`
	RunID         string `json:"run_id"`
	Status        string `json:"status"`
	Closed        int    `json:"closed"`
}

// renderRunCancel prints the cancel result as human-readable text or JSON.
// Always returns 0 — reaching here means the API accepted the cancel.
func renderRunCancel(result api.RunCancelResult, jsonOut bool, stdout io.Writer) int {
	if jsonOut {
		_ = json.NewEncoder(stdout).Encode(runCancelResult{ //nolint:errcheck // best-effort stdout
			SchemaVersion: "1",
			OK:            true,
			Command:       "gc run cancel",
			RunID:         result.RunID,
			Status:        result.Status,
			Closed:        result.Closed,
		})
		return 0
	}
	fmt.Fprintf(stdout, "Canceled run %s (status=%s, closed=%d)\n", result.RunID, result.Status, result.Closed) //nolint:errcheck // best-effort stdout
	return 0
}
