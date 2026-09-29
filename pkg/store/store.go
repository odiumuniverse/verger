// Package store owns the machine-global verger store and its trash.
package store

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/odiumuniverse/verger/pkg/fsutil"
	pkgid "github.com/odiumuniverse/verger/pkg/id"
)

const (
	// DirName is the store directory name under the data home.
	DirName = "verger"
	// DefaultRetention is the default trash retention window.
	DefaultRetention = 30 * 24 * time.Hour
)

const (
	dirData    = "data"
	dirTrash   = "trash"
	dirRuntime = "runtime"
	dirBin     = "bin"
	dirCache   = "cache"
	dirSynth   = "synth"
)

// Store is the machine-global package store root. It is never moved by home
// absorb/eject and never reads VERGER_HOME or BEADLE_HOME.
type Store struct {
	root      string
	now       func() time.Time
	retention time.Duration
	trash     *Trash
}

// Option configures a Store.
type Option func(*Store)

// WithTrashRetention sets the trash retention window; values <= 0 keep
// DefaultRetention.
func WithTrashRetention(d time.Duration) Option {
	return func(s *Store) {
		if d > 0 {
			s.retention = d
		}
	}
}

// TrashRetention reports the window a purge keeps entries for. It is
// readable so a front end can tell the user what their store will do rather
// than leaving them to infer it, and so a test can prove the option it passed
// reached the store instead of stopping at a facade field.
func (s *Store) TrashRetention() time.Duration {
	return s.retention
}

// WithClock replaces the time source; nil keeps time.Now.
func WithClock(now func() time.Time) Option {
	return func(s *Store) {
		if now != nil {
			s.now = now
		}
	}
}

// DefaultRoot resolves the store root: $XDG_DATA_HOME/verger when
// XDG_DATA_HOME is absolute, else ~/.local/share/verger. It creates nothing.
func DefaultRoot() (string, error) {
	if xdg := os.Getenv("XDG_DATA_HOME"); filepath.IsAbs(xdg) {
		return filepath.Join(xdg, DirName), nil
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve user home: %w", err)
	}

	return filepath.Join(home, ".local", "share", DirName), nil
}

// Open resolves a store root without touching the filesystem; an empty root
// uses DefaultRoot. The root accepts a leading tilde and is made absolute.
func Open(root string, opts ...Option) (*Store, error) {
	s := &Store{now: time.Now, retention: DefaultRetention}

	for _, opt := range opts {
		opt(s)
	}

	if s.retention <= 0 {
		s.retention = DefaultRetention
	}

	if root == "" {
		var err error

		root, err = DefaultRoot()
		if err != nil {
			return nil, err
		}
	}

	expanded, err := fsutil.ExpandHome(root)
	if err != nil {
		return nil, fmt.Errorf("store root %q: %w", root, err)
	}

	abs, err := filepath.Abs(expanded)
	if err != nil {
		return nil, fmt.Errorf("store root %q: %w", root, err)
	}

	s.root = abs
	s.trash = &Trash{dir: filepath.Join(abs, dirTrash), now: s.now, retention: s.retention}

	return s, nil
}

// Root returns the absolute store root.
func (s *Store) Root() string {
	return s.root
}

// Ensure creates the store root and every layout directory with mode 0700.
// It is idempotent and never re-chmods an existing directory.
func (s *Store) Ensure() error {
	for _, dir := range []string{s.root, s.DataDir(), s.TrashDir(), s.RuntimeDir(), s.BinDir(), s.CacheDir(), filepath.Join(s.root, dirSynth)} {
		if err := fsutil.EnsureDir(dir, 0o700); err != nil {
			return err
		}
	}

	return nil
}

// DataDir returns <root>/data, the package payload root.
func (s *Store) DataDir() string {
	return filepath.Join(s.root, dirData)
}

// TrashDir returns <root>/trash, the trash bucket root.
func (s *Store) TrashDir() string {
	return filepath.Join(s.root, dirTrash)
}

// RuntimeDir returns <root>/runtime, the runtime adapter root.
func (s *Store) RuntimeDir() string {
	return filepath.Join(s.root, dirRuntime)
}

// BinDir returns <root>/bin, the host CLI hardlink root.
func (s *Store) BinDir() string {
	return filepath.Join(s.root, dirBin)
}

// CacheDir returns <root>/cache, the registry and fetch cache root.
func (s *Store) CacheDir() string {
	return filepath.Join(s.root, dirCache)
}

// SynthPath returns <root>/synth/<owner>/<name>/<version> without creating it
// (decision F3): <root>/synth/<owner> is the owner marketplace root and every
// package of the owner is one <name>/<version> dir below it. The elements come
// from SynthElements.
func (s *Store) SynthPath(pkg, version string) (string, error) {
	owner, name, err := SynthElements(pkg)
	if err != nil {
		return "", err
	}

	if !pkgid.ValidateElement(version) {
		return "", &InvalidIDError{Value: version, Reason: "invalid version element"}
	}

	return filepath.Join(s.root, dirSynth, owner, name, version), nil
}

// SynthElements splits a package id into the owner and name elements of its
// synth dir: the first id segment and the rest of the id. Each is escaped
// reversibly — every byte outside [A-Za-z0-9._-] (the `/` and `//`
// separators, `+`, `:`, `@`) becomes `+HH` — so distinct ids never share a
// dir, while a plain owner/name id keeps its segments verbatim. An id without
// an owner has no synth dir.
func SynthElements(pkg string) (string, string, error) {
	if err := packageIDError(pkg); err != nil {
		return "", "", err
	}

	owner, rest, ok := strings.Cut(pkg, "/")
	if !ok || strings.HasPrefix(rest, "/") {
		return "", "", &InvalidIDError{Value: pkg, Reason: "a synth dir needs an owner/name id"}
	}

	return escapeSynthElement(owner), escapeSynthElement(rest), nil
}

// escapeSynthElement escapes every byte outside [A-Za-z0-9._-] as `+HH`.
func escapeSynthElement(value string) string {
	var out strings.Builder

	for i := range len(value) {
		c := value[i]

		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '.', c == '_', c == '-':
			out.WriteByte(c)
		default:
			fmt.Fprintf(&out, "+%02X", c)
		}
	}

	return out.String()
}

// EnsureSynthPath creates and returns <root>/synth/<owner>/<name>/<version>
// with 0700.
func (s *Store) EnsureSynthPath(pkg, version string) (string, error) {
	path, err := s.SynthPath(pkg, version)
	if err != nil {
		return "", err
	}

	if err := fsutil.EnsureDir(path, 0o700); err != nil {
		return "", err
	}

	return path, nil
}

// PackageDataPath returns <root>/data/<pkg>/<host> without creating it. A
// slashed package id nests directories; the canonical `//subpath` separator
// nests like any other slash.
func (s *Store) PackageDataPath(pkg, host string) (string, error) {
	if err := packageIDError(pkg); err != nil {
		return "", err
	}

	if !pkgid.ValidateElement(host) {
		return "", &InvalidIDError{Value: host, Reason: "invalid host element"}
	}

	return filepath.Join(s.root, dirData, filepath.FromSlash(pkg), host), nil
}

// EnsurePackageData creates and returns <root>/data/<pkg>/<host> with 0700.
func (s *Store) EnsurePackageData(pkg, host string) (string, error) {
	path, err := s.PackageDataPath(pkg, host)
	if err != nil {
		return "", err
	}

	if err := fsutil.EnsureDir(path, 0o700); err != nil {
		return "", err
	}

	return path, nil
}

// RuntimePath returns <root>/runtime/<host>/<version> without creating it.
func (s *Store) RuntimePath(host, version string) (string, error) {
	if !pkgid.ValidateElement(host) {
		return "", &InvalidIDError{Value: host, Reason: "invalid host element"}
	}

	if !pkgid.ValidateElement(version) {
		return "", &InvalidIDError{Value: version, Reason: "invalid version element"}
	}

	return filepath.Join(s.root, dirRuntime, host, version), nil
}

// EnsureRuntimePath creates and returns <root>/runtime/<host>/<version> with
// 0700.
func (s *Store) EnsureRuntimePath(host, version string) (string, error) {
	path, err := s.RuntimePath(host, version)
	if err != nil {
		return "", err
	}

	if err := fsutil.EnsureDir(path, 0o700); err != nil {
		return "", err
	}

	return path, nil
}

// Trash returns the store trash handle.
func (s *Store) Trash() *Trash {
	return s.trash
}

// packageIDError converts a package-id grammar violation into a store
// *InvalidIDError; a nil error means the id is valid. The grammar itself lives
// in pkg/id (T1.11 G.7).
func packageIDError(pkg string) error {
	err := pkgid.ValidatePackage(pkg)
	if err == nil {
		return nil
	}

	reason := err.Error()
	if target, ok := errors.AsType[*pkgid.InvalidIDError](err); ok {
		reason = target.Reason
	}

	return &InvalidIDError{Value: pkg, Reason: reason}
}

// InvalidIDError reports a package id, host, version or trash id that is not
// safe to use as a path element.
type InvalidIDError struct {
	Value  string
	Reason string
}

// Error implements error.
func (e *InvalidIDError) Error() string {
	return fmt.Sprintf("invalid id %q: %s", e.Value, e.Reason)
}

// NotFoundError reports a missing store or trash path.
type NotFoundError struct {
	Path string
}

// Error implements error.
func (e *NotFoundError) Error() string {
	return "not found: " + e.Path
}

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
