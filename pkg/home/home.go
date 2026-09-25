// Package home resolves the verger home directory and owns its state-dir
// locking: discovery per DESIGN §3.2, the state layout, the shared flock and
// the watcher lease (§3.3), and the cross-filesystem home move.
package home

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/odiumuniverse/verger/pkg/fsutil"
)

// Environment variables and directory names of the discovery contract.
const (
	EnvHome       = "VERGER_HOME"
	EnvBeadleHome = "BEADLE_HOME"

	DirName       = ".verger"
	BeadleDirName = ".beadle"
	BeadleSubdir  = "verger"
)

// Source names which discovery rule produced a home.
type Source string

// Discovery sources, in precedence order.
const (
	SourceEnv         Source = "env"          // $VERGER_HOME
	SourceBeadleVault Source = "beadle-vault" // $BEADLE_HOME/verger
	SourceDefault     Source = "default"      // ~/.verger
)

// Home is one resolved verger home directory. It is immutable after
// construction; all methods are safe for concurrent use.
type Home struct {
	root   string
	source Source
}

// config carries the injected discovery inputs.
type config struct {
	lookup      func(string) string
	userHome    string
	userHomeSet bool
}

// Option configures discovery.
type Option func(*config)

// WithEnv replaces environment lookup; nil restores os.Getenv.
func WithEnv(lookup func(string) string) Option {
	return func(c *config) {
		c.lookup = lookup
	}
}

// WithUserHome replaces os.UserHomeDir; an empty value reports NoHomeError.
func WithUserHome(dir string) Option {
	return func(c *config) {
		c.userHome = dir
		c.userHomeSet = true
	}
}

// Discover resolves the home directory: $VERGER_HOME wins unconditionally;
// otherwise an existing <BEADLE_HOME>/verger (default ~/.beadle/verger) wins;
// otherwise ~/.verger. Discovery is read-only.
func Discover(opts ...Option) (*Home, error) {
	cfg := config{lookup: os.Getenv}

	for _, opt := range opts {
		opt(&cfg)
	}

	if cfg.lookup == nil {
		cfg.lookup = os.Getenv
	}

	if value := strings.TrimSpace(cfg.lookup(EnvHome)); value != "" {
		root, err := resolvePath(value)
		if err != nil {
			return nil, err
		}

		return &Home{root: root, source: SourceEnv}, nil
	}

	// An explicit BEADLE_HOME vault does not require a user home: probe it
	// before resolving the user home (rule 1b).
	explicitBeadle := strings.TrimSpace(cfg.lookup(EnvBeadleHome))

	if explicitBeadle != "" {
		beadle, err := resolvePath(explicitBeadle)
		if err != nil {
			return nil, err
		}

		if vault := vaultAt(beadle); vault != "" {
			return &Home{root: vault, source: SourceBeadleVault}, nil
		}
	}

	userHome, err := cfg.resolveUserHome()
	if err != nil {
		return nil, err
	}

	if explicitBeadle == "" {
		if vault := vaultAt(filepath.Join(userHome, BeadleDirName)); vault != "" {
			return &Home{root: vault, source: SourceBeadleVault}, nil
		}
	}

	return &Home{root: filepath.Join(userHome, DirName), source: SourceDefault}, nil
}

// vaultAt returns <beadle>/verger when it is an existing directory, "" when it
// is missing or not a directory.
func vaultAt(beadle string) string {
	candidate := filepath.Join(beadle, BeadleSubdir)
	if info, err := os.Stat(candidate); err == nil && info.IsDir() {
		return candidate
	}

	return ""
}

// resolveUserHome returns the injected user home or os.UserHomeDir.
func (c config) resolveUserHome() (string, error) {
	if c.userHomeSet {
		if strings.TrimSpace(c.userHome) == "" {
			return "", &NoHomeError{Cause: errors.New("empty user home override")}
		}

		return resolvePath(c.userHome)
	}

	dir, err := os.UserHomeDir()
	if err != nil {
		return "", &NoHomeError{Cause: err}
	}

	return dir, nil
}

// New returns an explicitly rooted home. The root must be absolute after tilde
// expansion; its source is SourceEnv.
func New(root string) (*Home, error) {
	if strings.TrimSpace(root) == "" {
		return nil, &InvalidHomeError{Path: root, Reason: "root is empty"}
	}

	expanded, err := fsutil.ExpandHome(root)
	if err != nil {
		return nil, &InvalidHomeError{Path: root, Reason: err.Error()}
	}

	if !filepath.IsAbs(expanded) {
		return nil, &InvalidHomeError{Path: root, Reason: "root must be absolute"}
	}

	return &Home{root: filepath.Clean(expanded), source: SourceEnv}, nil
}

// resolvePath expands a tilde and makes the path absolute.
func resolvePath(value string) (string, error) {
	expanded, err := fsutil.ExpandHome(value)
	if err != nil {
		return "", fmt.Errorf("resolve path %q: %w", value, err)
	}

	abs, err := filepath.Abs(expanded)
	if err != nil {
		return "", fmt.Errorf("resolve path %q: %w", value, err)
	}

	return abs, nil
}

// Root returns the absolute home root.
func (h *Home) Root() string {
	return h.root
}

// Source returns which discovery rule produced the home.
func (h *Home) Source() Source {
	return h.source
}

// Exists reports whether the home root exists as a directory.
func (h *Home) Exists() bool {
	info, err := os.Stat(h.root)

	return err == nil && info.IsDir()
}

// SpecPath returns <root>/verger.toml.
func (h *Home) SpecPath() string {
	return filepath.Join(h.root, "verger.toml")
}

// LockPath returns <root>/verger.lock.
func (h *Home) LockPath() string {
	return filepath.Join(h.root, "verger.lock")
}

// StateDir returns <root>/state.
func (h *Home) StateDir() string {
	return filepath.Join(h.root, "state")
}

// JournalPath returns <root>/state/journal.jsonl.
func (h *Home) JournalPath() string {
	return filepath.Join(h.StateDir(), "journal.jsonl")
}

// TombstonesPath returns <root>/state/tombstones.json.
func (h *Home) TombstonesPath() string {
	return filepath.Join(h.StateDir(), "tombstones.json")
}

// ConsentPath returns <root>/state/consent.json.
func (h *Home) ConsentPath() string {
	return filepath.Join(h.StateDir(), "consent.json")
}

// SecretsPath returns <root>/state/secrets.json.
func (h *Home) SecretsPath() string {
	return filepath.Join(h.StateDir(), "secrets.json")
}

// TrustPath returns <root>/state/trust.json.
func (h *Home) TrustPath() string {
	return filepath.Join(h.StateDir(), "trust.json")
}

// ReceiptsDir returns <root>/state/receipts.
func (h *Home) ReceiptsDir() string {
	return filepath.Join(h.StateDir(), "receipts")
}

// LeasePath returns <root>/state/watch.lease.
func (h *Home) LeasePath() string {
	return filepath.Join(h.StateDir(), "watch.lease")
}

// FileLockPath returns <root>/state/.lock.
func (h *Home) FileLockPath() string {
	return filepath.Join(h.StateDir(), ".lock")
}

// Ensure creates the root and state dirs with 0700, idempotently. The leaf
// dirs belong to verger, so their mode is enforced on every call.
func (h *Home) Ensure() error {
	if err := ensureDir(h.root); err != nil {
		return err
	}

	return ensureDir(h.StateDir())
}

// ensureDir creates path with 0700 and enforces the leaf mode.
func ensureDir(path string) error {
	if err := fsutil.EnsureDir(path, 0o700); err != nil {
		return err
	}

	//nolint:gosec // G302: a directory needs the execute bit; 0700 is owner-only
	if err := os.Chmod(path, 0o700); err != nil {
		return fmt.Errorf("restrict %s: %w", path, err)
	}

	return nil
}

// NoHomeError reports that the user home directory cannot be resolved.
type NoHomeError struct {
	Cause error
}

// Error implements error.
func (e *NoHomeError) Error() string {
	return "cannot resolve the user home: " + e.Cause.Error()
}

// Unwrap returns the underlying cause.
func (e *NoHomeError) Unwrap() error {
	return e.Cause
}

// InvalidHomeError reports an explicit home root New cannot accept.
type InvalidHomeError struct {
	Path   string
	Reason string
}

// Error implements error.
func (e *InvalidHomeError) Error() string {
	return fmt.Sprintf("invalid home %q: %s", e.Path, e.Reason)
}
