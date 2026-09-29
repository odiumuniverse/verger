package secret

import (
	"errors"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"
)

// KeyringUnavailableError reports that this machine has no keyring verger is
// willing to talk to. It is a typed error rather than a string because the
// caller's two jobs differ: the CLI prints the sentence and exits 6, and the
// backend selector falls back to the file store without asking.
type KeyringUnavailableError struct {
	// Reason is the human sentence, already stating what to do instead.
	Reason string
}

// Error implements error.
func (e *KeyringUnavailableError) Error() string {
	if e.Reason == "" {
		return ErrKeyringUnavailable.Error()
	}

	return e.Reason
}

// Is lets errors.Is(err, ErrKeyringUnavailable) match, so the older sentinel
// keeps working for callers that predate the probe.
func (e *KeyringUnavailableError) Is(target error) bool {
	return target == ErrKeyringUnavailable || target == ErrKeyringUnsupported
}

// UnavailableMessage is what a person is told when the probe says no. It
// names the way out, because "no keyring here" on its own is a dead end the
// user cannot act on.
const UnavailableMessage = "no keychain available here — use: verger secret backend file"

// KeyringProbe answers one question without touching the OS secret store:
// can this machine keep a secret in a keyring right now, without a dialog?
//
// The probe exists because the alternative is a modal system prompt. On
// macOS an isolated HOME makes the keychain "not found" and the OS offers
// the user "Reset To Defaults" — an offer to wipe their keychain, triggered
// by a tool deciding where to store a string. Nothing here may invoke the
// real store, and nothing may be interactive: a question with a window is a
// question a CI job will hang on.
type KeyringProbe interface {
	// Available reports whether a keyring can be used, and why not when it
	// cannot. The reason is for logs and notes; the message a user sees is
	// UnavailableMessage.
	Available() (bool, string)
}

// HostKeyringProbe is the production probe. It reads the filesystem and the
// environment and nothing else: no `security`, no dbus call, no daemon
// probe that could spawn a prompt.
type HostKeyringProbe struct {
	// GOOS is the platform to judge; empty means the running one.
	GOOS string
	// UserHome is the account's home as the user database has it. It is the
	// second half of the darwin check, not the whole of it.
	UserHome string
}

// Available implements KeyringProbe.
func (p HostKeyringProbe) Available() (bool, string) {
	goos := p.GOOS
	if goos == "" {
		goos = runtime.GOOS
	}

	switch goos {
	case "darwin":
		return darwinKeyringPresent(p.UserHome)
	case "linux":
		return linuxSecretServicePresent()
	default:
		return false, "no keyring backend for " + goos
	}
}

// darwinKeyringPresent reports whether a keyring can be used without a
// dialog. The two halves are both required, and both are about the same
// question asked twice.
//
// The Security framework resolves a keychain through $HOME, not through the
// user database. So a probe that only looked at the account's real home
// would answer "available" on a machine running with an isolated $HOME, and
// the very next call would go looking under that $HOME, find no keychain,
// and raise the system dialog that offers to reset the user's keychain. The
// probe has to describe the call that will actually happen, so it demands
// that $HOME is the account's home — an isolated one is exactly the case
// where there is no safe keyring to use.
func darwinKeyringPresent(accountHome string) (bool, string) {
	envHome := os.Getenv("HOME")
	if envHome == "" {
		return false, "HOME is not set: the Security framework would have nowhere to look"
	}

	account := accountHome
	if account == "" {
		account = realHome()
	}

	if account == "" {
		return false, "cannot read the account's home directory"
	}

	if !sameDir(envHome, account) {
		return false, "HOME is " + envHome + ", not the account's home " + account +
			": the keychain the Security framework would open is not yours"
	}

	dir := filepath.Join(envHome, "Library", "Keychains")

	entries, err := os.ReadDir(dir)
	if err != nil {
		return false, "no keychain database under " + dir
	}

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}

		ext := filepath.Ext(entry.Name())
		if ext != ".keychain-db" && ext != ".keychain" {
			continue
		}

		info, statErr := entry.Info()
		if statErr != nil || info.Size() == 0 {
			continue
		}

		return true, ""
	}

	return false, "no non-empty keychain database under " + dir
}

// sameDir compares two home paths by the directory they name, so a trailing
// slash or a "." does not read as a different home and get a keyring refused
// on a machine that has one.
func sameDir(a, b string) bool {
	cleanA, errA := filepath.Abs(a)
	cleanB, errB := filepath.Abs(b)

	if errA != nil || errB != nil {
		return a == b
	}

	return filepath.Clean(cleanA) == filepath.Clean(cleanB)
}

// realHome resolves the account's home from the user database rather than
// the environment. os/user reads getpwuid, which an isolated HOME does not
// change — that is the whole reason it is used here.
func realHome() string {
	current, err := user.Current()
	if err != nil || current.HomeDir == "" {
		if home, homeErr := os.UserHomeDir(); homeErr == nil {
			return home
		}

		return ""
	}

	return current.HomeDir
}

// linuxSecretServicePresent reports whether a Secret Service is reachable
// without waking it. A session bus address is required, and the socket has
// to exist: on a headless box the address is often still exported while
// nothing is listening, and connecting anyway is what spawns a prompt agent.
func linuxSecretServicePresent() (bool, string) {
	address := os.Getenv("DBUS_SESSION_BUS_ADDRESS")
	if address == "" {
		return false, "no D-Bus session bus in this environment"
	}

	// Only the unix transport is checked, and only by stat: opening the
	// socket is a connect, and a connect to a bus that is not there is the
	// caller's daemon starting a service activation we did not ask for.
	path, ok := unixSocketPath(address)
	if !ok {
		return false, "D-Bus address is not a unix socket: " + address
	}

	if _, err := os.Stat(path); err != nil { //nolint:gosec // G703: the path is a D-Bus socket from the environment, and the check is a stat, not an open
		return false, "no D-Bus session socket at " + path
	}

	return true, ""
}

// unixSocketPath extracts the path from a D-Bus address list. An address is
// `unix:<key>=<value>,<key>=<value>`, and the one that matters is `path`:
// `abstract=` names a socket in the abstract namespace, which has no file to
// stat and is therefore not something this probe can vouch for.
func unixSocketPath(address string) (string, bool) {
	for _, part := range splitAddress(address) {
		if !strings.HasPrefix(part, "unix:") {
			continue
		}

		for param := range strings.SplitSeq(strings.TrimPrefix(part, "unix:"), ",") {
			if value, ok := strings.CutPrefix(param, "path="); ok {
				return value, true
			}
		}
	}

	return "", false
}

// splitAddress splits on semicolons, which is what a D-Bus address list uses.
func splitAddress(address string) []string {
	var (
		out   []string
		start int
	)

	for i := range len(address) {
		if address[i] == ';' {
			out = append(out, address[start:i])
			start = i + 1
		}
	}

	return append(out, address[start:])
}

// RequireKeyring returns the probe's verdict as a typed error, so a caller
// that was told to use the keyring and cannot is told why in one call.
//
// A nil probe is a caller that wired nothing — an unset option, a host with
// no keyring support, a front end that never built one — and it gets the
// same answer as a machine with no keychain, because that is what it is.
// Asking the question used to dereference it, which turned a missing
// dependency into a crash inside a diagnostic.
func RequireKeyring(probe KeyringProbe) error {
	if probe == nil {
		return &KeyringUnavailableError{Reason: UnavailableMessage}
	}

	ok, reason := probe.Available()
	if ok {
		return nil
	}

	if reason == "" {
		return &KeyringUnavailableError{Reason: UnavailableMessage}
	}

	return &KeyringUnavailableError{Reason: UnavailableMessage + " (" + reason + ")"}
}

// Unwrap makes the sentinels reachable through the typed error, so existing
// errors.Is(err, ErrKeyringUnsupported) call sites keep their behaviour.
func (e *KeyringUnavailableError) Unwrap() []error {
	return []error{ErrKeyringUnavailable, ErrKeyringUnsupported}
}

// Unavailable reports whether err is the typed "no keyring here" answer. It
// is the one check a caller should make instead of matching on a sentence.
func Unavailable(err error) bool {
	if _, ok := errors.AsType[*KeyringUnavailableError](err); ok {
		return true
	}

	return errors.Is(err, ErrKeyringUnavailable)
}
