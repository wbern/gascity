package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/session"
)

// This owns production-entry environment plumbing. The bd command adapter
// models only its deterministic claim/read/identity writes; native backend
// concurrency remains covered by conformance and the disposable runtime proof.
func TestHookRecordsStartedWorkFromProcessEnvironment(t *testing.T) {
	for _, scenario := range []struct{ epoch, mode, override string }{{"7", "require", ""}, {"6", "require", ""}, {"7", "off", "require"}, {"7", "off", ""}, {"federated", "require", ""}, {"federated-read-failure", "off", ""}, {"missing-token", "require", ""}, {"7", "require", "invalid"}} {
		t.Run(scenario.epoch+"/"+scenario.mode+"/"+scenario.override, func(t *testing.T) {
			epoch := scenario.epoch
			if epoch == "federated" || epoch == "missing-token" {
				epoch = "7"
			}
			clearGCEnv(t)
			t.Setenv("GC_BEADS_CONDITIONAL_WRITES", scenario.override)
			disableManagedDoltRecoveryForTest(t)
			t.Setenv("GC_BEADS", "file")
			city := ""
			if strings.HasPrefix(scenario.epoch, "federated") {
				city = writeFenceTestRigCity(t)
				if err := os.MkdirAll(filepath.Join(city, "rigs", "aoa"), 0o755); err != nil {
					t.Fatal(err)
				}
			} else {
				city = writeFenceTestCity(t)
			}
			configPath := filepath.Join(city, "city.toml")
			configText, err := os.ReadFile(configPath)
			if err != nil {
				t.Fatal(err)
			}
			if strings.HasPrefix(scenario.epoch, "federated") {
				configText = []byte(strings.Replace(string(configText), `dir = "aoa"`, `scope = "city"`, 1))
				// Scoped layout opens existing roots. Initialize each root through
				// the actual file adapter before activating that layout marker.
				for _, root := range []string{city, filepath.Join(city, "rigs", "aoa")} {
					if err := os.MkdirAll(filepath.Join(root, ".gc"), 0o755); err != nil {
						t.Fatal(err)
					}
					store, err := openScopeLocalFileStoreForCity(root, city)
					if err != nil {
						t.Fatal(err)
					}
					seed, err := store.Create(beads.Bead{Title: "initialize disposable store"})
					if err != nil {
						t.Fatal(err)
					}
					if err := store.Delete(seed.ID); err != nil {
						t.Fatal(err)
					}
				}
				if err := os.WriteFile(fileStoreLayoutMarkerPath(city), []byte(fileStoreLayoutScopedV1), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			configText = append(configText, []byte("\n[beads]\nconditional_writes = "+strconv.Quote(scenario.mode)+"\n")...)
			if err := os.WriteFile(configPath, configText, 0o644); err != nil {
				t.Fatal(err)
			}
			store, err := openCityStoreAt(city)
			if err != nil {
				t.Fatal(err)
			}
			if strings.HasPrefix(scenario.epoch, "federated") {
				cfg, err := loadCityConfigWithoutBuiltinPackRefresh(city, &bytes.Buffer{})
				if err != nil {
					t.Fatal(err)
				}
				store, err = openStoreAtForCityWithConfig(filepath.Join(city, "rigs", "aoa"), city, cfg)
				if err != nil {
					t.Fatal(err)
				}
			}
			work, err := store.Create(beads.Bead{Title: "execution environment proof", Type: "task", Metadata: map[string]string{"gc.routed_to": "worker"}})
			if err != nil {
				t.Fatal(err)
			}
			payload, err := json.Marshal([]beads.Bead{work})
			if err != nil {
				t.Fatal(err)
			}
			leakPath := filepath.Join(city, "incarnation-forwarding-evidence")
			quotedLeakPath := "'" + strings.ReplaceAll(leakPath, "'", "'\"'\"'") + "'"
			guard := "if [ -n \"$GC_INSTANCE_TOKEN\" ] || [ -n \"$GC_RUNTIME_EPOCH\" ]; then printf forwarded > " + quotedLeakPath + "; exit 98; fi; "
			query := guard + "printf '%s' '" + string(payload) + "'"
			if strings.HasPrefix(scenario.epoch, "federated") {
				query = guard + "if [ -z \"$GC_RIG\" ]; then printf '[]'; else printf '%s' '" + string(payload) + "'; fi"
			}
			text := strings.Replace(string(configText), "name = \"worker\"", "name = \"worker\"\nwork_query = "+strconv.Quote(query), 1)
			if err := os.WriteFile(configPath, []byte(text), 0o644); err != nil {
				t.Fatal(err)
			}
			metadata := map[string]string{"generation": "7"}
			persistedOff := scenario.mode == "off" && scenario.override == ""
			if persistedOff {
				metadata[session.WorkStartedKey] = `{"v":1,"claims":[]}`
			}
			id := newFenceSessionBeadWithMetadata(t, city, session.StateActive, "runtime-capability", metadata)
			setFenceClaimEnv(t, city, id, "runtime-capability")
			t.Setenv("GC_RUNTIME_EPOCH", epoch)
			if scenario.epoch == "missing-token" {
				t.Setenv("GC_INSTANCE_TOKEN", "")
			}
			original := hookClaimCommandRunnerWithEnvContext
			t.Cleanup(func() { hookClaimCommandRunnerWithEnvContext = original })
			hookClaimCommandRunnerWithEnvContext = func(_ context.Context, env map[string]string) beads.CommandRunner {
				if env["GC_INSTANCE_TOKEN"] != "" || env["GC_RUNTIME_EPOCH"] != "" {
					t.Fatal("incarnation capability leaked into a subprocess environment")
				}
				return func(_ string, name string, args ...string) ([]byte, error) {
					if name != "bd" {
						return nil, fmt.Errorf("unexpected executable %s", name)
					}
					switch {
					case len(args) == 4 && args[0] == "update" && args[1] == work.ID && args[2] == "--claim" && args[3] == "--json":
						status, owner := "in_progress", "worker-1"
						if err := store.Update(work.ID, beads.UpdateOpts{Status: &status, Assignee: &owner}); err != nil {
							return nil, err
						}
					case len(args) >= 3 && args[0] == "update" && args[1] == "--json" && args[2] == work.ID:
						patch := map[string]string{}
						for i := 3; i < len(args); i += 2 {
							if i+1 >= len(args) || args[i] != "--set-metadata" {
								return nil, fmt.Errorf("unexpected identity arguments %q", args)
							}
							key, value, ok := strings.Cut(args[i+1], "=")
							if !ok {
								return nil, fmt.Errorf("invalid metadata argument %q", args[i+1])
							}
							patch[key] = value
						}
						if err := store.SetMetadataBatch(work.ID, patch); err != nil {
							return nil, err
						}
					case len(args) == 3 && args[0] == "show" && args[1] == "--json" && args[2] == work.ID:
					default:
						return nil, fmt.Errorf("unexpected bd command %q", args)
					}
					got, err := store.Get(work.ID)
					if err != nil {
						return nil, err
					}
					return json.Marshal([]beads.Bead{got})
				}
			}
			// Keep the routed work store readable while hiding an already enrolled
			// session behind a genuine city-store open failure.
			if scenario.epoch == "federated-read-failure" {
				path := filepath.Join(city, ".gc", "beads.json")
				original, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					if err := os.WriteFile(path, original, 0o644); err != nil {
						t.Error(err)
					}
				})
				if err := os.WriteFile(path, []byte("{ invalid session store"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			var stdout, stderr bytes.Buffer
			code := cmdHookWithOptions(nil, hookCommandOptions{Claim: true, JSON: true}, &stdout, &stderr)
			if _, err := os.Stat(leakPath); err == nil {
				t.Fatal("actual process incarnation input reached a query child")
			} else if !os.IsNotExist(err) {
				t.Fatal(err)
			}
			if scenario.epoch == "federated-read-failure" {
				if code == 0 || stdout.Len() != 0 || !strings.Contains(stderr.String(), "execution evidence unavailable") {
					t.Fatalf("unknown enrollment admitted work: code=%d stdout=%q stderr=%s", code, stdout.String(), stderr.String())
				}
				return
			}
			front, err := sessionCurrentClaimFrontDoor()
			if err != nil {
				t.Fatal(err)
			}
			_, persisted, err := front.GetPersistedResponse(id)
			if err != nil {
				t.Fatal(err)
			}
			cell, err := session.DecodeWorkStarted(persisted.Metadata[session.WorkStartedKey])
			if err != nil {
				t.Fatal(err)
			}
			if epoch == "6" || persistedOff || scenario.epoch == "missing-token" {
				if code == 0 || stdout.Len() != 0 || len(cell.Claims) != 0 {
					t.Fatalf("stale process admitted work: code=%d stdout=%q cell=%+v stderr=%s", code, stdout.String(), cell, stderr.String())
				}
				return
			}
			var result hookClaimJSONResult
			if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
				t.Fatalf("missing work result: code=%d stdout=%q stderr=%s: %v", code, stdout.String(), stderr.String(), err)
			}
			if code != 0 || result.Action != "work" || result.BeadID != work.ID || !cell.Names("", work.ID) {
				t.Fatalf("production fence did not persist: code=%d result=%+v cell=%+v stderr=%s", code, result, cell, stderr.String())
			}
		})
	}
}
