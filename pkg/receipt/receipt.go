// Package receipt stores what verger put on disk, the reverse manifest to
// remove it, the append-only journal and removal tombstones.
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
	"strings"
	"time"

	"github.com/odiumuniverse/verger/pkg/digest"
	"github.com/odiumuniverse/verger/pkg/fsutil"
)

// Schema is the receipt, journal event and tombstone schema version.
const Schema = 1

// Scopes a receipt may belong to.
const (
	ScopeUser    = "user"
	ScopeProject = "project"
)

// Cause names why an artifact was removed (DESIGN §5.6).
type Cause string

// Tombstone causes.
const (
	CauseUser       Cause = "user"
	CauseCapability Cause = "capability"
	CauseHostReset  Cause = "host-reset"
)

// ---- receipts ----------------------------------------------------------------

// Store keeps one receipt file per (package, host, scope) cell under dir.
type Store struct {
	dir string
}

// NewStore returns a receipt store rooted at dir; the directory is created on
// the first write.
func NewStore(dir string) *Store {
	return &Store{dir: dir}
}

// Artifact is one thing verger put on disk.
type Artifact struct {
	Kind   string      `json:"kind"` // skill|mcp|agent|command|hook|rule|native-js|native-ts|native-other
	Name   string      `json:"name"`
	Path   string      `json:"path"`
	Digest digest.Hash `json:"digest"`
}

// OpKind names one reverse operation of the RMA manifest.
type OpKind string

// RMA operation kinds.
const (
	OpWriteFile   OpKind = "write-file"   // undo: delete, or restore Backup when Existed
	OpCopyTree    OpKind = "copy-tree"    // undo: delete tree
	OpSymlink     OpKind = "symlink"      // undo: delete
	OpHardlink    OpKind = "hardlink"     // undo: delete
	OpConfigKey   OpKind = "config-key"   // undo: restore prior value or unset
	OpHostInstall OpKind = "host-install" // undo: host CLI remove command
)

// Op is one reverse operation recorded at install time.
type Op struct {
	Kind    OpKind      `json:"kind"`
	Path    string      `json:"path,omitempty"`     // absolute target for file/tree/link ops
	KeyPath string      `json:"key_path,omitempty"` // dotted key for config-key ops
	Digest  digest.Hash `json:"digest,omitempty"`   // hash of what verger wrote
	Mode    uint32      `json:"mode,omitempty"`
	Existed bool        `json:"existed,omitempty"` // target existed before install
	Backup  string      `json:"backup,omitempty"`  // trash bucket or state backup holding prior bytes
	Command []string    `json:"command,omitempty"` // inverse host CLI argv for host-install
}

// Receipt is the recorded state of one package in one host and scope.
type Receipt struct {
	Schema         int        `json:"schema"`
	Package        string     `json:"package"`
	Host           string     `json:"host"`
	Scope          string     `json:"scope"`
	Strategy       string     `json:"strategy"`
	Version        string     `json:"version,omitempty"`
	RuntimeVersion string     `json:"runtime_version,omitempty"`
	CapsHash       string     `json:"caps_hash,omitempty"`
	InstalledAt    time.Time  `json:"installed_at,omitzero"`
	UpdatedAt      time.Time  `json:"updated_at,omitzero"`
	Artifacts      []Artifact `json:"artifacts,omitempty"`
	RMA            []Op       `json:"rma,omitempty"`

	extra map[string]json.RawMessage
}

// receiptWire is the wire shape of Receipt without its marshal methods.
type receiptWire Receipt

// MarshalJSON encodes the receipt with preserved unknown fields.
func (r Receipt) MarshalJSON() ([]byte, error) {
	return marshalWithExtras(receiptWire(r), r.extra)
}

// UnmarshalJSON decodes the receipt and stashes unknown fields for round-trip.
func (r *Receipt) UnmarshalJSON(data []byte) error {
	extras, err := unmarshalWithExtras(data, (*receiptWire)(r))
	if err != nil {
		return err
	}

	r.extra = extras

	return nil
}

// Validate checks the receipt shape and its RMA vocabulary.
func (r Receipt) Validate() error {
	fail := func(format string, args ...any) error {
		return &InvalidReceiptError{
			Package: r.Package,
			Host:    r.Host,
			Scope:   r.Scope,
			Cause:   fmt.Errorf(format, args...),
		}
	}

	if r.Package == "" {
		return fail("package is required")
	}

	if r.Host == "" {
		return fail("host is required")
	}

	if !validScope(r.Scope) {
		return fail("unknown scope %q", r.Scope)
	}

	if r.Strategy == "" {
		return fail("strategy is required")
	}

	paths := make(map[string]struct{}, len(r.Artifacts))

	for i, a := range r.Artifacts {
		if a.Kind == "" || a.Name == "" || a.Path == "" {
			return fail("artifact %d needs kind, name and path", i)
		}

		if !a.Digest.Valid() {
			return fail("artifact %d has an invalid digest", i)
		}

		if _, duplicate := paths[a.Path]; duplicate {
			return fail("duplicate artifact path %q", a.Path)
		}

		paths[a.Path] = struct{}{}
	}

	for i, op := range r.RMA {
		if err := op.validate(); err != nil {
			return fail("rma op %d: %v", i, err)
		}
	}

	return nil
}

// validate checks one reverse operation.
func (o Op) validate() error {
	if !validOpKind(o.Kind) {
		return fmt.Errorf("unknown op kind %q", o.Kind)
	}

	if o.Digest != "" && !o.Digest.Valid() {
		return fmt.Errorf("invalid digest %q", o.Digest)
	}

	switch o.Kind {
	case OpWriteFile, OpCopyTree, OpSymlink, OpHardlink:
		if !filepath.IsAbs(o.Path) {
			return fmt.Errorf("%s path %q is not absolute", o.Kind, o.Path)
		}
	case OpConfigKey:
		if o.Path == "" || o.KeyPath == "" {
			return errors.New("config-key needs path and key_path")
		}
	case OpHostInstall:
		if len(o.Command) == 0 {
			return errors.New("host-install needs a command")
		}
	}

	return nil
}

// validOpKind reports whether kind is one of the six RMA operations.
func validOpKind(kind OpKind) bool {
	switch kind {
	case OpWriteFile, OpCopyTree, OpSymlink, OpHardlink, OpConfigKey, OpHostInstall:
		return true
	default:
		return false
	}
}

// Put validates the receipt, refuses to write into a package directory that
// holds a differently-keyed receipt (case-variant package ids resolve to one
// directory on case-insensitive filesystems) and writes the cell file
// atomically with mode 0600.
func (s *Store) Put(r Receipt) error {
	r.Schema = Schema

	if err := r.Validate(); err != nil {
		return err
	}

	path, err := s.cellPath(r.Package, r.Host, r.Scope)
	if err != nil {
		return err
	}

	if err := checkKeyCollision(path, r.Package, r.Host, r.Scope); err != nil {
		return err
	}

	// Clone before sorting so the caller's backing array is never reordered.
	r.Artifacts = slices.Clone(r.Artifacts)

	slices.SortStableFunc(r.Artifacts, compareArtifacts)

	data, err := encodeIndented(r)
	if err != nil {
		return fmt.Errorf("encode receipt: %w", err)
	}

	if err := fsutil.EnsureDir(filepath.Dir(path), 0o700); err != nil {
		return err
	}

	return fsutil.WriteFileAtomic(path, data, 0o600)
}

// checkKeyCollision refuses to write a receipt that would share its package
// directory with a differently-keyed receipt. The collision domain is the
// case-folded package directory: on case-insensitive filesystems case-variant
// package ids resolve to one directory, so a differently-keyed leaf there would
// make List fail for the whole store. The target leaf keeps its exact-key
// check.
func checkKeyCollision(path, pkg, host, scope string) error {
	if err := checkPackageDirCollision(filepath.Dir(path), pkg, host, scope); err != nil {
		return err
	}

	return checkLeafKeyCollision(path, pkg, host, scope)
}

// checkPackageDirCollision decodes every *.json leaf in dir and refuses the
// write when a stored receipt's package differs exactly from the requested one;
// an undecodable leaf is never overwritten either — its decode error is
// returned.
func checkPackageDirCollision(dir, pkg, host, scope string) error {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}

	if err != nil {
		return fmt.Errorf("read receipts dir %s: %w", dir, err)
	}

	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}

		leafPath := filepath.Join(dir, entry.Name())

		data, err := os.ReadFile(leafPath) //nolint:gosec // G304: the path is walked from the store root
		if err != nil {
			return fmt.Errorf("read receipt %s: %w", leafPath, err)
		}

		existing, err := decodeReceipt(data, leafPath)
		if err != nil {
			return err
		}

		if existing.Package != pkg {
			return &KeyCollisionError{Package: pkg, Host: host, Scope: scope, Existing: existing}
		}
	}

	return nil
}

// checkLeafKeyCollision refuses to overwrite the receipt at the target path
// when its key differs exactly from the requested one; an undecodable leaf is
// left untouched and its decode error is returned.
func checkLeafKeyCollision(path, pkg, host, scope string) error {
	data, err := os.ReadFile(path) //nolint:gosec // G304: the path is derived from validated key components
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}

	if err != nil {
		return fmt.Errorf("read receipt %s: %w", path, err)
	}

	existing, err := decodeReceipt(data, path)
	if err != nil {
		return err
	}

	if existing.Package != pkg || existing.Host != host || existing.Scope != scope {
		return &KeyCollisionError{Package: pkg, Host: host, Scope: scope, Existing: existing}
	}

	return nil
}

// Get returns the cell receipt; a missing cell reports ok=false without an
// error.
func (s *Store) Get(pkg, host, scope string) (Receipt, bool, error) {
	path, err := s.cellPath(pkg, host, scope)
	if err != nil {
		return Receipt{}, false, err
	}

	data, err := os.ReadFile(path) //nolint:gosec // G304: the path is derived from validated key components
	if errors.Is(err, fs.ErrNotExist) {
		return Receipt{}, false, nil
	}

	if err != nil {
		return Receipt{}, false, fmt.Errorf("read receipt %s: %w", path, err)
	}

	r, err := decodeReceipt(data, path)
	if err != nil {
		return Receipt{}, false, err
	}

	if r.Package != pkg || r.Host != host || r.Scope != scope {
		return Receipt{}, false, &CorruptReceiptError{Path: path, Cause: errors.New("receipt does not match its key")}
	}

	return r, true, nil
}

// Delete removes a cell receipt; a missing cell is not an error. Empty
// directories left behind are pruned on a best-effort basis.
func (s *Store) Delete(pkg, host, scope string) error {
	path, err := s.cellPath(pkg, host, scope)
	if err != nil {
		return err
	}

	if err := os.Remove(path); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}

		return fmt.Errorf("remove receipt %s: %w", path, err)
	}

	// Prune now-empty package directories upward, stopping at the first
	// non-empty one.
	pruneEmptyParents(path, s.dir)

	return nil
}

// pruneEmptyParents removes now-empty directories upward from path (exclusive)
// until stop or the first non-empty directory.
func pruneEmptyParents(path, stop string) {
	for dir := filepath.Dir(path); dir != stop; dir = filepath.Dir(dir) {
		if err := os.Remove(dir); err != nil {
			return
		}
	}
}

// List walks the store recursively and returns every readable receipt sorted
// by package, host and scope. A corrupt receipt fails the walk loudly.
func (s *Store) List() ([]Receipt, error) {
	pkgDirs, err := os.ReadDir(s.dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}

	if err != nil {
		return nil, fmt.Errorf("read receipts dir %s: %w", s.dir, err)
	}

	var list []Receipt

	for _, pkgDir := range pkgDirs {
		if !pkgDir.IsDir() {
			continue
		}

		receipts, err := s.listTree(filepath.Join(s.dir, pkgDir.Name()), pkgDir.Name())
		if err != nil {
			return nil, err
		}

		list = append(list, receipts...)
	}

	slices.SortStableFunc(list, compareReceipts)

	return list, nil
}

// listTree reads one package subtree; pkg is the slash-joined package id built
// from the directory names on the way down.
func (s *Store) listTree(path, pkg string) ([]Receipt, error) {
	entries, err := os.ReadDir(path)
	if err != nil {
		return nil, fmt.Errorf("read receipts dir %s: %w", path, err)
	}

	var list []Receipt

	for _, entry := range entries {
		name := entry.Name()

		if entry.IsDir() {
			children, err := s.listTree(filepath.Join(path, name), pkg+"/"+name)
			if err != nil {
				return nil, err
			}

			list = append(list, children...)

			continue
		}

		host, scope, ok := leafKey(name)
		if !ok {
			continue
		}

		r, err := readReceiptFile(filepath.Join(path, name), pkg, host, scope)
		if err != nil {
			return nil, err
		}

		list = append(list, r)
	}

	return list, nil
}

// leafKey parses a receipt file name: <host>-<scope>.json.
func leafKey(name string) (host, scope string, ok bool) {
	if strings.HasPrefix(name, ".") {
		return "", "", false
	}

	for _, candidate := range []string{ScopeUser, ScopeProject} {
		trimmed, found := strings.CutSuffix(name, "-"+candidate+".json")
		if found && trimmed != "" && validElement(trimmed) {
			return trimmed, candidate, true
		}
	}

	return "", "", false
}

// readReceiptFile parses one receipt file and checks it against its key.
func readReceiptFile(path, pkg, host, scope string) (Receipt, error) {
	data, err := os.ReadFile(path) //nolint:gosec // G304: the path is walked from the store root
	if err != nil {
		return Receipt{}, fmt.Errorf("read receipt %s: %w", path, err)
	}

	r, err := decodeReceipt(data, path)
	if err != nil {
		return Receipt{}, err
	}

	if r.Package != pkg || r.Scope != scope || r.Host != host {
		return Receipt{}, &CorruptReceiptError{Path: path, Cause: errors.New("receipt does not match its key")}
	}

	return r, nil
}

// decodeReceipt parses and validates one stored receipt.
func decodeReceipt(data []byte, path string) (Receipt, error) {
	r := Receipt{}
	if err := json.Unmarshal(data, &r); err != nil {
		return Receipt{}, &CorruptReceiptError{Path: path, Cause: err}
	}

	switch {
	case r.Schema > Schema:
		return Receipt{}, &SchemaNewerError{Path: path, Found: r.Schema, Supported: Schema}
	case r.Schema < 1:
		return Receipt{}, &CorruptReceiptError{Path: path, Cause: fmt.Errorf("unsupported schema %d", r.Schema)}
	}

	if err := r.Validate(); err != nil {
		return Receipt{}, &CorruptReceiptError{Path: path, Cause: err}
	}

	return r, nil
}

// cellPath validates the key components and resolves the receipt file path:
// <dir>/<pkg with "/" expanded as directories>/<host>-<scope>.json. The mapping
// is injective: package segments nest (acme/foo and acme__foo never collide)
// and the leaf carries the host and scope.
func (s *Store) cellPath(pkg, host, scope string) (string, error) {
	if err := validatePackage(pkg); err != nil {
		return "", err
	}

	if err := validateKeyElement("host", host); err != nil {
		return "", err
	}

	if !validScope(scope) {
		return "", &InvalidKeyError{Field: "scope", Value: scope}
	}

	return filepath.Join(s.dir, filepath.FromSlash(pkg), host+"-"+scope+".json"), nil
}

// compareArtifacts orders artifacts by kind, name and path.
func compareArtifacts(a, b Artifact) int {
	return cmp.Or(
		cmp.Compare(a.Kind, b.Kind),
		cmp.Compare(a.Name, b.Name),
		cmp.Compare(a.Path, b.Path),
	)
}

// compareReceipts orders receipts by package, host and scope.
func compareReceipts(a, b Receipt) int {
	return cmp.Or(
		cmp.Compare(a.Package, b.Package),
		cmp.Compare(a.Host, b.Host),
		cmp.Compare(a.Scope, b.Scope),
	)
}

// ---- shared helpers ----------------------------------------------------------

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

// validScope reports whether scope is one of the known scopes.
func validScope(scope string) bool {
	return scope == ScopeUser || scope == ScopeProject
}

// validElement reports whether value is safe as one path element.
func validElement(value string) bool {
	if value == "" || value == "." || value == ".." {
		return false
	}

	for i := range len(value) {
		c := value[i]

		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '.', c == '_', c == '-', c == '+':
		default:
			return false
		}
	}

	return true
}

// validateKeyElement rejects unsafe host/scope path elements.
func validateKeyElement(field, value string) error {
	if !validElement(value) {
		return &InvalidKeyError{Field: field, Value: value}
	}

	return nil
}

// fieldPackage names the package key in InvalidKeyError.
const fieldPackage = "package"

// validatePackage checks a package id for path safety; "/" nests, so each
// segment must be safe.
func validatePackage(pkg string) error {
	switch {
	case pkg == "":
		return &InvalidKeyError{Field: fieldPackage, Value: pkg}
	case strings.IndexByte(pkg, 0) >= 0, strings.Contains(pkg, `\`):
		return &InvalidKeyError{Field: fieldPackage, Value: pkg}
	case filepath.IsAbs(pkg), strings.HasPrefix(pkg, "/"):
		return &InvalidKeyError{Field: fieldPackage, Value: pkg}
	case strings.HasSuffix(pkg, "/"), strings.Contains(pkg, "//"):
		return &InvalidKeyError{Field: fieldPackage, Value: pkg}
	}

	for seg := range strings.SplitSeq(pkg, "/") {
		if !validPackageSegment(seg) {
			return &InvalidKeyError{Field: fieldPackage, Value: pkg}
		}
	}

	return nil
}

// validPackageSegment reports whether one package id segment is safe; the
// registry forms additionally allow ":" and "@".
func validPackageSegment(seg string) bool {
	if seg == "" || seg == "." || seg == ".." {
		return false
	}

	for i := range len(seg) {
		c := seg[i]

		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '.', c == '_', c == '-', c == '+', c == ':', c == '@':
		default:
			return false
		}
	}

	return true
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

// InvalidKeyError reports an unsafe package, scope or host key component.
type InvalidKeyError struct {
	Field string
	Value string
}

// Error implements error.
func (e *InvalidKeyError) Error() string {
	return fmt.Sprintf("invalid %s key %q", e.Field, e.Value)
}

// CorruptReceiptError reports an unreadable or inconsistent receipt file.
type CorruptReceiptError struct {
	Path  string
	Cause error
}

// Error implements error.
func (e *CorruptReceiptError) Error() string {
	return fmt.Sprintf("corrupt receipt %s: %v", e.Path, e.Cause)
}

// Unwrap returns the underlying cause.
func (e *CorruptReceiptError) Unwrap() error {
	return e.Cause
}

// KeyCollisionError reports a Put that would overwrite a receipt stored at the
// same path under a different key. Case-variant keys resolve to one file on
// case-insensitive filesystems, so the store refuses the collision instead of
// silently replacing the stored receipt.
type KeyCollisionError struct {
	Package  string
	Host     string
	Scope    string
	Existing Receipt
}

// Error implements error.
func (e *KeyCollisionError) Error() string {
	return fmt.Sprintf(
		"key collision for %s/%s/%s: existing receipt has key %s/%s/%s",
		e.Package, e.Host, e.Scope,
		e.Existing.Package, e.Existing.Host, e.Existing.Scope,
	)
}

// InvalidReceiptError reports a receipt that fails validation before write.
type InvalidReceiptError struct {
	Package string
	Host    string
	Scope   string
	Cause   error
}

// Error implements error.
func (e *InvalidReceiptError) Error() string {
	return fmt.Sprintf("invalid receipt %s/%s/%s: %v", e.Package, e.Host, e.Scope, e.Cause)
}

// Unwrap returns the underlying cause.
func (e *InvalidReceiptError) Unwrap() error {
	return e.Cause
}

// InvalidEventError reports a journal event that fails validation.
type InvalidEventError struct {
	Cause error
}

// Error implements error.
func (e *InvalidEventError) Error() string {
	return "invalid journal event: " + e.Cause.Error()
}

// Unwrap returns the underlying cause.
func (e *InvalidEventError) Unwrap() error {
	return e.Cause
}
