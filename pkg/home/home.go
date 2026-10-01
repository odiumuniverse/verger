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

		if home, ok := resolveVaultHome(cfg, beadle); ok {
			return home, nil
		}
	}

	userHome, err := cfg.resolveUserHome()
	if err != nil {
		return nil, err
	}

	if explicitBeadle == "" {
		if home, ok := resolveVaultHome(cfg, filepath.Join(userHome, BeadleDirName)); ok {
			return home, nil
		}
	}

	return &Home{root: filepath.Join(userHome, DirName), source: SourceDefault}, nil
}

// resolveVaultHome resolves the home when a beadle VAULT exists, and reports
// false when there is no vault to speak of. Three clauses, in order, and the
// order is the whole point:
//
//  1. the vault's own home, when it HOLDS one — that is this machine's home;
//  2. otherwise ~/.verger, when it HOLDS one;
//  3. otherwise the vault home — a fresh machine.
//
// Clause 2 is the eject case, and it is why this is not "does the vault home
// exist". `beadle plugins eject` moves the state to ~/.verger, and beadle pS-17
// then REMOVES the vault's home directory instead of leaving it empty. So the
// directory is ABSENT — not empty, absent — and any rule phrased in terms of
// emptiness stops firing and hands the machine back to clause 3, which then names
// a directory that does not exist. Every command afterwards runs against it and
// reports no cells on a machine that is full of them.
//
// "Holds one" means not EMPTY, deliberately, with no marker file. A marker check
// couples discovery to a file layout, so a home whose contents move on reads as
// fresh — and that coupling is what made this rule fragile in the first place.
// The subdirectory is still not required to be a directory verger can use before
// clause 3 will name it: discovery is read-only, so naming a directory that does
// not exist yet is neither a write nor a risk.
func resolveVaultHome(cfg config, beadle string) (*Home, bool) {
	info, err := os.Stat(beadle)
	if err != nil || !info.IsDir() {
		return nil, false
	}

	candidate := filepath.Join(beadle, BeadleSubdir)

	// A FILE where the home goes is neither a home nor a vault to descend into.
	// That is a beadle home somebody else put there, and delivering into it
	// would fail later and less clearly than refusing it now.
	if sub, subErr := os.Stat(candidate); subErr == nil && !sub.IsDir() {
		return nil, false
	}

	if holdsFiles(candidate) {
		return &Home{root: candidate, source: SourceBeadleVault}, true
	}

	// The vault home holds nothing, so the state may be in ~/.verger. A user
	// home that cannot be resolved is not a failure here: clause 3 still names
	// a home, and a machine with a vault and no user home has never had a
	// ~/.verger to find.
	if userHome, homeErr := cfg.resolveUserHome(); homeErr == nil {
		defaultHome := filepath.Join(userHome, DirName)
		if holdsFiles(defaultHome) {
			return &Home{root: defaultHome, source: SourceDefault}, true
		}
	}

	return &Home{root: candidate, source: SourceBeadleVault}, true
}

// holdsFiles reports whether dir holds anything at all.
//
// An absent directory holds nothing, and neither does an empty one: the
// difference is not one this rule acts on, and conflating them is what let a
// removed vault home look like a fresh machine.
func holdsFiles(dir string) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}

	return len(entries) > 0
}

// CompetingHome reports a second home on this machine that discovery did NOT
// choose, so a caller can tell the user about it.
//
// Two real homes is the state this product refuses to create: two locks, two
// receipt trees, and the vault and the CLI stopping on what is installed. The
// vault still wins — that is unchanged — but it wins silently, and a user who
// has ended up here needs to be told which other directory is the one to look
// at.
//
// An emptied directory is not reported: it is not a home, and warning about it
// would be a false alarm about a directory nothing will use.
//
// The caller renders the warning. verger has no doctor of its own — the finding
// is printed by `beadle doctor`, which is the surface a user already runs when
// something looks wrong.
func CompetingHome(opts ...Option) (string, bool) {
	cfg := config{lookup: os.Getenv}

	for _, opt := range opts {
		opt(&cfg)
	}

	if cfg.lookup == nil {
		cfg.lookup = os.Getenv
	}

	// $VERGER_HOME is the user saying where the home is. There is nothing to
	// compete with it.
	if value := strings.TrimSpace(cfg.lookup(EnvHome)); value != "" {
		return "", false
	}

	chosen, err := Discover(opts...)
	if err != nil {
		return "", false
	}

	if chosen.Source() != SourceBeadleVault {
		return "", false
	}

	explicitBeadle := strings.TrimSpace(cfg.lookup(EnvBeadleHome))

	beadle := explicitBeadle

	if beadle == "" {
		userHome, homeErr := cfg.resolveUserHome()
		if homeErr != nil {
			return "", false
		}

		beadle = filepath.Join(userHome, BeadleDirName)
	}

	// beadle is <user home>/.beadle, so its parent IS the user home.
	defaultHome, err := resolvePath(filepath.Join(filepath.Dir(beadle), DirName))
	if err != nil || !holdsFiles(defaultHome) {
		return "", false
	}

	if defaultHome == chosen.Root() {
		return "", false
	}

	return defaultHome, true
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
