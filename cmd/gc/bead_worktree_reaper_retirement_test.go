package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/git"
	"github.com/gastownhall/gascity/internal/worktree"
)

type reapAfterTrackedVerificationProbe struct {
	gitProbe
	after func()
	calls int
}

func (p *reapAfterTrackedVerificationProbe) VerifyTrackedContent(head string) error {
	if err := p.gitProbe.VerifyTrackedContent(head); err != nil {
		return err
	}
	p.calls++
	if p.calls == 1 {
		p.after()
	}
	return nil
}

func TestReapClosedBeadWorktrees_FreshRetirementGuards(t *testing.T) {
	for _, scenario := range []string{"pointer_replaced", "late_borrower", "late_cwd", "lock_busy"} {
		t.Run(scenario, func(t *testing.T) {
			city, rig := initReapRig(t)
			wt := addClosedWorktree(t, rig, city, "builder", "ga-fence01")
			store := beads.NewMemStoreFrom(1, []beads.Bead{{ID: "ga-fence01", Status: "closed"}}, nil)
			scope := reapTestScope(map[string]beads.Store{reapTestRigName: store})
			injectLiveness(t, liveWorktreeState{scanned: true})
			triggered := false
			probe := &reapAfterTrackedVerificationProbe{gitProbe: git.New(wt), after: func() {
				triggered = true
				switch scenario {
				case "pointer_replaced":
					path := filepath.Join(wt, ".git")
					data, err := os.ReadFile(path)
					if err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(path+".replacement", data, 0o644); err != nil {
						t.Fatal(err)
					}
					if err := os.Rename(path+".replacement", path); err != nil {
						t.Fatal(err)
					}
				case "late_borrower":
					if _, err := scope.CityStore.Create(beads.Bead{Status: "open", Metadata: map[string]string{"gc.work_dir": wt}}); err != nil {
						t.Fatal(err)
					}
				case "late_cwd":
					collectLiveWorktreeStateFn = func() liveWorktreeState { return liveWorktreeState{scanned: true, cwds: []string{wt}} }
				case "lock_busy":
					unlock, err := worktree.TryLockRetirement(rig, wt)
					if err != nil {
						t.Fatal(err)
					}
					t.Cleanup(unlock)
				}
			}}
			injectReapGitProbe(t, wt, probe)
			var stderr bytes.Buffer
			report := reapClosedBeadWorktrees(city, reapTestConfig(rig), scope, nil, false, events.Discard, nil, &stderr)
			if !triggered {
				t.Fatal("late mutation checkpoint was not reached")
			}
			if len(report.Reaped) != 0 || len(report.Protected) != 1 {
				t.Fatalf("late %s not protected: %+v; %s", scenario, report, &stderr)
			}
			data, err := os.ReadFile(filepath.Join(wt, "README.md"))
			if err != nil || string(data) != "seed\n" {
				t.Fatalf("candidate changed: %q %v", data, err)
			}
		})
	}
}

func TestReapClosedBeadWorktrees_FinalContentCheckOwnsRetirementLock(t *testing.T) {
	city, rig := initReapRig(t)
	wt := addClosedWorktree(t, rig, city, "builder", "ga-lock01")
	store := beads.NewMemStoreFrom(1, []beads.Bead{{ID: "ga-lock01", Status: "closed"}}, nil)
	injectLiveness(t, liveWorktreeState{scanned: true})
	probe := &reapLockCheckingProbe{gitProbe: git.New(wt), t: t, rig: rig, path: wt}
	injectReapGitProbe(t, wt, probe)
	var stderr bytes.Buffer
	report := reapClosedBeadWorktrees(city, reapTestConfig(rig), reapTestScope(map[string]beads.Store{reapTestRigName: store}), nil, false, events.Discard, nil, &stderr)
	if len(report.Reaped) != 1 || probe.calls < 2 {
		t.Fatalf("removal/final content check missing: %+v calls=%d; %s", report, probe.calls, &stderr)
	}
}

type reapLockCheckingProbe struct {
	gitProbe
	t         *testing.T
	rig, path string
	calls     int
}

func (p *reapLockCheckingProbe) VerifyTrackedContent(head string) error {
	p.calls++
	if p.calls > 1 {
		unlock, err := worktree.TryLockRetirement(p.rig, p.path)
		if err == nil {
			unlock()
			p.t.Error("final content check ran outside canonical retirement lock")
		} else if !errors.Is(err, worktree.ErrRetirementLockBusy) {
			p.t.Fatalf("unexpected lock error: %v", err)
		}
	}
	return p.gitProbe.VerifyTrackedContent(head)
}
