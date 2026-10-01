// Package apply executes delivery plans against hosts: per-host parallelism,
// two phases (install new → oracle verify → remove old), trash instead of
// delete, RMA receipts, a per-host circuit breaker that leaves the host on
// last-known-good, and journal + lock commits (DESIGN §5.1–§5.3, §5.6).
//
// Run is the only entry point. It takes the home flock for the whole run,
// groups actions per host, runs hosts in parallel with a bounded limit and
// actions inside one host sequentially in plan order. Every action is
// committed through the journal first (an intent event), the host adapter
// second, and the receipt + lock last, so a crash between the phases is
// replayed deterministically by the next Run.
//
// A replay re-resolves file and tree backups from the store trash by target
// path, or by the previous receipt's digest when an adapter staged the bytes
// under its own source path. Config-key backups are staged by the adapters as
// temporary files instead, so an op that lost its backup id (a crash between
// the install and the receipt, or an adapter failure that returned an empty
// Result) hands off: the new value stays, the previous value stays in the
// trash — no bytes are lost — until the adapters return their executed
// partial RMA on failure.
package apply

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/vmkteam/embedlog"

	"github.com/odiumuniverse/verger/pkg/digest"
	"github.com/odiumuniverse/verger/pkg/fsutil"
	"github.com/odiumuniverse/verger/pkg/home"
	"github.com/odiumuniverse/verger/pkg/host"
	"github.com/odiumuniverse/verger/pkg/lock"
	"github.com/odiumuniverse/verger/pkg/receipt"
	"github.com/odiumuniverse/verger/pkg/store"
)

// Kind names one plan action.
type Kind string

// Action kinds.
const (
	ActionInstall Kind = "install"
	ActionUpdate  Kind = "update"
	ActionRemove  Kind = "remove"
	// ActionAdopt is a package taken over from a host's own list. It is a
	// delivery like the others — the spec now declares it — and it is named
	// separately because nothing was fetched to place it there.
	ActionAdopt Kind = "adopt"
)

// Action is one explicit delivery or removal.
type Action struct {
	Kind      Kind
	Host      host.ID
	Delivery  host.Delivery    // install/update: the target
	Previous  *receipt.Receipt // update/remove: the last installed cell
	Cause     string           // remove: user|capability|host-reset
	Initiator string           // remove: the host that started it (§5.6)
	// Restored marks a delivery that had no receipt on this machine: the
	// package arrived from the lock, so what the executor writes is a
	// restore, not an install. Only the plan knows what this machine had
	// before, so the verdict is made there and carried, not rediscovered.
	Restored bool
}

// SharedTarget is one physical path several hosts of a plan resolve to the same
// bytes. agy, codex, dsh and omp all read skills under ~/.agents, so one
// package names the same file once per host.
//
// Writer is the ONE host whose delivery writes the file; Hosts names every host
// whose receipt references it. The executor writes shared targets in their own
// serialized phase, before any per-host delivery starts, so no two hosts ever
// execute over one path. That is a property of the ORDER rather than of which
// goroutine happened to reach the path first.
type SharedTarget struct {
	Path   string
	Digest digest.Hash
	Writer host.ID
	Hosts  []host.ID
}

// Plan is the ordered set of actions one reconcile cycle executes.
type Plan struct {
	Actions []Action

	// Shared are the paths several hosts resolve to one file; the executor
	// writes them first, one host at a time, before the per-host phase.
	Shared []SharedTarget
}

// Status is the outcome of one cell action.
type Status string

// Cell statuses.
const (
	StatusCurrent      Status = "current"
	StatusMissing      Status = "missing"
	StatusSkew         Status = "skew" // verify failed, host kept last-known-good
	StatusHandsOff     Status = "hands-off"
	StatusFailed       Status = "failed"
	StatusNeedsAuth    Status = "needs-auth"
	StatusNeedsRuntime Status = "needs-runtime"
	StatusForeign      Status = "foreign"
	// StatusSkipped means the delivery ran and wrote nothing on purpose: the
	// package had nothing for this host, or all of it was already there. It
	// is deliberately not "current" — a cell that says it wrote files while
	// the disk holds none and the receipt holds no digest is the one answer
	// that must never be printed.
	StatusSkipped Status = "skipped"
)

// CellResult reports one executed action.
type CellResult struct {
	Package  string
	Host     host.ID
	Scope    string
	Kind     Kind
	Strategy lock.Strategy
	Status   Status
	Version  string
	Notes    []string
	// Backup is where the user's own copy of an overwritten file was kept, and
	// is set only by a forced run. It is empty otherwise, including when the
	// run wrote a file that had no user edit behind it.
	Backup string
	// Restored marks a cell delivered from the lock without a receipt of its
	// own: what arrived from a spec or a lock, not what this machine did. It
	// is the honest "restored from lock" in a render, because a cell with a
	// fresh receipt is an install, not a restore.
	Restored bool
}

// CircuitState is the typed per-host circuit breaker state (DESIGN §5.2): a
// host whose action failed is not retried in the same run and its later
// actions are skipped, leaving the host on last-known-good.
type CircuitState struct {
	Host    host.ID
	Tripped bool
	Status  Status // skew|failed
	Cause   string
}

// Report is the outcome of one Run. Cells preserve plan order.
type Report struct {
	Cells    []CellResult
	Notes    []string
	Breakers []CircuitState
	// PendingConsent names the packages whose hooks this run delivered nothing
	// for, because nobody answered. It is part of the result rather than state on
	// whoever ran it: a client outlives a run, and a record that outlived its run
	// made every later run report a consent failure for a package it had never
	// looked at.
	PendingConsent []string
	// Refusals names the operations a host declined to carry out. It is part
	// of the result rather than buried in a cell's notes because the exit
	// class is decided from it: a run that failed only because a host said no
	// is a different answer for a script than a run verger could not finish.
	Refusals []HostRefusedError
}

// Confirmer asks the user one question and reports the answer.
type Confirmer interface {
	Confirm(ctx context.Context, q Question) (bool, error)
}

// Question is one confirmation request.
type Question struct {
	Kind    string // "conflict" | "hooks" | "remove"
	Package string
	Host    host.ID
	Message string
}

// ErrConfirmationRequired reports a conflict or hands-off key that needs a
// Confirmer and had none.
var ErrConfirmationRequired = errors.New("confirmation required")

// defaultParallel bounds concurrent host execution when Options.Parallel is 0.
const defaultParallel = 4

// loadLockForRun reads the lock from disk under the flock, falling back to the
// caller's snapshot when the document cannot be read.
//
// A missing file is a first install, and that is not an error. A BROKEN path is
// not either: the run must reach commit so the save fails there and the caller
// gets the *LockError with its durable receipt, which is the contract
// TestRunLockSaveFailureIsFatal pins. Only a lock from a NEWER verger is fatal
// here - silently carrying on would overwrite a document this build has never
// understood, which is the one loss that cannot be undone.
func loadLockForRun(path string, fallback *lock.Lock) (*lock.Lock, error) {
	doc, err := lock.ParseFile(path)
	if err != nil {
		if _, newer := errors.AsType[*lock.SchemaNewerError](err); newer {
			return nil, err
		}

		return fallback, nil
	}

	return doc, nil
}

// Deps are the run dependencies; every member but LockPath is required.
type Deps struct {
	Home       *home.Home
	Store      *store.Store
	Receipts   *receipt.Store
	Journal    *receipt.Journal
	Tombstones *receipt.TombstoneStore
	Hosts      map[host.ID]host.Host
	Owned      host.PathOwner
	Lock       *lock.Lock
	LockPath   string
}

// Options configure one Run.
type Options struct {
	DryRun   bool
	Parallel int // per-host parallelism, default 4
	Logger   embedlog.Logger
	Now      func() time.Time
	Events   chan<- Event // optional progress, never closed by apply
	Confirm  Confirmer
	// Force overwrites a file whose content moved since the receipt recorded
	// it. The user's copy is kept first: Force is a way to get the package
	// back to the spec, never a way to lose work. BackupsRoot is where that
	// copy goes, and an empty root disables forcing rather than guessing a
	// place to put people's files.
	Force       bool
	BackupsRoot string
}

// Event is one progress record. At is filled by apply.
type Event struct {
	At      time.Time
	Package string
	Host    host.ID
	Step    string // plan|install|verify|remove|rollback|receipt|lock
	Message string
}

// ConfigError reports missing dependencies or an invalid plan.
type ConfigError struct {
	Cause error
}

// Error implements error.
func (e *ConfigError) Error() string {
	return "apply config: " + e.Cause.Error()
}

// Unwrap returns the underlying cause.
func (e *ConfigError) Unwrap() error {
	return e.Cause
}

// ReceiptError reports a receipt or tombstone persistence failure.
type ReceiptError struct {
	Package string
	Host    string
	Cause   error
}

// Error implements error.
func (e *ReceiptError) Error() string {
	return fmt.Sprintf("apply receipt %s/%s: %v", e.Package, e.Host, e.Cause)
}

// Unwrap returns the underlying cause.
func (e *ReceiptError) Unwrap() error {
	return e.Cause
}

// HostRefusedError reports that the host did not carry out an operation verger
// asked of it, so the run cannot honestly report the operation as done.
//
// It is a type of its own because "the host said no" is not any other failure.
// The removal ran, the host answered, and the answer was that the thing is
// still there — a condition the user can go and look at, on that host, with
// that host's own CLI. Reporting it as an unexpected error sent the run out at
// the code that means verger itself broke, which is the one answer nobody can
// act on.
//
// Output is what the host's CLI printed, verbatim. It is carried rather than
// summarised because the summary is the part the user already has: the point of
// the message is to say which host, what it still lists, and let the host speak
// for itself.
type HostRefusedError struct {
	Host    host.ID
	Package string
	Action  string
	Output  string
	Cause   error
}

// Error implements error.
func (e *HostRefusedError) Error() string {
	msg := fmt.Sprintf("%s did not %s %s", e.Host, e.Action, e.Package)
	if e.Output != "" {
		msg += ": " + e.Output
	}

	if e.Cause != nil {
		msg += ": " + e.Cause.Error()
	}

	return msg
}

// Unwrap returns the underlying cause.
func (e *HostRefusedError) Unwrap() error {
	return e.Cause
}

// LockError reports a lock load or save failure.
type LockError struct {
	Path  string
	Cause error
}

// Error implements error.
func (e *LockError) Error() string {
	return fmt.Sprintf("apply lock %s: %v", e.Path, e.Cause)
}

// Unwrap returns the underlying cause.
func (e *LockError) Unwrap() error {
	return e.Cause
}

// Run executes plan against deps: it takes the home flock for the whole run,
// replays interrupted actions from the journal, executes the plan per host
// (hosts in parallel, actions sequential inside a host) and commits the lock
// once at the end.
func Run(ctx context.Context, deps Deps, plan Plan, opts Options) (Report, error) {
	if err := validateDeps(deps); err != nil {
		return Report{}, err
	}

	if err := validatePlan(deps, plan); err != nil {
		return Report{}, err
	}

	if opts.Parallel <= 0 {
		opts.Parallel = defaultParallel
	}

	if opts.Now == nil {
		opts.Now = time.Now
	}

	unlock, err := deps.Home.Lock(ctx)
	if err != nil {
		return Report{}, err
	}

	defer func() { _ = unlock() }()

	// The lock is re-read HERE, inside the flock, not carried in from before it.
	//
	// deps.Lock was loaded before this process took the lock, so it is a snapshot
	// from before every other writer finished. Two processes then each read {}, each
	// took the flock in turn, and each SAVED ITS OWN SNAPSHOT plus its own cell -
	// the second write silently erasing the first. Six concurrent installs left
	// exactly one package in the lock while all six exited 0, because from the
	// caller's side a lost update looks exactly like a success.
	//
	// Reading under the lock is what makes the read-modify-write atomic: the
	// document read is the document no other writer has touched since it released.
	fresh, err := loadLockForRun(deps.LockPath, deps.Lock)
	if err != nil {
		return Report{}, err
	}

	deps.Lock = fresh

	r := newRunner(ctx, deps, plan, opts)

	if err := r.recover(); err != nil {
		return r.report(), err
	}

	r.execute()

	// The one barrier for every barrier-free write this run made. It belongs
	// here, after execute and before the lock is released, so the renames that
	// put those files in place are durable before another writer can interleave
	// its own flush. commit() still writes receipts, the journal and the lock
	// through the durable writer, so they do not wait for this.
	if err := fsutil.SyncPendingDirs(); err != nil {
		return r.report(), fmt.Errorf("sync delivered directories: %w", err)
	}

	if err := r.commit(); err != nil {
		return r.report(), err
	}

	return r.report(), r.runErr()
}
