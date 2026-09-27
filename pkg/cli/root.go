// Package cli implements the verger cobra command tree: the Ф1 command
// surface of DESIGN §8 with global --json/--home/--verbose, per-command
// -y/--dry-run, project scope, the no-TTY confirmation rule (§1.3) and the
// hooks consent question (D5). Commands reach the machine only through the
// facade and the wiring helpers in this package; no command calls os.Exit.
package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"github.com/vmkteam/embedlog"

	"github.com/odiumuniverse/verger/pkg/apply"
	"github.com/odiumuniverse/verger/pkg/verger"
)

// Options configure the command tree. Zero writers default to os.Stdout,
// os.Stderr and os.Stdin; a nil TTY uses the character-device check; a nil Now
// uses time.Now.
type Options struct {
	Version string
	Out     io.Writer // default os.Stdout
	Err     io.Writer // default os.Stderr
	In      io.Reader // default os.Stdin
	Logger  embedlog.Logger
	TTY     func(f *os.File) bool // default character-device check
	Now     func() time.Time

	// openOpts carries extra verger.Open options (tests inject fake hosts and
	// a fake keyring through it).
	openOpts []verger.Option
}

// ErrConfirmationRequired reports a pending question without a TTY and without
// -y: the plan was printed, nothing was written. It is the apply sentinel.
var ErrConfirmationRequired = apply.ErrConfirmationRequired

// UsageError reports bad arguments or flags (cobra maps the run to exit 1).
type UsageError struct {
	Cause error
}

// Error implements error.
func (e *UsageError) Error() string {
	return "usage: " + e.Cause.Error()
}

// Unwrap returns the underlying cause.
func (e *UsageError) Unwrap() error {
	return e.Cause
}

// TrustError reports an unreadable or untrusted project spec (D11).
type TrustError struct {
	Path  string
	Cause error
}

// Error implements error.
func (e *TrustError) Error() string {
	return fmt.Sprintf("project %s is not trusted: %v", e.Path, e.Cause)
}

// Unwrap returns the underlying cause.
func (e *TrustError) Unwrap() error {
	return e.Cause
}

// LockedError reports a --locked run whose effective result would change the
// lock (rule 7).
type LockedError struct {
	Reason string
}

// Error implements error.
func (e *LockedError) Error() string {
	return "locked: " + e.Reason
}

// NotAvailableError reports a command whose owning package is not part of this
// phase; the hint names the task that brings it.
type NotAvailableError struct {
	Feature string
	Hint    string
}

// Error implements error.
func (e *NotAvailableError) Error() string {
	message := e.Feature + " is not available yet"

	if e.Hint != "" {
		message += ": " + e.Hint
	}

	return message
}

// ApplyFailedError reports an applied plan with failed or skewed cells; the
// report itself is printed before it is returned, so the exit code is non-zero
// while stdout still carries the full outcome.
type ApplyFailedError struct {
	Cells []string // "<package>@<host>"
}

// Error implements error.
func (e *ApplyFailedError) Error() string {
	return fmt.Sprintf("%d cell(s) failed: %s", len(e.Cells), strings.Join(e.Cells, ", "))
}

// app is the per-invocation CLI state shared by every command.
type app struct {
	opts   Options
	out    io.Writer
	errW   io.Writer
	in     io.Reader
	now    func() time.Time
	tty    func(*os.File) bool
	logger embedlog.Logger

	jsonOut     bool
	verbose     bool
	logJSON     bool
	yes         bool
	dryRun      bool
	homeFlag    string
	projectFlag bool

	hostsFlag  []string
	exceptFlag []string
	hooksFlag  string
	pinFlag    string
	updateID   string

	client *verger.Client
}

// newApp applies the option defaults.
func newApp(opts Options) *app {
	a := &app{
		opts:   opts,
		out:    opts.Out,
		errW:   opts.Err,
		in:     opts.In,
		now:    opts.Now,
		tty:    opts.TTY,
		logger: opts.Logger,
	}

	if a.out == nil {
		a.out = os.Stdout
	}

	if a.errW == nil {
		a.errW = os.Stderr
	}

	if a.in == nil {
		a.in = os.Stdin
	}

	if a.now == nil {
		a.now = time.Now
	}

	if a.tty == nil {
		a.tty = isTerminal
	}

	return a
}

// isTerminal reports whether f is a character device.
func isTerminal(f *os.File) bool {
	if f == nil {
		return false
	}

	info, err := f.Stat()
	if err != nil {
		return false
	}

	return info.Mode()&os.ModeCharDevice != 0
}

// interactive reports whether questions may be asked on this run: a TTY and no
// -y.
func (a *app) interactive() bool {
	if a.yes || a.dryRun {
		return false
	}

	return a.tty(os.Stdin)
}

// NewRootCmd builds the complete verger command tree.
func NewRootCmd(opts Options) *cobra.Command {
	a := newApp(opts)

	root := &cobra.Command{
		Use:           "verger",
		Short:         "Install agent packages everywhere",
		Long:          "verger installs plugins, skills, MCP servers, agents, commands and hooks across every supported coding agent.",
		SilenceUsage:  true,
		SilenceErrors: true,
		Args:          usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			// The interactive TUI replaces this help screen in a later phase.
			return cmd.Help()
		},
	}

	root.SetFlagErrorFunc(func(_ *cobra.Command, err error) error {
		return &UsageError{Cause: err}
	})

	flags := root.PersistentFlags()
	flags.StringVar(&a.homeFlag, "home", "", "override the verger home directory")
	flags.BoolVar(&a.jsonOut, "json", false, "emit machine-readable JSON")
	flags.BoolVar(&a.verbose, "verbose", false, "verbose output")
	flags.BoolVar(&a.logJSON, "log-json", false, "structured JSON logs")

	addCommands(root, a)

	return root
}

// addCommands registers every Ф1 and stub command.
func addCommands(root *cobra.Command, a *app) {
	root.AddCommand(
		newInstallCmd(a),
		newRemoveCmd(a),
		newStatusCmd(a),
		newSyncCmd(a),
		newAdoptCmd(a),
		newWhyCmd(a),
		newDoctorCmd(a),
		newTrustCmd(a),
		newUntrustCmd(a),
		newApproveCmd(a),
		newRevokeCmd(a),
		newUpdateCmd(a),
		newOutdatedCmd(a),
		newPinCmd(a),
		newUnpinCmd(a),
		newRestoreCmd(a),
		newSecretCmd(a),
		newSourceCmd(a),
		newMarketplaceCmd(a),
		newWatchCmd(a),
		newPropagateCmd(a),
		newPackCmd(a),
		newLintCmd(a),
		newSelfUpdateCmd(a),
		newVersionCmd(a),
	)
}

// Execute runs the verger command tree with the release version, passing a
// context that cancels on SIGINT/SIGTERM.
func Execute(version string) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	return runArgs(ctx, Options{Version: version}, os.Args[1:])
}

// runArgs builds the tree, executes one command line and normalises cobra's
// own errors into *UsageError.
func runArgs(ctx context.Context, opts Options, args []string) error {
	root := NewRootCmd(opts)
	root.SetArgs(args)

	err := root.ExecuteContext(ctx)
	if err == nil {
		return nil
	}

	if _, ok := errors.AsType[*UsageError](err); ok {
		return err
	}

	if isCobraUsageError(err) {
		return &UsageError{Cause: err}
	}

	return err
}

// isCobraUsageError reports whether err is one of cobra's argument-free
// parsing errors.
func isCobraUsageError(err error) bool {
	message := err.Error()

	for _, prefix := range []string{"unknown command", "unknown flag", "unknown shorthand flag", "flag needs an argument"} {
		if strings.HasPrefix(message, prefix) {
			return true
		}
	}

	return false
}

// usageArgs wraps an Args validator so its error is a *UsageError.
func usageArgs(validator cobra.PositionalArgs) cobra.PositionalArgs {
	return func(cmd *cobra.Command, args []string) error {
		if err := validator(cmd, args); err != nil {
			return &UsageError{Cause: err}
		}

		return nil
	}
}

// exitCode maps one error to the process exit code: 0 on success, 1 on every
// failure (rule 13).
func exitCode(err error) int {
	if err == nil {
		return 0
	}

	return 1
}
