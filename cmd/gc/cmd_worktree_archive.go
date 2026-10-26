package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"

	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/spf13/cobra"
)

func readWorktreeArchiveRequest(path string) (worktreeArchiveRequest, error) {
	var req worktreeArchiveRequest
	data, _, err := fsys.ReadRegularFileStableBounded(fsys.OSFS{}, path, 64<<10)
	if err != nil {
		return req, err
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		return req, err
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return req, fmt.Errorf("request must contain exactly one JSON object")
	}
	return req, nil
}

func newWorktreeArchiveReapCmd(stdout, stderr io.Writer) *cobra.Command {
	var request string
	var execute, jsonFlag, capabilities bool
	cmd := &cobra.Command{Use: "archive-reap", Short: "Archive qualified task artifacts and reclaim a completed worktree", Args: cobra.NoArgs}
	cmd.RunE = func(_ *cobra.Command, _ []string) error {
		if !jsonFlag {
			fmt.Fprintln(stderr, "archive-reap requires --json") //nolint:errcheck // best-effort diagnostic
			return errExit
		}
		if capabilities {
			if execute || request != "" {
				return errExit
			}
			return json.NewEncoder(stdout).Encode(worktreeArchiveCapabilities{SchemaVersion: "1", OK: true, Operation: "worktree.archive-reap", RequestSchemaVersion: "1", ReportSchemaVersion: "1", PlanNoMutation: true})
		}
		report := doWorktreeArchiveReap(request, execute, stderr)
		if err := json.NewEncoder(stdout).Encode(report); err != nil {
			return err
		}
		if report.Outcome == "refused" || report.Outcome == "error" || !report.Complete {
			return errExit
		}
		return nil
	}
	cmd.Flags().StringVar(&request, "request", "", "Private JSON file containing exact source expectations and approved artifact leaves")
	cmd.Flags().BoolVar(&execute, "execute", false, "Archive and reclaim after all guards pass (default: read-only plan)")
	cmd.Flags().BoolVar(&jsonFlag, "json", false, "Output structured JSON")
	cmd.Flags().BoolVar(&capabilities, "capabilities", false, "Report the supported archive protocol without reading city state")
	return cmd
}

func doWorktreeArchiveReap(path string, execute bool, stderr io.Writer) worktreeArchiveReport {
	report := worktreeArchiveReport{SchemaVersion: "1", Artifacts: []worktreeArchiveArtifact{}}
	req, err := readWorktreeArchiveRequest(path)
	if err != nil {
		return archiveReportStop(report, "refused", "invalid_request", err.Error())
	}
	city, err := worktreeResolveCity()
	if err != nil {
		return archiveReportStop(report, "error", "city_unavailable", err.Error())
	}
	cfg, err := loadCityConfigWithoutBuiltinPackRefresh(city, stderr)
	if err != nil {
		return archiveReportStop(report, "error", "configuration_unavailable", err.Error())
	}
	stores, err := worktreeReapRigStores(city, cfg, stderr)
	if err != nil {
		return archiveReportStop(report, "error", "scope_unavailable", err.Error())
	}
	cityStore, err := worktreeOpenCityStoreAt(city)
	if err != nil {
		return archiveReportStop(report, "error", "city_scope_unavailable", err.Error())
	}
	sessions, err := worktreeLiveWorkerDirsFn(city)
	if err != nil {
		return archiveReportStop(report, "error", "sessions_unavailable", err.Error())
	}
	dirs := make([]string, 0, len(sessions))
	for dir := range sessions {
		dirs = append(dirs, dir)
	}
	sort.Strings(dirs)
	return archiveReapWorktree(city, cfg, reapStoreScope{RigStores: stores, CityStore: cityStore}, dirs, req, execute, stderr)
}
