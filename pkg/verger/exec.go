package verger

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/odiumuniverse/verger/pkg/apply"
	"github.com/odiumuniverse/verger/pkg/consent"
	"github.com/odiumuniverse/verger/pkg/host"
	"github.com/odiumuniverse/verger/pkg/lock"
	"github.com/odiumuniverse/verger/pkg/receipt"
	"github.com/odiumuniverse/verger/pkg/spec"
)

// Confirmer answers one question the executor asks. It is the only way a
// library operation can ask anything: the CLI supplies a TTY prompt, a second
// front end its own, and `-y` a yes-confirmer.
type Confirmer = apply.Confirmer

// HooksMode decides how Apply handles a payload that ships hooks (D5, U2:
// on by default, switchable off).
type HooksMode int

// Hooks modes. HooksAsk asks through the Confirmer unless the content hash is
// already approved; HooksYes approves without asking; HooksSkip never delivers
// hooks.
const (
	// HooksAsk asks once per package, skipping the question for an approved
	// content hash.
	HooksAsk HooksMode = iota
	// HooksYes accepts the hooks and records the approval.
	HooksYes
	// HooksSkip never delivers hooks.
	HooksSkip
)

// ApplyOptions carry everything an execution needs beyond its plan. The zero
// value asks before writing and asks about hooks.
type ApplyOptions struct {
	// DryRun plans and reports without writing anything.
	DryRun bool
	// Now is the clock; the zero value is time.Now.
	Now func() time.Time
	// Confirm answers the executor's questions. Nil means no interactive
	// confirmation, so a conflict is declined rather than resolved.
	Confirm Confirmer
	// Hooks decides how the hooks question is answered.
	Hooks HooksMode
	// Events receives progress; a nil channel drops events.
	Events chan<- Event
	// Switches carries the per-host feature switches of the run (U2). The zero
	// value switches everything on, so a caller that never resolves them keeps
	// every behaviour.
	Switches Switches
	// Scope is the trusted project root for a project-scope delivery.
	Scope string
	// Force overwrites files the user has edited since the receipt recorded
	// them. The edit is not discarded: it is copied under state/backups first
	// and the copy's path is reported, so forcing is recoverable rather than
	// destructive. The zero value never overwrites, which is the only safe
	// default for something that can take a person's work.
	Force bool
}

// Event is one progress event of an operation, so a second front end can render
// progress in its own UI instead of parsing logs.
type Event struct {
	Kind    EventKind
	Package string
	Host    string
	Message string
}

// EventKind classifies one progress event.
type EventKind string

// Event kinds.
const (
	// EventPlanned reports one planned cell before anything is written.
	EventPlanned EventKind = "planned"
	// EventApplied reports one executed cell.
	EventApplied EventKind = "applied"
	// EventSkipped reports one cell that was not executed.
	EventSkipped EventKind = "skipped"
	// EventNote reports a delivery note the caller should show.
	EventNote EventKind = "note"
)

// applyDeps assembles the executor dependencies of one scope.
func (c *Client) applyDeps(adapters []host.Host, paths Paths) (apply.Deps, error) {
	lockDoc, err := LoadLock(paths.LockPath)
	if err != nil {
		return apply.Deps{}, err
	}

	hostMap := map[host.ID]host.Host{}

	for _, adapter := range adapters {
		hostMap[adapter.ID()] = adapter
	}

	receipts := receipt.NewStore(paths.ReceiptsDir)

	return apply.Deps{
		Home:       c.Home(),
		Store:      c.Store(),
		Receipts:   receipts,
		Journal:    receipt.OpenJournal(paths.JournalPath),
		Tombstones: receipt.NewTombstoneStore(paths.TombstonesPath),
		Hosts:      hostMap,
		Owned:      NewOwnership(paths.ReceiptsDir),
		Lock:       lockDoc,
		LockPath:   paths.LockPath,
	}, nil
}

// LoadLock reads a lock document; a missing file is an empty lock.
func LoadLock(path string) (*lock.Lock, error) {
	data, err := os.ReadFile(path) //nolint:gosec // G304: the caller names the state file
	if errors.Is(err, os.ErrNotExist) {
		return lock.New(), nil
	}

	if err != nil {
		return nil, fmt.Errorf("read lock %s: %w", path, err)
	}

	parsed, err := lock.Parse(data)
	if err != nil {
		return nil, err
	}

	return parsed, nil
}

// checkSchemaVersions reads the persisted documents whose schema this build
// might be too old to understand, and reports the first one written by a
// newer tool.
//
// It runs before every other verdict — before "no spec at …", before the
// host check, before any write — because a document an older binary cannot
// read is not something the user can fix by re-running with different flags:
// the only correct answer is exit 7 and "update", never a silent success that
// rewrites the file. A missing file is not a failure: a home that has never
// synced has no lock and no spec, and that is the normal case.
func (c *Client) checkSchemaVersions(paths Paths) error {
	if paths.LockPath != "" {
		if _, err := LoadLock(paths.LockPath); err != nil {
			return schemaRefusal(err)
		}
	}

	if paths.SpecPath != "" {
		if _, _, err := LoadSpec(paths.SpecPath); err != nil {
			return schemaRefusal(err)
		}
	}

	return nil
}

// schemaRefusal keeps a schema-newer error as itself and passes everything
// else through: only a version we cannot read changes the verdict, so every
// other failure keeps the shape its own command already produced.
func schemaRefusal(err error) error {
	if err == nil {
		return nil
	}

	if _, ok := errors.AsType[*lock.SchemaNewerError](err); ok {
		return err
	}

	if _, ok := errors.AsType[*spec.SchemaNewerError](err); ok {
		return err
	}

	return nil
}

// execOptions renders the executor options of one run.
func (c *Client) execOptions(opts ApplyOptions) apply.Options {
	out := apply.Options{
		DryRun: opts.DryRun,
		Now:    opts.Now,
		Logger: c.logger,
	}

	if opts.Confirm != nil {
		out.Confirm = opts.Confirm
	}

	if opts.Force {
		// Force without a root would have to invent a place to put people's
		// files, so it stays off until a caller names the directory.
		out.Force = true
		out.BackupsRoot = filepath.Join(c.Home().StateDir(), "backups")
	}

	return out
}

// ConsentStore loads the hooks consent store.
func (c *Client) ConsentStore() (*consent.Store, error) {
	store := consent.NewStore(c.Home().ConsentPath())

	if err := store.Load(); err != nil {
		return nil, err
	}

	return store, nil
}

// TrustStore loads the project trust store.
func (c *Client) TrustStore() (*consent.TrustStore, error) {
	store := consent.NewTrustStore(c.Home().TrustPath())

	if err := store.Load(); err != nil {
		return nil, err
	}

	return store, nil
}

// ApproveHooks records the current content hash of one package's hooks.
func (c *Client) ApproveHooks(pkg host.Package) error {
	if len(pkg.Hooks) == 0 {
		return nil
	}

	store, err := c.ConsentStore()
	if err != nil {
		return err
	}

	hash := consent.HookHash(pkg.ID, pkg.Version, pkg.Hooks)

	if err := store.ApproveHooks(pkg.ID, pkg.Version, hash); err != nil {
		return err
	}

	return store.Save()
}

// HooksDecision resolves the D5 hooks question for one package: the mode
// overrides the spec default; an approved content hash skips the question; a
// dry run previews the default answer without writing; anything else asks
// through the Confirmer.
func (c *Client) HooksDecision(pkg host.Package, mode HooksMode, confirm Confirmer, dryRun bool) (bool, error) {
	// A payload can carry hooks without a declarative hook: a host module in
	// runtime/<host>/hooks/{pre,post}/ is code the host runs on every session,
	// so it asks the same question (pkg/host.HasHookModules).
	if len(pkg.Hooks) == 0 && !host.HasHookModules(pkg.Root) {
		return false, nil
	}

	switch mode {
	case HooksSkip:
		return false, nil
	case HooksYes:
		if dryRun {
			return true, nil
		}

		return true, c.ApproveHooks(pkg)
	default:
		store, err := c.ConsentStore()
		if err != nil {
			return false, err
		}

		hash := consent.HookHash(pkg.ID, pkg.Version, pkg.Hooks)
		if store.HooksApproved(pkg.ID, pkg.Version, hash) {
			return true, nil
		}

		if dryRun {
			return true, nil // a preview: the default answer, nothing is written
		}

		if confirm == nil {
			return true, c.ApproveHooks(pkg)
		}

		allow, err := confirm.Confirm(context.Background(), apply.Question{
			Kind:    "hooks",
			Package: pkg.ID,
			Message: fmt.Sprintf("Install hooks of %s %s on all agents?", pkg.ID, pkg.Version),
		})
		if err != nil {
			return false, err
		}

		if allow {
			return true, c.ApproveHooks(pkg)
		}

		return false, nil
	}
}

// RequireTrust refuses a project-scoped write when the project spec is absent
// or its hash is untrusted (D11).
func (c *Client) RequireTrust(paths Paths) error {
	if paths.Scope != Project {
		return nil
	}

	specDoc, err := spec.ParseFile(paths.SpecPath)
	if err != nil {
		return &UntrustedProjectError{Path: paths.SpecPath, Cause: err}
	}

	store, err := c.TrustStore()
	if err != nil {
		return err
	}

	trusted, err := store.Trusted(paths.Project, consent.SpecHash(specDoc))
	if err != nil {
		return err
	}

	if !trusted {
		return &UntrustedProjectError{Path: paths.SpecPath, Cause: errors.New("run `verger trust` in the project")}
	}

	return nil
}

// UntrustedProjectError reports a project-scoped write the trust gate refused.
type UntrustedProjectError struct {
	Path  string
	Cause error
}

// Error implements error.
func (e *UntrustedProjectError) Error() string {
	return fmt.Sprintf("%s: %s", e.Path, e.Cause)
}

// Unwrap returns the underlying cause.
func (e *UntrustedProjectError) Unwrap() error {
	return e.Cause
}

// yesConfirmer accepts safe defaults and declines destructive questions: it
// is what `-y` injects, and what a front end with no TTY should get rather than a
// blocked run.
type yesConfirmer struct{}

// Confirm implements Confirmer.
func (yesConfirmer) Confirm(context.Context, apply.Question) (bool, error) {
	return false, nil
}

// YesConfirmer returns the confirmer `-y` uses: it accepts defaults and never
// resolves a destructive conflict (rule 14). A second front end that wants the
// same policy injects this instead of writing its own.
func YesConfirmer() Confirmer {
	return yesConfirmer{}
}

// ApplyDeps assembles the executor dependencies of one scope over a set of
// adapters. A second front end that drives pkg/apply itself (rather than
// through Install/Remove) injects the same deps the built-in operations use, so
// receipts, ownership and the lock resolve identically.
func (c *Client) ApplyDeps(adapters []host.Host, paths Paths) (apply.Deps, error) {
	return c.applyDeps(adapters, paths)
}

// ExecOptions renders the executor options of one run, so a caller that drives
// pkg/apply directly gets this client's logger, clock and confirmer.
func (c *Client) ExecOptions(opts ApplyOptions) apply.Options {
	return c.execOptions(opts)
}
