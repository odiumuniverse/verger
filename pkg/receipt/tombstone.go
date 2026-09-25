package receipt

import (
	"bytes"
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/odiumuniverse/verger/pkg/digest"
	"github.com/odiumuniverse/verger/pkg/fsutil"
)

// Tombstone records one removed cell for the §5.6 guard data.
type Tombstone struct {
	Schema        int         `json:"schema"`
	Package       string      `json:"package"`
	Host          string      `json:"host"` // initiator host
	Scope         string      `json:"scope"`
	RemovedAt     time.Time   `json:"removed_at"`
	Cause         Cause       `json:"cause"`
	TrashID       string      `json:"trash_id,omitempty"` // store trash bucket
	ReceiptDigest digest.Hash `json:"receipt_digest,omitempty"`

	extra map[string]json.RawMessage
}

// tombstoneWire is the wire shape of Tombstone without its marshal methods.
type tombstoneWire Tombstone

// MarshalJSON encodes the tombstone with preserved unknown fields.
func (t Tombstone) MarshalJSON() ([]byte, error) {
	return marshalWithExtras(tombstoneWire(t), t.extra)
}

// UnmarshalJSON decodes the tombstone and stashes unknown fields.
func (t *Tombstone) UnmarshalJSON(data []byte) error {
	extras, err := unmarshalWithExtras(data, (*tombstoneWire)(t))
	if err != nil {
		return err
	}

	t.extra = extras

	return nil
}

// Validate checks the tombstone shape: identifiers, scope and cause enums and
// a non-zero removal time.
func (t Tombstone) Validate() error {
	fail := func(format string, args ...any) error {
		return &InvalidTombstoneError{Cause: fmt.Errorf(format, args...)}
	}

	if t.Package == "" {
		return fail("package is required")
	}

	if t.Host == "" {
		return fail("host is required")
	}

	if !validScope(t.Scope) {
		return fail("unknown scope %q", t.Scope)
	}

	if !validCause(t.Cause) {
		return fail("unknown cause %q", t.Cause)
	}

	if t.RemovedAt.IsZero() {
		return fail("removed_at is required")
	}

	return nil
}

// validCause reports whether cause is a known tombstone cause.
func validCause(cause Cause) bool {
	switch cause {
	case CauseUser, CauseCapability, CauseHostReset:
		return true
	default:
		return false
	}
}

// TombstoneStore keeps the tombstone document at one path.
type TombstoneStore struct {
	path string
}

// NewTombstoneStore returns a tombstone store for path; the file is created on
// the first change.
func NewTombstoneStore(path string) *TombstoneStore {
	return &TombstoneStore{path: path}
}

// tombstoneDoc is the decoded on-disk document with preserved unknown
// top-level fields.
type tombstoneDoc struct {
	Tombstones []Tombstone
	extra      map[string]json.RawMessage
}

// tombstoneDocWire is the wire shape of the tombstone document.
type tombstoneDocWire struct {
	Schema     int         `json:"schema"`
	Tombstones []Tombstone `json:"tombstones"`
}

// Load returns every tombstone sorted by package, host and scope.
func (s *TombstoneStore) Load() ([]Tombstone, error) {
	doc, err := s.load()
	if err != nil {
		return nil, err
	}

	return slices.Clone(doc.Tombstones), nil
}

// Add validates the record and upserts it by (package, host, scope), forcing
// the current schema.
func (s *TombstoneStore) Add(record Tombstone) error {
	record.Schema = Schema

	if err := record.Validate(); err != nil {
		return err
	}

	if err := validateTombstoneKeys(record); err != nil {
		return err
	}

	doc, err := s.load()
	if err != nil {
		return err
	}

	replaced := false

	for i := range doc.Tombstones {
		if sameKey(doc.Tombstones[i], record.Package, record.Host, record.Scope) {
			doc.Tombstones[i] = record
			replaced = true

			break
		}
	}

	if !replaced {
		doc.Tombstones = append(doc.Tombstones, record)
	}

	slices.SortStableFunc(doc.Tombstones, compareTombstones)

	return s.save(doc)
}

// Delete removes one record and reports whether it existed.
func (s *TombstoneStore) Delete(pkg, host, scope string) (bool, error) {
	doc, err := s.load()
	if err != nil {
		return false, err
	}

	kept := slices.DeleteFunc(doc.Tombstones, func(t Tombstone) bool {
		return sameKey(t, pkg, host, scope)
	})

	if len(kept) == len(doc.Tombstones) {
		return false, nil
	}

	doc.Tombstones = kept

	if err := s.save(doc); err != nil {
		return false, err
	}

	return true, nil
}

// Prune removes records whose RemovedAt is strictly before the cutoff and
// returns the count. The 30-day window is the caller's policy.
func (s *TombstoneStore) Prune(before time.Time) (int, error) {
	doc, err := s.load()
	if err != nil {
		return 0, err
	}

	kept := make([]Tombstone, 0, len(doc.Tombstones))
	count := 0

	for _, record := range doc.Tombstones {
		if record.RemovedAt.Before(before) {
			count++

			continue
		}

		kept = append(kept, record)
	}

	if count == 0 {
		return 0, nil
	}

	doc.Tombstones = kept

	if err := s.save(doc); err != nil {
		return 0, err
	}

	return count, nil
}

// load reads and validates the document; a missing file is an empty document.
func (s *TombstoneStore) load() (tombstoneDoc, error) {
	doc := tombstoneDoc{}

	data, err := os.ReadFile(s.path) //nolint:gosec // G304: the path is owned by the caller
	if errors.Is(err, fs.ErrNotExist) {
		return doc, nil
	}

	if err != nil {
		return tombstoneDoc{}, &CorruptTombstonesError{Path: s.path, Cause: err}
	}

	if len(bytes.TrimSpace(data)) == 0 {
		return doc, nil
	}

	known := tombstoneDocWire{}

	extras, err := unmarshalWithExtras(data, &known)
	if err != nil {
		return tombstoneDoc{}, &CorruptTombstonesError{Path: s.path, Cause: err}
	}

	switch {
	case known.Schema > Schema:
		return tombstoneDoc{}, &SchemaNewerError{Path: s.path, Found: known.Schema, Supported: Schema}
	case known.Schema < 1:
		return tombstoneDoc{}, &CorruptTombstonesError{Path: s.path, Cause: errors.New("unsupported schema")}
	}

	for i, record := range known.Tombstones {
		if err := record.Validate(); err != nil {
			return tombstoneDoc{}, &CorruptTombstonesError{Path: s.path, Cause: fmt.Errorf("record %d: %w", i, err)}
		}

		if err := validateTombstoneKeys(record); err != nil {
			return tombstoneDoc{}, &CorruptTombstonesError{Path: s.path, Cause: fmt.Errorf("record %d: %w", i, err)}
		}
	}

	doc.Tombstones = known.Tombstones
	doc.extra = extras

	slices.SortStableFunc(doc.Tombstones, compareTombstones)

	return doc, nil
}

// save writes the document atomically with mode 0600.
func (s *TombstoneStore) save(doc tombstoneDoc) error {
	data, err := encodeIndented(tombstoneDocWire{Schema: Schema, Tombstones: doc.Tombstones})
	if err != nil {
		return fmt.Errorf("encode tombstones: %w", err)
	}

	if len(doc.extra) > 0 {
		data, err = reattachExtras(data, doc.extra)
		if err != nil {
			return fmt.Errorf("encode tombstones: %w", err)
		}
	}

	if err := fsutil.EnsureDir(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}

	return fsutil.WriteFileAtomic(s.path, data, 0o600)
}

// reattachExtras merges preserved fields into an already indented document.
func reattachExtras(data []byte, extras map[string]json.RawMessage) ([]byte, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return nil, err
	}

	for key, raw := range extras {
		if _, exists := fields[key]; !exists {
			fields[key] = raw
		}
	}

	return encodeIndented(fields)
}

// validateTombstoneKeys rejects unsafe key components.
func validateTombstoneKeys(record Tombstone) error {
	if err := validatePackage(record.Package); err != nil {
		return err
	}

	if err := validateKeyElement("host", record.Host); err != nil {
		return err
	}

	if !validScope(record.Scope) {
		return &InvalidKeyError{Field: "scope", Value: record.Scope}
	}

	return nil
}

// sameKey reports whether one tombstone matches a key.
func sameKey(record Tombstone, pkg, host, scope string) bool {
	return record.Package == pkg && record.Host == host && record.Scope == scope
}

// compareTombstones orders records by package, host and scope.
func compareTombstones(a, b Tombstone) int {
	return cmp.Or(
		cmp.Compare(a.Package, b.Package),
		cmp.Compare(a.Host, b.Host),
		cmp.Compare(a.Scope, b.Scope),
	)
}

// CorruptTombstonesError reports an unreadable or inconsistent tombstone file.
type CorruptTombstonesError struct {
	Path  string
	Cause error
}

// Error implements error.
func (e *CorruptTombstonesError) Error() string {
	return fmt.Sprintf("corrupt tombstones %s: %v", e.Path, e.Cause)
}

// Unwrap returns the underlying cause.
func (e *CorruptTombstonesError) Unwrap() error {
	return e.Cause
}

// InvalidTombstoneError reports a tombstone that fails validation.
type InvalidTombstoneError struct {
	Cause error
}

// Error implements error.
func (e *InvalidTombstoneError) Error() string {
	return "invalid tombstone: " + e.Cause.Error()
}

// Unwrap returns the underlying cause.
func (e *InvalidTombstoneError) Unwrap() error {
	return e.Cause
}
