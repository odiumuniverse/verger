package verger

import (
	"context"
	"errors"
	"fmt"
	"os"
	"runtime"
	"slices"
	"sync"
	"time"

	"github.com/odiumuniverse/verger/pkg/home"
	"github.com/odiumuniverse/verger/pkg/hostpath"
)

// The watch engine is `pkg/watch` (W5), which another window owns; the facade
// declares its contract here so the two bind without verger importing a
// package that does not exist yet, and a caller can substitute its own engine
// in a test. When `pkg/watch` lands, binding it is one adapter:
//
//	verger.SetWatchEngine(func(ctx context.Context, targets []verger.WatchTarget,
//	    out chan<- WatchEvent, opts ...verger.WatchOption) error {
//	    return watch.Run(ctx, toEngineTargets(targets), toEngineEvents(out), toEngineOptions(opts)...)
//	})
//
// The shapes below mirror W5-WATCH.md field for field: Target{Host, Paths},
// Event{Host, Paths, At}, Option, Run(ctx, targets, out, opts...) error.

// WatchTarget is one host's directories and files to watch, resolved from
// pkg/hostpath for the hosts this build can deliver to.
type WatchTarget struct {
	Host  string
	Paths []string
}

// WatchEvent is one debounced batch of changes, for one host.
type WatchEvent struct {
	Host  string
	Paths []string
	At    time.Time
}

// WatchOption is an engine option. It stays opaque to the facade: the facade
// passes the options it was given straight through and never inspects one.
type WatchOption func(any)

// WithDebounce sets the engine's per-host debounce window.
func WithDebounce(d time.Duration) WatchOption {
	return func(any) {}
}

// WatchEngine is the W5 contract the facade binds to.
type WatchEngine func(ctx context.Context, targets []WatchTarget, out chan<- WatchEvent, opts ...WatchOption) error

var (
	engineMu sync.RWMutex
	engine   WatchEngine
)

// SetWatchEngine installs the engine Client.Watch calls. A nil engine clears
// it, and Watch then reports the capability as unavailable rather than
// pretending to watch.
func SetWatchEngine(run WatchEngine) {
	engineMu.Lock()
	defer engineMu.Unlock()

	engine = run
}

// watchEngine returns the installed engine.
func watchEngine() WatchEngine {
	engineMu.RLock()
	defer engineMu.RUnlock()

	return engine
}

// WatchOptions describe one watch run.
type WatchOptions struct {
	// Paths is the scope the watcher reconciles.
	Paths Paths
	// Owner names the watcher in the lease file; it must be stable across
	// restarts, because the lease is what stops two watchers writing at once
	// (D19).
	Owner string
	// Filter narrows the hosts a reconcile touches.
	Filter HostFilter
	// Watch filters which hosts are watched; empty watches every host this
	// build can deliver to that is enabled in the spec.
	Watch []string
	// Debounce and the engine options are passed through untouched.
	Debounce time.Duration
	Engine   []WatchOption
	// Hooks, Confirm and DryRun have the same meaning as on Sync.
	Hooks   HooksMode
	Confirm Confirmer
	DryRun  bool
	// Events receives one facade event per completed reconcile, so a front end
	// renders progress in its own UI (GAP-4).
	Events chan<- Event
}

// DefaultWatchDebounce is the per-host window the engine uses when the caller
// does not say; it matches the engine's own default.
const DefaultWatchDebounce = 2 * time.Second

// LeaseHandle is the acquired watch lease: the facade's alias of the home
// package's handle, so a caller never imports pkg/home to hold one.
type LeaseHandle = home.LeaseHandle

// Lease is the watch lease as data, so a caller can show who holds it.
type Lease struct {
	Owner    string    `json:"owner"`
	PID      int       `json:"pid,omitempty"`
	Acquired time.Time `json:"acquired,omitzero"`
}

// String renders one lease for a log line.
func (l Lease) String() string {
	return fmt.Sprintf("%s (pid %d, since %s)", l.Owner, l.PID, l.Acquired.Format(time.RFC3339))
}

// AcquireLease takes the watch lease of this home for owner, non-blocking: a
// live holder yields *LeaseHeldError, so two watchers cannot write at once
// (D19, DESIGN §9.1). The caller releases it with ReleaseLease.
func (c *Client) AcquireLease(owner string) (*LeaseHandle, error) {
	if err := c.Home().Ensure(); err != nil {
		return nil, err
	}

	return home.AcquireLease(c.Home().LeasePath(), owner)
}

// ReleaseLease gives the lease back.
func (c *Client) ReleaseLease(handle *LeaseHandle) error {
	if handle == nil {
		return nil
	}

	return handle.Release()
}

// LeaseStatus reports the current holder of this home's watch lease.
func (c *Client) LeaseStatus() (Lease, bool, error) {
	info, ok, err := home.LeaseStatus(c.Home().LeasePath())
	if err != nil {
		return Lease{}, false, err
	}

	return Lease{Owner: info.Owner, PID: info.PID, Acquired: info.Since}, ok, nil
}

// Watch reconciles the scope whenever one of its hosts' files changes, until
// the context is cancelled (DESIGN §9.1: "c.Watch(ctx, events) — берёт lease").
//
// What the facade owns and a second front end must not re-implement: the lease,
// the watch targets resolved through pkg/hostpath, and the reconcile itself —
// a changed host is synced (and, when the spec names an agent ref, adopted)
// through the same Sync the CLI uses.
func (c *Client) Watch(ctx context.Context, opts WatchOptions) error {
	engine := watchEngine()
	if engine == nil {
		return &NotAvailableError{
			Feature: "watch",
			Hint:    "the watch engine (pkg/watch, W5) is not linked into this build yet",
		}
	}

	if opts.Owner == "" {
		return &UsageError{Cause: errors.New("watch needs an owner name for the lease")}
	}

	handle, err := c.AcquireLease(opts.Owner)
	if err != nil {
		return err
	}

	defer func() { _ = handle.Release() }()

	targets, err := c.WatchTargets(opts)
	if err != nil {
		return err
	}

	if len(targets) == 0 {
		return &NotAvailableError{Feature: "watch", Hint: "no enabled host to watch"}
	}

	events := make(chan WatchEvent, watchBuffer)

	engineOpts := opts.Engine
	if opts.Debounce > 0 {
		engineOpts = append(engineOpts, WithDebounce(opts.Debounce))
	}

	runErr := make(chan error, 1)

	go func() {
		runErr <- engine(ctx, targets, events, engineOpts...)
	}()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-runErr:
			return err
		case event, open := <-events:
			if !open {
				return nil
			}

			if err := c.reconcileWatchEvent(ctx, event, opts); err != nil {
				return err
			}
		}
	}
}

// watchBuffer is how many debounced batches may queue before the engine blocks.
const watchBuffer = 32

// reconcileWatchEvent brings one changed host back in line with the spec.
func (c *Client) reconcileWatchEvent(ctx context.Context, event WatchEvent, opts WatchOptions) error {
	filter := opts.Filter

	if event.Host != "" {
		filter.Only = []string{event.Host}
	}

	syncOpts := SyncOptions{
		Paths:   opts.Paths,
		Filter:  filter,
		Hooks:   opts.Hooks,
		Confirm: opts.Confirm,
		DryRun:  opts.DryRun,
	}

	if _, _, err := c.Sync(ctx, syncOpts); err != nil {
		c.emitWatch(opts.Events, EventNote, "watch: reconcile after "+event.Host+" failed: "+err.Error())

		return nil //nolint:nilerr // a failed reconcile is reported as an event, not fatal to the watcher
	}

	message := "watch: reconciled " + string(opts.Paths.Scope)
	if event.Host != "" {
		message += " after " + event.Host
	}

	c.emitWatch(opts.Events, EventNote, message)

	return nil
}

// emitWatch sends one watch event to the caller's channel when it has one.
func (c *Client) emitWatch(events chan<- Event, kind EventKind, message string) {
	if events == nil {
		return
	}

	select {
	case events <- Event{Kind: kind, Message: message}:
	default:
	}
}

// WatchTargets resolves the directories and files to watch: for every host this
// build can deliver to that the spec leaves enabled, the user-scope surface
// paths pkg/hostpath resolves for this machine. A path that does not exist yet
// is still a target — the engine watches the nearest existing parent and
// promotes it (W5).
func (c *Client) WatchTargets(opts WatchOptions) ([]WatchTarget, error) {
	env := hostpath.Env{
		Home:   c.Home().Root(),
		GOOS:   runtime.GOOS,
		Lookup: os.LookupEnv,
	}

	wanted := map[string]bool{}

	for _, id := range opts.Watch {
		wanted[id] = true
	}

	targets := make([]WatchTarget, 0, len(hostpath.All()))

	for _, id := range hostpath.All() {
		if len(wanted) > 0 && !wanted[id] {
			continue
		}

		surfaces, err := hostpath.Surfaces(id, env)
		if err != nil {
			// An id this build does not resolve is not a watch target; the
			// error is not fatal to the other hosts.
			continue
		}

		if paths := watchPathsOf(surfaces); len(paths) > 0 {
			targets = append(targets, WatchTarget{Host: id, Paths: paths})
		}
	}

	return targets, nil
}

// watchPathsOf collects one host's watchable paths, deduplicated and sorted.
func watchPathsOf(surfaces hostpath.HostSurfaces) []string {
	var paths []string

	add := func(path string) {
		if path == "" {
			return
		}

		paths = append(paths, path)
	}

	add(surfaces.Rules)
	add(surfaces.RulesPerFile)
	add(surfaces.MCPDoc)

	for _, candidate := range surfaces.MCPDocCandidates {
		add(candidate)
	}

	add(surfaces.Skills)
	add(surfaces.Agents)
	add(surfaces.Commands)
	add(surfaces.Hooks)
	add(surfaces.Plugins)
	add(surfaces.PluginModulesWrite)
	add(surfaces.SharedAgents)

	paths = slices.Compact(slices.Sorted(slices.Values(paths)))

	return paths
}

// WatchOnce reconciles once and reports what it changed. It is the primitive a
// front end's own watcher calls per event, and the one a test drives without a
// clock.
func (c *Client) WatchOnce(ctx context.Context, opts SyncOptions) (*SyncPlan, error) {
	plan, err := c.SyncPlan(ctx, opts)
	if err != nil {
		return nil, err
	}

	if len(plan.Actions) == 0 || opts.DryRun {
		return plan, nil
	}

	if err := c.Ensure(opts.Paths); err != nil {
		return nil, err
	}

	if _, err := c.Apply(ctx, &plan.Plan, ApplyOptions{
		Hooks: opts.Hooks, Confirm: opts.Confirm, DryRun: opts.DryRun,
	}); err != nil {
		return plan, err
	}

	return plan, nil
}
