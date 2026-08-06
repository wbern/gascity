package api

import (
	"testing"

	"github.com/gastownhall/gascity/internal/mail"
)

// overlapMailProvider returns the SAME message under every recipient, modelling
// a message reachable through more than one resolved route (an alias and a
// session id, say). Only All/Count are exercised; the embedded interface is nil
// so any other call would panic loudly rather than silently pass.
type overlapMailProvider struct {
	mail.Provider
	msg mail.Message
}

func (o overlapMailProvider) All(string) ([]mail.Message, error) {
	return []mail.Message{o.msg}, nil
}

func (o overlapMailProvider) Count(string) (int, int, error) {
	unread := 0
	if !o.msg.Read {
		unread = 1
	}
	return 1, unread, nil
}

func TestMailCountForRecipientsCountsOverlappingRoutesOnce(t *testing.T) {
	mp := overlapMailProvider{msg: mail.Message{ID: "m1", To: "agent/one"}}

	total, unread, err := mailCountForRecipients(mp, []string{"agent/one", "sess-abc"})
	if err != nil {
		t.Fatalf("mailCountForRecipients: %v", err)
	}
	if total != 1 || unread != 1 {
		t.Errorf("overlapping routes: got total=%d unread=%d, want 1/1", total, unread)
	}
}

func TestMailCountForRecipientsSingleRouteIsUnchanged(t *testing.T) {
	mp := overlapMailProvider{msg: mail.Message{ID: "m1", To: "agent/one"}}

	total, unread, err := mailCountForRecipients(mp, []string{"agent/one"})
	if err != nil {
		t.Fatalf("mailCountForRecipients: %v", err)
	}
	if total != 1 || unread != 1 {
		t.Errorf("single route: got total=%d unread=%d, want 1/1", total, unread)
	}
}

func TestMailCountForRecipientsReadMessageIsNotUnread(t *testing.T) {
	mp := overlapMailProvider{msg: mail.Message{ID: "m1", To: "agent/one", Read: true}}

	total, unread, err := mailCountForRecipients(mp, []string{"agent/one", "sess-abc"})
	if err != nil {
		t.Fatalf("mailCountForRecipients: %v", err)
	}
	if total != 1 || unread != 0 {
		t.Errorf("read message: got total=%d unread=%d, want 1/0", total, unread)
	}
}
