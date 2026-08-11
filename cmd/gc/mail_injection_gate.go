package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/gastownhall/gascity/internal/mail"
)

const (
	mailInjectionReminderMaxBytes = 128
	mailInjectionFullMaxBytes     = 2048
	mailInjectionFingerprintKey   = "mail_injection_fingerprint"
	mailInjectionFenceKey         = "mail_injection_fence"
)

// mailInjectionState is the durable portion of a prompt hook's delivery
// decision. Its fingerprint intentionally contains no message body.
type mailInjectionState struct {
	fingerprint string
}

type mailInjectionStateCoordinator struct {
	load func() (mailInjectionState, func(mailInjectionState) error, bool, error)
}

var mailInjectionStateLoader = currentMailInjectionState

func (c mailInjectionStateCoordinator) prepare(messages []mail.Message) (string, func() error, error) {
	text := formatInjectOutput(messages)
	previous, persist, enabled, err := c.load()
	if err != nil || !enabled {
		return text, nil, err
	}
	text, next := gateMailInjection(messages, previous)
	return text, func() error { return persist(next) }, nil
}

// gateMailInjection returns full mail detail when the unread set changed and a
// small retrieval pointer when it did not. Message priority is part of the
// fingerprint because a priority change must be visible on the next prompt.
func gateMailInjection(messages []mail.Message, previous mailInjectionState) (string, mailInjectionState) {
	fingerprint := mailInjectionFingerprint(messages)
	next := mailInjectionState{fingerprint: fingerprint}
	if fingerprint == previous.fingerprint && fingerprint != "" {
		return boundedMailInjectionReminder(), next
	}
	return formatInjectOutput(messages), next
}

func mailInjectionFingerprint(messages []mail.Message) string {
	if len(messages) == 0 {
		return ""
	}
	canonical := make([]mail.Message, len(messages))
	copy(canonical, messages)
	sort.Slice(canonical, func(i, j int) bool {
		if canonical[i].Priority != canonical[j].Priority {
			return canonical[i].Priority > canonical[j].Priority
		}
		return canonical[i].ID < canonical[j].ID
	})
	h := sha256.New()
	for _, message := range canonical {
		_, _ = io.WriteString(h, strconv.Itoa(len(message.ID)))
		_, _ = io.WriteString(h, ":")
		_, _ = io.WriteString(h, message.ID)
		_, _ = io.WriteString(h, ":")
		_, _ = io.WriteString(h, strconv.Itoa(message.Priority))
		_, _ = io.WriteString(h, ";")
	}
	return hex.EncodeToString(h.Sum(nil))
}

func boundedMailInjectionReminder() string {
	return "<system-reminder>\nUnread mail is unchanged. Run 'gc mail inbox' for details.\n</system-reminder>\n"
}

// currentMailInjectionState reads and writes state on the current session bead.
// The runtime token and continuation epoch fence a recycled session from using
// or overwriting an earlier incarnation's delivery decision.
func currentMailInjectionState() (mailInjectionState, func(mailInjectionState) error, bool, error) {
	sessionID := strings.TrimSpace(os.Getenv("GC_SESSION_ID"))
	token := strings.TrimSpace(os.Getenv("GC_INSTANCE_TOKEN"))
	epoch := strings.TrimSpace(os.Getenv("GC_CONTINUATION_EPOCH"))
	if sessionID == "" || token == "" || epoch == "" {
		return mailInjectionState{}, nil, false, nil
	}
	cityPath, err := resolveCity()
	if err != nil {
		return mailInjectionState{}, nil, false, fmt.Errorf("resolving city for mail injection state: %w", err)
	}
	cfg, err := loadCityConfig(cityPath, io.Discard)
	if err != nil {
		return mailInjectionState{}, nil, false, fmt.Errorf("loading city config for mail injection state: %w", err)
	}
	store, err := openCityStoreAt(cityPath)
	if err != nil {
		return mailInjectionState{}, nil, false, fmt.Errorf("opening session store for mail injection state: %w", err)
	}
	sessFront := cliSessionFrontDoor(store, cfg, cityPath)
	_, persisted, err := sessFront.GetPersistedResponse(sessionID)
	if err != nil {
		return mailInjectionState{}, nil, false, fmt.Errorf("reading session mail injection state: %w", err)
	}
	fence := token + ":" + epoch
	if persisted.Metadata["instance_token"] != token || persisted.Metadata["continuation_epoch"] != epoch {
		return mailInjectionState{}, nil, false, nil
	}
	state := mailInjectionState{}
	if persisted.Metadata[mailInjectionFenceKey] == fence {
		state.fingerprint = persisted.Metadata[mailInjectionFingerprintKey]
	}
	persist := func(next mailInjectionState) error {
		fresh, freshPersisted, err := sessFront.GetPersistedResponse(sessionID)
		if err != nil {
			return fmt.Errorf("re-reading session mail injection state: %w", err)
		}
		if freshPersisted.Metadata["instance_token"] != token || freshPersisted.Metadata["continuation_epoch"] != epoch {
			return fmt.Errorf("session mail injection state fence changed")
		}
		_, err = sessFront.UpdateMetadataInfo(fresh, map[string]string{
			mailInjectionFenceKey:       fence,
			mailInjectionFingerprintKey: next.fingerprint,
		})
		return err
	}
	return state, persist, true, nil
}

func boundedMailInjectionPayload(text string) string {
	if len(text) <= mailInjectionFullMaxBytes {
		return text
	}
	return "<system-reminder>\nUnread mail is available. Run 'gc mail inbox' for the bounded message list.\n</system-reminder>\n"
}
