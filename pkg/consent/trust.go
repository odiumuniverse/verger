package consent

import (
	"cmp"
	"encoding/json"
	"path/filepath"
	"slices"
	"time"

	"github.com/odiumuniverse/verger/pkg/digest"
	"github.com/odiumuniverse/verger/pkg/spec"
)

// TrustRecord is one trusted project spec digest.
type TrustRecord struct {
	Project   string      `json:"project"` // canonical absolute project root
	SpecHash  digest.Hash `json:"spec_hash"`
	TrustedAt time.Time   `json:"trusted_at"`

	extra map[string]json.RawMessage
}

// trustRecordWire is the wire shape of TrustRecord without its marshal methods.
type trustRecordWire TrustRecord

// MarshalJSON encodes the record with preserved unknown fields.
func (r TrustRecord) MarshalJSON() ([]byte, error) {
	return marshalWithExtras(trustRecordWire(r), r.extra)
}

// UnmarshalJSON decodes the record and stashes unknown fields for round-trip.
func (r *TrustRecord) UnmarshalJSON(data []byte) error {
	extras, err := unmarshalWithExtras(data, (*trustRecordWire)(r))
	if err != nil {
		return err
	}

	r.extra = extras

	return nil
}

// trustDoc is the on-disk trust document.
type trustDoc struct {
	Schema   int                    `json:"schema"`
	Projects map[string]TrustRecord `json:"projects"`

	extra map[string]json.RawMessage
}

// trustDocWire is the wire shape of trustDoc without its marshal methods.
type trustDocWire trustDoc

// MarshalJSON encodes the document with preserved unknown fields.
func (d trustDoc) MarshalJSON() ([]byte, error) {
	return marshalWithExtras(trustDocWire(d), d.extra)
}

// UnmarshalJSON decodes the document and stashes unknown fields for round-trip.
func (d *trustDoc) UnmarshalJSON(data []byte) error {
	extras, err := unmarshalWithExtras(data, (*trustDocWire)(d))
	if err != nil {
		return err
	}

	d.extra = extras

	return nil
}

// TrustStore keeps project trust decisions in memory and reads/writes
// <home>/state/trust.json. It is not goroutine-safe; callers hold the home
// flock when writing.
type TrustStore struct {
	core stateStore[trustDoc, string, TrustRecord]
}

// NewTrustStore returns an empty trust store for path; the file is read on
// first use. Options are shared with NewStore; only WithClock affects a trust
// store.
func NewTrustStore(path string, opts ...Option) *TrustStore {
	carrier := &Store{}

	for _, opt := range opts {
		opt(carrier)
	}

	now := carrier.core.now
	if now == nil {
		now = time.Now
	}

	return &TrustStore{core: stateStore[trustDoc, string, TrustRecord]{
		path:      path,
		records:   map[string]TrustRecord{},
		now:       now,
		schema:    func(doc trustDoc) int { return doc.Schema },
		recordsOf: func(doc trustDoc) map[string]TrustRecord { return doc.Projects },
		keyOf:     func(rec TrustRecord) string { return rec.Project },
		extrasOf:  func(doc trustDoc) map[string]json.RawMessage { return doc.extra },
	}}
}

// Load reads the trust file; a missing file yields an empty store. Corrupt
// documents and unsupported schemas are typed errors and never overwrite the
// file.
func (t *TrustStore) Load() error {
	return t.core.load()
}

// Save writes the trust file atomically with mode 0600, creating the parent
// state directory 0700. A file that cannot be decoded is never overwritten.
func (t *TrustStore) Save() error {
	return t.core.save(trustDoc{Schema: Schema, Projects: t.core.records, extra: t.core.extras})
}

// Trust upserts trust for project at specHash. It mutates memory only; call
// Save to persist.
func (t *TrustStore) Trust(project string, specHash digest.Hash) error {
	key, err := canonicalProject(project)
	if err != nil {
		return err
	}

	if err := t.core.ensureLoaded(); err != nil {
		return err
	}

	t.core.records[key] = TrustRecord{Project: key, SpecHash: specHash, TrustedAt: t.core.now().UTC()}

	return nil
}

// Untrust deletes trust for project; untrusting a missing project is not an
// error.
func (t *TrustStore) Untrust(project string) error {
	key, err := canonicalProject(project)
	if err != nil {
		return err
	}

	if err := t.core.ensureLoaded(); err != nil {
		return err
	}

	delete(t.core.records, key)

	return nil
}

// Trusted reports whether project is trusted for exactly specHash (D11: a
// changed spec is untrusted and re-asked).
func (t *TrustStore) Trusted(project string, specHash digest.Hash) (bool, error) {
	key, err := canonicalProject(project)
	if err != nil {
		return false, err
	}

	if err := t.core.ensureLoaded(); err != nil {
		return false, err
	}

	rec, ok := t.core.records[key]

	return ok && rec.SpecHash == specHash, nil
}

// Records returns every trust record sorted by project.
func (t *TrustStore) Records() ([]TrustRecord, error) {
	if err := t.core.ensureLoaded(); err != nil {
		return nil, err
	}

	out := make([]TrustRecord, 0, len(t.core.records))

	for _, rec := range t.core.records {
		out = append(out, rec)
	}

	slices.SortFunc(out, func(a, b TrustRecord) int {
		return cmp.Compare(a.Project, b.Project)
	})

	return out, nil
}

// SpecHash is the trust input: the canonical spec digest (spec.Digest); a nil
// spec hashes to the zero value.
func SpecHash(s *spec.Spec) digest.Hash {
	if s == nil {
		return ""
	}

	return s.Digest()
}

// canonicalProject validates and normalizes a project root: it must be
// absolute; when the path exists, symlinks are resolved so a repo reached
// through a link and its real path share one record; the result is lexically
// cleaned.
func canonicalProject(project string) (string, error) {
	if !filepath.IsAbs(project) {
		return "", &InvalidProjectError{Project: project}
	}

	resolved, err := filepath.EvalSymlinks(project)
	if err == nil {
		return filepath.Clean(resolved), nil
	}

	return filepath.Clean(project), nil
}
