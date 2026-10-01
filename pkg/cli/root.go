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
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/odiumuniverse/verger/pkg/exitcode"

	"github.com/odiumuniverse/verger/pkg/secret"

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
	// keyringProbe and buildKeyring are test seams for the secrets backend.
	// They exist so no test can reach the OS secret store: a nil probe means
	// the production one, which reads the filesystem and the environment and
	// never opens a keychain.
	keyringProbe secret.KeyringProbe
	buildKeyring func(runner secret.Runner, service string) (secret.Keyring, error)
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

// ExitClass reports that a command named a feature this phase does not have.
// The user's next action is to run the task that brings it, which is not a
// disagreement on disk and not something verger can fix by retrying.
func (e *NotAvailableError) ExitClass() int { return exitcode.HostUnavailable }

// ExitClass reports that the package is installed, but not on any host. The run
// cannot ask a host to do something when there is no host to ask.
func (e *ApplyFailedError) ExitClass() int { return exitcode.HostUnavailable }

// ExitClass reports a refusal to write because the run was --locked. Rule 7 is
// what refused it, and the way out is to run without the flag.
func (e *LockedError) ExitClass() int { return exitcode.Policy }

// ExitClass reports a project manifest waiting for the user to say they trust
// it: the same question as an unanswered hook consent.
func (e *TrustError) ExitClass() int { return exitcode.Consent }

// app is the per-invocation CLI state shared by every command.
type app struct {
	opts   Options
	out    io.Writer
	errW   io.Writer
	in     io.Reader
	now    func() time.Time
	tty    func(*os.File) bool
	logger embedlog.Logger
	// diag carries the --verbose / --log-json diagnostics. embedlog sends
	// Info to stdout, which would pollute a command's real output, so the CLI
	// owns a stderr logger of its own; it is nil when neither flag is set.
	diag *slog.Logger

	jsonOut     bool
	verbose     bool
	logJSON     bool
	yes         bool
	dryRun      bool
	force       bool
	homeFlag    string
	projectFlag bool
	// allowDowngrade is the one update flag that can undo what is installed,
	// so it is named on the command rather than folded into --force: forcing
	// files and choosing an older version are different decisions.
	allowDowngrade bool
	fixFlag        bool

	hostsFlag  []string
	exceptFlag []string
	hooksFlag  string
	pinFlag    string
	updateID   string
	// keyringProbe answers "is there a keyring here" without opening one. It
	// is a field so a test can hand over a fake: the real probe reads the
	// filesystem and the environment, but a test must never depend on the
	// machine it runs on, and must never reach the OS secret store.
	keyringProbe secret.KeyringProbe
	// buildKeyring constructs the platform keyring. nil means the production
	// builder; a test passes one that returns a fake, so no test can write to
	// the user's keychain by accident.
	buildKeyring func(runner secret.Runner, service string) (secret.Keyring, error)

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

	// The production probe never opens a keychain: it reads the filesystem
	// and the environment. A test seam replaces both it and the builder.
	a.keyringProbe = secret.HostKeyringProbe{}

	if opts.keyringProbe != nil {
		a.keyringProbe = opts.keyringProbe
		a.buildKeyring = opts.buildKeyring
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

	// Build a real embedlog logger from --verbose / --log-json when the
	// caller did not inject one.
	if a.logger.Log() == nil {
		a.logger = embedlog.NewLogger(a.verbose, a.logJSON)
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

// debug writes one diagnostic line to stderr when --verbose or --log-json is
// set, and does nothing otherwise, so the flags change the observable
// output without changing the command's own.
func (a *app) debug(msg string, args ...any) {
	if a.diag == nil {
		return
	}

	a.diag.Info(msg, args...)
}

// initDiag builds the stderr diagnostic logger from the flags. It must run
// after flag parsing: newApp runs before cobra has bound --verbose and
// --log-json, so building the logger there would always see them false.
func (a *app) initDiag() {
	if a.diag != nil || (!a.verbose && !a.logJSON) {
		return
	}

	// --verbose renders text, --log-json renders one JSON object per line;
	// both go to stderr so stdout carries only the command's own result, and
	// both log at info so either flag alone produces a line.
	opts := &slog.HandlerOptions{Level: slog.LevelInfo}
	if a.logJSON {
		a.diag = slog.New(slog.NewJSONHandler(a.errW, opts))
	} else {
		a.diag = slog.New(slog.NewTextHandler(a.errW, opts))
	}
}

// NewRootCmd builds the complete verger command tree.
func NewRootCmd(opts Options) *cobra.Command {
	a := newApp(opts)
	// The bare run gets its own -y/--dry-run variables rather than sharing
	// `a.yes`/`a.dryRun`: every subcommand's flag registration writes its
	// default straight into the shared field, so a root flag bound to the same
	// variable is silently reset by whichever command registers last. These
	// are separate flags with separate answers.
	var rootYes, rootDryRun, rootVersion bool

	root := &cobra.Command{
		Use:           "verger",
		Short:         "Install agent packages everywhere",
		Long:          "verger installs plugins, skills, MCP servers, agents, commands and hooks across every supported coding agent.",
		SilenceUsage:  true,
		SilenceErrors: true,
		Args:          usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			// `--version`, `-v` and `verger version` answer the same
			// question, and beadle accepts the flags too. A user moving
			// between the two tools should not have to remember which one
			// has which spelling.
			if rootVersion {
				return a.printVersion()
			}

			// A bare run on a machine with no home is a first run: show what
			// was found and ask once. With a home it is an ordinary
			// invocation and prints the help screen, so the two never look
			// the same (W7-UX-SPEC §5.1).
			client, err := a.open(cmd.Context())
			if err != nil {
				return err
			}

			if homeExists(client) {
				return cmd.Help()
			}

			return a.onboard(client, rootYes, rootDryRun)
		},
	}

	// The first-run screen is the one place a bare invocation writes, so it
	// carries --dry-run and -y like every write command. They are registered
	// on the root alone, not persistently, so the per-command flags remain the
	// only place a write command declares them.
	root.Flags().BoolVar(&rootDryRun, "dry-run", false, "show what would be created without creating it")
	root.Flags().BoolVarP(&rootYes, "yes", "y", false, "create the home without asking")
	// Registered on the root alone, not persistently: the answer belongs to
	// the tool, not to a subcommand, and a persistent flag would make
	// `verger install --version` look like it had answered something.
	root.Flags().BoolVarP(&rootVersion, "version", "v", false, "print the verger version and exit")

	root.SetFlagErrorFunc(func(_ *cobra.Command, err error) error {
		return &UsageError{Cause: err}
	})

	flags := root.PersistentFlags()
	flags.StringVar(&a.homeFlag, "home", "", "override the verger home directory")
	flags.BoolVar(&a.jsonOut, "json", false, "emit machine-readable JSON")
	flags.BoolVar(&a.verbose, "verbose", false, "verbose output")
	flags.BoolVar(&a.logJSON, "log-json", false, "structured JSON logs")

	// One line per run makes --verbose and --log-json observable without the
	// caller having to know in advance which command is running.
	root.PersistentPreRun = func(cmd *cobra.Command, _ []string) {
		a.initDiag()
		a.debug("command start", "cmd", cmd.CommandPath())
	}

	addCommands(root, a)
	// Completions are wired from the root after every command is registered,
	// so a new command cannot forget to ask for its own and the lists stay in
	// one place (completion.go).
	a.registerCompletions(root)

	return root
}

// addCommands registers every command of the §8 surface.
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
		newInfoCmd(a),
		newSearchCmd(a),
		newApproveCmd(a),
		newRevokeCmd(a),
		newUpdateCmd(a),
		newOutdatedCmd(a),
		newPinCmd(a),
		newUnpinCmd(a),
		newDisableCmd(a),
		newEnableCmd(a),
		newRestoreCmd(a),
		newImportCmd(a),
		newSecretCmd(a),
		newSourceCmd(a),
		newPropagateCmd(a),
		newPackCmd(a),
		newLintCmd(a),
		newVersionCmd(a),
		newGCCmd(a),
		newWatchCmd(a),
		newServiceCmd(a),
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
