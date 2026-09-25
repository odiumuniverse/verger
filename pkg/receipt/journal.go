package receipt

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/odiumuniverse/verger/pkg/digest"
	"github.com/odiumuniverse/verger/pkg/fsutil"
)

// EventKind names one journal event.
type EventKind string

// Journal event kinds.
const (
	EventInstall EventKind = "install"
	EventRemove  EventKind = "remove"
	EventUpdate  EventKind = "update"
	EventDisable EventKind = "disable"
	EventEnable  EventKind = "enable"
	EventAdopt   EventKind = "adopt"
	EventRestore EventKind = "restore"
	EventPin     EventKind = "pin"
	EventLock    EventKind = "lock" // lock generation for `verger rollback`
)

// Event is one append-only journal record.
type Event struct {
	Seq            int64                      `json:"seq"`
	At             time.Time                  `json:"at"`
	Kind           EventKind                  `json:"kind"`
	Package        string                     `json:"package,omitempty"`
	Host           string                     `json:"host,omitempty"`
	Scope          string                     `json:"scope,omitempty"`
	Version        string                     `json:"version,omitempty"`
	Cause          string                     `json:"cause,omitempty"`
	Initiator      string                     `json:"initiator,omitempty"` // host that initiated a removal (§5.6)
	LockGeneration int                        `json:"lock_generation,omitempty"`
	LockDigest     digest.Hash                `json:"lock_digest,omitempty"`
	Extra          map[string]json.RawMessage `json:"extra,omitempty"`
}

// Validate checks the event kind, scope and numeric fields.
func (e Event) Validate() error {
	if !validEventKind(e.Kind) {
		return &InvalidEventError{Cause: fmt.Errorf("unknown kind %q", e.Kind)}
	}

	if e.Scope != "" && !validScope(e.Scope) {
		return &InvalidEventError{Cause: fmt.Errorf("unknown scope %q", e.Scope)}
	}

	if e.Seq < 0 {
		return &InvalidEventError{Cause: fmt.Errorf("negative seq %d", e.Seq)}
	}

	if e.LockGeneration < 0 {
		return &InvalidEventError{Cause: fmt.Errorf("negative lock generation %d", e.LockGeneration)}
	}

	if e.LockDigest != "" && !e.LockDigest.Valid() {
		return &InvalidEventError{Cause: fmt.Errorf("invalid lock digest %q", e.LockDigest)}
	}

	return nil
}

// validEventKind reports whether kind is a known event.
func validEventKind(kind EventKind) bool {
	switch kind {
	case EventInstall, EventRemove, EventUpdate, EventDisable, EventEnable,
		EventAdopt, EventRestore, EventPin, EventLock:
		return true
	default:
		return false
	}
}

// Journal appends events to a JSONL file. One instance is goroutine-safe;
// cross-process serialization is the caller's job via the home flock.
type Journal struct {
	path string
	mu   sync.Mutex
	now  func() time.Time
}

// OpenJournal returns a journal handle for path; the file is created on the
// first append.
func OpenJournal(path string) *Journal {
	return &Journal{path: path, now: time.Now}
}

// Append validates every event, assigns Seq (last+1) and At (UTC now) to the
// zero values, repairs a torn trailing line, then writes all lines in one
// buffered write and fsyncs.
func (j *Journal) Append(events ...Event) error {
	if len(events) == 0 {
		return nil
	}

	j.mu.Lock()
	defer j.mu.Unlock()

	for _, e := range events {
		if err := e.Validate(); err != nil {
			return err
		}
	}

	prepared, err := j.assignLocked(events)
	if err != nil {
		return err
	}

	var buf bytes.Buffer

	for _, e := range prepared {
		line, err := json.Marshal(e)
		if err != nil {
			return fmt.Errorf("encode journal event: %w", err)
		}

		buf.Write(line)
		buf.WriteByte('\n')
	}

	if err := j.repairTailLocked(); err != nil {
		return err
	}

	if err := fsutil.EnsureDir(filepath.Dir(j.path), 0o700); err != nil {
		return err
	}

	return j.writeLocked(buf.Bytes())
}

// Read returns every complete event in file order; a trailing line without a
// newline (a torn write) is ignored.
func (j *Journal) Read() ([]Event, error) {
	j.mu.Lock()
	defer j.mu.Unlock()

	return j.readLocked()
}

// LastSeq returns the sequence number of the last complete event, or 0 for an
// empty journal.
func (j *Journal) LastSeq() (int64, error) {
	j.mu.Lock()
	defer j.mu.Unlock()

	events, err := j.readLocked()
	if err != nil {
		return 0, err
	}

	if len(events) == 0 {
		return 0, nil
	}

	return events[len(events)-1].Seq, nil
}

// assignLocked fills Seq and At and enforces a strictly increasing sequence.
func (j *Journal) assignLocked(events []Event) ([]Event, error) {
	last, err := j.lastSeqLocked()
	if err != nil {
		return nil, err
	}

	next := last
	out := make([]Event, len(events))

	for i, e := range events {
		if e.Seq == 0 {
			next++
			e.Seq = next
		} else {
			if e.Seq <= next {
				return nil, &InvalidEventError{Cause: fmt.Errorf("seq %d is not after %d", e.Seq, next)}
			}

			next = e.Seq
		}

		e.At = e.At.UTC()
		if e.At.IsZero() {
			e.At = j.now().UTC()
		}

		out[i] = e
	}

	return out, nil
}

// lastSeqLocked reads the last complete event's sequence.
func (j *Journal) lastSeqLocked() (int64, error) {
	events, err := j.readLocked()
	if err != nil {
		return 0, err
	}

	if len(events) == 0 {
		return 0, nil
	}

	return events[len(events)-1].Seq, nil
}

// readLocked parses the journal file. Callers hold the mutex.
func (j *Journal) readLocked() ([]Event, error) {
	data, err := os.ReadFile(j.path) //nolint:gosec // G304: the journal path is owned by the caller
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}

	if err != nil {
		return nil, fmt.Errorf("read journal %s: %w", j.path, err)
	}

	var events []Event

	line := 0

	for len(data) > 0 {
		idx := bytes.IndexByte(data, '\n')
		if idx < 0 {
			// A trailing line without a newline is a torn write: ignore it.
			break
		}

		raw := data[:idx]
		data = data[idx+1:]
		line++

		if len(raw) == 0 {
			return nil, &CorruptJournalError{Line: line, Cause: errors.New("empty line")}
		}

		e := Event{}
		if err := json.Unmarshal(raw, &e); err != nil {
			return nil, &CorruptJournalError{Line: line, Cause: err}
		}

		if err := e.Validate(); err != nil {
			return nil, &CorruptJournalError{Line: line, Cause: err}
		}

		events = append(events, e)
	}

	return events, nil
}

// repairTailLocked truncates a torn trailing line so a restart after a crash
// cannot concatenate the next event onto garbage.
func (j *Journal) repairTailLocked() error {
	data, err := os.ReadFile(j.path) //nolint:gosec // G304: the journal path is owned by the caller
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}

	if err != nil {
		return fmt.Errorf("read journal %s: %w", j.path, err)
	}

	if len(data) == 0 || data[len(data)-1] == '\n' {
		return nil
	}

	idx := bytes.LastIndexByte(data, '\n')

	if err := os.Truncate(j.path, int64(idx+1)); err != nil {
		return fmt.Errorf("repair journal %s: %w", j.path, err)
	}

	return nil
}

// writeLocked appends the buffered lines and fsyncs the file.
func (j *Journal) writeLocked(lines []byte) error {
	file, err := os.OpenFile(j.path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600) //nolint:gosec // G304: the journal path is owned by the caller
	if err != nil {
		return fmt.Errorf("open journal %s: %w", j.path, err)
	}

	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()

		return fmt.Errorf("chmod journal %s: %w", j.path, err)
	}

	if _, err := file.Write(lines); err != nil {
		_ = file.Close()

		return fmt.Errorf("append journal %s: %w", j.path, err)
	}

	if err := file.Sync(); err != nil {
		_ = file.Close()

		return fmt.Errorf("sync journal %s: %w", j.path, err)
	}

	if err := file.Close(); err != nil {
		return fmt.Errorf("close journal %s: %w", j.path, err)
	}

	return nil
}

// CorruptJournalError reports a complete journal line that cannot be read.
type CorruptJournalError struct {
	Line  int
	Cause error
}

// Error implements error.
func (e *CorruptJournalError) Error() string {
	return fmt.Sprintf("corrupt journal line %d: %v", e.Line, e.Cause)
}

// Unwrap returns the underlying cause.
func (e *CorruptJournalError) Unwrap() error {
	return e.Cause
}
