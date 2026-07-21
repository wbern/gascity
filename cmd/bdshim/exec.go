package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/gastownhall/gascity/internal/processretry"
)

// realBdEnvVar names the environment variable holding the absolute path of the
// real bd binary. Passthrough must resolve bd through this, never exec.LookPath,
// because this binary is installed as `bd` first on PATH and a LookPath would
// resolve back to it and recurse. Mirrors cmd/gc's realBdEnvVar (GC_BD_REAL).
const realBdEnvVar = "GC_BD_REAL"

// resolveRealBdPath returns the absolute path of the real bd binary. An explicit
// GC_BD_REAL remains the normal production path. The PATH fallback keeps the
// standalone binary usable, but it must never resolve to this running bdshim:
// doing so would recursively spawn the shim for every passthrough command.
func resolveRealBdPath() (string, error) {
	if raw := strings.TrimSpace(os.Getenv(realBdEnvVar)); raw != "" {
		if !filepath.IsAbs(raw) {
			return "", fmt.Errorf("%s must be an absolute path, got %q", realBdEnvVar, raw)
		}
		if _, err := os.Stat(raw); err != nil {
			return "", fmt.Errorf("%s=%q: %w", realBdEnvVar, raw, err)
		}
		if err := refuseSelfBdPath(raw, realBdEnvVar); err != nil {
			return "", err
		}
		return raw, nil
	}
	path, err := exec.LookPath("bd")
	if err != nil {
		return "", fmt.Errorf("bd not found: set %s to the real bd binary or put bd on PATH: %w", realBdEnvVar, err)
	}
	if err := refuseSelfBdPath(path, "PATH"); err != nil {
		return "", err
	}
	return path, nil
}

// refuseSelfBdPath rejects a passthrough target that names this executable. It
// compares file identity rather than spelling so a city shimbin symlink and a
// hard link are both refused before execRealBd can spawn a child.
func refuseSelfBdPath(candidate, source string) error {
	candidateInfo, err := os.Stat(candidate)
	if err != nil {
		return fmt.Errorf("statting bd passthrough target %q from %s: %w", candidate, source, err)
	}
	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolving bdshim executable before %s passthrough: %w", source, err)
	}
	selfInfo, err := os.Stat(self)
	if err != nil {
		return fmt.Errorf("statting bdshim executable %q before %s passthrough: %w", self, source, err)
	}
	if os.SameFile(candidateInfo, selfInfo) {
		return fmt.Errorf("refusing recursive bdshim passthrough: %s target %q resolves to this executable %q", source, candidate, self)
	}
	return nil
}

// execRealBd runs the real bd binary with the given args, streaming its stdio and
// propagating its exit code (preserving bd's exit-code contract). A nil env
// defaults to the process environment and an empty dir defaults to the caller's
// cwd — which is correct for an agent call, whose env and cwd already scope the
// store. Mirrors cmd/gc's execRealBd.
func execRealBd(args []string, dir string, env []string, stdin io.Reader, stdout, stderr io.Writer) int {
	bdPath, err := resolveRealBdPath()
	if err != nil {
		fmt.Fprintf(stderr, "bdshim: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	if env == nil {
		env = os.Environ()
	}
	err = processretry.RunWithTransientStartRetry(func() error {
		cmd := exec.Command(bdPath, args...)
		if dir != "" {
			cmd.Dir = dir
		}
		cmd.Stdin = stdin
		cmd.Stdout = stdout
		cmd.Stderr = stderr
		cmd.Env = env
		return cmd.Run()
	})
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return exitErr.ExitCode()
		}
		fmt.Fprintf(stderr, "bdshim: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	return 0
}
