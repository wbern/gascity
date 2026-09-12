package usage

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

const maxTerminalOutcomeFieldBytes = 256

// Terminal-outcome vocabulary is deliberately bounded because these values are
// consumed as telemetry dimensions. The values cover authoritative work-record
// dispositions and control-plane terminal dispositions without accepting free
// text that could carry secrets or create unbounded labels.
const (
	TerminalOutcomeShipped     = "shipped"
	TerminalOutcomeNoOp        = "no-op"
	TerminalOutcomeBlocked     = "blocked"
	TerminalOutcomeAbandoned   = "abandoned"
	TerminalOutcomePass        = "pass"
	TerminalOutcomeFail        = "fail"
	TerminalOutcomeSkipped     = "skipped"
	TerminalOutcomeCanceled    = "canceled"
	TerminalOutcomeMissingRoot = "missing_root"
)

// TerminalOutcome is the durable, authoritative terminal record for one work
// attempt. Its key is intentionally independent of a session: closing a
// session is not a successful work outcome, and reopening a bead with a new
// attempt produces a new key.
type TerminalOutcome struct {
	City       string `json:"city"`
	Rig        string `json:"rig"`
	WorkBeadID string `json:"work_bead_id"`
	Attempt    string `json:"attempt"`
	Outcome    string `json:"outcome"`
	At         int64  `json:"at"`
	Key        string `json:"key"`
}

// NewTerminalOutcome constructs the only supported terminal-outbox payload.
// The caller must have observed an authoritative work-bead terminal transition
// before creating it; this constructor deliberately has no session argument.
func NewTerminalOutcome(city, rig, workBeadID, attempt, outcome string, at int64) (TerminalOutcome, error) {
	record := TerminalOutcome{
		City:       strings.TrimSpace(city),
		Rig:        strings.TrimSpace(rig),
		WorkBeadID: strings.TrimSpace(workBeadID),
		Attempt:    strings.TrimSpace(attempt),
		Outcome:    strings.TrimSpace(outcome),
		At:         at,
	}
	record.Key = TerminalOutcomeKey(record.City, record.Rig, record.WorkBeadID, record.Attempt)
	if err := record.Validate(); err != nil {
		return TerminalOutcome{}, err
	}
	return record, nil
}

// TerminalOutcomeKey returns the stable idempotency key for a terminal work
// attempt. Outcome and timestamp are intentionally excluded: a terminal
// transition is identified by city, rig, bead, and attempt, while a reopen is
// represented by a new attempt value.
func TerminalOutcomeKey(city, rig, workBeadID, attempt string) string {
	return hashKey("terminal:" + strings.TrimSpace(city) + "\x00" + strings.TrimSpace(rig) + "\x00" + strings.TrimSpace(workBeadID) + "\x00" + strings.TrimSpace(attempt))
}

// Validate reports whether o is safe to persist and use as a terminal-outbox
// record. Bounded identifiers prevent this telemetry seam from becoming an
// unbounded-label or secret-carrying side channel.
func (o TerminalOutcome) Validate() error {
	for _, field := range []struct {
		name  string
		value string
	}{
		{"city", o.City},
		{"rig", o.Rig},
		{"work bead id", o.WorkBeadID},
		{"attempt", o.Attempt},
		{"outcome", o.Outcome},
	} {
		if field.value == "" {
			return fmt.Errorf("terminal outcome requires %s", field.name)
		}
		if len(field.value) > maxTerminalOutcomeFieldBytes {
			return fmt.Errorf("terminal outcome %s exceeds %d bytes", field.name, maxTerminalOutcomeFieldBytes)
		}
	}
	if o.At < 0 {
		return fmt.Errorf("terminal outcome has negative timestamp")
	}
	if !isTerminalOutcomeValue(o.Outcome) {
		return fmt.Errorf("terminal outcome has unknown outcome %q", o.Outcome)
	}
	if want := TerminalOutcomeKey(o.City, o.Rig, o.WorkBeadID, o.Attempt); o.Key != want {
		return fmt.Errorf("terminal outcome key does not match identity")
	}
	return nil
}

func isTerminalOutcomeValue(value string) bool {
	switch value {
	case TerminalOutcomeShipped,
		TerminalOutcomeNoOp,
		TerminalOutcomeBlocked,
		TerminalOutcomeAbandoned,
		TerminalOutcomePass,
		TerminalOutcomeFail,
		TerminalOutcomeSkipped,
		TerminalOutcomeCanceled,
		TerminalOutcomeMissingRoot:
		return true
	default:
		return false
	}
}

// TerminalEmitter writes a terminal outcome to an external consumer. It must
// treat TerminalOutcome.Key as its idempotency key because a crash after its
// successful write but before the outbox acknowledgement causes an intentional
// replay.
type TerminalEmitter func(context.Context, TerminalOutcome) error

// TerminalOutbox persists terminal outcomes before external delivery. It is an
// append-only journal: enqueue and delivered acknowledgements are each fsynced,
// so a process restart can recover every unacknowledged record.
type TerminalOutbox struct {
	mu   sync.Mutex
	path string
}

// NewTerminalOutbox returns a durable outbox rooted at path. The file and its
// parent directory are created lazily on the first enqueue or acknowledgement.
func NewTerminalOutbox(path string) *TerminalOutbox { return &TerminalOutbox{path: path} }

// Enqueue durably records outcome exactly once per terminal-outcome key. It
// does not call an external emitter, making an authoritative close independent
// of exporter availability.
func (o *TerminalOutbox) Enqueue(_ context.Context, outcome TerminalOutcome) error {
	if o == nil || strings.TrimSpace(o.path) == "" {
		return errors.New("terminal outbox path is required")
	}
	if err := outcome.Validate(); err != nil {
		return err
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	state, err := readTerminalOutbox(o.path)
	if err != nil {
		return err
	}
	if existing, ok := state.enqueued[outcome.Key]; ok {
		// A retried observation can carry a later local timestamp. The first
		// durable record remains authoritative as long as the terminal
		// disposition for the same immutable attempt agrees.
		if existing.Outcome == outcome.Outcome {
			return nil
		}
		return fmt.Errorf("terminal outcome collision for %q", outcome.Key)
	}
	return appendTerminalOutboxEntry(o.path, terminalOutboxEntry{Kind: terminalOutboxEnqueued, Outcome: outcome})
}

// Deliver emits every currently pending terminal outcome, then durably records
// its acknowledgement. On a failure the pending entry remains intact for a
// later replay; successful earlier entries are never replayed by this outbox.
func (o *TerminalOutbox) Deliver(ctx context.Context, emit TerminalEmitter) error {
	if o == nil || strings.TrimSpace(o.path) == "" {
		return errors.New("terminal outbox path is required")
	}
	if emit == nil {
		return errors.New("terminal outbox emitter is required")
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	state, err := readTerminalOutbox(o.path)
	if err != nil {
		return err
	}
	for _, outcome := range state.pending {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := emit(ctx, outcome); err != nil {
			return fmt.Errorf("emit terminal outcome %s: %w", outcome.Key, err)
		}
		if err := appendTerminalOutboxEntry(o.path, terminalOutboxEntry{Kind: terminalOutboxDelivered, Key: outcome.Key}); err != nil {
			return fmt.Errorf("acknowledge terminal outcome %s: %w", outcome.Key, err)
		}
	}
	return nil
}

const (
	terminalOutboxEnqueued  = "enqueued"
	terminalOutboxDelivered = "delivered"
)

type terminalOutboxEntry struct {
	Kind    string          `json:"kind"`
	Key     string          `json:"key,omitempty"`
	Outcome TerminalOutcome `json:"outcome,omitempty"`
}

type terminalOutboxState struct {
	enqueued  map[string]TerminalOutcome
	delivered map[string]struct{}
	pending   []TerminalOutcome
}

func readTerminalOutbox(path string) (terminalOutboxState, error) {
	state := terminalOutboxState{
		enqueued:  make(map[string]TerminalOutcome),
		delivered: make(map[string]struct{}),
	}
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return state, nil
	}
	if err != nil {
		return terminalOutboxState{}, err
	}
	defer file.Close() //nolint:errcheck // read-only handle

	reader := bufio.NewReaderSize(file, 64*1024)
	lineNo := 0
	for {
		line, readErr := reader.ReadString('\n')
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return terminalOutboxState{}, readErr
		}
		atEOF := errors.Is(readErr, io.EOF)
		if atEOF && line == "" {
			break
		}
		lineNo++
		if content := strings.TrimSpace(line); content != "" {
			var entry terminalOutboxEntry
			if err := json.Unmarshal([]byte(content), &entry); err != nil {
				return terminalOutboxState{}, fmt.Errorf("decode terminal outbox %s:%d: %w", path, lineNo, err)
			}
			switch entry.Kind {
			case terminalOutboxEnqueued:
				if err := entry.Outcome.Validate(); err != nil {
					return terminalOutboxState{}, fmt.Errorf("invalid terminal outbox outcome at %s:%d: %w", path, lineNo, err)
				}
				if _, seen := state.enqueued[entry.Outcome.Key]; !seen {
					state.enqueued[entry.Outcome.Key] = entry.Outcome
				}
			case terminalOutboxDelivered:
				if strings.TrimSpace(entry.Key) == "" {
					return terminalOutboxState{}, fmt.Errorf("terminal outbox acknowledgement at %s:%d has empty key", path, lineNo)
				}
				state.delivered[entry.Key] = struct{}{}
			default:
				return terminalOutboxState{}, fmt.Errorf("terminal outbox entry at %s:%d has unknown kind %q", path, lineNo, entry.Kind)
			}
		}
		if atEOF {
			break
		}
	}
	for key, outcome := range state.enqueued {
		if _, done := state.delivered[key]; !done {
			state.pending = append(state.pending, outcome)
		}
	}
	return state, nil
}

func appendTerminalOutboxEntry(path string, entry terminalOutboxEntry) error {
	line, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	line = append(line, '\n')
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	if _, err := file.Write(line); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
}
