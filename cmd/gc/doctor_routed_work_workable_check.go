package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/agentutil"
	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/suspensionstate"
	"github.com/gastownhall/gascity/internal/workable"
)

type routedWorkWorkableCheck struct {
	cfg           *config.City
	cityPath      string
	newStore      func(string) (beads.Store, error)
	eventsRec     events.Recorder
	isHeadCurrent func(prNumber, head string) (bool, bool)
}

func newRoutedWorkWorkableCheck(cfg *config.City, cityPath string, newStore func(string) (beads.Store, error)) *routedWorkWorkableCheck {
	var rec events.Recorder
	if cfg != nil {
		eventsPath := filepath.Join(cityPath, ".gc", "events.jsonl")
		p, err := newEventsProviderForNameWithConfig(cfg.Events.Provider, eventsPath, io.Discard, cfg.Events)
		if err == nil {
			rec = p
		}
	}
	return &routedWorkWorkableCheck{
		cfg:       cfg,
		cityPath:  cityPath,
		newStore:  newStore,
		eventsRec: rec,
	}
}

func (c *routedWorkWorkableCheck) Name() string { return "routed-work-workable" }

func (c *routedWorkWorkableCheck) CanFix() bool { return true }

type unworkableFinding struct {
	scope  string
	store  beads.Store
	bead   beads.Bead
	reason string
}

func (f unworkableFinding) describe() string {
	return fmt.Sprintf("%s bead %s (routed to %q) is unworkable: %s",
		f.scope, f.bead.ID, f.bead.Metadata[beadmeta.RoutedToMetadataKey], f.reason)
}

func (c *routedWorkWorkableCheck) Run(_ *doctor.CheckContext) *doctor.CheckResult {
	if c.cfg == nil {
		return okCheck(c.Name(), "no config available")
	}

	findings, skipped := c.collect()
	if len(findings) == 0 && len(skipped) == 0 {
		return okCheck(c.Name(), "all open routed beads are workable")
	}

	details := make([]string, 0, len(findings)+len(skipped))
	for _, f := range findings {
		details = append(details, f.describe())
	}
	details = append(details, skipped...)
	sort.Strings(details)

	if len(findings) == 0 {
		return warnCheck(c.Name(),
			fmt.Sprintf("routed work workable check skipped %d scope(s)", len(skipped)),
			"fix bead store access, then rerun gc doctor",
			details)
	}

	msg := fmt.Sprintf("%d unworkable routed bead(s) found", len(findings))
	if len(skipped) > 0 {
		msg = fmt.Sprintf("%s; %d scope(s) skipped", msg, len(skipped))
	}
	return warnCheck(c.Name(),
		msg,
		"run gc doctor --fix to park unworkable beads (clear route, stamp reason, and defer)",
		details)
}

func (c *routedWorkWorkableCheck) Fix(_ *doctor.CheckContext) error {
	if c.cfg == nil {
		return nil
	}

	findings, _ := c.collect()
	if len(findings) == 0 {
		return nil
	}

	callCtx := context.Background()

	var errs []error
	for _, f := range findings {
		err := workable.ParkBead(callCtx, f.bead, f.reason, 10*time.Minute, f.store, c.eventsRec)
		if err != nil {
			errs = append(errs, fmt.Errorf("parking bead %s: %w", f.bead.ID, err))
		}
	}

	return errors.Join(errs...)
}

func (c *routedWorkWorkableCheck) collect() (findings []unworkableFinding, skipped []string) {
	scopes := []struct{ label, path string }{{"city", c.cityPath}}
	if c.cfg != nil {
		suspState, _ := loadSuspensionState(fsys.OSFS{}, c.cityPath)
		for _, rig := range c.cfg.Rigs {
			if suspensionstate.EffectiveRigSuspended(suspState, rig.Name, rig.EffectiveSuspendedOnStart()) || strings.TrimSpace(rig.Path) == "" {
				continue
			}
			scopes = append(scopes, struct{ label, path string }{"rig " + rig.Name, rig.Path})
		}
	}

	checkCtx := &workable.Context{
		AgentExists: func(target string) bool {
			if c.cfg == nil {
				return false
			}
			_, ok := agentutil.ResolveAgent(c.cfg, target, agentutil.ResolveOpts{AllowPoolMembers: true})
			return ok
		},
		IsHeadCurrent: c.isHeadCurrent,
		WorkKinds:     c.cfg.WorkKinds,
	}

	for _, sc := range scopes {
		if c.newStore == nil || strings.TrimSpace(sc.path) == "" {
			continue
		}
		store, err := c.newStore(sc.path)
		if err != nil {
			skipped = append(skipped, fmt.Sprintf("%s skipped: opening bead store: %v", sc.label, err))
			continue
		}

		allOpen, err := store.List(beads.ListQuery{Status: "open", AllowScan: true})
		if err != nil {
			skipped = append(skipped, fmt.Sprintf("%s skipped: listing beads: %v", sc.label, err))
			continue
		}

		for _, b := range allOpen {
			if strings.TrimSpace(b.Metadata[beadmeta.RoutedToMetadataKey]) == "" {
				continue
			}
			res := workable.Check(b, checkCtx)
			if !res.Workable {
				findings = append(findings, unworkableFinding{
					scope:  sc.label,
					store:  store,
					bead:   b,
					reason: res.Reason,
				})
			}
		}
	}

	return findings, skipped
}
