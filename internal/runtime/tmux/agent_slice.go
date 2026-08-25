package tmux

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/gastownhall/gascity/internal/shellquote"
)

// AgentSliceEnv names the environment variable that, when set to a systemd
// user slice (e.g. "gascity-agents.slice"), makes the tmux provider wrap
// every pane's initial command in a transient systemd user scope:
//
//	systemd-run --user --scope --unit=gascity-pane-<random>.scope --slice=<slice> --collect --quiet -- sh -c '<command>'
//
// Rationale: systemd-enabled tmux builds (stock Ubuntu) move every pane into
// a transient tmux-spawn-*.scope under the default user slice, so agent
// processes escape whatever slice the tmux server itself runs in. Wrapping
// the pane command re-parents the agent's process tree into a dedicated user
// slice where resource weights can be applied. Default-off: when unset, pane
// commands run unwrapped exactly as before.
const AgentSliceEnv = "GC_AGENT_SLICE"

// agentSliceProbeTimeout bounds the one-time systemd-run availability probe.
// Test-overridable.
var agentSliceProbeTimeout = 5 * time.Second

var gasCityPaneScopePattern = regexp.MustCompile(`^gascity-pane-[a-f0-9]{32}\.scope$`)

// newGasCityPaneScope returns a collision-resistant scope name for one pane
// incarnation. It intentionally has its own namespace rather than borrowing
// tmux's outer transient scope, which may contain sibling panes.
func newGasCityPaneScope() (string, error) {
	var entropy [16]byte
	if _, err := rand.Read(entropy[:]); err != nil {
		return "", fmt.Errorf("creating pane scope identity: %w", err)
	}
	return "gascity-pane-" + hex.EncodeToString(entropy[:]) + ".scope", nil
}

func isGasCityPaneScope(unit string) bool {
	return gasCityPaneScopePattern.MatchString(unit)
}

// wrapperCommands lists pane-root wrapper binaries produced by pane-command
// wrapping. A wrapped pane reports the wrapper as pane_current_command for
// the pane's whole lifetime, so command-wait and detection paths must treat
// these like shells: the agent is identified through descendant inspection,
// never by the pane command itself.
var wrapperCommands = []string{"systemd-run"}

// isWrapperCommand reports whether cmd is a known pane-root wrapper binary
// (see wrapperCommands).
func isWrapperCommand(cmd string) bool {
	for _, w := range wrapperCommands {
		if cmd == w {
			return true
		}
	}
	return false
}

// probeAgentSliceSupport verifies that systemd-run exists and the systemd
// user manager responds by running a no-op command in a transient scope on
// the target slice. The probe runs in the gc process's environment, while
// pane commands execute with the tmux server's environment. gc normally
// spawns the tmux server itself, so the server inherits gc's environment
// and the probe is representative — but a pre-existing server whose global
// environment lacks a reachable user bus (XDG_RUNTIME_DIR,
// DBUS_SESSION_BUS_ADDRESS) can still fail wrapped spawns after a
// successful probe here.
func probeAgentSliceSupport(slice string) error {
	if _, err := exec.LookPath("systemd-run"); err != nil {
		return fmt.Errorf("systemd-run not found: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), agentSliceProbeTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "systemd-run",
		"--user", "--scope", "--slice="+slice, "--collect", "--quiet", "--", "true")
	out, err := cmd.CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if msg != "" {
			return fmt.Errorf("systemd user manager probe failed: %w: %s", err, msg)
		}
		return fmt.Errorf("systemd user manager probe failed: %w", err)
	}
	return nil
}

// agentSliceWrapper decides whether pane commands are wrapped in a transient
// systemd user scope. The availability probe runs at most once per Tmux
// instance; on failure it warns once and all subsequent commands run
// unwrapped (graceful fallback).
type agentSliceWrapper struct {
	probe func(slice string) error // test seam; nil means probeAgentSliceSupport
	warn  io.Writer                // test seam; nil means the standard logger
	once  sync.Once
	ok    bool
}

// wrap returns command wrapped for the given slice, or command unchanged
// when slice is empty, command is empty, or transient user scopes are
// unavailable on this host.
func (w *agentSliceWrapper) wrap(slice, command string) (string, string) {
	if slice == "" || command == "" {
		return command, ""
	}
	w.once.Do(func() {
		probe := w.probe
		if probe == nil {
			probe = probeAgentSliceSupport
		}
		if err := probe(slice); err != nil {
			msg := fmt.Sprintf("%s=%q set but transient user scopes are unavailable; pane commands run unwrapped: %v",
				AgentSliceEnv, slice, err)
			if w.warn != nil {
				_, _ = fmt.Fprintln(w.warn, "gc: "+msg)
			} else {
				log.Printf("tmux agent slice: %s", msg)
			}
			return
		}
		w.ok = true
	})
	if !w.ok {
		return command, ""
	}
	unit, err := newGasCityPaneScope()
	if err != nil {
		msg := fmt.Sprintf("creating a dedicated pane scope failed; pane command runs unwrapped: %v", err)
		if w.warn != nil {
			_, _ = fmt.Fprintln(w.warn, "gc: "+msg)
		} else {
			log.Printf("tmux agent slice: %s", msg)
		}
		return command, ""
	}
	return shellquote.Join([]string{
		"systemd-run", "--user", "--scope", "--unit=" + unit, "--slice=" + slice,
		"--collect", "--quiet", "--", "sh", "-c", command,
	}), unit
}

// wrapPaneCommand applies the GC_AGENT_SLICE systemd user-scope wrapper to a
// pane's initial command. See [AgentSliceEnv]. The environment variable is
// read per call but the availability probe result is cached, so the first
// non-empty slice value decides whether wrapping is active for this Tmux.
func (t *Tmux) wrapPaneCommand(target, command string) string {
	wrapped, unit := t.agentSlice.wrap(os.Getenv(AgentSliceEnv), command)
	t.rememberPendingOwnedScope(target, unit)
	return wrapped
}
