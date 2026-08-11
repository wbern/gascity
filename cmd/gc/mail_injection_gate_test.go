package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/mail"
	"github.com/gastownhall/gascity/internal/mail/beadmail"
	"github.com/gastownhall/gascity/internal/session"
)

func TestMailInjectionGateSuppressesUnchangedUnreadDetail(t *testing.T) {
	messages := []mail.Message{{ID: "gc-1", From: "sender", Subject: "subject", Body: strings.Repeat("detail ", 80)}}
	state := mailInjectionState{}

	first, state := gateMailInjection(messages, state)
	if !strings.Contains(first, messages[0].Body) {
		t.Fatalf("first injection omitted mail detail: %q", first)
	}

	second, _ := gateMailInjection(messages, state)
	if strings.Contains(second, messages[0].Body) {
		t.Fatalf("unchanged unread mail repeated full detail: %q", second)
	}
	if len(second) > mailInjectionReminderMaxBytes {
		t.Fatalf("unchanged unread reminder length = %d, want at most %d", len(second), mailInjectionReminderMaxBytes)
	}
}

func TestCurrentMailInjectionStateIsFencedToSessionIncarnation(t *testing.T) {
	clearInheritedBeadsEnv(t)
	cityPath := t.TempDir()
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_BEADS_SCOPE_ROOT", "")
	t.Setenv("GC_CITY", cityPath)
	t.Setenv("GC_CITY_PATH", cityPath)
	t.Setenv("GC_SESSION_ID", "")
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte("[workspace]\nname = \"test-city\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	store, err := openCityStoreAt(cityPath)
	if err != nil {
		t.Fatal(err)
	}
	sessionBead, err := store.Create(beads.Bead{Type: session.BeadType, Labels: []string{session.LabelSession}, Metadata: map[string]string{
		"instance_token": "token-a", "continuation_epoch": "4",
	}})
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("GC_SESSION_ID", sessionBead.ID)
	t.Setenv("GC_INSTANCE_TOKEN", "token-a")
	t.Setenv("GC_CONTINUATION_EPOCH", "4")

	state, persist, enabled, err := currentMailInjectionState()
	if err != nil || !enabled || state.fingerprint != "" {
		t.Fatalf("initial state = %#v, enabled=%v, err=%v", state, enabled, err)
	}
	wantFingerprint := mailInjectionFingerprint([]mail.Message{{ID: "gc-1"}})
	if err := persist(mailInjectionState{fingerprint: wantFingerprint}); err != nil {
		t.Fatal(err)
	}
	state, _, enabled, err = currentMailInjectionState()
	if err != nil || !enabled || state.fingerprint != wantFingerprint {
		t.Fatalf("persisted state = %#v, enabled=%v, err=%v", state, enabled, err)
	}
	t.Setenv("GC_INSTANCE_TOKEN", "token-b")
	_, _, enabled, err = currentMailInjectionState()
	if err != nil || enabled {
		t.Fatalf("stale incarnation enabled=%v, err=%v", enabled, err)
	}
}

func TestMailInjectionGateReportsNewIDAndPriorityChange(t *testing.T) {
	first := []mail.Message{{ID: "gc-1", Priority: 0, Body: "first detail"}}
	_, state := gateMailInjection(first, mailInjectionState{})
	for _, changed := range [][]mail.Message{
		{{ID: "gc-1", Priority: 0, Body: "first detail"}, {ID: "gc-2", Priority: 0, Body: "new detail"}},
		{{ID: "gc-1", Priority: 1, Body: "first detail"}},
	} {
		got, _ := gateMailInjection(changed, state)
		if strings.Contains(got, "Unread mail is unchanged") {
			t.Fatalf("changed unread set was suppressed: %q", got)
		}
	}
}

func TestFormatInjectOutputBoundsLongIdentityFields(t *testing.T) {
	got := formatInjectOutput([]mail.Message{{ID: strings.Repeat("i", 4000), From: strings.Repeat("s", 4000), Body: "detail"}})
	if len(got) > mailInjectionFullMaxBytes {
		t.Fatalf("payload length = %d, want at most %d", len(got), mailInjectionFullMaxBytes)
	}
	if !strings.Contains(got, "detail") {
		t.Fatalf("bounded changed-mail payload lost detail: %q", got)
	}
}

func TestMailInjectionDoesNotPersistWhenHookOutputFails(t *testing.T) {
	clearInheritedBeadsEnv(t)
	cityPath := t.TempDir()
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_BEADS_SCOPE_ROOT", "")
	t.Setenv("GC_CITY", cityPath)
	t.Setenv("GC_CITY_PATH", cityPath)
	t.Setenv("GC_INSTANCE_TOKEN", "token-a")
	t.Setenv("GC_CONTINUATION_EPOCH", "1")
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte("[workspace]\nname = \"test-city\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	store, err := openCityStoreAt(cityPath)
	if err != nil {
		t.Fatal(err)
	}
	sessionBead, err := store.Create(beads.Bead{Type: session.BeadType, Labels: []string{session.LabelSession}, Metadata: map[string]string{"instance_token": "token-a", "continuation_epoch": "1"}})
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("GC_SESSION_ID", sessionBead.ID)
	mp := beadmail.New(beads.NewMemStore())
	if _, err := mp.Send("sender", "recipient", "subject", "detail"); err != nil {
		t.Fatal(err)
	}
	if code := doMailCheckTargetWithFormat(mp, resolvedMailTarget{display: "recipient", recipients: []string{"recipient"}}, true, "", errWriter{}, io.Discard, nil); code != 0 {
		t.Fatalf("code=%d", code)
	}
	state, _, enabled, err := currentMailInjectionState()
	if err != nil || !enabled || state.fingerprint != "" {
		t.Fatalf("state persisted after output failure: %#v enabled=%v err=%v", state, enabled, err)
	}
}
