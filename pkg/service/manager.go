package service

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"strings"
)

// Domain is the launchd domain a user agent belongs in. A login service is
// always in the GUI domain for the calling user — never the system domain,
// which would need root and would watch the wrong user's store.
const domain = "gui"

// The platforms this package implements. Named once because every switch
// over goos repeats them, and a typo in a GOOS string would fall through to
// UnsupportedPlatformError without anyone noticing.
const (
	goosDarwin = "darwin"
	goosLinux  = "linux"
)

// register loads the unit with the platform manager. On macOS a unit that is
// already bootstrapped is bootout first, so installing a changed unit replaces
// the running one instead of failing with "service already loaded".
func register(ctx context.Context, spec Spec, run Runner) error {
	switch goos {
	case goosDarwin:
		// Best effort: a service that is not loaded is exactly the case
		// bootout complains about, and bootstrap below is the command that
		// has to succeed.
		_, _ = run.Run(ctx, "launchctl", "bootout", domain+"/"+spec.LabelOrDefault())

		if _, err := run.Run(ctx, "launchctl", "bootstrap", domain, UnitPathOrFail(spec)); err != nil {
			return fmt.Errorf("service: load the launchd agent: %w", err)
		}

		if _, err := run.Run(ctx, "launchctl", "enable", domain+"/"+spec.LabelOrDefault()); err != nil {
			return fmt.Errorf("service: enable the launchd agent: %w", err)
		}

		return nil
	case goosLinux:
		if err := requireUserSystemd(ctx, run); err != nil {
			return err
		}

		if _, err := run.Run(ctx, "systemctl", "--user", "enable", "--now", unitName(spec.LabelOrDefault())); err != nil {
			return fmt.Errorf("service: enable the systemd user unit: %w", err)
		}

		return nil
	default:
		return &UnsupportedPlatformError{GOOS: goos}
	}
}

// unregister stops the service and removes it from the platform manager. Both
// branches tolerate a service that is already gone: an uninstall is idempotent
// because a user should be able to run it twice without reading about it.
func unregister(ctx context.Context, spec Spec, run Runner) error {
	switch goos {
	case goosDarwin:
		if _, err := run.Run(ctx, "launchctl", "bootout", domain+"/"+spec.LabelOrDefault()); err != nil {
			// Not loaded, or already gone: the file removal below still runs,
			// which is what "uninstall" means to the user.
			_ = err
		}

		return nil
	case goosLinux:
		if err := requireUserSystemd(ctx, run); err != nil {
			return err
		}

		// disable --now covers both halves: stop it and drop the enablement,
		// so a reboot does not bring it back.
		if _, err := run.Run(ctx, "systemctl", "--user", "disable", "--now", unitName(spec.LabelOrDefault())); err != nil {
			_, _ = run.Run(ctx, "systemctl", "--user", "disable", unitName(spec.LabelOrDefault()))
		}

		return nil
	default:
		return &UnsupportedPlatformError{GOOS: goos}
	}
}

// active reports whether the platform manager has the service running.
func active(ctx context.Context, label string, run Runner) (bool, error) {
	switch goos {
	case goosDarwin:
		out, err := run.Run(ctx, "launchctl", "print", domain+"/"+label)
		if err != nil {
			// A non-zero exit is the answer here, not a failure to report: it
			// means launchctl does not know the label. Returning the error
			// would turn "not installed" into a hard error, and answering
			// that question is this function's whole job.
			return false, nil //nolint:nilerr // non-zero means "not loaded"
		}

		return containsRunning(string(out)), nil
	case goosLinux:
		out, err := run.Run(ctx, "systemctl", "--user", "is-active", unitName(label))
		if err != nil {
			// Same contract as the darwin branch above: a non-zero exit from
			// `is-active` means the unit is not active, which is an answer.
			return false, nil //nolint:nilerr // non-zero means "not active"
		}

		return strings.TrimSpace(string(out)) == "active", nil
	default:
		return false, &UnsupportedPlatformError{GOOS: goos}
	}
}

// loaded reports whether the platform manager knows the service at all.
func loaded(ctx context.Context, label string, run Runner) (bool, error) {
	running, err := active(ctx, label, run)
	if err != nil || running {
		return running, err
	}

	// Not running is not the same as not known: a stopped-but-enabled unit is
	// loaded. Ask the manager the second question.
	switch goos {
	case goosDarwin:
		_, err := run.Run(ctx, "launchctl", "print", domain+"/"+label)

		return err == nil, nil
	case goosLinux:
		out, err := run.Run(ctx, "systemctl", "--user", "is-enabled", unitName(label))
		if err != nil {
			// And the same again: `is-enabled` exiting non-zero means the unit
			// is not enabled, which is a state rather than an error.
			return false, nil //nolint:nilerr // non-zero means "not enabled"
		}

		enabled := strings.TrimSpace(string(out))

		return enabled != "disabled" && enabled != "", nil
	default:
		return false, &UnsupportedPlatformError{GOOS: goos}
	}
}

// requireUserSystemd returns a typed error when there is no user bus, so a
// container or a bare ssh shell gets a message it can act on instead of a
// permission error from systemctl. It never falls back to a root unit: verger
// watches one user's store, and a root unit would be the wrong service.
func requireUserSystemd(ctx context.Context, run Runner) error {
	dir := os.Getenv("XDG_RUNTIME_DIR")
	if dir == "" {
		return &NoUserSystemdError{Detail: "XDG_RUNTIME_DIR is not set"}
	}

	// G703 is a false positive here: the taint is XDG_RUNTIME_DIR, which the
	// user's own session sets, and the only thing done with it is a Stat.
	//nolint:gosec // the path is the caller's own XDG_RUNTIME_DIR, only stat'ed
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		return &NoUserSystemdError{Detail: "XDG_RUNTIME_DIR " + dir + " is not a directory"}
	}

	out, err := run.Run(ctx, "systemctl", "--user", "is-system-running")
	if err != nil {
		return &NoUserSystemdError{Detail: "systemctl --user answered: " + strings.TrimSpace(string(out))}
	}

	// "degraded" and "running" both mean there is a user manager to talk to;
	// anything else (offline, unknown) does not.
	switch strings.TrimSpace(string(out)) {
	case "running", "degraded":
		return nil
	default:
		return &NoUserSystemdError{
			Detail: "the user manager is " + strings.TrimSpace(string(out)) + ", not running or degraded",
		}
	}
}

// containsRunning reports whether launchctl print output says the service is
// running. launchctl has no is-active verb, so the answer is in the text; the
// key is checked because "state = running" is what it prints.
func containsRunning(out string) bool {
	for line := range strings.SplitSeq(out, "\n") {
		key, value, found := strings.Cut(line, "=")
		if found && strings.TrimSpace(key) == "state" && strings.Contains(strings.TrimSpace(value), "running") {
			return true
		}
	}

	return false
}

// UnitPathOrFail is UnitPath for a call site that has already checked the
// platform, so the error path is the unsupported-platform one.
func UnitPathOrFail(spec Spec) string {
	path, err := UnitPath(spec.Home, spec.LabelOrDefault())
	if err != nil {
		return ""
	}

	return path
}

// goos is the platform the manager calls branch on. It is a variable so a test
// can drive both platforms from one host; nothing in production sets it.
var goos = runtime.GOOS
