package main

import (
	"context"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
)

type blockedMaintenanceHistory struct {
	events.Provider
	entered chan struct{}
	release chan struct{}
	lastRun time.Time
}

func (p *blockedMaintenanceHistory) List(_ events.Filter) ([]events.Event, error) {
	close(p.entered)
	<-p.release
	return []events.Event{{Type: events.StoreMaintenanceDone, Ts: p.lastRun}}, nil
}

func TestMaintenanceHistoryDoesNotBlockCityStartup(t *testing.T) {
	for _, cancelBeforeSeed := range []bool{false, true} {
		name := "seed-before-scheduling"
		if cancelBeforeSeed {
			name = "cancel-before-seed"
		}
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				p := &blockedMaintenanceHistory{Provider: events.NewFake(), entered: make(chan struct{}), release: make(chan struct{}), lastRun: time.Now().Add(-time.Hour)}
				cs := &controllerState{cfg: &config.City{Maintenance: config.MaintenanceConfig{Dolt: config.DoltMaintenance{Enabled: true}}}, eventProv: p}
				returned := make(chan struct{})
				go func() {
					cs.startMaintenanceLoop(ctx)
					close(returned)
				}()
				synctest.Wait()
				select {
				case <-p.entered:
				default:
					t.Error("maintenance history was not read")
				}
				select {
				case <-returned:
				default:
					t.Error("city startup waits for retained maintenance history")
				}
				if cs.maintenanceLoop != nil {
					t.Error("maintenance was published before history was seeded")
				}
				if cancelBeforeSeed {
					cancel()
				}
				close(p.release)
				synctest.Wait()
				if cancelBeforeSeed {
					if cs.maintenanceLoop != nil {
						t.Error("canceled startup published a maintenance loop")
					}
				} else if cs.maintenanceLoop == nil || !cs.maintenanceLoop.LastRunAt().Equal(p.lastRun) {
					t.Error("maintenance did not retain the historical scheduling fence")
				}
			})
		})
	}
}

// TestMaintenanceStartupLine verifies the always-on startup banner reports
// the interval and distinguishes the active (GC wired) and observe-only
// (no-op) modes, so operators can confirm the loop initialized from the
// supervisor log. (gascity ga-tp7)
func TestMaintenanceStartupLine(t *testing.T) {
	t.Run("observe-only-when-not-active", func(t *testing.T) {
		got := maintenanceStartupLine(168*time.Hour, false)
		for _, want := range []string{"store-maintenance: loop started", "interval=168h", "observe-only"} {
			if !strings.Contains(got, want) {
				t.Errorf("startup line missing %q\ngot: %q", want, got)
			}
		}
		if strings.Contains(got, "mode=active") {
			t.Errorf("observe-only line must not claim active mode; got: %q", got)
		}
	})

	t.Run("active-when-wired", func(t *testing.T) {
		got := maintenanceStartupLine(24*time.Hour, true)
		for _, want := range []string{"store-maintenance: loop started", "interval=24h", "mode=active"} {
			if !strings.Contains(got, want) {
				t.Errorf("startup line missing %q\ngot: %q", want, got)
			}
		}
		if strings.Contains(got, "observe-only") {
			t.Errorf("active line must not claim observe-only; got: %q", got)
		}
	})
}
