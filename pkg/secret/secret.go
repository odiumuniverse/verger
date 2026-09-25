// Package secret stores named secrets and resolves {secret:NAME} references.
// Values live either in a 0600 state/secrets.json document or in the OS
// keychain (payload v1:<hex>); spec, lock and log surfaces never see a literal
// value.
package secret

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/odiumuniverse/verger/pkg/fsutil"
)

// FileName is the secrets document name under the home state directory.
const FileName = "secrets.json"

// DefaultService is the keychain service name of a standalone verger.
const DefaultService = "verger"

// Resolution modes of ResolveJSON.
const (
	ModeLiteral = "literal" // value written as-is
	ModeEnv     = "env"     // value replaced by {env:NAME}
)

// Storage backends of a Store.
const (
	BackendFile    = "file"
	BackendKeyring = "keyring"
)

const (
	refPrefix = "{secret:"
	envPrefix = "{env:"
	refSuffix = "}"

	fileVersion    = 1
	keyringVersion = 2
	fingerprintN   = 8
)

var namePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

var embeddedEnvRef = regexp.MustCompile(`\{env:[A-Za-z_][A-Za-z0-9_]*\}`)

var compoundKeyHints = []string{
	"api_key",
	"access_key",
	"private_key",
}

var wordKeyHints = []string{
	"apikey",
	"secret",
	"token",
	"password",
	"passwd",
	"credential",
	"authorization",
	"auth",
	"cookie",
	"session",
	"bearer",
	"signature",
}

var keyTailHints = []string{
	"key",
	"keys",
	"value",
	"values",
}

var valueHints = []string{
	"bearer ",
	"basic ",
	"token ",
	"sk-",
	"ghp_",
	"gho_",
	"github_pat_",
	"glpat-",
	"xoxb-",
	"xoxp-",
	"akia",
	"ya29.",
	"-----begin ",
}

// Store holds named secrets of one home. It is not goroutine-safe; callers
// serialize access through the home flock.
type Store struct {
	path       string
	values     map[string]string
	backend    string
	keyring    Keyring
	runner     Runner
	service    string
	keyringErr error
	touched    map[string]struct{}
	deleted    map[string]struct{}
	migrated   bool
	changed    bool
}

// document is the on-disk secrets representation: version 1 with literal
// values, version 2 as a keyring name index with empty placeholders.
type document struct {
	Version int               `json:"version"`
	Backend string            `json:"backend,omitempty"`
	Secrets map[string]string `json:"secrets"`
}

// Option configures a Store at Load time.
type Option func(*Store)

// WithKeyring injects the keychain backend, bypassing the shell keyring.
func WithKeyring(keyring Keyring) Option {
	return func(s *Store) { s.keyring = keyring }
}

// WithRunner injects the process runner of the shell keyring; default is
// ExecRunner.
func WithRunner(r Runner) Option {
	return func(s *Store) { s.runner = r }
}

// WithService sets the keychain service name ("verger" standalone, "beadle"
// when embedded in the vault); default is DefaultService.
func WithService(name string) Option {
	return func(s *Store) { s.service = name }
}

// Load reads the secrets document at path. A missing file yields an empty
// file-backed store, never an error; unreadable, corrupt or unknown-backend
// documents yield *SecretParseError. A keyring-backed document prefetches its
// values through the keychain and records a failure in KeyringErr.
func Load(path string, opts ...Option) (*Store, error) {
	store := &Store{
		path:    path,
		values:  map[string]string{},
		backend: BackendFile,
		runner:  ExecRunner{},
		service: DefaultService,
	}

	for _, opt := range opts {
		opt(store)
	}

	if store.service == "" {
		store.service = DefaultService
	}

	data, err := os.ReadFile(path) //nolint:gosec // G304: path is the caller-provided secrets document
	if errors.Is(err, fs.ErrNotExist) {
		return store, nil
	}

	if err != nil {
		return nil, &SecretParseError{Path: path, Cause: err}
	}

	if len(data) == 0 {
		return store, nil
	}

	doc := document{}
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, &SecretParseError{Path: path, Cause: err}
	}

	switch doc.Backend {
	case "", BackendFile:
	case BackendKeyring:
		store.backend = BackendKeyring
	default:
		return nil, &SecretParseError{Path: path, Cause: fmt.Errorf("unknown backend %q", doc.Backend)}
	}

	if doc.Secrets != nil {
		store.values = doc.Secrets
	}

	store.prefetch()

	return store, nil
}

// prefetch resolves keyring-backed values, recording any keychain failure.
func (s *Store) prefetch() {
	if s.backend != BackendKeyring {
		return
	}

	keyring, err := s.shellKeyring()
	if err != nil {
		s.keyringErr = err
		s.values = map[string]string{}

		return
	}

	values := make(map[string]string, len(s.values))

	for _, name := range slices.Sorted(maps.Keys(s.values)) {
		value, found, err := keyring.Get(name)
		if err != nil {
			s.keyringErr = err
			s.values = map[string]string{}

			return
		}

		if found {
			values[name] = value
		}
	}

	s.values = values
}

// shellKeyring returns the injected keyring or builds the platform one.
func (s *Store) shellKeyring() (Keyring, error) {
	if s.keyring != nil {
		return s.keyring, nil
	}

	runner := s.runner
	if runner == nil {
		runner = ExecRunner{}
	}

	keyring, err := NewShellKeyring(runner, s.service)
	if err != nil {
		return nil, err
	}

	s.keyring = keyring

	return keyring, nil
}

// Backend returns the active backend (BackendFile or BackendKeyring).
func (s *Store) Backend() string {
	return s.backend
}

// KeyringErr returns the keychain failure recorded during Load, nil when the
// keyring was not used or worked.
func (s *Store) KeyringErr() error {
	return s.keyringErr
}

// Probe checks that the keychain is usable: one get/delete round-trip against
// the reserved probe account. A file-backed store needs no keychain and
// returns nil without touching it; a recorded KeyringErr is surfaced first.
func (s *Store) Probe() error {
	if s.keyringErr != nil {
		return s.keyringErr
	}

	if s.backend != BackendKeyring {
		return nil
	}

	keyring, err := s.shellKeyring()
	if err != nil {
		return err
	}

	account := probeAccount(s.service)

	if _, _, err := keyring.Get(account); err != nil {
		return fmt.Errorf("keyring probe: %w", err)
	}

	if _, err := keyring.Delete(account); err != nil {
		return fmt.Errorf("keyring probe: %w", err)
	}

	return nil
}

// SetBackend switches between the file and keyring backends; a keyring switch
// re-uploads every value on the next Save.
func (s *Store) SetBackend(backend string) error {
	switch backend {
	case BackendFile, BackendKeyring:
	default:
		return fmt.Errorf("unknown secrets backend %q", backend)
	}

	if backend == s.backend {
		return nil
	}

	s.backend = backend
	s.migrated = backend == BackendKeyring
	s.changed = true

	return nil
}

// Path returns the secrets document path given to Load.
func (s *Store) Path() string {
	return s.path
}

// Changed reports whether the store has unsaved changes.
func (s *Store) Changed() bool {
	return s.changed
}

// Get returns the value stored under name.
func (s *Store) Get(name string) (string, bool) {
	value, ok := s.values[name]

	return value, ok
}

// Has reports whether name is stored.
func (s *Store) Has(name string) bool {
	_, ok := s.values[name]

	return ok
}

// Set stores value under name; an identical value is a no-op.
func (s *Store) Set(name, value string) {
	if current, ok := s.values[name]; ok && current == value {
		return
	}

	s.values[name] = value
	s.changed = true

	if s.touched == nil {
		s.touched = map[string]struct{}{}
	}

	s.touched[name] = struct{}{}

	delete(s.deleted, name)
}

// Delete removes name, reporting whether it was present.
func (s *Store) Delete(name string) bool {
	if _, ok := s.values[name]; !ok {
		return false
	}

	delete(s.values, name)

	s.changed = true

	if s.deleted == nil {
		s.deleted = map[string]struct{}{}
	}

	s.deleted[name] = struct{}{}

	delete(s.touched, name)

	return true
}

// Names returns every stored name, sorted.
func (s *Store) Names() []string {
	return slices.Sorted(maps.Keys(s.values))
}

// Len returns the number of stored names.
func (s *Store) Len() int {
	return len(s.values)
}

// Save uploads pending values to the keyring when that backend is active, then
// writes the document atomically with 0600. It is a no-op when Changed is
// false; a keychain failure leaves the store dirty and the document untouched.
func (s *Store) Save() error {
	if !s.changed {
		return nil
	}

	if err := s.saveKeyring(); err != nil {
		return err
	}

	data, err := json.MarshalIndent(s.document(), "", "  ")
	if err != nil {
		return fmt.Errorf("encode secrets: %w", err)
	}

	dir := filepath.Dir(s.path)
	if err := fsutil.EnsureDir(dir, 0o700); err != nil {
		return fmt.Errorf("write secrets %s: %w", s.path, err)
	}

	if err := fsutil.WriteFileAtomic(s.path, append(data, '\n'), 0o600); err != nil {
		return fmt.Errorf("write secrets %s: %w", s.path, err)
	}

	s.changed = false
	s.migrated = false
	s.touched = nil
	s.deleted = nil

	return nil
}

// document renders the on-disk shape for the active backend.
func (s *Store) document() document {
	if s.backend != BackendKeyring {
		return document{Version: fileVersion, Secrets: s.values}
	}

	secrets := make(map[string]string, len(s.values))

	for name := range s.values {
		secrets[name] = ""
	}

	return document{Version: keyringVersion, Backend: BackendKeyring, Secrets: secrets}
}

// saveKeyring pushes touched and migrated values, then deletions.
func (s *Store) saveKeyring() error {
	if s.backend != BackendKeyring {
		return nil
	}

	if s.keyringErr != nil {
		return fmt.Errorf("save secrets to the keyring: %w", s.keyringErr)
	}

	keyring, err := s.shellKeyring()
	if err != nil {
		return fmt.Errorf("save secrets to the keyring: %w", err)
	}

	for _, name := range s.upsertNames() {
		if err := keyring.Set(name, s.values[name]); err != nil {
			return fmt.Errorf("save secrets to the keyring: %w", err)
		}
	}

	for _, name := range slices.Sorted(maps.Keys(s.deleted)) {
		if _, err := keyring.Delete(name); err != nil {
			return fmt.Errorf("remove secrets from the keyring: %w", err)
		}
	}

	return nil
}

// upsertNames returns the names to upload: everything after a backend switch,
// otherwise only what was touched.
func (s *Store) upsertNames() []string {
	if s.migrated {
		return slices.Sorted(maps.Keys(s.values))
	}

	return slices.Sorted(maps.Keys(s.touched))
}

// nameFor picks a stable name for a key/value pair: an existing name holding
// the same value is reused, a name collision gets a fingerprint suffix.
func (s *Store) nameFor(key, value string) string {
	base := NormalizeName(key)

	if existing, ok := s.nameOfValue(base, value); ok {
		return existing
	}

	if current, ok := s.values[base]; ok && current != value {
		return base + "_" + Fingerprint(value)
	}

	return base
}

// nameOfValue finds a stored name holding value, preferring the base name.
func (s *Store) nameOfValue(base, value string) (string, bool) {
	var fallback string

	var found bool

	for _, name := range s.Names() {
		if s.values[name] != value {
			continue
		}

		if name == base || strings.HasPrefix(name, base+"_") {
			return name, true
		}

		if !found {
			fallback, found = name, true
		}
	}

	return fallback, found
}

// NormalizeName maps an arbitrary key to the secret name grammar: uppercase,
// non-name runes become underscores, leading digits get an S_ prefix.
func NormalizeName(key string) string {
	name := strings.ToUpper(strings.TrimSpace(key))
	name = strings.Map(func(r rune) rune {
		if (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' {
			return r
		}

		return '_'
	}, name)

	for strings.Contains(name, "__") {
		name = strings.ReplaceAll(name, "__", "_")
	}

	name = strings.Trim(name, "_")

	if name == "" {
		return "SECRET"
	}

	if name[0] >= '0' && name[0] <= '9' {
		return "S_" + name
	}

	return name
}

// Fingerprint returns the 8-character uppercase hex prefix of the value hash,
// stable across runs and safe for the spec/lock surfaces.
func Fingerprint(value string) string {
	sum := sha256.Sum256([]byte(value))

	return strings.ToUpper(hex.EncodeToString(sum[:]))[:fingerprintN]
}

// ValidName reports whether name matches ^[A-Za-z_][A-Za-z0-9_]*$.
func ValidName(name string) bool {
	return namePattern.MatchString(name)
}

// Ref renders the {secret:NAME} reference of name.
func Ref(name string) string {
	return refPrefix + name + refSuffix
}

// EnvRef renders the {env:NAME} reference of name.
func EnvRef(name string) string {
	return envPrefix + name + refSuffix
}

// ParseRef extracts the name of a whole {secret:NAME} value; the name is not
// re-validated (ValidName is the grammar gate).
func ParseRef(value string) (string, bool) {
	name, ok := strings.CutPrefix(value, refPrefix)
	if !ok {
		return "", false
	}

	return strings.CutSuffix(name, refSuffix)
}

// IsRef reports whether value is a whole {secret:NAME} reference.
func IsRef(value string) bool {
	_, ok := ParseRef(value)

	return ok
}

// ParseEnvRef extracts the name of a whole {env:NAME} value.
func ParseEnvRef(value string) (string, bool) {
	name, ok := strings.CutPrefix(value, envPrefix)
	if !ok {
		return "", false
	}

	return strings.CutSuffix(name, refSuffix)
}

// isSecret classifies a key/value pair as a literal secret worth extracting.
func isSecret(key, value string) bool {
	if value == "" || IsRef(value) {
		return false
	}

	if _, ok := ParseEnvRef(value); ok {
		return false
	}

	if embeddedEnvRef.MatchString(value) {
		return false
	}

	lower := strings.ToLower(key)

	if lower == "key" || strings.HasSuffix(lower, "_key") {
		return true
	}

	if matchKeyHint(key) {
		return true
	}

	trimmed := strings.ToLower(strings.TrimSpace(value))

	return slices.ContainsFunc(valueHints, func(hint string) bool { return strings.HasPrefix(trimmed, hint) })
}

// matchKeyHint classifies a key by its token hints (api_key, token, …).
func matchKeyHint(key string) bool {
	tokens := keyTokens(key)
	if len(tokens) == 0 {
		return false
	}

	joined := strings.Join(tokens, "_")

	if slices.ContainsFunc(compoundKeyHints, func(hint string) bool { return strings.Contains(joined, hint) }) {
		return true
	}

	last := tokens[len(tokens)-1]

	if slices.ContainsFunc(wordKeyHints, func(hint string) bool { return hintWord(last, hint) }) {
		return true
	}

	if !slices.Contains(keyTailHints, last) {
		return false
	}

	return slices.ContainsFunc(tokens[:len(tokens)-1], func(token string) bool {
		return slices.ContainsFunc(wordKeyHints, func(hint string) bool { return hintWord(token, hint) })
	})
}

// hintWord matches a hint as a whole token or its plural.
func hintWord(token, hint string) bool {
	return token == hint || token == hint+"s"
}

// keyTokens splits a key on non-alphanumeric runes and camelCase boundaries.
func keyTokens(key string) []string {
	var (
		tokens  []string
		current strings.Builder
	)

	flush := func() {
		if current.Len() > 0 {
			tokens = append(tokens, strings.ToLower(current.String()))
			current.Reset()
		}
	}

	var prev rune

	for _, r := range key {
		switch {
		case isKeyRune(r):
			if isUpperRune(r) && (isLowerRune(prev) || isDigitRune(prev)) {
				flush()
			}

			current.WriteRune(r)
		default:
			flush()
		}

		prev = r
	}

	flush()

	return tokens
}

func isKeyRune(r rune) bool {
	return isLowerRune(r) || isUpperRune(r) || isDigitRune(r)
}

func isLowerRune(r rune) bool {
	return r >= 'a' && r <= 'z'
}

func isUpperRune(r rune) bool {
	return r >= 'A' && r <= 'Z'
}

func isDigitRune(r rune) bool {
	return r >= '0' && r <= '9'
}

// SecretParseError reports an unreadable, corrupt or unknown-backend secrets
// document.
type SecretParseError struct {
	Path  string
	Cause error
}

// Error implements error.
func (e *SecretParseError) Error() string {
	return fmt.Sprintf("parse secrets %s: %v", e.Path, e.Cause)
}

// Unwrap returns the underlying cause.
func (e *SecretParseError) Unwrap() error {
	return e.Cause
}
