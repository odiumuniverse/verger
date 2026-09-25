// Package lock models verger.lock: the exact versions, digests and delivery
// strategies of every (package, host, scope) cell, plus the registry snapshot
// the state was resolved against.
//
// The document is machine-generated and synced between machines, so Marshal
// emits it deterministically: cells sorted by package, host and scope, JSON
// object keys alphabetical (map merge), snake_case fields, two-space
// indentation, no HTML escaping and a trailing newline. Unknown fields from
// newer minor versions survive Parse→Marshal at the lock level and per cell.
//
// JSON decoding follows encoding/json semantics: the last occurrence of a
// duplicate key wins, a byte-order mark is a syntax error, and invalid UTF-8
// inside a string is replaced with U+FFFD rather than rejected.
package lock

import (
	"bytes"
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/odiumuniverse/verger/pkg/digest"
	"github.com/odiumuniverse/verger/pkg/fsutil"
)

// Schema is the lock schema version this build reads and writes.
const Schema = 1

// lockKeys are the JSON fields Lock declares; everything else is kept as an
// unknown field for round-trip.
var lockKeys = [...]string{"schema", "snapshot", "cells"}

// snapshotKeys are the JSON fields Snapshot declares.
var snapshotKeys = [...]string{"generated_at", "key_id", "digest"}

// Snapshot identifies the registry snapshot the lock was resolved against.
// Digest is opaque here; T3.1 defines what it covers.
type Snapshot struct {
	GeneratedAt time.Time   `json:"generated_at,omitzero"`
	KeyID       string      `json:"key_id,omitempty"` // minisign key id
	Digest      digest.Hash `json:"digest"`
}

// Lock is the root of verger.lock.
type Lock struct {
	Schema   int      `json:"schema"`
	Snapshot Snapshot `json:"snapshot,omitzero"`
	Cells    []Cell   `json:"cells"`

	extra map[string]json.RawMessage
}

// SchemaNewerError reports a lock written by a newer verger.
type SchemaNewerError struct {
	Path      string
	Found     int
	Supported int
}

// Error implements error.
func (e *SchemaNewerError) Error() string {
	return fmt.Sprintf("lock %s: schema %d is newer than supported %d", e.Path, e.Found, e.Supported)
}

// SchemaInvalidError reports a lock without a usable schema version.
type SchemaInvalidError struct {
	Path  string
	Found int
}

// Error implements error.
func (e *SchemaInvalidError) Error() string {
	return fmt.Sprintf("lock %s: invalid schema %d", e.Path, e.Found)
}

// InvalidLockError reports bytes that are not a lock document: JSON syntax or
// shape errors and duplicate cells.
type InvalidLockError struct {
	Cause error
}

// Error implements error.
func (e *InvalidLockError) Error() string {
	return fmt.Sprintf("invalid lock: %v", e.Cause)
}

// Unwrap reports the underlying cause.
func (e *InvalidLockError) Unwrap() error {
	return e.Cause
}

// InvalidCellError reports a cell that cannot enter the lock.
type InvalidCellError struct {
	Package string
	Host    string
	Scope   string
	Cause   error
}

// Error implements error.
func (e *InvalidCellError) Error() string {
	return fmt.Sprintf("invalid cell %s %s/%s: %s", e.Package, e.Host, e.Scope, e.Cause)
}

// Unwrap reports the underlying cause.
func (e *InvalidCellError) Unwrap() error {
	return e.Cause
}

// New returns an empty current-schema lock.
func New() *Lock {
	return &Lock{Schema: Schema, Cells: []Cell{}}
}

// Parse decodes and validates a lock document: schema guard, every cell valid
// and no duplicate (package, host, scope).
func Parse(data []byte) (*Lock, error) {
	return parseLock(data, "")
}

// ParseFile loads and validates the lock at path. A missing file reports an
// error matching fs.ErrNotExist; a newer schema reports *SchemaNewerError with
// the path filled in.
func ParseFile(path string) (*Lock, error) {
	data, err := os.ReadFile(path) //nolint:gosec // G304: the caller names the state file
	if err != nil {
		return nil, fmt.Errorf("parse lock %s: %w", path, err)
	}

	return parseLock(data, path)
}

func parseLock(data []byte, path string) (*Lock, error) {
	var lock Lock

	if err := json.Unmarshal(data, &lock); err != nil {
		return nil, &InvalidLockError{Cause: err}
	}

	switch {
	case lock.Schema > Schema:
		return nil, &SchemaNewerError{Path: path, Found: lock.Schema, Supported: Schema}
	case lock.Schema <= 0:
		return nil, &SchemaInvalidError{Path: path, Found: lock.Schema}
	}

	seen := make(map[cellKey]struct{}, len(lock.Cells))

	for _, cell := range lock.Cells {
		if err := ValidateCell(cell); err != nil {
			return nil, err
		}

		key := cellKey{cell.Package, cell.Host, cell.Scope}

		if _, ok := seen[key]; ok {
			return nil, &InvalidLockError{Cause: fmt.Errorf("duplicate cell %s %s/%s", cell.Package, cell.Host, cell.Scope)}
		}

		seen[key] = struct{}{}
	}

	return &lock, nil
}

// Marshal returns the canonical JSON: cells sorted by (package, host, scope),
// unknown fields merged back at both levels, two-space indentation, no HTML
// escaping and a trailing newline. The receiver is not modified. A newer schema
// is refused with *SchemaNewerError.
func (l *Lock) Marshal() ([]byte, error) {
	compact, err := l.marshalCompact()
	if err != nil {
		return nil, err
	}

	var out bytes.Buffer

	if err := json.Indent(&out, compact, "", "  "); err != nil {
		return nil, &InvalidLockError{Cause: err}
	}

	out.WriteByte('\n')

	return out.Bytes(), nil
}

// Save marshals the lock and atomically writes it to path. New files get mode
// 0600; an existing file keeps its mode. The parent directory must exist. A
// newer schema is refused with *SchemaNewerError before anything is written.
func (l *Lock) Save(path string) error {
	data, err := l.Marshal()
	if err != nil {
		return err
	}

	perm := os.FileMode(0o600)

	info, statErr := os.Stat(path)

	switch {
	case statErr == nil:
		perm = info.Mode().Perm()
	case !errors.Is(statErr, fs.ErrNotExist):
		return fmt.Errorf("stat lock %s: %w", path, statErr)
	}

	if err := fsutil.WriteFileAtomic(path, data, perm); err != nil {
		return fmt.Errorf("save lock %s: %w", path, err)
	}

	return nil
}

// Digest hashes the canonical bytes of the lock; the journal records it for
// lock generations. It returns the zero Hash when the lock cannot be marshaled.
func (l *Lock) Digest() digest.Hash {
	data, err := l.Marshal()
	if err != nil {
		return ""
	}

	return digest.Bytes(data)
}

// Cell returns the cell for the given key.
func (l *Lock) Cell(pkg, host, scope string) (Cell, bool) {
	index := l.cellIndex(pkg, host, scope)
	if index < 0 {
		return Cell{}, false
	}

	return l.Cells[index], true
}

// Upsert replaces or appends cells by (package, host, scope) key. Every input
// is validated first, so a bad cell leaves the lock untouched; replacing a cell
// keeps its previously stored unknown fields, and incoming extras win.
func (l *Lock) Upsert(cells ...Cell) error {
	for _, cell := range cells {
		if err := ValidateCell(cell); err != nil {
			return err
		}
	}

	for _, cell := range cells {
		next := cell.clone()
		index := l.cellIndex(cell.Package, cell.Host, cell.Scope)

		if index < 0 {
			l.Cells = append(l.Cells, next)

			continue
		}

		next.extra = mergeCellExtras(l.Cells[index].extra, next.extra)
		l.Cells[index] = next
	}

	return nil
}

// Delete removes the matching cells and returns how many were removed.
func (l *Lock) Delete(pkg, host, scope string) int {
	kept := l.Cells[:0]
	removed := 0

	for _, cell := range l.Cells {
		if cell.Package == pkg && cell.Host == host && cell.Scope == scope {
			removed++

			continue
		}

		kept = append(kept, cell)
	}

	l.Cells = kept

	return removed
}

// Sort orders cells by (package, host, scope); stable for equal keys.
func (l *Lock) Sort() {
	slices.SortStableFunc(l.Cells, compareCells)
}

func (l *Lock) cellIndex(pkg, host, scope string) int {
	for index, cell := range l.Cells {
		if cell.Package == pkg && cell.Host == host && cell.Scope == scope {
			return index
		}
	}

	return -1
}

// MarshalJSON encodes the snapshot with snake_case keys in deterministic
// (alphabetical) order.
func (s Snapshot) MarshalJSON() ([]byte, error) {
	type plain Snapshot

	known, err := encodeJSON(plain(s))
	if err != nil {
		return nil, err
	}

	return mergeExtras(known, snapshotKeys[:], nil)
}

// UnmarshalJSON decodes the lock and keeps unknown fields for round-trip. Use
// Parse for the typed schema/validation error taxonomy.
func (l *Lock) UnmarshalJSON(data []byte) error {
	type plain Lock

	var parsed plain

	if err := json.Unmarshal(data, &parsed); err != nil {
		return err
	}

	var all map[string]json.RawMessage
	if err := json.Unmarshal(data, &all); err != nil {
		return err
	}

	*l = Lock(parsed)
	l.extra = dropKeys(all, lockKeys[:])

	return nil
}

func (l *Lock) marshalCompact() ([]byte, error) {
	if l == nil {
		return nil, &InvalidLockError{Cause: errors.New("nil lock")}
	}

	if l.Schema > Schema {
		return nil, &SchemaNewerError{Found: l.Schema}
	}

	copied := *l
	copied.Cells = slices.Clone(l.Cells)

	if copied.Cells == nil {
		copied.Cells = []Cell{}
	}

	slices.SortStableFunc(copied.Cells, compareCells)

	known, err := encodeJSON(copied)
	if err != nil {
		return nil, &InvalidLockError{Cause: err}
	}

	return mergeExtras(known, lockKeys[:], l.extra)
}

type cellKey struct {
	pkg   string
	host  string
	scope string
}

func compareCells(a, b Cell) int {
	return cmp.Or(
		cmp.Compare(a.Package, b.Package),
		cmp.Compare(a.Host, b.Host),
		cmp.Compare(a.Scope, b.Scope),
	)
}

// encodeJSON encodes without HTML escaping: lock values are data, and `<pkg>`
// must survive as itself.
func encodeJSON(value any) ([]byte, error) {
	var buf bytes.Buffer

	encoder := json.NewEncoder(&buf)
	encoder.SetEscapeHTML(false)

	if err := encoder.Encode(value); err != nil {
		return nil, err
	}

	return bytes.TrimSuffix(buf.Bytes(), []byte{'\n'}), nil
}

// mergeExtras re-encodes the known object as a sorted-key map with the unknown
// fields spliced back in; declared names win over case-insensitive collisions.
func mergeExtras(known []byte, names []string, extras map[string]json.RawMessage) ([]byte, error) {
	var fields map[string]json.RawMessage

	if err := json.Unmarshal(known, &fields); err != nil {
		return nil, err
	}

	lower := make(map[string]struct{}, len(fields)+len(names))

	for key := range fields {
		lower[strings.ToLower(key)] = struct{}{}
	}

	for _, name := range names {
		lower[strings.ToLower(name)] = struct{}{}
	}

	for key, raw := range extras {
		if _, ok := lower[strings.ToLower(key)]; !ok {
			fields[key] = raw
		}
	}

	return encodeJSON(fields)
}
