// Package store owns the machine-global verger store and its trash.
package store

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/odiumuniverse/verger/pkg/fsutil"
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

// SynthPath returns <root>/synth/<pkg>/<version> without creating it. A
// slashed package id nests directories.
func (s *Store) SynthPath(pkg, version string) (string, error) {
	if err := validatePackageID(pkg); err != nil {
		return "", err
	}

	if !ValidElement(version) {
		return "", &InvalidIDError{Value: version, Reason: "invalid version element"}
	}

	return filepath.Join(s.root, dirSynth, filepath.FromSlash(pkg), version), nil
}

// EnsureSynthPath creates and returns <root>/synth/<pkg>/<version> with 0700.
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
// slashed package id nests directories.
func (s *Store) PackageDataPath(pkg, host string) (string, error) {
	if err := validatePackageID(pkg); err != nil {
		return "", err
	}

	if !ValidElement(host) {
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
	if !ValidElement(host) {
		return "", &InvalidIDError{Value: host, Reason: "invalid host element"}
	}

	if !ValidElement(version) {
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

// ValidElement reports whether s is safe as one path element (host, version,
// trash id): non-empty, not "."/"..", and only letters, digits, `._-+`.
func ValidElement(s string) bool {
	return validSegment(s, "")
}

// validSegment reports whether seg is a safe path element; extra lists
// additional single-byte characters the segment may contain.
func validSegment(seg, extra string) bool {
	if seg == "" || seg == "." || seg == ".." {
		return false
	}

	for i := range len(seg) {
		c := seg[i]

		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '.', c == '_', c == '-', c == '+':
		case extra != "" && strings.IndexByte(extra, c) >= 0:
		default:
			return false
		}
	}

	return true
}

// validatePackageID checks a package id for path safety: segments of
// letters/digits/`._-+:@` separated by single slashes, no absolute path.
func validatePackageID(pkg string) error {
	switch {
	case pkg == "":
		return &InvalidIDError{Value: pkg, Reason: "empty package id"}
	case strings.IndexByte(pkg, 0) >= 0:
		return &InvalidIDError{Value: pkg, Reason: "NUL byte"}
	case strings.Contains(pkg, `\`):
		return &InvalidIDError{Value: pkg, Reason: "backslash"}
	case filepath.IsAbs(pkg), strings.HasPrefix(pkg, "/"):
		return &InvalidIDError{Value: pkg, Reason: "absolute path"}
	case strings.HasSuffix(pkg, "/"), strings.Contains(pkg, "//"):
		return &InvalidIDError{Value: pkg, Reason: "empty path segment"}
	}

	for seg := range strings.SplitSeq(pkg, "/") {
		if !validSegment(seg, ":@") {
			return &InvalidIDError{Value: pkg, Reason: "invalid path segment " + seg}
		}
	}

	return nil
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
