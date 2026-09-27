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
)

// Action is one explicit delivery or removal.
type Action struct {
	Kind      Kind
	Host      host.ID
	Delivery  host.Delivery    // install/update: the target
	Previous  *receipt.Receipt // update/remove: the last installed cell
	Cause     string           // remove: user|capability|host-reset
	Initiator string           // remove: the host that started it (§5.6)
}

// Plan is the ordered set of actions one reconcile cycle executes.
type Plan struct {
	Actions []Action
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

	r := newRunner(ctx, deps, plan, opts)

	if err := r.recover(); err != nil {
		return r.report(), err
	}

	r.execute()

	if err := r.commit(); err != nil {
		return r.report(), err
	}

	return r.report(), r.runErr()
}
