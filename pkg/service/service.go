// Package service renders and manages `verger watch` as a login service: a
// launchd agent on macOS and a systemd --user unit on Linux. One implementation
// serves both, and the OS calls sit behind Runner so a test never touches the
// real system and an install never needs a root shell.
//
// It is modelled on beadle pkg/daemon, which already does the same for beadle;
// the shape is close enough that beadle can later adopt this package as its own
// rather than keep two copies.
package service

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

// DefaultLabel is the service label used when a Spec names none.
const DefaultLabel = "dev.odiumuniverse.verger.watch"

// DefaultArgs is what the unit runs: the watch verb and nothing else.
func DefaultArgs() []string { return []string{"watch"} }

// Spec is everything a rendered unit needs. The caller supplies the binary
// path and arguments because only the caller knows how it was invoked.
type Spec struct {
	// Label is the launchd label / systemd unit name. Empty means
	// DefaultLabel.
	Label string

	// Binary is the absolute path to the verger executable the unit runs. A
	// status check reports a unit whose binary has moved, because a unit that
	// points at a vanished path is installed and broken at the same time.
	Binary string

	// Args are the arguments after the binary. Empty means DefaultArgs.
	Args []string

	// Home is the user's home: the unit is written under it, and it is the
	// HOME the unit pins so the service resolves the same store the CLI does.
	Home string

	// LogPath and ErrLogPath are where the platform manager sends the output.
	// They live under the store so nothing is written outside it.
	LogPath    string
	ErrLogPath string

	// Env is the explicit environment the unit carries. A platform manager
	// starts a unit with its own environment, so without this the watcher
	// would resolve a different home than the CLI did.
	Env map[string]string

	// FileLimit is the launchd soft limit on open files. Zero means 8192.
	FileLimit int
}

// LabelOrDefault is the label a spec renders under, filling in the default.
func (s Spec) LabelOrDefault() string {
	if s.Label == "" {
		return DefaultLabel
	}

	return s.Label
}

// ArgsOrDefault is the spec's arguments, filling in `watch`.
func (s Spec) ArgsOrDefault() []string {
	if len(s.Args) == 0 {
		return DefaultArgs()
	}

	return s.Args
}

// Runner runs a platform command. Every OS call this package makes goes through
// one, so tests inject a recorder and never shell out.
type Runner interface {
	Run(ctx context.Context, name string, args ...string) (stdout []byte, err error)
}

// UnitEnv builds the environment a unit must pin from the invoking one. HOME
// and the verger root are always explicit; VERGER_HOME travels only when it
// differs from the default, and PATH travels when set.
func UnitEnv(home, vergerRoot string) map[string]string {
	env := map[string]string{"HOME": home}

	if vergerRoot != "" && vergerRoot != filepath.Join(home, ".verger") {
		env["VERGER_HOME"] = vergerRoot
	}

	if value := os.Getenv("PATH"); value != "" {
		env["PATH"] = value
	}

	return env
}

// TemporaryHome reports whether a path lives under a temporary root. A unit
// installed for such a home would outlive the tree it was built in, so an
// install refuses it rather than pinning an environment that will vanish.
func TemporaryHome(path string) bool {
	if path == "" {
		return false
	}

	abs, err := filepath.Abs(path)
	if err != nil {
		return false
	}

	// A symlink is not a way around the guard. macOS hands out /var/folders
	// and /tmp as links to /private/var/folders and /private/tmp, so a home
	// reached through one spelling and checked against the other would pass
	// a test the reader would call a bug. Both spellings are checked
	// below; resolving first is what makes the caller-side spelling moot.
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		abs = resolved
	}

	for _, root := range temporaryRoots() {
		root = filepath.Clean(root)
		if abs == root {
			return true
		}

		// Rel rather than a prefix test: os.TempDir() may carry a trailing
		// separator, and a naive prefix would then miss the one case the
		// guard exists for.
		rel, err := filepath.Rel(root, abs)
		if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return true
		}
	}

	return false
}

// ErrTemporaryHome is returned by Install when the home is a temporary tree.
//
// The name does not follow the `XxxError` convention on purpose: it is part of
// this package's public API and is referenced by name from pkg/cli and
// pkg/exitcode, which are outside this package and outside this change.
// Renaming it here would break those callers, so the convention is suppressed
// rather than the API broken.
//
//nolint:errname // public API shared with pkg/cli and pkg/exitcode; renaming breaks callers
type ErrTemporaryHome struct{ Home string }

// Error implements error.
func (e *ErrTemporaryHome) Error() string {
	return "service: refusing to install a login service for a temporary home (" + e.Home +
		"): it would outlive the tree it was built in; use a real home directory"
}

// UnsupportedPlatformError names a platform with no unit format.
type UnsupportedPlatformError struct{ GOOS string }

// Error implements error.
func (e *UnsupportedPlatformError) Error() string {
	return "service: unsupported platform " + e.GOOS + ": verger's watch service is launchd on macOS and systemd --user on Linux"
}

// NoUserSystemdError is returned when a Linux install cannot find a user bus —
// a container, a WSL-like environment, or a shell with no dbus session. It is
// always an error and never a reason to fall back to a root unit: a login
// service that needs root is not the thing the user asked for.
type NoUserSystemdError struct{ Detail string }

// Error implements error, and carries the hint in the message because the
// remedy is a command, not a field a caller is likely to print.
func (e *NoUserSystemdError) Error() string {
	detail := e.Detail
	if detail == "" {
		detail = "no user systemd session"
	}

	return "service: " + detail + ": a --user unit needs a user bus. Run this from a login " +
		"session on the host (not ssh without a session, not a container), or set " +
		"XDG_RUNTIME_DIR to your runtime directory. verger will not install a root unit instead."
}

// IsNoUserSystemd reports whether err is a missing-user-bus error.
func IsNoUserSystemd(err error) bool {
	var target *NoUserSystemdError

	return errors.As(err, &target)
}

// Render returns the unit file path and content for the current platform.
func Render(spec Spec) (string, string, error) {
	switch goos {
	case goosDarwin:
		return RenderLaunchd(spec)
	case goosLinux:
		return RenderSystemd(spec)
	default:
		return "", "", &UnsupportedPlatformError{GOOS: goos}
	}
}

// UnitPath returns the unit file path Render would write for this home and
// label, without rendering anything.
func UnitPath(home, label string) (string, error) {
	spec := Spec{Label: label, Home: home}

	switch goos {
	case goosDarwin:
		return filepath.Join(home, "Library", "LaunchAgents", spec.LabelOrDefault()+".plist"), nil
	case goosLinux:
		return filepath.Join(home, ".config", "systemd", "user", unitName(spec.LabelOrDefault())), nil
	default:
		return "", &UnsupportedPlatformError{GOOS: goos}
	}
}

// Install writes the unit file and registers it with the platform manager. It
// is idempotent: installing over an identical unit is a no-op, and installing
// over a changed one re-registers so the running service matches the file.
func Install(ctx context.Context, spec Spec, run Runner) (string, error) {
	if TemporaryHome(spec.Home) {
		return "", &ErrTemporaryHome{Home: spec.Home}
	}

	path, content, err := Render(spec)
	if err != nil {
		return "", err
	}

	previous, readErr := os.ReadFile(path) //nolint:gosec // G304: the path is derived from the home
	changed := readErr != nil || string(previous) != content

	if changed {
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			return "", fmt.Errorf("service: create the unit directory: %w", err)
		}

		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			return "", fmt.Errorf("service: write the unit: %w", err)
		}
	}

	if err := register(ctx, spec, run); err != nil {
		// A unit file the platform manager never accepted is a lie about
		// this machine: `service status` would find it and the next install
		// would call it "unchanged" instead of re-registering. Put the
		// machine back the way it was — the previous content, or no file at
		// all if there was none.
		if rollbackErr := restoreUnit(path, previous, readErr == nil); rollbackErr != nil {
			return "", errors.Join(err, rollbackErr)
		}

		return "", err
	}

	return path, nil
}

// restoreUnit puts the unit file back the way it was before a failed
// install. hadPrevious says whether there was a file to restore; when there
// was not, the failed write is removed rather than left as a half-installed
// service nobody asked for.
func restoreUnit(path string, previous []byte, hadPrevious bool) error {
	if !hadPrevious {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("service: roll back the unit: %w", err)
		}

		return nil
	}

	// G703 is a false positive: `path` is the unit path this package built
	// under the user's own home, and this writes back the bytes it read from
	// that same file moments earlier. There is no untrusted input.
	//nolint:gosec // path is the package's own unit file under the user's home
	if err := os.WriteFile(path, previous, 0o600); err != nil {
		return fmt.Errorf("service: roll back the unit: %w", err)
	}

	return nil
}

// Uninstall stops the service and removes its unit file. It is idempotent: a
// unit that is already gone, or already stopped, is not an error.
func Uninstall(ctx context.Context, spec Spec, run Runner) (string, error) {
	path, err := UnitPath(spec.Home, spec.LabelOrDefault())
	if err != nil {
		return "", err
	}

	if err := unregister(ctx, spec, run); err != nil {
		return "", err
	}

	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return "", fmt.Errorf("service: remove the unit: %w", err)
	}

	return path, nil
}

// State is what Status found, as distinct from a yes/no answer: a unit that
// exists and is stopped is a different situation from one that was never
// installed, and from one whose binary has moved out from under it.
type State string

// The four states a unit can be in.
const (
	// StateNotInstalled: no unit file and no registered service.
	StateNotInstalled State = "not-installed"
	// StateStopped: the unit file exists but the manager is not running it.
	StateStopped State = "installed-stopped"
	// StateRunning: the manager has it loaded and running.
	StateRunning State = "running"
	// StateBinaryMoved: the unit file exists and names a binary that is no
	// longer there, so the service cannot start until it is reinstalled.
	StateBinaryMoved State = "installed-but-binary-moved"
)

// Status is the result of a read-only check.
type Status struct {
	// State is the four-way answer above.
	State State

	// Path is where the unit file would live, whether or not it exists.
	Path string

	// Installed is true when the unit file is on disk.
	Installed bool

	// Loaded is true when the platform manager reports the service loaded.
	Loaded bool

	// Binary is the executable the installed unit names, empty when no unit
	// was found. It is read out of the unit rather than taken from a Spec,
	// because the question "did the binary move" is about the file on disk.
	Binary string
}

// Check inspects the service without changing anything: whether the unit file
// exists, whether the platform manager has it loaded, and — when a unit is
// found — whether the binary it names is still there.
func Check(ctx context.Context, home, label string, run Runner) (Status, error) {
	path, err := UnitPath(home, label)
	if err != nil {
		return Status{}, err
	}

	out := Status{Path: path, State: StateNotInstalled}

	content, err := os.ReadFile(path) //nolint:gosec // G304: the path is derived from the home

	// A missing unit file is not an error: the answer is StateNotInstalled.
	// It gets its own function so the "is it just absent?" question and the
	// "read the unit" question do not nest three deep.
	if errors.Is(err, fs.ErrNotExist) {
		return absentUnit(ctx, out, label, run)
	}

	if err != nil {
		return out, fmt.Errorf("service: read the unit: %w", err)
	}

	out.Installed = true
	out.State = StateStopped
	out.Binary = binaryFrom(string(content))

	active, aerr := active(ctx, label, run)
	if aerr != nil {
		return out, aerr
	}

	out.Loaded = active

	switch {
	case out.Binary == "":
		// A unit with no readable program argument is installed and stopped
		// rather than moved; there is nothing to compare against.
		out.State = StateStopped
	case !exists(out.Binary):
		// The unit is on disk and the manager may well have it loaded, but it
		// names a program that is not there, so the running-service answer
		// would be misleading. This is the state that needs a reinstall.
		out.State = StateBinaryMoved
	case active:
		out.State = StateRunning
	default:
		out.State = StateStopped
	}

	return out, nil
}

// absentUnit answers Check when there is no unit file at all. The platform
// manager may still know the label from a file that has since been deleted,
// so it is asked once; a not-installed answer does not depend on the reply.
func absentUnit(ctx context.Context, out Status, label string, run Runner) (Status, error) {
	loaded, err := loaded(ctx, label, run)
	if err != nil {
		return out, err
	}

	if loaded {
		out.Loaded = true
	}

	return out, nil
}

// exists reports whether a path is a runnable file.
func exists(path string) bool {
	// G703 is a false positive: the paths reaching here are the launchd and
	// systemd unit paths under the user's own home, and the only operation is
	// a Stat that decides whether a unit is present.
	//nolint:gosec // the unit path is derived from the user's own home; only stat'ed
	info, err := os.Stat(path)

	return err == nil && !info.IsDir()
}

// EnvPairs renders an environment map as sorted key/value pairs, so a rendered
// unit is byte-stable and a golden test can compare it.
func EnvPairs(env map[string]string) [][2]string {
	keys := make([]string, 0, len(env))
	for key := range env {
		keys = append(keys, key)
	}

	slices.Sort(keys)

	pairs := make([][2]string, 0, len(keys))
	for _, key := range keys {
		pairs = append(pairs, [2]string{key, env[key]})
	}

	return pairs
}

// limit renders an int for a plist, and is the one place the default file
// limit is decided.
func fileLimit(spec Spec) string {
	limit := spec.FileLimit
	if limit <= 0 {
		limit = 8192
	}

	return strconv.Itoa(limit)
}

// WatchSpec is the Spec for `verger watch` as a login service, with every path
// a caller would otherwise have to assemble: the binary, the watch verb, the
// unit env pinned from the invoking environment, and both log paths under the
// store so nothing is written outside the tree the user can see and eject.
//
// A caller that wants a different label or a different binary edits the result
// rather than rebuilding one field at a time.
func WatchSpec(binary, home, vergerRoot string) Spec {
	logPath, errLogPath := storeLogs(home, vergerRoot)

	return Spec{
		Label:      DefaultLabel,
		Binary:     binary,
		Args:       DefaultArgs(),
		Home:       home,
		LogPath:    logPath,
		ErrLogPath: errLogPath,
		Env:        UnitEnv(home, vergerRoot),
	}
}

// temporaryRoots is the list TemporaryHome checks. It is a variable so a test
// can drive the guard without a real home directory: a test cannot create one,
// because any directory it creates is itself temporary.
var temporaryRoots = func() []string {
	return []string{os.TempDir(), "/tmp", "/private/var/folders", "/private/tmp"}
}
