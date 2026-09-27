// Package consent records hook approvals and project trust: D5 hooks consent
// keyed by content hash and package version, D11 project trust keyed by the
// canonical spec digest.
//
// A Store reads its file on first use (Load or the first error-returning call)
// and mutates memory only; callers persist with Save while holding the home
// flock. Boolean accessors ignore read errors on purpose: an unreadable store
// reads as "nothing approved", which fails safe (the caller asks again).
package consent

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/odiumuniverse/verger/pkg/digest"
	"github.com/odiumuniverse/verger/pkg/fsutil"
	"github.com/odiumuniverse/verger/pkg/manifest"
)

// Schema is the consent and trust file schema version.
const Schema = 1

// Hooks consent modes mirroring spec.HooksMode decisions passed by the CLI.
const (
	ModeAsk = "ask"
	ModeYes = "yes"
	ModeNo  = "no"
)

// HookHash is the D5 content hash: canonical hook definitions plus the package
// version, independent of hook order. The canonical bytes are one
// "<pkg>@<version>" line followed by one sorted line per hook,
// "<event>\x00<matcher>\x00<command>\x00<timeout>"; an empty hook list still
// hashes its version header, so a package without hooks keeps a stable record.
// The origin dialect is not content and never changes the hash.
func HookHash(pkg, version string, hooks []manifest.Hook) digest.Hash {
	lines := make([]string, 0, len(hooks))

	for _, h := range hooks {
		lines = append(lines, strings.Join([]string{h.Event, h.Matcher, h.Command, strconv.Itoa(h.Timeout)}, "\x00"))
	}

	slices.Sort(lines)

	var b strings.Builder

	b.WriteString(pkg)
	b.WriteByte('@')
	b.WriteString(version)
	b.WriteByte('\n')

	for _, line := range lines {
		b.WriteString(line)
		b.WriteByte('\n')
	}

	return digest.Bytes([]byte(b.String()))
}

// HookRecord is one approved hooks decision: the package at a version is
// approved for exactly one content hash.
type HookRecord struct {
	Package    string      `json:"package"`
	Version    string      `json:"version"`
	Hash       digest.Hash `json:"hash"`
	ApprovedAt time.Time   `json:"approved_at"`

	extra map[string]json.RawMessage
}

// hookRecordWire is the wire shape of HookRecord without its marshal methods.
type hookRecordWire HookRecord

// MarshalJSON encodes the record with preserved unknown fields.
func (r HookRecord) MarshalJSON() ([]byte, error) {
	return marshalWithExtras(hookRecordWire(r), r.extra)
}

// UnmarshalJSON decodes the record and stashes unknown fields for round-trip.
func (r *HookRecord) UnmarshalJSON(data []byte) error {
	extras, err := unmarshalWithExtras(data, (*hookRecordWire)(r))
	if err != nil {
		return err
	}

	r.extra = extras

	return nil
}

// consentDoc is the on-disk consent document.
type consentDoc struct {
	Schema int                   `json:"schema"`
	Hooks  map[string]HookRecord `json:"hooks"`

	extra map[string]json.RawMessage
}

// consentDocWire is the wire shape of consentDoc without its marshal methods.
type consentDocWire consentDoc

// MarshalJSON encodes the document with preserved unknown fields.
func (d consentDoc) MarshalJSON() ([]byte, error) {
	return marshalWithExtras(consentDocWire(d), d.extra)
}

// UnmarshalJSON decodes the document and stashes unknown fields for round-trip.
func (d *consentDoc) UnmarshalJSON(data []byte) error {
	extras, err := unmarshalWithExtras(data, (*consentDocWire)(d))
	if err != nil {
		return err
	}

	d.extra = extras

	return nil
}

// Store keeps hook approvals in memory and reads/writes
// <home>/state/consent.json. It is not goroutine-safe; callers hold the home
// flock when writing.
type Store struct {
	core stateStore[consentDoc, string, HookRecord]
}

// Option configures a Store or TrustStore constructor; WithClock is the only
// exported option.
type Option func(*Store)

// WithClock replaces the clock used to stamp records.
func WithClock(now func() time.Time) Option {
	return func(s *Store) {
		s.core.now = now
	}
}

// NewStore returns an empty hook consent store for path; the file is read on
// first use.
func NewStore(path string, opts ...Option) *Store {
	s := &Store{core: stateStore[consentDoc, string, HookRecord]{
		path:      path,
		records:   map[string]HookRecord{},
		now:       time.Now,
		schema:    func(doc consentDoc) int { return doc.Schema },
		recordsOf: func(doc consentDoc) map[string]HookRecord { return doc.Hooks },
		keyOf:     func(rec HookRecord) string { return rec.Package },
		extrasOf:  func(doc consentDoc) map[string]json.RawMessage { return doc.extra },
	}}

	for _, opt := range opts {
		opt(s)
	}

	if s.core.now == nil {
		s.core.now = time.Now
	}

	return s
}

// Load reads the consent file; a missing file yields an empty store. Corrupt
// documents and unsupported schemas are typed errors and never overwrite the
// file.
func (s *Store) Load() error {
	return s.core.load()
}

// Save writes the consent file atomically with mode 0600, creating the parent
// state directory 0700. A file that cannot be decoded is never overwritten.
func (s *Store) Save() error {
	return s.core.save(consentDoc{Schema: Schema, Hooks: s.core.records, extra: s.core.extras})
}

// Hooks returns the stored approval for pkg.
func (s *Store) Hooks(pkg string) (HookRecord, bool) {
	_ = s.core.ensureLoaded()

	rec, ok := s.core.records[pkg]

	return rec, ok
}

// ApproveHooks upserts an explicit approval for pkg at version and hash. It
// mutates memory only; call Save to persist.
func (s *Store) ApproveHooks(pkg, version string, hash digest.Hash) error {
	if err := s.core.ensureLoaded(); err != nil {
		return err
	}

	s.core.records[pkg] = HookRecord{Package: pkg, Version: version, Hash: hash, ApprovedAt: s.core.now().UTC()}

	return nil
}

// RevokeHooks deletes the approval for pkg; revoking a missing package is not
// an error.
func (s *Store) RevokeHooks(pkg string) error {
	if err := s.core.ensureLoaded(); err != nil {
		return err
	}

	delete(s.core.records, pkg)

	return nil
}

// HooksApproved reports whether pkg at version is approved for exactly hash
// (D5: both the hash and the version must match).
func (s *Store) HooksApproved(pkg, version string, hash digest.Hash) bool {
	_ = s.core.ensureLoaded()

	rec, ok := s.core.records[pkg]

	return ok && rec.Version == version && rec.Hash == hash
}

// Pending reports hooks that changed under an already-approved package: a
// record exists but its hash or version differs, so the background path
// delivers without hooks and writes a note.
func (s *Store) Pending(pkg, version string, hash digest.Hash) bool {
	_ = s.core.ensureLoaded()

	rec, ok := s.core.records[pkg]
	if !ok {
		return false
	}

	return rec.Version != version || rec.Hash != hash
}

// ---- shared machinery --------------------------------------------------------

// stateStore is the shared core of the consent and trust stores: a versioned
// JSON document of keyed records, read lazily and written atomically. The
// document adapters extract its schema, keyed records and preserved unknown
// fields.
type stateStore[T any, K comparable, R any] struct {
	path    string
	records map[K]R
	extras  map[string]json.RawMessage
	loaded  bool
	now     func() time.Time

	schema    func(T) int
	recordsOf func(T) map[K]R
	keyOf     func(R) K
	extrasOf  func(T) map[string]json.RawMessage
}

// load reads the file once, guards the schema and validates record-key
// agreement; a missing file yields empty records.
func (c *stateStore[T, K, R]) load() error {
	doc, ok, err := loadState[T](c.path)
	if err != nil {
		return err
	}

	if !ok {
		c.records = map[K]R{}
		c.extras = nil
		c.loaded = true

		return nil
	}

	if err := checkSchema(c.path, c.schema(doc)); err != nil {
		return err
	}

	records := c.recordsOf(doc)
	if err := checkRecordKeys(c.path, records, c.keyOf); err != nil {
		return err
	}

	if records == nil {
		records = map[K]R{}
	}

	c.records = records
	c.extras = c.extrasOf(doc)
	c.loaded = true

	return nil
}

// ensureLoaded reads the file once.
func (c *stateStore[T, K, R]) ensureLoaded() error {
	if c.loaded {
		return nil
	}

	return c.load()
}

// save writes doc atomically with mode 0600, creating the parent directory
// 0700. A file that cannot be decoded is never overwritten.
func (c *stateStore[T, K, R]) save(doc T) error {
	if err := c.ensureLoaded(); err != nil {
		return err
	}

	data, err := encodeIndented(doc)
	if err != nil {
		return fmt.Errorf("encode state: %w", err)
	}

	if err := fsutil.EnsureDir(filepath.Dir(c.path), 0o700); err != nil {
		return err
	}

	return fsutil.WriteFileAtomic(c.path, data, 0o600)
}

// loadState reads and decodes a versioned state document; ok is false when the
// file does not exist yet. Decode failures are ConsentParseError; the caller
// guards the schema.
func loadState[T any](path string) (T, bool, error) {
	var doc T

	data, err := os.ReadFile(path) //nolint:gosec // G304: the caller chooses the state path
	if errors.Is(err, fs.ErrNotExist) {
		return doc, false, nil
	}

	if err != nil {
		return doc, false, &ConsentParseError{Path: path, Cause: err}
	}

	if err := json.Unmarshal(data, &doc); err != nil {
		return doc, false, &ConsentParseError{Path: path, Cause: err}
	}

	return doc, true, nil
}

// checkRecordKeys verifies that every record's key field equals its map key.
func checkRecordKeys[K comparable, R any](path string, records map[K]R, key func(R) K) error {
	for mapKey, rec := range records {
		if key(rec) != mapKey {
			return &ConsentParseError{Path: path, Cause: fmt.Errorf("record %v does not match its key", mapKey)}
		}
	}

	return nil
}

// checkSchema guards a state document's schema version.
func checkSchema(path string, found int) error {
	switch {
	case found > Schema:
		return &SchemaNewerError{Path: path, Found: found, Supported: Schema}
	case found < 1:
		return &SchemaInvalidError{Path: path, Found: found}
	default:
		return nil
	}
}

// marshalWithExtras encodes known and re-attaches preserved unknown fields.
// The result is deterministic: the merged object is key-sorted.
func marshalWithExtras(known any, extras map[string]json.RawMessage) ([]byte, error) {
	data, err := json.Marshal(known)
	if err != nil {
		return nil, err
	}

	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return nil, fmt.Errorf("merge fields: %w", err)
	}

	for key, raw := range extras {
		if _, exists := fields[key]; !exists {
			fields[key] = raw
		}
	}

	return json.Marshal(fields)
}

// unmarshalWithExtras decodes known and returns the fields the known shape
// does not carry.
func unmarshalWithExtras(data []byte, known any) (map[string]json.RawMessage, error) {
	if err := json.Unmarshal(data, known); err != nil {
		return nil, err
	}

	var all map[string]json.RawMessage
	if err := json.Unmarshal(data, &all); err != nil {
		return nil, err
	}

	knownData, err := json.Marshal(known)
	if err != nil {
		return nil, err
	}

	var knownFields map[string]json.RawMessage
	if err := json.Unmarshal(knownData, &knownFields); err != nil {
		return nil, err
	}

	for key := range all {
		for knownKey := range knownFields {
			if strings.EqualFold(key, knownKey) {
				delete(all, key)

				break
			}
		}
	}

	if len(all) == 0 {
		return nil, nil
	}

	return all, nil
}

// encodeIndented renders a state value as indented JSON with a trailing
// newline.
func encodeIndented(value any) ([]byte, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}

	var buf bytes.Buffer
	if err := json.Indent(&buf, data, "", "  "); err != nil {
		return nil, err
	}

	return append(buf.Bytes(), '\n'), nil
}

// ---- errors ------------------------------------------------------------------

// SchemaNewerError reports a persisted schema this build cannot read.
type SchemaNewerError struct {
	Path      string
	Found     int
	Supported int
}

// Error implements error.
func (e *SchemaNewerError) Error() string {
	return fmt.Sprintf("%s: schema %d is newer than the supported %d", e.Path, e.Found, e.Supported)
}

// SchemaInvalidError reports a missing or non-positive schema.
type SchemaInvalidError struct {
	Path  string
	Found int
}

// Error implements error.
func (e *SchemaInvalidError) Error() string {
	return fmt.Sprintf("%s: invalid schema %d", e.Path, e.Found)
}

// ConsentParseError reports an unreadable or inconsistent consent/trust file.
type ConsentParseError struct {
	Path  string
	Cause error
}

// Error implements error.
func (e *ConsentParseError) Error() string {
	return fmt.Sprintf("parse consent %s: %v", e.Path, e.Cause)
}

// Unwrap returns the underlying cause.
func (e *ConsentParseError) Unwrap() error {
	return e.Cause
}

// InvalidProjectError reports a project root that is not absolute.
type InvalidProjectError struct {
	Project string
}

// Error implements error.
func (e *InvalidProjectError) Error() string {
	return fmt.Sprintf("invalid project root %q: must be absolute", e.Project)
}
