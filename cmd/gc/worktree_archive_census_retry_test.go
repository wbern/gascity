package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

func TestArchiveForeignUnknownStateDoesNotExpandUIDScope(t *testing.T) {
	ids, err := parseArchiveProcessIDs([]byte("42 501 S\n77 0 ?\n78 -2 ?\n"), 501, 42, true)
	if err != nil || len(ids) != 1 || !ids[42] {
		t.Fatalf("foreign state blocked current-UID census: %v %v", ids, err)
	}
	for _, row := range []string{"77 501 ?", "77 unknown ?", "77 4294967296 ?", "bad 0 ?", "77 0 ? extra"} {
		if _, err := parseArchiveProcessIDs([]byte("42 501 S\n"+row+"\n"), 501, 42, true); err == nil {
			t.Fatalf("uncertain ownership/current state accepted: %q", row)
		}
	}
}

func TestArchiveCensusRetryIsWholeAndBounded(t *testing.T) {
	for _, scenario := range []string{"transient_state", "persistent_state", "alternating_omissions", "persistent_revoked", "positive_hold", "shared_deadline"} {
		t.Run(scenario, func(t *testing.T) {
			fixture, err := normalizeReapBorrowPath(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			root := filepath.Join(fixture, "checkout")
			if err := os.Mkdir(root, 0o700); err != nil {
				t.Fatal(err)
			}
			outside := archiveTestHandleRecord(t, os.Getpid(), "cwd", "r", "DIR", fixture, fixture)
			rowA := archiveTestHandleRecord(t, 2147483001, "5", "r", "DIR", fixture, fixture)
			rowB := archiveTestHandleRecord(t, 2147483002, "5", "r", "DIR", fixture, fixture)
			held := archiveTestHandleRecord(t, 2147483001, "5", "r", "DIR", root, root)
			for name, data := range map[string]string{"outside": outside, "a": outside + rowA, "b": outside + rowB, "held": outside + held, "revoked": fmt.Sprintf("p%d\x00f10\x00a \x00n(revoked)\x00", os.Getpid())} {
				if err := os.WriteFile(filepath.Join(fixture, name), []byte(data), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			ps := `#!/bin/sh
n=0
[ ! -f "$GC_TEST_CENSUS_DIR/ps-count" ] || n=$(cat "$GC_TEST_CENSUS_DIR/ps-count")
n=$((n+1))
printf '%s\n' "$n" > "$GC_TEST_CENSUS_DIR/ps-count"
printf '%s %s S\n' "$GC_TEST_CENSUS_PID" "$GC_TEST_CENSUS_UID"
case "$GC_TEST_CENSUS_CASE" in
 transient_state|shared_deadline) [ "$n" -ne 2 ] || printf '2147483001 %s ?\n' "$GC_TEST_CENSUS_UID";;
 persistent_state) [ "$((n%2))" -ne 0 ] || printf '2147483001 %s ?\n' "$GC_TEST_CENSUS_UID";;
 alternating_omissions) printf '2147483001 %s S\n2147483002 %s S\n' "$GC_TEST_CENSUS_UID" "$GC_TEST_CENSUS_UID";;
 positive_hold) printf '2147483001 %s S\n' "$GC_TEST_CENSUS_UID";;
esac
exit 0
`
			lsof := `#!/bin/sh
n=0
[ ! -f "$GC_TEST_CENSUS_DIR/lsof-count" ] || n=$(cat "$GC_TEST_CENSUS_DIR/lsof-count")
n=$((n+1))
printf '%s\n' "$n" > "$GC_TEST_CENSUS_DIR/lsof-count"
case "$GC_TEST_CENSUS_CASE" in
 alternating_omissions) if [ "$n" -eq 1 ]; then cat "$GC_TEST_CENSUS_DIR/a"; else cat "$GC_TEST_CENSUS_DIR/b"; fi;;
 persistent_revoked) cat "$GC_TEST_CENSUS_DIR/revoked";;
 positive_hold) cat "$GC_TEST_CENSUS_DIR/held";;
 shared_deadline) if [ "$n" -gt 1 ]; then /bin/sleep 10; fi; cat "$GC_TEST_CENSUS_DIR/outside";;
 *) cat "$GC_TEST_CENSUS_DIR/outside";;
esac
`
			for name, data := range map[string]string{"ps": ps, "lsof": lsof} {
				if err := os.WriteFile(filepath.Join(fixture, name), []byte(data), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			t.Setenv("PATH", fixture+string(os.PathListSeparator)+os.Getenv("PATH"))
			t.Setenv("GC_TEST_CENSUS_DIR", fixture)
			t.Setenv("GC_TEST_CENSUS_PID", strconv.Itoa(os.Getpid()))
			t.Setenv("GC_TEST_CENSUS_UID", strconv.Itoa(os.Geteuid()))
			t.Setenv("GC_TEST_CENSUS_CASE", scenario)
			budget := 2 * time.Second
			if scenario == "shared_deadline" {
				budget = 5 * time.Second
			}
			ctx, cancel := context.WithTimeout(context.Background(), budget)
			defer cancel()
			snapshots := 0
			if scenario == "shared_deadline" {
				original := archiveHandleSnapshot
				deadline, _ := ctx.Deadline()
				archiveHandleSnapshot = func(probeCtx context.Context) ([]byte, error) {
					snapshots++
					observed, ok := probeCtx.Deadline()
					if !ok || !observed.Equal(deadline) {
						t.Fatal("retry replaced the caller's original deadline")
					}
					return original(probeCtx)
				}
				t.Cleanup(func() { archiveHandleSnapshot = original })
			}
			start := time.Now()
			err = archivePlatformHandlesClear(ctx, root)
			elapsed := time.Since(start)
			data, readErr := os.ReadFile(filepath.Join(fixture, "lsof-count"))
			if readErr != nil {
				t.Fatal(readErr)
			}
			calls, parseErr := strconv.Atoi(string(bytes.TrimSpace(data)))
			if parseErr != nil {
				t.Fatal(parseErr)
			}
			if calls > 2 {
				t.Fatalf("unbounded retry: %d", calls)
			}
			switch scenario {
			case "transient_state":
				if err != nil || calls != 2 {
					t.Fatalf("fresh complete second census rejected: calls=%d err=%v", calls, err)
				}
			case "positive_hold":
				if !errors.Is(err, errArchiveHeldHandle) || calls != 1 {
					t.Fatalf("positive hold must stop immediately: calls=%d err=%v", calls, err)
				}
			default:
				if err == nil {
					t.Fatalf("incomplete/alternating census accepted: %s", scenario)
				}
			}
			if scenario == "shared_deadline" && (calls != 2 || snapshots != 2 || elapsed > budget+2*time.Second) {
				t.Fatalf("retry changed shared deadline: calls=%d snapshots=%d elapsed=%s", calls, snapshots, elapsed)
			}
		})
	}
}
