package ssh

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
	"time"
)

// stagedEnv holds remote paths used to keep credential-bearing environment
// values off both local ssh and remote shell command lines.
type stagedEnv struct {
	dir      string
	envPath  string
	tmuxPath string
}

func (s stagedEnv) staged() bool { return s.dir != "" }

const (
	envFileName             = "env.sh"
	tmuxFileName            = "session.tmux"
	stagedDirPrefix         = "gc-session-"
	staleStagedDirMinutes   = 60
	cleanupStagedEnvTimeout = 10 * time.Second
)

// stageSecretEnv sends secrets only over stdin to a remote shell, which writes
// them under a private directory. tmuxArgv is staged for Start; Relaunch passes
// nil because it reuses the session environment.
func (p *Provider) stageSecretEnv(ctx context.Context, secretEnv map[string]string, tmuxArgv []string) (stagedEnv, error) {
	if len(secretEnv) == 0 {
		return stagedEnv{}, nil
	}
	envBody := shellEnvFile(secretEnv)
	var tmuxBody string
	if len(tmuxArgv) > 0 {
		tmuxBody = tmuxCommandLine(tmuxArgv) + "\n"
	}
	script, err := stagingScript(envBody, tmuxBody)
	if err != nil {
		return stagedEnv{}, err
	}
	out, code, err := p.conn.execScript(ctx, []byte(script))
	if err != nil {
		return stagedEnv{}, fmt.Errorf("staging session env on box: %w", err)
	}
	dir := lastLine(string(out))
	if code != 0 || !strings.HasPrefix(dir, "/") || !strings.Contains(dir, "/"+stagedDirPrefix) {
		return stagedEnv{}, fmt.Errorf("staging session env on box: staging script exited %d without a staged directory path", code)
	}
	staged := stagedEnv{dir: dir, envPath: dir + "/" + envFileName}
	if tmuxBody != "" {
		staged.tmuxPath = dir + "/" + tmuxFileName
	}
	return staged, nil
}

func (p *Provider) cleanupStagedEnv(ctx context.Context, name string, staged stagedEnv) {
	if !staged.staged() {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupStagedEnvTimeout)
	defer cancel()
	_, _, _ = p.conn.Exec(ctx, name, []string{"rm", "-rf", staged.dir})
}

func (p *Provider) launchSession(ctx context.Context, name string, args []string, staged stagedEnv) (string, int, error) {
	if staged.tmuxPath == "" {
		return p.tmux(ctx, name, args...)
	}
	return p.tmux(ctx, name, "start-server", ";", "source-file", staged.tmuxPath)
}

func stagingScript(envBody, tmuxBody string) (string, error) {
	envTag, err := heredocTag(envBody)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	b.WriteString("umask 077\n")
	b.WriteString(`t="${TMPDIR:-/tmp}"` + "\n")
	fmt.Fprintf(&b, `find "$t" -maxdepth 1 -type d -name '%s*' -mmin +%d -exec rm -rf {} + 2>/dev/null`+"\n", stagedDirPrefix, staleStagedDirMinutes)
	fmt.Fprintf(&b, `d=$(mktemp -d "$t/%sXXXXXX") || exit 1`+"\n", stagedDirPrefix)
	b.WriteString(`fail() { rm -rf "$d"; exit 1; }` + "\n")
	writeFile := func(name, body, tag string) {
		b.WriteString(`cat > "$d/` + name + `" <<'` + tag + `' || fail` + "\n")
		b.WriteString(body)
		b.WriteString(tag + "\n")
		fmt.Fprintf(&b, `[ "$(wc -c < "$d/%s")" -eq %d ] || fail`+"\n", name, len(body))
	}
	writeFile(envFileName, envBody, envTag)
	if tmuxBody != "" {
		tmuxTag, err := heredocTag(tmuxBody)
		if err != nil {
			return "", err
		}
		writeFile(tmuxFileName, tmuxBody, tmuxTag)
	}
	b.WriteString(`chmod 700 "$d" && chmod 600 "$d"/* || fail` + "\n")
	b.WriteString(`printf '%s\n' "$d"` + "\n")
	return b.String(), nil
}

func lastLine(s string) string {
	lines := strings.Split(s, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if line := strings.TrimSpace(lines[i]); line != "" {
			return line
		}
	}
	return ""
}

func heredocTag(body string) (string, error) {
	for attempt := 0; attempt < 8; attempt++ {
		var raw [8]byte
		if _, err := rand.Read(raw[:]); err != nil {
			return "", fmt.Errorf("generating heredoc delimiter: %w", err)
		}
		tag := "GC_EOF_" + strings.ToUpper(hex.EncodeToString(raw[:]))
		if !strings.Contains(body, tag) {
			return tag, nil
		}
	}
	return "", fmt.Errorf("generating heredoc delimiter: no delimiter free of the staged content")
}

func shellEnvFile(env map[string]string) string {
	var b strings.Builder
	for _, key := range sortedKeys(env) {
		b.WriteString(key + "=" + shellQuote([]string{env[key]}) + "\n")
		b.WriteString("export " + key + "\n")
	}
	return b.String()
}

func tmuxCommandLine(args []string) string {
	quoted := make([]string, len(args))
	for i, arg := range args {
		quoted[i] = tmuxQuote(arg)
	}
	return strings.Join(quoted, " ")
}

func tmuxQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
