package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/mail"
	"github.com/gastownhall/gascity/internal/mail/beadmail"
	"github.com/gastownhall/gascity/internal/session"
)

func TestMailInjectionGateSuppressesUnchangedUnreadDetail(t *testing.T) {
	messages := []mail.Message{{ID: "gc-1", From: "sender", Subject: "subject", Body: strings.Repeat("detail ", 80)}}
	state := mailInjectionState{}

	first, state := gateMailInjection(messages, state)
	if !strings.Contains(first, "detail") || !strings.Contains(first, "[preview truncated]") {
		t.Fatalf("first injection omitted the bounded mail preview: %q", first)
	}
	if len(first) > mailInjectionFullMaxBytes {
		t.Fatalf("first injection length = %d, want at most %d", len(first), mailInjectionFullMaxBytes)
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
	stalePersist := persist
	if err := store.Update(sessionBead.ID, beads.UpdateOpts{Metadata: map[string]string{"continuation_epoch": "5"}}); err != nil {
		t.Fatal(err)
	}
	if err := stalePersist(mailInjectionState{fingerprint: "stale"}); err == nil {
		t.Fatal("stale persist succeeded after continuation epoch changed")
	}
	if err := store.Update(sessionBead.ID, beads.UpdateOpts{Metadata: map[string]string{"continuation_epoch": "4"}}); err != nil {
		t.Fatal(err)
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
	t.Setenv("GC_INSTANCE_TOKEN", "token-a")
	t.Setenv("GC_CONTINUATION_EPOCH", "")
	_, _, enabled, err = currentMailInjectionState()
	if err != nil || enabled {
		t.Fatalf("missing continuation epoch enabled=%v, err=%v", enabled, err)
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
		if strings.Contains(got, "Unread mail is unchanged") || !strings.Contains(got, "first detail") {
			t.Fatalf("changed unread set omitted bounded detail: %q", got)
		}
	}
}

func TestMailInjectionGateReportsRemovedID(t *testing.T) {
	_, state := gateMailInjection([]mail.Message{{ID: "gc-1"}, {ID: "gc-2"}}, mailInjectionState{})
	got, _ := gateMailInjection([]mail.Message{{ID: "gc-1"}}, state)
	if strings.Contains(got, "unchanged") {
		t.Fatalf("removed ID was suppressed: %q", got)
	}
}

func TestMailInjectionGateDoesNotAlterSessionStartDetailRenderer(t *testing.T) {
	messages := []mail.Message{{ID: "gc-1", Body: "startup detail"}}
	_, state := gateMailInjection(messages, mailInjectionState{})
	_, _ = gateMailInjection(messages, state)
	startup := formatInjectOutput(messages)
	if !strings.Contains(startup, "startup detail") || strings.Contains(startup, "Unread mail is unchanged") {
		t.Fatalf("SessionStart detail renderer was gated: %q", startup)
	}
}

func TestMailInjectionFingerprintIgnoresProviderOrder(t *testing.T) {
	first := []mail.Message{{ID: "gc-1", Priority: 0}, {ID: "gc-2", Priority: 0}}
	reordered := []mail.Message{{ID: "gc-2", Priority: 0}, {ID: "gc-1", Priority: 0}}

	if got, want := mailInjectionFingerprint(reordered), mailInjectionFingerprint(first); got != want {
		t.Fatalf("reordered fingerprint = %q, want %q", got, want)
	}
}

func TestBoundedMailInjectionReminderIsAtMost128Bytes(t *testing.T) {
	if got := boundedMailInjectionReminder(); len(got) > 128 {
		t.Fatalf("reminder length = %d, want at most 128", len(got))
	}
}

func TestBoundedMailInjectionReminderNeverInterpolatesMessageIDs(t *testing.T) {
	got := boundedMailInjectionReminder()

	if strings.Contains(got, "gc-1") || strings.Contains(got, "</system-reminder></system-reminder>") {
		t.Fatalf("reminder interpolated message ID: %q", got)
	}
	if len(got) > mailInjectionReminderMaxBytes || !utf8.ValidString(got) {
		t.Fatalf("reminder is not bounded valid UTF-8: %q", got)
	}
}

func TestMailInjectionGateClearsStateWhenUnreadIsEmpty(t *testing.T) {
	_, state := gateMailInjection([]mail.Message{{ID: "gc-1"}}, mailInjectionState{})
	_, cleared := gateMailInjection(nil, state)

	if cleared.fingerprint != "" {
		t.Fatalf("empty unread fingerprint = %q, want empty", cleared.fingerprint)
	}
}

func TestMailInjectionCoordinatorPersistsEmptyReset(t *testing.T) {
	state := mailInjectionState{}
	coordinator := mailInjectionStateCoordinator{load: func() (mailInjectionState, func(mailInjectionState) error, bool, error) {
		return state, func(next mailInjectionState) error { state = next; return nil }, true, nil
	}}
	messages := []mail.Message{{ID: "gc-1", Body: "detail"}}

	first, persist, err := coordinator.prepare(messages)
	if err != nil || !strings.Contains(first, "detail") || persist == nil {
		t.Fatalf("first prepare = %q, persist=%v, err=%v", first, persist != nil, err)
	}
	if err := persist(); err != nil {
		t.Fatal(err)
	}
	unchanged, unchangedPersist, err := coordinator.prepare(messages)
	if err != nil || !strings.Contains(unchanged, "unchanged") || unchangedPersist != nil {
		t.Fatalf("unchanged prepare = %q, persist=%v, err=%v", unchanged, unchangedPersist != nil, err)
	}
	_, clearState, err := coordinator.prepare(nil)
	if err != nil || clearState == nil {
		t.Fatalf("empty prepare persist=%v, err=%v", clearState != nil, err)
	}
	if err := clearState(); err != nil {
		t.Fatal(err)
	}
	_, emptyPersist, err := coordinator.prepare(nil)
	if err != nil || emptyPersist != nil {
		t.Fatalf("unchanged empty prepare persist=%v, err=%v", emptyPersist != nil, err)
	}
	again, _, err := coordinator.prepare(messages)
	if err != nil || !strings.Contains(again, "detail") {
		t.Fatalf("post-empty prepare = %q, err=%v", again, err)
	}
}

func TestDetailedMailInjectionArchivesOnlyRenderedMessages(t *testing.T) {
	messages := []mail.Message{
		{ID: strings.Repeat("i", 128), From: strings.Repeat("f", 129), Subject: strings.Repeat("s", 241), Body: strings.Repeat("b", 241)},
		{ID: strings.Repeat("j", 128), From: strings.Repeat("f", 129), Subject: strings.Repeat("s", 241), Body: strings.Repeat("b", 241)},
		{ID: strings.Repeat("k", 128), From: strings.Repeat("f", 129), Subject: strings.Repeat("s", 241), Body: strings.Repeat("b", 241)},
	}

	text, rendered := formatInjectOutputWithMessages(messages)
	if len(text) > mailInjectionFullMaxBytes || !strings.Contains(text, "truncated") {
		t.Fatalf("bounded detailed output = %d bytes: %q", len(text), text)
	}
	if len(rendered) == 0 || len(rendered) >= len(messages) {
		t.Fatalf("represented messages = %d, want a strict bounded subset of %d", len(rendered), len(messages))
	}
	for _, message := range rendered {
		if !strings.Contains(text, message.ID) {
			t.Fatalf("rendered message %q is not retrievable from %q", message.ID, text)
		}
	}
}

func TestDetailedMailInjectionDoesNotArchiveSanitizedIDs(t *testing.T) {
	message := mail.Message{ID: "gc-1\nnot-retrievable", From: "sender", Body: "detail"}
	text, rendered := formatInjectOutputWithMessages([]mail.Message{message})
	if !strings.Contains(text, "gc-1 not-retrievable") {
		t.Fatalf("sanitized output = %q", text)
	}
	if len(rendered) != 0 {
		t.Fatalf("sanitized ID must not be archive-eligible: %#v", rendered)
	}
}

func TestOversizedMailInjectionLeavesFailedArchiveMessagesRetrievable(t *testing.T) {
	store := beads.NewMemStore()
	provider := &failingMailInjectionArchiver{Provider: beadmail.New(store)}
	for i := 0; i < mailInjectMaxMessages; i++ {
		_, err := provider.Send("sender", "recipient", "subject", "body")
		if err != nil {
			t.Fatal(err)
		}
	}
	provider.transform = func(messages []mail.Message) []mail.Message {
		for i := range messages {
			messages[i].From = strings.Repeat("f", 129)
			messages[i].Subject = strings.Repeat("s", 241)
			messages[i].Body = strings.Repeat("b", 241)
		}
		return messages
	}

	var stdout, stderr bytes.Buffer
	if code := doMailCheckTargetWithFormat(provider, resolvedMailTarget{display: "recipient", recipients: []string{"recipient"}}, true, "", &stdout, &stderr, nil); code != 0 {
		t.Fatalf("code=%d", code)
	}
	if len(stdout.String()) > mailInjectionFullMaxBytes || len(provider.archived) == 0 || !strings.Contains(stderr.String(), "archiving injected auto handoff mail") {
		t.Fatalf("stdout=%d bytes archived=%v stderr=%q", len(stdout.String()), provider.archived, stderr.String())
	}
	for _, id := range provider.archived {
		if !strings.Contains(stdout.String(), id) {
			t.Fatalf("archived id %q is absent from bounded output %q", id, stdout.String())
		}
		if _, err := provider.Get(id); err != nil {
			t.Fatalf("failed archive hid %q: %v", id, err)
		}
	}
}

func TestOversizedSessionStartMailInjectionRemainsDetailedAndBounded(t *testing.T) {
	messages := []mail.Message{
		{ID: strings.Repeat("i", 128), From: strings.Repeat("f", 129), Subject: strings.Repeat("s", 241), Body: strings.Repeat("b", 241)},
		{ID: strings.Repeat("j", 128), From: strings.Repeat("f", 129), Subject: strings.Repeat("s", 241), Body: strings.Repeat("b", 241)},
		{ID: strings.Repeat("k", 128), From: strings.Repeat("f", 129), Subject: strings.Repeat("s", 241), Body: strings.Repeat("b", 241)},
	}
	got := formatInjectOutput(messages)
	if len(got) > mailInjectionFullMaxBytes || strings.Contains(got, "Unread mail is available.") || !strings.Contains(got, "truncated") {
		t.Fatalf("SessionStart output = %d bytes: %q", len(got), got)
	}
}

type failingMailInjectionArchiver struct {
	*beadmail.Provider
	archived  []string
	transform func([]mail.Message) []mail.Message
}

func (p *failingMailInjectionArchiver) Check(recipient string) ([]mail.Message, error) {
	messages, err := p.Provider.Check(recipient)
	if err != nil || p.transform == nil {
		return messages, err
	}
	return p.transform(messages), nil
}

func (p *failingMailInjectionArchiver) ArchiveInjectedAutoHandoffs(ids []string) error {
	p.archived = append(p.archived, ids...)
	return errors.New("archive unavailable")
}

func TestMailInjectionCoordinatorFailsOpenOnStateLoadFailure(t *testing.T) {
	coordinator := mailInjectionStateCoordinator{load: func() (mailInjectionState, func(mailInjectionState) error, bool, error) {
		return mailInjectionState{}, nil, false, errors.New("state unavailable")
	}}
	got, persist, err := coordinator.prepare([]mail.Message{{ID: "gc-1", Body: "detail"}})
	if err == nil || persist != nil || !strings.Contains(got, "detail") {
		t.Fatalf("prepare = %q, persist=%v, err=%v", got, persist != nil, err)
	}
}

func TestMailInjectionCoordinatorSurfacesPersistFailure(t *testing.T) {
	coordinator := mailInjectionStateCoordinator{load: func() (mailInjectionState, func(mailInjectionState) error, bool, error) {
		return mailInjectionState{}, func(mailInjectionState) error { return errors.New("fence changed") }, true, nil
	}}
	_, persist, err := coordinator.prepare([]mail.Message{{ID: "gc-1", Body: "detail"}})
	if err != nil || persist == nil {
		t.Fatalf("prepare persist=%v, err=%v", persist != nil, err)
	}
	if err := persist(); err == nil || !strings.Contains(err.Error(), "fence changed") {
		t.Fatalf("persist error = %v", err)
	}
}

func TestMailInjectionObservationRetainsDeliveryFactsAfterStateFailure(t *testing.T) {
	observation := &mailInjectionObservation{}
	observation.injected([]mail.Message{{ID: "gc-1"}}, "bounded detail")
	observation.fail(continuationErrorMailState)
	if observation.outcome != continuationOutcomeFailed || observation.errorCode != continuationErrorMailState || observation.messageCount != 1 || observation.bodyBytes == 0 || len(observation.mailIDs) != 1 {
		t.Fatalf("observation = %#v", observation)
	}
}

func TestFormatInjectOutputBoundsLongIdentityFields(t *testing.T) {
	got := formatInjectOutput([]mail.Message{{ID: strings.Repeat("i", 4000), From: strings.Repeat("s", 4000), Body: "detail"}})
	if len(got) > mailInjectionFullMaxBytes {
		t.Fatalf("payload length = %d, want at most %d", len(got), mailInjectionFullMaxBytes)
	}
	if !utf8.ValidString(got) {
		t.Fatalf("payload is invalid UTF-8: %q", got)
	}
	if !strings.Contains(got, "detail") {
		t.Fatalf("bounded changed-mail payload lost detail: %q", got)
	}
}

func TestCodexHookContextPreservesBoundedValidUTF8(t *testing.T) {
	text := formatInjectOutput([]mail.Message{{ID: strings.Repeat("i", 4000), Body: strings.Repeat("å", 4000)}})
	var output bytes.Buffer
	if err := writeProviderHookContextForEvent(&output, hookOutputFormatCodex, "UserPromptSubmit", text); err != nil {
		t.Fatal(err)
	}
	var payload struct {
		HookSpecificOutput struct {
			AdditionalContext string `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal(output.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.HookSpecificOutput.AdditionalContext) > mailInjectionFullMaxBytes || !utf8.ValidString(payload.HookSpecificOutput.AdditionalContext) {
		t.Fatalf("decoded context is not bounded valid UTF-8: %q", payload.HookSpecificOutput.AdditionalContext)
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

func TestMailInjectionStateLoadFailureRecordsFailedDelivery(t *testing.T) {
	oldLoader := mailInjectionStateLoader
	mailInjectionStateLoader = func() (mailInjectionState, func(mailInjectionState) error, bool, error) {
		return mailInjectionState{}, nil, false, errors.New("state unavailable")
	}
	t.Cleanup(func() { mailInjectionStateLoader = oldLoader })
	mp := beadmail.New(beads.NewMemStore())
	if _, err := mp.Send("sender", "recipient", "subject", "detail"); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	observation := &mailInjectionObservation{}
	if code := doMailCheckTargetWithFormat(mp, resolvedMailTarget{display: "recipient", recipients: []string{"recipient"}}, true, "", &stdout, &stderr, observation); code != 0 {
		t.Fatalf("code=%d", code)
	}
	if !strings.Contains(stdout.String(), "detail") || !strings.Contains(stderr.String(), "mail injection state") || observation.outcome != continuationOutcomeFailed || observation.errorCode != continuationErrorMailState || observation.messageCount != 1 || observation.bodyBytes == 0 {
		t.Fatalf("stdout=%q stderr=%q observation=%#v", stdout.String(), stderr.String(), observation)
	}
}

func TestMailInjectionPersistFailureRecordsFailedDelivery(t *testing.T) {
	oldLoader := mailInjectionStateLoader
	mailInjectionStateLoader = func() (mailInjectionState, func(mailInjectionState) error, bool, error) {
		return mailInjectionState{}, func(mailInjectionState) error { return errors.New("persist failed") }, true, nil
	}
	t.Cleanup(func() { mailInjectionStateLoader = oldLoader })
	mp := beadmail.New(beads.NewMemStore())
	if _, err := mp.Send("sender", "recipient", "subject", "detail"); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	observation := &mailInjectionObservation{}
	doMailCheckTargetWithFormat(mp, resolvedMailTarget{display: "recipient", recipients: []string{"recipient"}}, true, "", &stdout, &stderr, observation)
	if !strings.Contains(stdout.String(), "detail") || !strings.Contains(stderr.String(), "persisting mail injection state") || observation.outcome != continuationOutcomeFailed || observation.errorCode != continuationErrorMailState || observation.messageCount != 1 || observation.bodyBytes == 0 {
		t.Fatalf("stdout=%q stderr=%q observation=%#v", stdout.String(), stderr.String(), observation)
	}
}

func TestMailInjectionClearFailureRecordsFailedEmptyObservation(t *testing.T) {
	oldLoader := mailInjectionStateLoader
	mailInjectionStateLoader = func() (mailInjectionState, func(mailInjectionState) error, bool, error) {
		return mailInjectionState{fingerprint: "old"}, func(mailInjectionState) error { return errors.New("clear failed") }, true, nil
	}
	t.Cleanup(func() { mailInjectionStateLoader = oldLoader })
	var stdout, stderr bytes.Buffer
	observation := &mailInjectionObservation{outcome: continuationOutcomeEmpty}
	doMailCheckTargetWithFormat(beadmail.New(beads.NewMemStore()), resolvedMailTarget{display: "recipient", recipients: []string{"recipient"}}, true, "", &stdout, &stderr, observation)
	if stdout.Len() != 0 || !strings.Contains(stderr.String(), "clearing mail injection state") || observation.outcome != continuationOutcomeFailed || observation.errorCode != continuationErrorMailState || observation.messageCount != 0 || observation.bodyBytes != 0 {
		t.Fatalf("stdout=%q stderr=%q observation=%#v", stdout.String(), stderr.String(), observation)
	}
}

func TestMailInjectionEmptyPassRecordsZeroFields(t *testing.T) {
	oldLoader := mailInjectionStateLoader
	mailInjectionStateLoader = func() (mailInjectionState, func(mailInjectionState) error, bool, error) {
		return mailInjectionState{}, func(mailInjectionState) error { return nil }, true, nil
	}
	t.Cleanup(func() { mailInjectionStateLoader = oldLoader })
	var stdout, stderr bytes.Buffer
	observation := &mailInjectionObservation{outcome: continuationOutcomeEmpty}
	doMailCheckTargetWithFormat(beadmail.New(beads.NewMemStore()), resolvedMailTarget{display: "recipient", recipients: []string{"recipient"}}, true, "", &stdout, &stderr, observation)
	if stdout.Len() != 0 || stderr.Len() != 0 || observation.outcome != continuationOutcomeEmpty || observation.messageCount != 0 || observation.bodyBytes != 0 || len(observation.mailIDs) != 0 {
		t.Fatalf("stdout=%q stderr=%q observation=%#v", stdout.String(), stderr.String(), observation)
	}
}

func TestMailInjectionUnchangedPassRecordsBoundedPointer(t *testing.T) {
	mp := beadmail.New(beads.NewMemStore())
	sent, err := mp.Send("sender", "recipient", "subject", "secret body")
	if err != nil {
		t.Fatal(err)
	}
	messages := []mail.Message{{ID: sent.ID}}
	oldLoader := mailInjectionStateLoader
	mailInjectionStateLoader = func() (mailInjectionState, func(mailInjectionState) error, bool, error) {
		return mailInjectionState{fingerprint: mailInjectionFingerprint(messages)}, func(mailInjectionState) error { return nil }, true, nil
	}
	t.Cleanup(func() { mailInjectionStateLoader = oldLoader })
	var stdout, stderr bytes.Buffer
	observation := &mailInjectionObservation{}
	doMailCheckTargetWithFormat(mp, resolvedMailTarget{display: "recipient", recipients: []string{"recipient"}}, true, "", &stdout, &stderr, observation)
	if len(stdout.String()) > mailInjectionReminderMaxBytes || strings.Contains(stdout.String(), "secret body") || strings.Contains(stdout.String(), "sender") || strings.Contains(stdout.String(), "subject") || observation.outcome != continuationOutcomeInjected || observation.errorCode != "" || observation.messageCount != 1 || observation.bodyBytes == 0 || len(observation.mailIDs) != 1 || observation.mailIDs[0] != sent.ID {
		t.Fatalf("stdout=%q observation=%#v", stdout.String(), observation)
	}
}
