package ssh

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

func TestShellEnvFileExportsSortedEntries(t *testing.T) {
	got := shellEnvFile(map[string]string{"B": "two", "A": `a'b`})
	want := "A='a'\\''b'\nexport A\nB='two'\nexport B\n"
	if got != want {
		t.Errorf("shellEnvFile = %q, want %q", got, want)
	}
}

func TestStagingScriptUsesPrivateVerifiedFiles(t *testing.T) {
	envBody := "K='v'\nexport K\n"
	tmuxBody := "'new-session' '-d'\n"
	script, err := stagingScript(envBody, tmuxBody)
	if err != nil {
		t.Fatalf("stagingScript: %v", err)
	}
	for _, want := range []string{
		"umask 077",
		`mktemp -d "$t/` + stagedDirPrefix + `XXXXXX"`,
		`cat > "$d/env.sh" <<'`,
		`cat > "$d/session.tmux" <<'`,
		fmt.Sprintf(`[ "$(wc -c < "$d/env.sh")" -eq %d ] || fail`, len(envBody)),
		fmt.Sprintf(`[ "$(wc -c < "$d/session.tmux")" -eq %d ] || fail`, len(tmuxBody)),
		`chmod 700 "$d" && chmod 600 "$d"/* || fail`,
		`-name 'gc-session-*' -mmin +60`,
	} {
		if !strings.Contains(script, want) {
			t.Errorf("staging script missing %q", want)
		}
	}
}

func TestStageSecretEnvRejectsUnsafeRemoteOutput(t *testing.T) {
	for name, output := range map[string]string{
		"relative":         "gc-session-x\n",
		"unrelated":        "/etc\n",
		"trailing chatter": "/tmp/gc-session-x\nchatter\n",
		"empty":            "",
	} {
		t.Run(name, func(t *testing.T) {
			f := &fakeRunner{out: []byte(output)}
			p := providerWith(f)
			if _, err := p.stageSecretEnv(context.Background(), map[string]string{"K": "v"}, nil); err == nil {
				t.Fatal("stageSecretEnv must reject unsafe remote output")
			}
		})
	}
}

func TestStageSecretEnvUsesLastLineAndIsNoOpWithoutSecrets(t *testing.T) {
	f := &fakeRunner{out: []byte("profile greeting\n/tmp/gc-session-x\n")}
	p := providerWith(f)
	staged, err := p.stageSecretEnv(context.Background(), map[string]string{"K": "v"}, nil)
	if err != nil {
		t.Fatalf("stageSecretEnv: %v", err)
	}
	if staged.dir != "/tmp/gc-session-x" || staged.tmuxPath != "" {
		t.Errorf("staged = %+v", staged)
	}
	f = &fakeRunner{}
	p = providerWith(f)
	staged, err = p.stageSecretEnv(context.Background(), nil, []string{"new-session"})
	if err != nil || staged.staged() || len(f.calls) != 0 {
		t.Errorf("no-secret staging = (%+v, %v), calls=%v", staged, err, f.calls)
	}
}
