package tmux

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

const (
	stagedDirPrefix   = "gc-tmux-session-"
	stagedFileName    = "session.tmux"
	staleStagedDirAge = time.Hour
)

// runNewSession issues a prepared new-session argv, keeping secret environment
// values out of the process argument vector. An all-inert environment keeps
// the plain argv path; when secret staging fails, it fails closed rather than
// falling back to the argv leak this path prevents.
func (t *Tmux) runNewSession(args []string, env map[string]string) error {
	if !runtime.EnvHasArgvSecrets(env) {
		_, err := t.run(args...)
		return err
	}
	sweepStaleStagedDirs()
	path, cleanup, err := stageTmuxCommandFile(tmuxCommandLine(args))
	if err != nil {
		return fmt.Errorf("staging tmux command file (session env holds secrets that must not reach argv): %w", err)
	}
	defer cleanup()
	_, err = t.run("start-server", ";", "source-file", path)
	return err
}

// stageTmuxCommandFile writes a command to a 0600 file in a 0700 directory.
func stageTmuxCommandFile(line string) (path string, cleanup func(), err error) {
	dir, err := os.MkdirTemp("", stagedDirPrefix+"*")
	if err != nil {
		return "", nil, err
	}
	remove := func() { _ = os.RemoveAll(dir) }
	if err := os.Chmod(dir, 0o700); err != nil {
		remove()
		return "", nil, err
	}
	path = filepath.Join(dir, stagedFileName)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		remove()
		return "", nil, err
	}
	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		remove()
		return "", nil, err
	}
	if _, err := f.WriteString(line + "\n"); err != nil {
		_ = f.Close()
		remove()
		return "", nil, err
	}
	if err := f.Close(); err != nil {
		remove()
		return "", nil, err
	}
	return path, remove, nil
}

// sweepStaleStagedDirs collects orphaned private files left by a process killed
// between staging and deferred cleanup. Failures are deliberately best-effort:
// this is litter collection, never permission to bypass secret staging.
func sweepStaleStagedDirs() {
	entries, err := os.ReadDir(os.TempDir())
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-staleStagedDirAge)
	for _, entry := range entries {
		if !entry.IsDir() || !strings.HasPrefix(entry.Name(), stagedDirPrefix) {
			continue
		}
		info, err := entry.Info()
		if err != nil || info.ModTime().After(cutoff) {
			continue
		}
		_ = os.RemoveAll(filepath.Join(os.TempDir(), entry.Name()))
	}
}

func tmuxCommandLine(args []string) string {
	quoted := make([]string, len(args))
	for i, arg := range args {
		quoted[i] = tmuxQuote(arg)
	}
	return strings.Join(quoted, " ")
}

func tmuxQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'"
}
