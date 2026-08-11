package main

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
	"github.com/gastownhall/gascity/internal/hooks"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/shellquote"
)

func TestCodexHooksDriftCheckReportsManagedMissingPreCompact(t *testing.T) {
	dir := t.TempDir()
	writeCodexHooksForDoctorTest(t, dir, `{
  "hooks": {
    "SessionStart": [{
      "hooks": [{
        "type": "command",
        "command": "export PATH=\"$HOME/go/bin:$HOME/.local/bin:$PATH\" && gc prime --hook --hook-format codex"
      }]
    }]
  }
}`)

	check := newCodexHooksDriftCheck(dir, []string{dir})
	result := check.Run(&doctor.CheckContext{})

	if result.Status != doctor.StatusWarning {
		t.Fatalf("status = %v, want warning; message=%s", result.Status, result.Message)
	}
	if !strings.Contains(result.Message, "legacy Gas City handlers") {
		t.Fatalf("message = %q, want legacy handler diagnosis", result.Message)
	}
}

func TestCodexHooksDriftCheckReportsHybridHandlerCardinalityAndManagedBehaviors(t *testing.T) {
	dir := t.TempDir()
	city := shellquote.Quote(dir)
	writeCodexHooksForDoctorTest(t, dir, fmt.Sprintf(`{
  "hooks": {
    "SessionStart": [{"hooks":[{"type":"command","command":"bd codex-hook session-start"}]}, {"matcher":"startup","hooks":[{"type":"command","command":"gc --city %s prime --hook --hook-format codex"}]}, {"matcher":"startup","hooks":[{"type":"command","command":"gc --city %s prime --hook --hook-format codex"}]}],
    "PreCompact": [{"hooks":[{"type":"command","command":"bd codex-hook pre-compact"}]}, {"matcher":"","hooks":[{"type":"command","command":"gc --city %s handoff --auto --hook-format codex \"context cycle\""}]}],
    "UserPromptSubmit": [{"hooks":[{"type":"command","command":"bd codex-hook prompt"}]}, {"matcher":"","hooks":[{"type":"command","command":"gc --city %s hook run --timeout 15s --timeout-exit-code 0 -- nudge drain --inject --hook-format codex"},{"type":"command","command":"gc --city %s hook run --timeout 15s --timeout-exit-code 0 -- mail check --inject --hook-format codex"}]}]
  }
}`, city, city, city, city, city))

	before, err := os.ReadFile(filepath.Join(dir, ".codex", "hooks.json"))
	if err != nil {
		t.Fatalf("ReadFile before Run: %v", err)
	}
	result := newCodexHooksDriftCheck(dir, []string{dir}).Run(&doctor.CheckContext{})
	after, err := os.ReadFile(filepath.Join(dir, ".codex", "hooks.json"))
	if err != nil {
		t.Fatalf("ReadFile after Run: %v", err)
	}

	if result.Status != doctor.StatusWarning {
		t.Fatalf("status = %v, want warning; message=%s details=%v", result.Status, result.Message, result.Details)
	}
	details := strings.Join(result.Details, "\n")
	for _, want := range []string{
		"source=project",
		"sha256=",
		"handlers=SessionStart:3,PreCompact:2,UserPromptSubmit:3",
		"managed=mail:1,nudge:1,pre-compact:1,session-start:2",
	} {
		if !strings.Contains(details, want) {
			t.Errorf("details missing %q:\n%s", want, details)
		}
	}
	if strings.Contains(details, "managed=bd-codex-hook") {
		t.Fatalf("custom bd codex-hook classified as managed:\n%s", details)
	}
	if string(after) != string(before) {
		t.Fatal("read-only doctor Run changed hooks.json")
	}
}

func TestCodexHooksDriftCheckReportsDuplicateOnlyCanonicalHandlers(t *testing.T) {
	dir := t.TempDir()
	city := shellquote.Quote(dir)
	writeCodexHooksForDoctorTest(t, dir, fmt.Sprintf(`{"hooks":{
  "SessionStart":[
    {"matcher":"startup","hooks":[{"type":"command","command":"gc --city %s prime --hook --hook-format codex"}]},
    {"matcher":"startup","hooks":[{"type":"command","command":"gc --city %s prime --hook --hook-format codex"}]}
  ],
  "PreCompact":[
    {"matcher":"","hooks":[{"type":"command","command":"gc --city %s handoff --auto --hook-format codex \"context cycle\""}]},
    {"matcher":"","hooks":[{"type":"command","command":"gc --city %s handoff --auto --hook-format codex \"context cycle\""}]}
  ],
  "UserPromptSubmit":[{"matcher":"","hooks":[
    {"type":"command","command":"gc --city %s hook run --timeout 15s --timeout-exit-code 0 -- nudge drain --inject --hook-format codex"},
    {"type":"command","command":"gc --city %s hook run --timeout 15s --timeout-exit-code 0 -- mail check --inject --hook-format codex"},
    {"type":"command","command":"gc --city %s hook run --timeout 15s --timeout-exit-code 0 -- nudge drain --inject --hook-format codex"},
    {"type":"command","command":"gc --city %s hook run --timeout 15s --timeout-exit-code 0 -- mail check --inject --hook-format codex"}
  ]}]
}}`, city, city, city, city, city, city, city, city))

	result := newCodexHooksDriftCheck(dir, []string{dir}).Run(&doctor.CheckContext{})

	if result.Status != doctor.StatusWarning {
		t.Fatalf("status = %v, want warning; message=%q details=%v", result.Status, result.Message, result.Details)
	}
	details := strings.Join(result.Details, "\n")
	for _, want := range []string{
		"handlers=SessionStart:2,PreCompact:2,UserPromptSubmit:4",
		"managed=mail:2,nudge:2,pre-compact:2,session-start:2",
	} {
		if !strings.Contains(details, want) {
			t.Errorf("details missing %q:\n%s", want, details)
		}
	}
}

func TestCodexHooksDriftCheckWarnsForDuplicateActiveFilesystemSources(t *testing.T) {
	cityDir := t.TempDir()
	userDir := filepath.Join(t.TempDir(), "user")
	rootDir := filepath.Join(t.TempDir(), "root")
	linkedDir := filepath.Join(t.TempDir(), "linked")
	city := shellquote.Quote(cityDir)
	current := fmt.Sprintf(`{"hooks":{"SessionStart":[{"matcher":"startup","hooks":[{"type":"command","command":"gc --city %s prime --hook --hook-format codex"}]}],"PreCompact":[{"matcher":"","hooks":[{"type":"command","command":"gc --city %s handoff --auto --hook-format codex \"context cycle\""}]}]}}`, city, city)
	writeCodexHooksForDoctorTest(t, userDir, current)
	writeCodexHooksForDoctorTest(t, rootDir, current)

	check := newCodexHooksDriftCheck(cityDir, []string{linkedDir})
	check.userHooksPath = filepath.Join(userDir, ".codex", "hooks.json")
	check.resolveProjectRoot = func(string) (string, error) { return rootDir, nil }
	check.ownership = codexHookOwnership{fileSourcesActive: true, filesystemOwned: true}
	result := check.Run(&doctor.CheckContext{})
	if result.Status == doctor.StatusOK {
		t.Fatalf("status = OK, want duplicate active source warning; message=%s details=%v", result.Message, result.Details)
	}
	details := strings.Join(result.Details, "\n")
	for _, want := range []string{check.userHooksPath, filepath.Join(rootDir, ".codex", "hooks.json"), "session-start:2"} {
		if !strings.Contains(details, want) {
			t.Fatalf("duplicate report missing %q:\n%s", want, details)
		}
	}
}

func TestCodexHooksDriftCheckKeepsIndependentConsumersSeparate(t *testing.T) {
	cityDir := t.TempDir()
	first, second := filepath.Join(t.TempDir(), "first"), filepath.Join(t.TempDir(), "second")
	installCodex(t, cityDir, first)
	installCodex(t, cityDir, second)
	check := newCodexHooksDriftCheck(cityDir, []string{first, second})
	check.userHooksPath = ""
	check.ownership = codexHookOwnership{fileSourcesActive: true, filesystemOwned: true}
	result := check.Run(&doctor.CheckContext{})
	if result.Status != doctor.StatusOK {
		t.Fatalf("status = %v, want OK for independent exact-one consumers; message=%s details=%v", result.Status, result.Message, result.Details)
	}
}

func TestCodexHooksDriftCheckIgnoresDormantConfiguredConsumerButBlocksLiveMissingOwner(t *testing.T) {
	cityDir := t.TempDir()
	bdDogDir := filepath.Join(cityDir, ".gc", "worktrees", "bd.dog")
	packsDir := filepath.Join(cityDir, ".gc", "worktrees", "gascity-packs")
	check := newCodexHooksDriftCheck(cityDir, []string{bdDogDir, packsDir})
	check.userHooksPath = ""
	check.ownership = codexHookOwnership{fileSourcesActive: true, filesystemOwned: true}
	check.activeConsumers = func() ([]codexHookConsumer, error) { return nil, nil }

	result := check.Run(&doctor.CheckContext{})
	if result.Status != doctor.StatusOK {
		t.Fatalf("dormant configured consumer status = %v, want OK; message=%q details=%v", result.Status, result.Message, result.Details)
	}
	if details := strings.Join(result.Details, "\n"); !strings.Contains(details, "consumer=configured-dormant workdir="+bdDogDir) || !strings.Contains(details, "consumer=configured-dormant workdir="+packsDir) {
		t.Fatalf("dormant consumers are not identified in details:\n%s", details)
	}

	check.activeConsumers = func() ([]codexHookConsumer, error) {
		return []codexHookConsumer{{workDir: bdDogDir, sessionName: "live-codex"}}, nil
	}
	result = check.Run(&doctor.CheckContext{})
	if result.Status != doctor.StatusWarning || !strings.Contains(result.Message, "not the exact current-city owner") {
		t.Fatalf("live missing owner status/message = %v/%q, want exact-owner warning; details=%v", result.Status, result.Message, result.Details)
	}
	if details := strings.Join(result.Details, "\n"); !strings.Contains(details, "consumer=runtime-active workdir="+bdDogDir+" sessions=live-codex") {
		t.Fatalf("live consumer identity missing from details:\n%s", details)
	}
}

func TestCodexHooksDriftCheckGroupsLiveLinkedWorktreeSessionsByRoot(t *testing.T) {
	cityDir := t.TempDir()
	root, linked := filepath.Join(cityDir, "root"), filepath.Join(cityDir, "linked")
	installCodex(t, cityDir, root)
	check := newCodexHooksDriftCheck(cityDir, []string{linked})
	check.userHooksPath = ""
	check.ownership = codexHookOwnership{fileSourcesActive: true, filesystemOwned: true}
	check.resolveProjectRoot = func(dir string) (string, error) {
		if dir == linked {
			return root, nil
		}
		return dir, nil
	}
	check.activeConsumers = func() ([]codexHookConsumer, error) {
		return []codexHookConsumer{{workDir: linked, sessionName: "one"}, {workDir: linked, sessionName: "two"}}, nil
	}

	result := check.Run(&doctor.CheckContext{})
	if result.Status != doctor.StatusOK {
		t.Fatalf("shared live root status = %v, want OK; message=%q details=%v", result.Status, result.Message, result.Details)
	}
	details := strings.Join(result.Details, "\n")
	if !strings.Contains(details, "consumer=runtime-active workdir="+root+" sessions=one,two") {
		t.Fatalf("grouped root/session details missing:\n%s", details)
	}
	if strings.Count(details, "consumer=runtime-active") != 1 {
		t.Fatalf("details have duplicate active consumer groups:\n%s", details)
	}
}

func TestCodexHooksDriftCheckFailsClosedWhenActiveConsumerEnumerationFails(t *testing.T) {
	cityDir := t.TempDir()
	check := newCodexHooksDriftCheck(cityDir, []string{filepath.Join(cityDir, "consumer")})
	check.userHooksPath = ""
	check.ownership = codexHookOwnership{fileSourcesActive: true, filesystemOwned: true}
	check.activeConsumers = func() ([]codexHookConsumer, error) { return nil, errors.New("session store offline") }

	result := check.Run(&doctor.CheckContext{})
	if result.Status != doctor.StatusWarning || !strings.Contains(result.Message, "cannot enumerate runtime-active Codex consumers") {
		t.Fatalf("enumeration failure status/message = %v/%q, want blocking diagnostic; details=%v", result.Status, result.Message, result.Details)
	}
}

func TestCodexHookConsumersFromSessionInfosRequiresPersistedActiveAndRuntimeLiveness(t *testing.T) {
	cfg := &config.City{
		Workspace: config.Workspace{InstallAgentHooks: []string{"codex"}},
		Agents: []config.Agent{
			{Name: "native-codex", Provider: "codex"},
			{Name: "hook-consumer", Provider: "claude"},
		},
	}
	infos := []session.Info{
		{SessionName: "stale-active", Template: "native-codex", Provider: "codex", State: session.StateActive, WorkDir: "/work/stale"},
		{SessionName: "asleep", Template: "native-codex", Provider: "codex", State: session.StateAsleep, WorkDir: "/work/asleep"},
		{SessionName: "suspended", Template: "native-codex", Provider: "codex", State: session.StateSuspended, WorkDir: "/work/suspended"},
		{SessionName: "closed", Template: "native-codex", Provider: "codex", State: session.StateActive, Closed: true, WorkDir: "/work/closed"},
		{SessionName: "start-pending", Template: "native-codex", Provider: "codex", State: session.StateStartPending, WorkDir: "/work/start-pending"},
		{SessionName: "unprovisioned", Template: "native-codex", Provider: "codex", State: session.StateActive},
		{SessionName: "live-codex", Template: "native-codex", Provider: "codex", State: session.StateActive, WorkDir: "/work/live-codex"},
		{SessionName: "live-install-hook", Template: "hook-consumer", Provider: "claude", State: session.StateActive, WorkDir: "/work/live-install-hook"},
	}
	observed := []string{}
	consumers, err := codexHookConsumersFromSessionInfos(cfg, infos, func(name string, _ []string) (runtime.Liveness, error) {
		observed = append(observed, name)
		return runtime.Liveness{Running: name != "stale-active"}, nil
	})
	if err != nil {
		t.Fatalf("codexHookConsumersFromSessionInfos: %v", err)
	}
	if got, want := consumers, []codexHookConsumer{
		{workDir: "/work/live-codex", sessionName: "live-codex"},
		{workDir: "/work/live-install-hook", sessionName: "live-install-hook"},
	}; !slices.Equal(got, want) {
		t.Fatalf("consumers = %#v, want %#v", got, want)
	}
	if got, want := observed, []string{"stale-active", "live-codex", "live-install-hook"}; !slices.Equal(got, want) {
		t.Fatalf("observed sessions = %v, want %v", got, want)
	}
}

func TestCodexHookConsumersFromSessionInfosFailsClosedOnLivenessError(t *testing.T) {
	infos := []session.Info{{SessionName: "live", Provider: "codex", State: session.StateActive, WorkDir: "/work/live"}}
	_, err := codexHookConsumersFromSessionInfos(&config.City{}, infos, func(string, []string) (runtime.Liveness, error) {
		return runtime.Liveness{}, errors.New("runtime status timed out")
	})
	if err == nil || !strings.Contains(err.Error(), "runtime status timed out") {
		t.Fatalf("error = %v, want liveness failure", err)
	}
}

func TestCodexHooksDriftCheckRejectsExactCountsBoundToWrongCity(t *testing.T) {
	cityDir := t.TempDir()
	wrongCity := filepath.Join(t.TempDir(), "other-city")
	writeCodexHooksForDoctorTest(t, cityDir, fmt.Sprintf(`{"hooks":{"SessionStart":[{"matcher":"startup","hooks":[{"type":"command","command":"gc --city %s prime --hook --hook-format codex"}]}],"PreCompact":[{"matcher":"","hooks":[{"type":"command","command":"gc --city %s handoff --auto --hook-format codex \"context cycle\""}]}],"UserPromptSubmit":[{"matcher":"","hooks":[{"type":"command","command":"gc --city %s hook run --timeout 15s --timeout-exit-code 0 -- nudge drain --inject --hook-format codex"},{"type":"command","command":"gc --city %s hook run --timeout 15s --timeout-exit-code 0 -- mail check --inject --hook-format codex"}]}]}}`, shellquote.Quote(wrongCity), shellquote.Quote(wrongCity), shellquote.Quote(wrongCity), shellquote.Quote(wrongCity)))
	check := newCodexHooksDriftCheck(cityDir, []string{cityDir})
	check.userHooksPath = ""
	check.ownership = codexHookOwnership{fileSourcesActive: true, filesystemOwned: true}

	result := check.Run(&doctor.CheckContext{})
	if result.Status != doctor.StatusWarning {
		t.Fatalf("status = %v, want warning for wrong-city binding; message=%s details=%v", result.Status, result.Message, result.Details)
	}
	if !strings.Contains(result.Message, "not the exact current-city owner") {
		t.Fatalf("message = %q, want structural/binding diagnosis", result.Message)
	}
}

func TestCodexHooksDriftCheckAuditsUserRootSessionFlagsAndInertLinkedWorktree(t *testing.T) {
	cityDir := t.TempDir()
	rootDir := filepath.Join(cityDir, "root")
	linkedDir := filepath.Join(cityDir, "linked")
	userPath := filepath.Join(cityDir, "home", ".codex", "hooks.json")
	for _, dir := range []string{rootDir, linkedDir, filepath.Dir(filepath.Dir(userPath))} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("MkdirAll(%s): %v", dir, err)
		}
	}
	writeCodexHooksForDoctorTest(t, rootDir, `{"hooks":{"SessionStart":[{"hooks":[{"type":"command","command":"gc prime --hook --hook-format codex"}]}]}}`)
	writeCodexHooksForDoctorTest(t, linkedDir, `{"hooks":{"PreCompact":[{"hooks":[{"type":"command","command":"gc handoff --auto --hook-format codex \"context cycle\""}]}]}}`)
	if err := os.MkdirAll(filepath.Dir(userPath), 0o755); err != nil {
		t.Fatalf("MkdirAll user hooks: %v", err)
	}
	if err := os.WriteFile(userPath, []byte(`{"hooks":{"SessionStart":[{"hooks":[{"type":"command","command":"bd codex-hook session-start"}]}]}}`), 0o644); err != nil {
		t.Fatalf("WriteFile user hooks: %v", err)
	}
	sourcePaths := []string{userPath, filepath.Join(rootDir, ".codex", "hooks.json"), filepath.Join(linkedDir, ".codex", "hooks.json")}
	before := map[string]string{}
	for _, path := range sourcePaths {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("ReadFile before Run %s: %v", path, err)
		}
		before[path] = string(data)
	}

	check := newCodexHooksDriftCheck(cityDir, []string{linkedDir})
	check.userHooksPath = userPath
	check.resolveProjectRoot = func(dir string) (string, error) {
		if dir != linkedDir {
			t.Fatalf("resolve project root dir = %q, want %q", dir, linkedDir)
		}
		return rootDir, nil
	}
	result := check.Run(&doctor.CheckContext{})

	if result.Status != doctor.StatusWarning {
		t.Fatalf("status = %v, want warning; message=%q details=%v", result.Status, result.Message, result.Details)
	}
	details := strings.Join(result.Details, "\n")
	for _, want := range []string{
		"source=sessionFlags active=true",
		"source=user active=true path=" + userPath,
		"source=project-root active=true path=" + filepath.Join(rootDir, ".codex", "hooks.json"),
		"source=inert-worktree active=false path=" + filepath.Join(linkedDir, ".codex", "hooks.json"),
		"source=active-total active=true managed=mail:1,nudge:1,pre-compact:1,session-start:2",
	} {
		if !strings.Contains(details, want) {
			t.Errorf("details missing %q:\n%s", want, details)
		}
	}
	userLine := codexDoctorDetailLine(details, "source=user ")
	if strings.Contains(userLine, "managed=") {
		t.Fatalf("user bd codex-hook classified as managed: %s", userLine)
	}
	for _, path := range sourcePaths {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("ReadFile after Run %s: %v", path, err)
		}
		if string(data) != before[path] {
			t.Errorf("read-only Run changed %s", path)
		}
	}
}

func TestResolveCodexProjectRootFollowsLinkedWorktreeCommonDir(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "root")
	linked := filepath.Join(base, "linked")
	admin := filepath.Join(root, ".git", "worktrees", "linked")
	if err := os.MkdirAll(admin, 0o755); err != nil {
		t.Fatalf("MkdirAll admin: %v", err)
	}
	if err := os.MkdirAll(linked, 0o755); err != nil {
		t.Fatalf("MkdirAll linked: %v", err)
	}
	if err := os.WriteFile(filepath.Join(linked, ".git"), []byte("gitdir: "+admin+"\n"), 0o644); err != nil {
		t.Fatalf("WriteFile .git: %v", err)
	}
	if err := os.WriteFile(filepath.Join(admin, "commondir"), []byte("../..\n"), 0o644); err != nil {
		t.Fatalf("WriteFile commondir: %v", err)
	}

	got, err := resolveCodexProjectRoot(linked)
	if err != nil {
		t.Fatalf("resolveCodexProjectRoot: %v", err)
	}
	if got != root {
		t.Fatalf("root = %q, want %q", got, root)
	}
}

func TestResolveCodexProjectRootKeepsSubmoduleCheckoutWithoutCommonDir(t *testing.T) {
	base := t.TempDir()
	project := filepath.Join(base, "project", "submodule")
	adminDir := filepath.Join(base, "project", ".git", "modules", "submodule")
	if err := os.MkdirAll(project, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(adminDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(project, ".git"), []byte("gitdir: "+adminDir+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := resolveCodexProjectRoot(project)
	if err != nil {
		t.Fatalf("resolveCodexProjectRoot: %v", err)
	}
	if got != project {
		t.Fatalf("resolved root = %q, want submodule checkout %q", got, project)
	}
}

func TestCodexHookSourcesRetainsLinkedRootProvenanceWhenRootIsAlsoConfigured(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "a-root")
	linked := filepath.Join(base, "z-linked")
	check := newCodexHooksDriftCheck(base, []string{root, linked})
	check.userHooksPath = ""
	check.resolveProjectRoot = func(dir string) (string, error) {
		if dir == linked {
			return root, nil
		}
		return dir, nil
	}

	sources := check.codexHookSources()
	rootPath := filepath.Join(root, ".codex", "hooks.json")
	for _, source := range sources {
		if source.path == rootPath {
			if source.kind != "project-root" || !source.active {
				t.Fatalf("root source = %+v, want active project-root", source)
			}
			return
		}
	}
	t.Fatalf("sources = %+v, want root source %s", sources, rootPath)
}

func TestCodexHooksDriftCheckReportsMalformedSourceLoudly(t *testing.T) {
	dir := t.TempDir()
	writeCodexHooksForDoctorTest(t, dir, `{not-json`)
	path := filepath.Join(dir, ".codex", "hooks.json")

	result := newCodexHooksDriftCheck(dir, []string{dir}).Run(&doctor.CheckContext{})

	if result.Status != doctor.StatusError {
		t.Fatalf("status = %v, want error; message=%q details=%v", result.Status, result.Message, result.Details)
	}
	details := strings.Join(result.Details, "\n")
	for _, want := range []string{"source=project", "path=" + path, "state=malformed", "invalid character"} {
		if !strings.Contains(details, want) {
			t.Errorf("details missing %q:\n%s", want, details)
		}
	}
}

func TestCodexHooksDriftCheckReportsUnreadableSourceLoudly(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".codex", "hooks.json")
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatalf("MkdirAll non-file hook path: %v", err)
	}

	result := newCodexHooksDriftCheck(dir, []string{dir}).Run(&doctor.CheckContext{})

	if result.Status != doctor.StatusError {
		t.Fatalf("status = %v, want error; message=%q details=%v", result.Status, result.Message, result.Details)
	}
	details := strings.Join(result.Details, "\n")
	for _, want := range []string{"path=" + path, "state=unreadable"} {
		if !strings.Contains(details, want) {
			t.Errorf("details missing %q:\n%s", want, details)
		}
	}
}

func TestCodexHooksDriftCheckReportsProjectRootResolutionFailure(t *testing.T) {
	dir := t.TempDir()
	check := newCodexHooksDriftCheck(dir, []string{dir})
	check.userHooksPath = ""
	check.resolveProjectRoot = func(string) (string, error) { return "", fmt.Errorf("broken gitdir pointer") }

	result := check.Run(&doctor.CheckContext{})

	if result.Status != doctor.StatusError {
		t.Fatalf("status = %v, want error; message=%q details=%v", result.Status, result.Message, result.Details)
	}
	details := strings.Join(result.Details, "\n")
	for _, want := range []string{"source=project-resolution", "path=" + dir, "state=unresolved", "broken gitdir pointer"} {
		if !strings.Contains(details, want) {
			t.Errorf("details missing %q:\n%s", want, details)
		}
	}
}

func TestCodexHooksDriftCheckReportsMissingSourceLocations(t *testing.T) {
	base := t.TempDir()
	project := filepath.Join(base, "project")
	userPath := filepath.Join(base, "home", ".codex", "hooks.json")
	if err := os.MkdirAll(project, 0o755); err != nil {
		t.Fatalf("MkdirAll project: %v", err)
	}
	check := newCodexHooksDriftCheck(base, []string{project})
	check.userHooksPath = userPath

	result := check.Run(&doctor.CheckContext{})

	if result.Status != doctor.StatusOK {
		t.Fatalf("status = %v, want ok; message=%q details=%v", result.Status, result.Message, result.Details)
	}
	details := strings.Join(result.Details, "\n")
	for _, want := range []string{
		"source=user active=true path=" + userPath + " state=missing",
		"source=project active=true path=" + filepath.Join(project, ".codex", "hooks.json") + " state=missing",
	} {
		if !strings.Contains(details, want) {
			t.Errorf("details missing %q:\n%s", want, details)
		}
	}
}

func TestCodexHooksDriftCheckRejectsInvalidCodexHookSchema(t *testing.T) {
	dir := t.TempDir()
	writeCodexHooksForDoctorTest(t, dir, `{"hooks":{"SessionStart":[{"hooks":[{"type":"command"}]}]}}`)

	result := newCodexHooksDriftCheck(dir, []string{dir}).Run(&doctor.CheckContext{})

	if result.Status != doctor.StatusError {
		t.Fatalf("status = %v, want error; message=%q details=%v", result.Status, result.Message, result.Details)
	}
	if details := strings.Join(result.Details, "\n"); !strings.Contains(details, "command hook requires a non-empty string command") {
		t.Fatalf("details do not explain invalid command handler:\n%s", details)
	}
}

func TestCodexHooksDriftCheckFixBacksUpAndSelectivelyRemovesManagedHandlers(t *testing.T) {
	dir := t.TempDir()
	writeCurrentCityCodexHooksForDoctorTest(t, dir, dir, `{
  "schemaVersion": 9007199254740993,
  "customTop": {"keep": true},
  "hooks": {
    "SessionStart": [{
      "matcher": "startup",
      "entryExtra": "keep-entry",
      "hooks": [
        {"type":"command","command":"bd codex-hook session-start","customExtra":"keep-handler"}
      ]
    }],
    "UserPromptSubmit": [{"matcher":"","hooks":[
      {"type":"command","command":"printf custom","unknown": {"keep": 1}}
    ]}],
    "FutureEvent": [{"type":"command","command":"gc prime --hook --hook-format codex","futureField":"keep"}]
  }
}`)
	path := filepath.Join(dir, ".codex", "hooks.json")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile before fix: %v", err)
	}
	check := newCodexHooksDriftCheck(dir, []string{dir})
	check.userHooksPath = ""

	if err := check.Fix(&doctor.CheckContext{}); err != nil {
		t.Fatalf("Fix: %v", err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile after fix: %v", err)
	}
	audit, err := hooks.AuditCodexHooks(after)
	if err != nil {
		t.Fatalf("AuditCodexHooks after fix: %v", err)
	}
	if len(audit.ManagedBehaviorCounts) != 0 {
		t.Fatalf("managed handlers remain after fix: %v\n%s", audit.ManagedBehaviorCounts, after)
	}
	for _, want := range []string{
		`"schemaVersion": 9007199254740993`,
		`"customTop"`,
		`"entryExtra": "keep-entry"`,
		`"command": "bd codex-hook session-start"`,
		`"customExtra": "keep-handler"`,
		`"command": "printf custom"`,
		`"FutureEvent"`,
		`"command": "gc prime --hook --hook-format codex"`,
		`"futureField": "keep"`,
	} {
		if !strings.Contains(string(after), want) {
			t.Errorf("migrated hooks missing %q:\n%s", want, after)
		}
	}
	backups := codexDoctorBackupFiles(t, filepath.Dir(path))
	if len(backups) != 1 {
		t.Fatalf("backup files = %v, want exactly one", backups)
	}
	backupData, err := os.ReadFile(filepath.Join(filepath.Dir(path), backups[0]))
	if err != nil {
		t.Fatalf("ReadFile backup: %v", err)
	}
	if string(backupData) != string(before) {
		t.Fatal("backup does not contain exact original bytes")
	}

	if err := check.Fix(&doctor.CheckContext{}); err != nil {
		t.Fatalf("second Fix: %v", err)
	}
	second, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile after second fix: %v", err)
	}
	if string(second) != string(after) {
		t.Fatal("repeat fix changed already-migrated bytes")
	}
	if got := codexDoctorBackupFiles(t, filepath.Dir(path)); strings.Join(got, "\n") != strings.Join(backups, "\n") {
		t.Fatalf("repeat fix changed backups: before=%v after=%v", backups, got)
	}
}

func TestCodexHooksDriftCheckFixMigratesUserRootAndInertWorktreeSources(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "root")
	linked := filepath.Join(base, "linked")
	userDir := filepath.Join(base, "home")
	for _, dir := range []string{root, linked, userDir} {
		writeCurrentCityCodexHooksForDoctorTest(t, base, dir, `{"hooks":{"SessionStart":[{"matcher":"startup","hooks":[{"type":"command","command":"bd codex-hook session-start"}]}]}}`)
	}
	userPath := filepath.Join(userDir, ".codex", "hooks.json")
	check := newCodexHooksDriftCheck(base, []string{linked})
	check.userHooksPath = userPath
	check.resolveProjectRoot = func(string) (string, error) { return root, nil }

	if err := check.Fix(&doctor.CheckContext{}); err != nil {
		t.Fatalf("Fix: %v", err)
	}
	for _, path := range []string{
		userPath,
		filepath.Join(root, ".codex", "hooks.json"),
		filepath.Join(linked, ".codex", "hooks.json"),
	} {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("ReadFile(%s): %v", path, err)
		}
		audit, err := hooks.AuditCodexHooks(data)
		if err != nil {
			t.Fatalf("AuditCodexHooks(%s): %v", path, err)
		}
		if len(audit.ManagedBehaviorCounts) != 0 {
			t.Errorf("managed handlers remain in %s: %v", path, audit.ManagedBehaviorCounts)
		}
		if !strings.Contains(string(data), "bd codex-hook session-start") {
			t.Errorf("custom bd handler missing from %s:\n%s", path, data)
		}
		if backups := codexDoctorBackupFiles(t, filepath.Dir(path)); len(backups) != 1 {
			t.Errorf("backup files beside %s = %v, want one", path, backups)
		}
	}
}

func TestCodexHooksDriftCheckFixPreflightsEverySourceBeforeWriting(t *testing.T) {
	base := t.TempDir()
	project := filepath.Join(base, "project")
	userDir := filepath.Join(base, "home")
	writeCodexHooksForDoctorTest(t, userDir, `{"hooks":{"SessionStart":[{"hooks":[{"type":"command","command":"gc prime --hook --hook-format codex"}]}]}}`)
	writeCodexHooksForDoctorTest(t, project, `{not-json`)
	userPath := filepath.Join(userDir, ".codex", "hooks.json")
	before, err := os.ReadFile(userPath)
	if err != nil {
		t.Fatalf("ReadFile user before fix: %v", err)
	}
	check := newCodexHooksDriftCheck(base, []string{project})
	check.userHooksPath = userPath

	if err := check.Fix(&doctor.CheckContext{}); err == nil {
		t.Fatal("Fix error = nil, want malformed project source to abort migration")
	}
	after, err := os.ReadFile(userPath)
	if err != nil {
		t.Fatalf("ReadFile user after fix: %v", err)
	}
	if string(after) != string(before) {
		t.Fatal("user source changed before all sources passed preflight")
	}
	if backups := codexDoctorBackupFiles(t, filepath.Dir(userPath)); len(backups) != 0 {
		t.Fatalf("backups created before all sources passed preflight: %v", backups)
	}
}

func TestCodexHooksDriftCheckFixPreflightsReplacementBeforeBackup(t *testing.T) {
	dir := t.TempDir()
	writeCodexHooksForDoctorTest(t, dir, `{"hooks":{"SessionStart":[{"hooks":[{"type":"command","command":"gc prime --hook --hook-format codex"}]}]}}`)
	path := filepath.Join(dir, ".codex", "hooks.json")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile before fix: %v", err)
	}
	check := newCodexHooksDriftCheck(dir, []string{dir})
	check.userHooksPath = ""
	check.preflightReplacement = func(string) error { return fmt.Errorf("replacement unavailable") }

	err = check.Fix(&doctor.CheckContext{})

	if err == nil || !strings.Contains(err.Error(), "replacement unavailable") {
		t.Fatalf("Fix error = %v, want replacement preflight failure", err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile after fix: %v", err)
	}
	if string(after) != string(before) {
		t.Fatal("replacement preflight failure changed hooks.json")
	}
	if backups := codexDoctorBackupFiles(t, filepath.Dir(path)); len(backups) != 0 {
		t.Fatalf("replacement preflight failure created backups: %v", backups)
	}
}

func TestCodexHooksDriftCheckFixCreatesEveryBackupBeforeFirstSourceWrite(t *testing.T) {
	base := t.TempDir()
	userDir := filepath.Join(base, "a-user")
	projectDir := filepath.Join(base, "z-project")
	custom := `{"hooks":{"SessionStart":[{"matcher":"startup","hooks":[{"type":"command","command":"printf custom"}]}]}}`
	writeCurrentCityCodexHooksForDoctorTest(t, base, userDir, custom)
	writeCurrentCityCodexHooksForDoctorTest(t, base, projectDir, custom)
	userPath := filepath.Join(userDir, ".codex", "hooks.json")
	projectPath := filepath.Join(projectDir, ".codex", "hooks.json")
	userBefore, err := os.ReadFile(userPath)
	if err != nil {
		t.Fatal(err)
	}
	projectBefore, err := os.ReadFile(projectPath)
	if err != nil {
		t.Fatal(err)
	}
	projectDigest := fmt.Sprintf("%x", sha256.Sum256(projectBefore))
	conflictingBackup := projectPath + ".gc-backup-" + projectDigest
	if err := os.WriteFile(conflictingBackup, []byte("not the original"), 0o600); err != nil {
		t.Fatal(err)
	}
	check := newCodexHooksDriftCheck(base, []string{projectDir})
	check.userHooksPath = userPath

	err = check.Fix(&doctor.CheckContext{})

	if err == nil || !strings.Contains(err.Error(), "non-matching Codex hook backup") {
		t.Fatalf("Fix error = %v, want conflicting later backup failure", err)
	}
	for path, before := range map[string][]byte{userPath: userBefore, projectPath: projectBefore} {
		after, readErr := os.ReadFile(path)
		if readErr != nil {
			t.Fatalf("ReadFile(%s): %v", path, readErr)
		}
		if string(after) != string(before) {
			t.Errorf("source %s changed before every backup was ready", path)
		}
	}
}

func TestCodexHooksDriftCheckFixRollsBackEarlierSourceWhenLaterWriteFails(t *testing.T) {
	base := t.TempDir()
	userDir := filepath.Join(base, "a-user")
	projectDir := filepath.Join(base, "z-project")
	custom := `{"hooks":{"SessionStart":[{"matcher":"startup","hooks":[{"type":"command","command":"printf custom"}]}]}}`
	writeCurrentCityCodexHooksForDoctorTest(t, base, userDir, custom)
	writeCurrentCityCodexHooksForDoctorTest(t, base, projectDir, custom)
	userPath := filepath.Join(userDir, ".codex", "hooks.json")
	projectPath := filepath.Join(projectDir, ".codex", "hooks.json")
	original := map[string][]byte{}
	for _, path := range []string{userPath, projectPath} {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("ReadFile(%s): %v", path, err)
		}
		original[path] = data
	}
	check := newCodexHooksDriftCheck(base, []string{projectDir})
	check.userHooksPath = userPath
	realWrite := check.writeFileAtomic
	writes := 0
	check.writeFileAtomic = func(path string, data []byte, mode os.FileMode) error {
		writes++
		if writes == 2 {
			return errors.New("injected second source write failure")
		}
		return realWrite(path, data, mode)
	}

	err := check.Fix(&doctor.CheckContext{})

	if err == nil || !strings.Contains(err.Error(), "injected second source write failure") {
		t.Fatalf("Fix error = %v, want injected later write failure", err)
	}
	if writes != 4 {
		t.Fatalf("atomic writes = %d, want failed second write plus two rollback writes", writes)
	}
	for path, want := range original {
		got, readErr := os.ReadFile(path)
		if readErr != nil {
			t.Fatalf("ReadFile(%s) after rollback: %v", path, readErr)
		}
		if string(got) != string(want) {
			t.Errorf("source %s was not restored byte-for-byte", path)
		}
		if backups := codexDoctorBackupFiles(t, filepath.Dir(path)); len(backups) != 1 {
			t.Errorf("backup files beside %s = %v, want one", path, backups)
		}
	}
}

func codexDoctorBackupFiles(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir(%s): %v", dir, err)
	}
	var backups []string
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "hooks.json.gc-backup-") {
			backups = append(backups, entry.Name())
		}
	}
	return backups
}

func codexDoctorDetailLine(details, prefix string) string {
	for _, line := range strings.Split(details, "\n") {
		if strings.HasPrefix(line, prefix) {
			return line
		}
	}
	return ""
}

func TestCodexHooksDriftCheckReportsCurrentFileHandlersAsLegacy(t *testing.T) {
	dir := t.TempDir()
	writeCodexHooksForDoctorTest(t, dir, fmt.Sprintf(`{
  "hooks": {
    "SessionStart": [{
      "hooks": [{
        "type": "command",
        "command": "export PATH=\"$HOME/go/bin:$HOME/.local/bin:$PATH\" && GC_MANAGED_SESSION_HOOK=1 GC_HOOK_EVENT_NAME=SessionStart gc --city %s prime --hook --hook-format codex"
      }]
    }],
    "PreCompact": [{
      "hooks": [{
        "type": "command",
        "command": "export PATH=\"$HOME/go/bin:$HOME/.local/bin:$PATH\" && gc --city %s handoff --auto --hook-format codex \"context cycle\""
      }]
    }]
  }
}`, shellquote.Quote(dir), shellquote.Quote(dir)))

	check := newCodexHooksDriftCheck(dir, []string{dir})
	result := check.Run(&doctor.CheckContext{})

	if result.Status != doctor.StatusWarning {
		t.Fatalf("status = %v, want warning; message=%s", result.Status, result.Message)
	}
}

func TestCodexHooksDriftCheckRetainsFilesystemHooksForNonT3Codex(t *testing.T) {
	dir := t.TempDir()
	writeCodexHooksForDoctorTest(t, dir, `{"hooks":{"SessionStart":[{"hooks":[{"type":"command","command":"gc prime --hook --hook-format codex"}]}]}}`)
	path := filepath.Join(dir, ".codex", "hooks.json")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile before doctor: %v", err)
	}
	cfg := &config.City{
		Session: config.SessionConfig{Provider: "tmux"},
		Agents:  []config.Agent{{Name: "worker", Provider: "codex"}},
	}
	check := newCodexHooksDriftCheck(dir, []string{dir}, cfg)
	check.userHooksPath = ""
	check.activeConsumers = func() ([]codexHookConsumer, error) {
		return []codexHookConsumer{{workDir: dir, sessionName: "live-worker"}}, nil
	}

	result := check.Run(&doctor.CheckContext{})

	if result.Status != doctor.StatusWarning {
		t.Fatalf("status = %v, want warning for incomplete filesystem-owned hooks; message=%q details=%v", result.Status, result.Message, result.Details)
	}
	details := strings.Join(result.Details, "\n")
	for _, want := range []string{
		"source=sessionFlags active=false",
		"source=project active=true",
		"managed=session-start:1",
		"consumer=runtime-active workdir=" + dir + " sessions=live-worker",
	} {
		if !strings.Contains(details, want) {
			t.Errorf("details missing %q:\n%s", want, details)
		}
	}
	if !strings.Contains(result.Message, "not the exact current-city owner") {
		t.Errorf("message = %q, want exact owner diagnosis", result.Message)
	}
	if check.CanFix() {
		t.Fatal("CanFix = true for non-T3 Codex filesystem ownership")
	}
	if err := check.Fix(&doctor.CheckContext{}); err == nil {
		t.Fatal("Fix error = nil, want migration refusal for non-T3 Codex")
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile after refused fix: %v", err)
	}
	if string(after) != string(before) {
		t.Fatal("refused fix changed filesystem-owned Codex hooks")
	}
}

func TestCodexHooksDriftCheckTreatsNonT3CodexWithoutRuntimeAsDormant(t *testing.T) {
	dir := t.TempDir()
	writeCodexHooksForDoctorTest(t, dir, `{"hooks":{"SessionStart":[{"hooks":[{"type":"command","command":"gc prime --hook --hook-format codex"}]}]}}`)
	cfg := &config.City{
		Session: config.SessionConfig{Provider: "tmux"},
		Agents:  []config.Agent{{Name: "worker", Provider: "codex"}},
	}
	check := newCodexHooksDriftCheck(dir, []string{dir}, cfg)
	check.userHooksPath = ""
	check.activeConsumers = func() ([]codexHookConsumer, error) { return nil, nil }

	result := check.Run(&doctor.CheckContext{})
	if result.Status != doctor.StatusOK {
		t.Fatalf("status = %v, want OK for dormant non-T3 consumer; message=%q details=%v", result.Status, result.Message, result.Details)
	}
	if details := strings.Join(result.Details, "\n"); !strings.Contains(details, "consumer=configured-dormant workdir="+dir) {
		t.Fatalf("dormant non-T3 consumer missing from details:\n%s", details)
	}
}

func TestCodexHooksDriftCheckRetainsFilesystemHooksForMixedT3Codex(t *testing.T) {
	dir := t.TempDir()
	writeCodexHooksForDoctorTest(t, dir, `{"hooks":{"SessionStart":[{"hooks":[{"type":"command","command":"gc prime --hook --hook-format codex"}]}]}}`)
	cfg := &config.City{
		Session: config.SessionConfig{Provider: "t3bridge"},
		Agents: []config.Agent{
			{Name: "generated", Provider: "codex"},
			{Name: "filesystem", Provider: "codex", Session: "tmux"},
		},
	}
	check := newCodexHooksDriftCheck(dir, []string{dir}, cfg)
	check.userHooksPath = ""

	result := check.Run(&doctor.CheckContext{})

	if result.Status != doctor.StatusWarning {
		t.Fatalf("status = %v, want warning for mixed generated/filesystem overlap; message=%q details=%v", result.Status, result.Message, result.Details)
	}
	if !strings.Contains(result.Message, "mixed Codex hook ownership") {
		t.Fatalf("message = %q, want mixed ownership diagnosis", result.Message)
	}
	if check.CanFix() {
		t.Fatal("CanFix = true for mixed T3/non-T3 Codex consumers")
	}
	details := strings.Join(result.Details, "\n")
	for _, want := range []string{"source=sessionFlags active=true", "source=project active=true", "source=active-total active=true", "session-start:2"} {
		if !strings.Contains(details, want) {
			t.Errorf("details missing %q:\n%s", want, details)
		}
	}
}

func TestCodexHooksDriftCheckMigratesFilesystemHooksForT3OnlyCodex(t *testing.T) {
	dir := t.TempDir()
	writeCurrentCityCodexHooksForDoctorTest(t, dir, dir, `{"hooks":{"SessionStart":[{"matcher":"startup","hooks":[{"type":"command","command":"printf custom"}]}]}}`)
	cfg := &config.City{
		Session: config.SessionConfig{Provider: "t3bridge"},
		Agents:  []config.Agent{{Name: "generated", Provider: "codex"}},
	}
	check := newCodexHooksDriftCheck(dir, []string{dir}, cfg)
	check.userHooksPath = ""

	if !check.CanFix() {
		t.Fatal("CanFix = false for T3-only Codex consumers")
	}
	if result := check.Run(&doctor.CheckContext{}); result.Status != doctor.StatusWarning {
		t.Fatalf("status before fix = %v, want warning; message=%q details=%v", result.Status, result.Message, result.Details)
	}
	if err := check.Fix(&doctor.CheckContext{}); err != nil {
		t.Fatalf("Fix: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(dir, ".codex", "hooks.json"))
	if err != nil {
		t.Fatalf("ReadFile after fix: %v", err)
	}
	if strings.Contains(string(data), "gc prime") || !strings.Contains(string(data), "printf custom") {
		t.Fatalf("T3-only migration did not selectively retain custom handler:\n%s", data)
	}
}

func TestCodexHooksDriftCheckIgnoresCustomHooks(t *testing.T) {
	dir := t.TempDir()
	writeCodexHooksForDoctorTest(t, dir, `{
  "hooks": {
    "UserPromptSubmit": [{
      "hooks": [{
        "type": "command",
        "command": "printf custom-codex-hook"
      }]
    }]
  }
}`)

	check := newCodexHooksDriftCheck(dir, []string{dir})
	result := check.Run(&doctor.CheckContext{})

	if result.Status != doctor.StatusOK {
		t.Fatalf("status = %v, want ok for user-owned hooks; message=%s", result.Status, result.Message)
	}
}

func TestCodexHooksDriftCheckFixRemovesManagedHooks(t *testing.T) {
	dir := t.TempDir()
	writeCurrentCityCodexHooksForDoctorTest(t, dir, dir, `{}`)

	check := newCodexHooksDriftCheck(dir, []string{dir})
	if err := check.Fix(&doctor.CheckContext{}); err != nil {
		t.Fatalf("Fix: %v", err)
	}
	result := check.Run(&doctor.CheckContext{})
	if result.Status != doctor.StatusOK {
		t.Fatalf("status after fix = %v, want ok; message=%s", result.Status, result.Message)
	}
	data, err := os.ReadFile(filepath.Join(dir, ".codex", "hooks.json"))
	if err != nil {
		t.Fatalf("read hooks: %v", err)
	}
	audit, err := hooks.AuditCodexHooks(data)
	if err != nil {
		t.Fatalf("audit fixed hooks: %v", err)
	}
	if len(audit.ManagedBehaviorCounts) != 0 {
		t.Fatalf("fixed hooks retain managed behaviors: %v\n%s", audit.ManagedBehaviorCounts, string(data))
	}
}

func TestNewCodexHooksDriftCheckCleansDedupesAndSortsDirs(t *testing.T) {
	check := newCodexHooksDriftCheck("/city", []string{" /z/../z ", "", "/a", "/a/."})

	if got, want := strings.Join(check.dirs, ","), "/a,/z"; got != want {
		t.Fatalf("dirs = %q, want %q", got, want)
	}
	if got, want := check.Name(), "codex-hooks-drift"; got != want {
		t.Fatalf("Name = %q, want %q", got, want)
	}
	if !check.CanFix() {
		t.Fatal("CanFix = false, want true")
	}
}

func TestNewCodexHooksDriftCheckUsesEffectiveCodexHome(t *testing.T) {
	codexHome := filepath.Join(t.TempDir(), "custom-codex-home")
	t.Setenv("CODEX_HOME", codexHome)

	check := newCodexHooksDriftCheck("/city", []string{"/city"})

	if got, want := check.userHooksPath, filepath.Join(codexHome, "hooks.json"); got != want {
		t.Fatalf("userHooksPath = %q, want effective CODEX_HOME path %q", got, want)
	}
}

func TestCodexHooksDriftCheckFixRefusesUnboundAgentWorkDirHandler(t *testing.T) {
	cityDir := t.TempDir()
	agentDir := filepath.Join(cityDir, ".gc", "agents", "reviewer")
	writeCodexHooksForDoctorTest(t, agentDir, `{
  "hooks": {
    "SessionStart": [{
      "hooks": [{
        "type": "command",
        "command": "export PATH=\"$HOME/go/bin:$HOME/.local/bin:$PATH\" && gc prime --hook --hook-format codex"
      }]
    }]
  }
}`)

	path := filepath.Join(agentDir, ".codex", "hooks.json")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read hooks before fix: %v", err)
	}
	check := newCodexHooksDriftCheck(cityDir, []string{agentDir})
	if err := check.Fix(&doctor.CheckContext{}); err == nil || !strings.Contains(err.Error(), "without proven current-city ownership") {
		t.Fatalf("Fix error = %v, want ownership refusal", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read hooks: %v", err)
	}
	if string(data) != string(before) {
		t.Fatalf("refused fix changed unbound hooks:\nbefore=%s\nafter=%s", before, data)
	}
}

func TestCodexHooksDriftCheckReportsManagedWrongCityBinding(t *testing.T) {
	cityDir := t.TempDir()
	writeCodexHooksForDoctorTest(t, cityDir, `{
  "hooks": {
    "SessionStart": [{
      "hooks": [{
        "type": "command",
        "command": "export PATH=\"$HOME/go/bin:$HOME/.local/bin:$PATH\" && GC_MANAGED_SESSION_HOOK=1 GC_HOOK_EVENT_NAME=SessionStart gc --city /old/city prime --hook --hook-format codex"
      }]
    }],
    "PreCompact": [{
      "hooks": [{
        "type": "command",
        "command": "export PATH=\"$HOME/go/bin:$HOME/.local/bin:$PATH\" && gc --city /old/city handoff --auto --hook-format codex \"context cycle\""
      }]
    }]
  }
}`)

	check := newCodexHooksDriftCheck(cityDir, []string{cityDir})
	result := check.Run(&doctor.CheckContext{})
	if result.Status != doctor.StatusWarning {
		t.Fatalf("status = %v, want warning; message=%s", result.Status, result.Message)
	}
}

func TestCodexHooksDriftCheckFixRefusesManagedWrongCityBinding(t *testing.T) {
	cityDir := t.TempDir()
	writeCodexHooksForDoctorTest(t, cityDir, `{
  "hooks": {
    "SessionStart": [{
      "hooks": [{
        "type": "command",
        "command": "export PATH=\"$HOME/go/bin:$HOME/.local/bin:$PATH\" && GC_MANAGED_SESSION_HOOK=1 GC_HOOK_EVENT_NAME=SessionStart gc --city /old/city prime --hook --hook-format codex"
      }]
    }],
    "PreCompact": [{
      "hooks": [{
        "type": "command",
        "command": "export PATH=\"$HOME/go/bin:$HOME/.local/bin:$PATH\" && gc --city /old/city handoff --auto --hook-format codex \"context cycle\""
      }]
    }]
  }
}`)

	path := filepath.Join(cityDir, ".codex", "hooks.json")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read hooks before fix: %v", err)
	}
	check := newCodexHooksDriftCheck(cityDir, []string{cityDir})
	if err := check.Fix(&doctor.CheckContext{}); err == nil || !strings.Contains(err.Error(), "without proven current-city ownership") {
		t.Fatalf("Fix error = %v, want ownership refusal", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read hooks: %v", err)
	}
	if string(data) != string(before) {
		t.Fatalf("refused fix changed wrong-city hooks:\nbefore=%s\nafter=%s", before, data)
	}
}

func TestCodexHookWorkDirsIncludesActiveRigPaths(t *testing.T) {
	cfg := &config.City{
		Rigs: []config.Rig{
			{Name: "active", Path: "/rig/active"},
			{Name: "blank", Path: " "},
			{Name: "suspended", Path: "/rig/suspended", SuspendedOnStart: true},
		},
	}

	got := codexHookWorkDirs("/city", cfg)
	if strings.Join(got, ",") != "/city,/rig/active" {
		t.Fatalf("work dirs = %#v, want city plus active rig only", got)
	}
	if got := codexHookWorkDirs("/city", nil); len(got) != 1 || got[0] != "/city" {
		t.Fatalf("nil config work dirs = %#v, want city only", got)
	}
}

func TestCodexHookOwnershipIgnoresSuspendedRigConsumers(t *testing.T) {
	cityDir := t.TempDir()
	cfg := &config.City{
		Session: config.SessionConfig{Provider: "t3bridge"},
		Rigs:    []config.Rig{{Name: "parked", Path: filepath.Join(cityDir, "rigs", "parked"), SuspendedOnStart: true}},
		Agents:  []config.Agent{{Name: "worker", Dir: "parked", Provider: "codex"}},
	}
	check := newCodexHooksDriftCheck(cityDir, codexHookWorkDirs(cityDir, cfg), cfg)
	check.userHooksPath = ""

	result := check.Run(&doctor.CheckContext{})

	if check.CanFix() {
		t.Fatal("CanFix = true when every Codex consumer belongs to a suspended rig")
	}
	if details := strings.Join(result.Details, "\n"); !strings.Contains(details, "source=sessionFlags active=false") {
		t.Fatalf("suspended-only details report session flags active:\n%s", details)
	}
}

func TestCodexHookOwnershipIgnoresSuspendedCityConsumers(t *testing.T) {
	cityDir := t.TempDir()
	cfg := &config.City{
		Workspace: config.Workspace{SuspendedOnStart: true},
		Session:   config.SessionConfig{Provider: "t3bridge"},
		Agents:    []config.Agent{{Name: "worker", Provider: "codex"}},
	}
	check := newCodexHooksDriftCheck(cityDir, codexHookWorkDirs(cityDir, cfg), cfg)
	check.userHooksPath = ""

	result := check.Run(&doctor.CheckContext{})

	if check.CanFix() {
		t.Fatal("CanFix = true while the city is suspended")
	}
	details := strings.Join(result.Details, "\n")
	for _, want := range []string{"source=sessionFlags active=false", "source=active-total active=false"} {
		if !strings.Contains(details, want) {
			t.Fatalf("suspended-city details missing %q:\n%s", want, details)
		}
	}
}

func TestCodexHookOwnershipDoesNotAssumeProviderFamilyThroughStartCommand(t *testing.T) {
	cityDir := t.TempDir()
	cfg := &config.City{
		Session: config.SessionConfig{Provider: "t3bridge"},
		Agents: []config.Agent{{
			Name:         "custom-launch",
			Provider:     "codex",
			StartCommand: "custom-agent-command",
		}},
	}
	check := newCodexHooksDriftCheck(cityDir, codexHookWorkDirs(cityDir, cfg), cfg)
	check.userHooksPath = ""

	result := check.Run(&doctor.CheckContext{})

	if check.CanFix() {
		t.Fatal("CanFix = true when start_command removes the resolved Codex provider family")
	}
	if details := strings.Join(result.Details, "\n"); !strings.Contains(details, "source=sessionFlags active=false") {
		t.Fatalf("start_command details report generated session flags active:\n%s", details)
	}
}

func TestCodexHookWorkDirsIncludesResolvedAgentWorkDirs(t *testing.T) {
	cityDir := t.TempDir()
	activeRig := filepath.Join(cityDir, "rigs", "active")
	suspendedRig := filepath.Join(cityDir, "rigs", "suspended")
	agentWorkDir := filepath.Join(cityDir, ".gc", "agents", "reviewer")
	cfg := &config.City{
		Workspace: config.Workspace{InstallAgentHooks: []string{"codex"}},
		Rigs: []config.Rig{
			{Name: "active", Path: activeRig},
			{Name: "suspended", Path: suspendedRig, SuspendedOnStart: true},
		},
		Agents: []config.Agent{
			{Name: "reviewer", Dir: "active", WorkDir: agentWorkDir},
			{Name: "gemini", Dir: "active", InstallAgentHooks: []string{"gemini"}, WorkDir: filepath.Join(cityDir, ".gc", "agents", "gemini")},
			{Name: "parked", Dir: "active", WorkDir: filepath.Join(cityDir, ".gc", "agents", "parked"), Suspended: true},
			{Name: "codex", Dir: "suspended", WorkDir: filepath.Join(cityDir, ".gc", "agents", "suspended")},
		},
	}

	got := codexHookWorkDirs(cityDir, cfg)

	assertDoctorPathPresent(t, got, cityDir)
	assertDoctorPathPresent(t, got, activeRig)
	assertDoctorPathPresent(t, got, agentWorkDir)
	assertDoctorPathAbsent(t, got, suspendedRig)
	assertDoctorPathAbsent(t, got, filepath.Join(cityDir, ".gc", "agents", "gemini"))
	assertDoctorPathAbsent(t, got, filepath.Join(cityDir, ".gc", "agents", "parked"))
	assertDoctorPathAbsent(t, got, filepath.Join(cityDir, ".gc", "agents", "suspended"))
}

func TestCodexHookWorkDirsIncludesBoundedPoolInstanceWorkDirs(t *testing.T) {
	cityDir := t.TempDir()
	rigDir := filepath.Join(cityDir, "rigs", "active")
	maxSessions := 2
	cfg := &config.City{
		Workspace: config.Workspace{InstallAgentHooks: []string{"codex"}},
		Rigs:      []config.Rig{{Name: "active", Path: rigDir}},
		Agents: []config.Agent{{
			Name:              "worker",
			Dir:               "active",
			WorkDir:           filepath.Join(".gc", "worktrees", "{{.Rig}}", "{{.AgentBase}}"),
			MaxActiveSessions: &maxSessions,
		}},
	}

	got := codexHookWorkDirs(cityDir, cfg)

	assertDoctorPathPresent(t, got, filepath.Join(cityDir, ".gc", "worktrees", "active", "worker"))
	assertDoctorPathPresent(t, got, filepath.Join(cityDir, ".gc", "worktrees", "active", "worker-1"))
	assertDoctorPathPresent(t, got, filepath.Join(cityDir, ".gc", "worktrees", "active", "worker-2"))
}

func assertDoctorPathPresent(t *testing.T, paths []string, want string) {
	t.Helper()
	want = filepath.Clean(want)
	for _, path := range paths {
		if filepath.Clean(path) == want {
			return
		}
	}
	t.Fatalf("paths = %#v, want %s present", paths, want)
}

func assertDoctorPathAbsent(t *testing.T, paths []string, want string) {
	t.Helper()
	want = filepath.Clean(want)
	for _, path := range paths {
		if filepath.Clean(path) == want {
			t.Fatalf("paths = %#v, want %s absent", paths, want)
		}
	}
}

func writeCodexHooksForDoctorTest(t *testing.T, dir, data string) {
	t.Helper()
	hookDir := filepath.Join(dir, ".codex")
	if err := os.MkdirAll(hookDir, 0o755); err != nil {
		t.Fatalf("mkdir hooks dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(hookDir, "hooks.json"), []byte(data), 0o644); err != nil {
		t.Fatalf("write hooks: %v", err)
	}
}

func writeCurrentCityCodexHooksForDoctorTest(t *testing.T, cityDir, workDir, custom string) {
	t.Helper()
	writeCodexHooksForDoctorTest(t, workDir, custom)
	installCodex(t, cityDir, workDir)
}
