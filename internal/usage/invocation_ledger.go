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

// InvocationRecord is the durable correlation between an exact provider turn
// and its immutable work attribution. SessionID scopes the provider runtime;
// Attribution.InvocationID is supplied at the producer boundary and is the
// only key accepted by delayed completion recovery.
type InvocationRecord struct {
	SessionID         string                `json:"session_id"`
	Attribution       InvocationAttribution `json:"attribution"`
	UpstreamRequestID string                `json:"upstream_request_id,omitempty"`
}

// Complete reports whether a provider completion has been durably observed.
func (r InvocationRecord) Complete() bool {
	return strings.TrimSpace(r.UpstreamRequestID) != "" && r.Attribution.CompletionHead != (Head{})
}

func (r InvocationRecord) validateBound() error {
	if strings.TrimSpace(r.SessionID) == "" {
		return errors.New("invocation record requires session id")
	}
	if len(r.SessionID) > maxTerminalOutcomeFieldBytes {
		return fmt.Errorf("invocation record session id exceeds %d bytes", maxTerminalOutcomeFieldBytes)
	}
	if r.Attribution.State != AttributionBound {
		return errors.New("invocation record requires bound attribution")
	}
	if err := r.Attribution.Validate(); err != nil {
		return err
	}
	if len(r.UpstreamRequestID) > maxTerminalOutcomeFieldBytes {
		return fmt.Errorf("invocation record upstream request id exceeds %d bytes", maxTerminalOutcomeFieldBytes)
	}
	if r.Complete() && strings.TrimSpace(r.UpstreamRequestID) == "" {
		return errors.New("completed invocation record requires upstream request id")
	}
	if !r.Complete() && (strings.TrimSpace(r.UpstreamRequestID) != "" || r.Attribution.CompletionHead != (Head{})) {
		return errors.New("incomplete invocation record has partial completion")
	}
	return nil
}

func invocationRecordKey(sessionID, invocationID string) string {
	return strings.TrimSpace(sessionID) + "\x00" + strings.TrimSpace(invocationID)
}

// InvocationLedger persists pre-turn bindings and the exact provider completion
// that settles each one. It never queries session assignment state, workdirs,
// names, timestamps, or transcript position to fill a binding.
type InvocationLedger struct {
	mu   sync.Mutex
	path string
}

// NewInvocationLedger returns a durable ledger rooted at path. The journal is
// append-only and is created lazily on the first successful operation.
func NewInvocationLedger(path string) *InvocationLedger { return &InvocationLedger{path: path} }

// Bind durably writes a complete pre-turn work binding before provider
// execution. Repeating the same binding is idempotent; a different binding for
// the same session and invocation id is rejected rather than overwritten.
func (l *InvocationLedger) Bind(_ context.Context, record InvocationRecord) error {
	if l == nil || strings.TrimSpace(l.path) == "" {
		return errors.New("invocation ledger path is required")
	}
	if err := record.validateBound(); err != nil {
		return err
	}
	if record.Complete() {
		return errors.New("invocation binding must not be completed")
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	state, err := readInvocationLedger(l.path)
	if err != nil {
		return err
	}
	key := invocationRecordKey(record.SessionID, record.Attribution.InvocationID)
	if existing, ok := state.records[key]; ok {
		if sameInvocationBinding(existing, record) {
			return nil
		}
		return fmt.Errorf("invocation binding collision for %q", record.Attribution.InvocationID)
	}
	return appendInvocationLedgerEntry(l.path, invocationLedgerEntry{Kind: invocationLedgerBound, Record: record})
}

// Complete records an exact provider terminal observation. It succeeds only
// for an already-persisted session+invocation binding and returns that binding
// with its immutable completion head. A replay with identical values is
// idempotent; any divergent completion is rejected.
func (l *InvocationLedger) Complete(_ context.Context, sessionID, invocationID, upstreamRequestID string, completionHead Head) (InvocationRecord, error) {
	if l == nil || strings.TrimSpace(l.path) == "" {
		return InvocationRecord{}, errors.New("invocation ledger path is required")
	}
	sessionID = strings.TrimSpace(sessionID)
	invocationID = strings.TrimSpace(invocationID)
	upstreamRequestID = strings.TrimSpace(upstreamRequestID)
	if sessionID == "" || invocationID == "" || upstreamRequestID == "" {
		return InvocationRecord{}, errors.New("invocation completion requires session id, invocation id, and upstream request id")
	}
	if !completionHead.Valid() {
		return InvocationRecord{}, fmt.Errorf("invalid completion head state %q", completionHead.State)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	state, err := readInvocationLedger(l.path)
	if err != nil {
		return InvocationRecord{}, err
	}
	key := invocationRecordKey(sessionID, invocationID)
	record, ok := state.records[key]
	if !ok {
		return InvocationRecord{}, fmt.Errorf("no persisted invocation binding for %q", invocationID)
	}
	if record.Complete() {
		if record.UpstreamRequestID == upstreamRequestID && record.Attribution.CompletionHead == completionHead {
			return record, nil
		}
		return InvocationRecord{}, fmt.Errorf("invocation completion collision for %q", invocationID)
	}
	attribution, err := record.Attribution.WithCompletionHead(completionHead)
	if err != nil {
		return InvocationRecord{}, err
	}
	record.Attribution = attribution
	record.UpstreamRequestID = upstreamRequestID
	if err := record.validateBound(); err != nil {
		return InvocationRecord{}, err
	}
	if err := appendInvocationLedgerEntry(l.path, invocationLedgerEntry{Kind: invocationLedgerCompleted, Record: record}); err != nil {
		return InvocationRecord{}, err
	}
	return record, nil
}

// CompletedForUpstreamRequestID returns the completed persisted binding for one
// exact session and provider response id. It never derives attribution from
// mutable session state. An ambiguous provider response id fails closed rather
// than choosing one invocation by transcript position or timestamp.
func (l *InvocationLedger) CompletedForUpstreamRequestID(_ context.Context, sessionID, upstreamRequestID string) (InvocationRecord, bool, error) {
	if l == nil || strings.TrimSpace(l.path) == "" {
		return InvocationRecord{}, false, errors.New("invocation ledger path is required")
	}
	sessionID = strings.TrimSpace(sessionID)
	upstreamRequestID = strings.TrimSpace(upstreamRequestID)
	if sessionID == "" || upstreamRequestID == "" {
		return InvocationRecord{}, false, errors.New("completed invocation lookup requires session id and upstream request id")
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	state, err := readInvocationLedger(l.path)
	if err != nil {
		return InvocationRecord{}, false, err
	}
	var matched InvocationRecord
	for _, record := range state.records {
		if record.SessionID != sessionID || !record.Complete() || record.UpstreamRequestID != upstreamRequestID {
			continue
		}
		if matched != (InvocationRecord{}) {
			return InvocationRecord{}, false, fmt.Errorf("ambiguous completed invocation for provider response %q", upstreamRequestID)
		}
		matched = record
	}
	if matched == (InvocationRecord{}) {
		return InvocationRecord{}, false, nil
	}
	return matched, true, nil
}

const (
	invocationLedgerBound     = "bound"
	invocationLedgerCompleted = "completed"
)

type invocationLedgerEntry struct {
	Kind   string           `json:"kind"`
	Record InvocationRecord `json:"record"`
}

type invocationLedgerState struct {
	records map[string]InvocationRecord
}

func readInvocationLedger(path string) (invocationLedgerState, error) {
	state := invocationLedgerState{records: make(map[string]InvocationRecord)}
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return state, nil
	}
	if err != nil {
		return invocationLedgerState{}, err
	}
	defer file.Close() //nolint:errcheck // read-only handle

	reader := bufio.NewReaderSize(file, 64*1024)
	lineNo := 0
	for {
		line, readErr := reader.ReadString('\n')
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return invocationLedgerState{}, readErr
		}
		atEOF := errors.Is(readErr, io.EOF)
		if atEOF && line == "" {
			break
		}
		lineNo++
		if content := strings.TrimSpace(line); content != "" {
			var entry invocationLedgerEntry
			if err := json.Unmarshal([]byte(content), &entry); err != nil {
				return invocationLedgerState{}, fmt.Errorf("decode invocation ledger %s:%d: %w", path, lineNo, err)
			}
			if err := entry.Record.validateBound(); err != nil {
				return invocationLedgerState{}, fmt.Errorf("invalid invocation ledger record at %s:%d: %w", path, lineNo, err)
			}
			key := invocationRecordKey(entry.Record.SessionID, entry.Record.Attribution.InvocationID)
			existing, exists := state.records[key]
			switch entry.Kind {
			case invocationLedgerBound:
				if entry.Record.Complete() {
					return invocationLedgerState{}, fmt.Errorf("completed binding at %s:%d", path, lineNo)
				}
				if exists && !sameInvocationBinding(existing, entry.Record) {
					return invocationLedgerState{}, fmt.Errorf("invocation binding collision at %s:%d", path, lineNo)
				}
				if !exists {
					state.records[key] = entry.Record
				}
			case invocationLedgerCompleted:
				if !entry.Record.Complete() {
					return invocationLedgerState{}, fmt.Errorf("incomplete completion at %s:%d", path, lineNo)
				}
				if !exists || !sameInvocationBinding(existing, entry.Record) {
					return invocationLedgerState{}, fmt.Errorf("completion without matching binding at %s:%d", path, lineNo)
				}
				if existing.Complete() && existing != entry.Record {
					return invocationLedgerState{}, fmt.Errorf("invocation completion collision at %s:%d", path, lineNo)
				}
				state.records[key] = entry.Record
			default:
				return invocationLedgerState{}, fmt.Errorf("invocation ledger entry at %s:%d has unknown kind %q", path, lineNo, entry.Kind)
			}
		}
		if atEOF {
			break
		}
	}
	return state, nil
}

func sameInvocationBinding(a, b InvocationRecord) bool {
	return a.SessionID == b.SessionID &&
		a.Attribution.State == b.Attribution.State &&
		a.Attribution.City == b.Attribution.City &&
		a.Attribution.Rig == b.Attribution.Rig &&
		a.Attribution.WorkBeadID == b.Attribution.WorkBeadID &&
		a.Attribution.Attempt == b.Attribution.Attempt &&
		a.Attribution.InvocationID == b.Attribution.InvocationID &&
		a.Attribution.StartHead == b.Attribution.StartHead
}

func appendInvocationLedgerEntry(path string, entry invocationLedgerEntry) error {
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
