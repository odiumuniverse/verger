package verger

import (
	"context"
	"errors"
	"fmt"

	"github.com/odiumuniverse/verger/pkg/apply"
	"github.com/odiumuniverse/verger/pkg/host"
	"github.com/odiumuniverse/verger/pkg/receipt"
	"github.com/odiumuniverse/verger/pkg/source"
	"github.com/odiumuniverse/verger/pkg/spec"
)

// SyncOptions describe one reconcile run against the spec.
type SyncOptions struct {
	// Paths is the scope whose spec is the desired state.
	Paths Paths
	// Filter narrows the hosts a sync touches.
	Filter HostFilter
	// Only restricts the run to one package id; empty means the whole spec.
	Only string
	// Check plans without writing and fails when a change is planned. It is
	// what `sync --locked` runs, so a CI can prove the spec matches the machine.
	Check bool
	// Switches override the spec's `[hosts.<id>]` table and `defaults` for one
	// run (U2); nil means "read them from the spec".
	Switches *SwitchOptions
	// Hooks and DryRun have the same meaning as on Apply.
	Hooks  HooksMode
	DryRun bool
	// Confirm answers the executor's questions; nil declines them.
	Confirm Confirmer
}

// SyncPlan is what a reconcile would do, as data.
type SyncPlan struct {
	Plan

	// spec is the document the reconcile read; the helpers below need it to
	// resolve one id without re-reading the file.
	specDoc *spec.Spec

	// Switches are the per-host feature switches this run resolved (U2).
	Switches Switches

	// Install and Remove are the actions by kind, so a caller can render the
	// removals a reconcile implies without walking Plan.Actions.
	Install []PlannedPackage
	Remove  []Receipt
}

// SyncPlan plans a reconcile without writing or asking: it reads the spec,
// fetches what is missing and plans the removals of what the spec dropped. A
// caller renders Plan.Cells, confirms through its own Confirmer, and then calls
// Sync — or, once satisfied, runs the plan itself through Apply.
func (c *Client) SyncPlan(ctx context.Context, opts SyncOptions) (*SyncPlan, error) {
	if err := c.RequireTrust(opts.Paths); err != nil {
		return nil, err
	}

	doc, ok, err := LoadSpec(opts.Paths.SpecPath)
	if err != nil {
		return nil, err
	}

	if !ok {
		return nil, &UsageError{Cause: fmt.Errorf("no spec at %s", opts.Paths.SpecPath)}
	}

	adapters, err := c.Targets(opts.Filter)
	if err != nil {
		return nil, err
	}

	// U2: the per-host switches and a package's own `except` list decide the
	// real targets, so one host or one package can be switched off without
	// touching the others.
	switches, err := c.switchesFor(opts.Paths, opts.Switches)
	if err != nil {
		return nil, err
	}

	adapters = switches.Filter(adapters, nil)

	if len(adapters) == 0 {
		return nil, errors.New("no detected hosts; pass Hosts to target one explicitly")
	}

	plan, err := c.planSync(ctx, doc, adapters, opts, switches)
	if err != nil {
		return nil, err
	}

	// The install actions are built before the emptiness check: a reconcile
	// whose only work is installing has no removals to find, and the check must
	// see the whole plan.
	if err := c.buildInstallActions(ctx, &plan.Plan, ApplyOptions{
		Hooks:   opts.Hooks,
		Confirm: opts.Confirm,
		DryRun:  opts.DryRun,
	}); err != nil {
		return nil, err
	}

	return plan, nil
}

// Sync reconciles the machine with the spec (DESIGN §9.3: "beadle calls
// c.Plan/Apply in its own sync"). It is the entry point the second front end
// drives instead of re-implementing the loop: SyncPlan plans it, then the same
// Apply every other operation uses executes it.
func (c *Client) Sync(ctx context.Context, opts SyncOptions) (*SyncPlan, *apply.Report, error) {
	plan, err := c.SyncPlan(ctx, opts)
	if err != nil {
		return nil, nil, err
	}

	if opts.Check && len(plan.Actions) > 0 {
		return plan, nil, &CheckFailedError{Planned: len(plan.Actions), LockPath: opts.Paths.LockPath}
	}

	if len(plan.Actions) == 0 {
		return plan, &apply.Report{Notes: plan.Notes}, nil
	}

	if !opts.DryRun {
		if err := c.Ensure(opts.Paths); err != nil {
			return nil, nil, err
		}
	}

	report, err := c.Apply(ctx, &plan.Plan, ApplyOptions{
		Hooks:    opts.Hooks,
		Confirm:  opts.Confirm,
		DryRun:   opts.DryRun,
		Switches: plan.Switches,
	})
	if err != nil {
		return plan, nil, err
	}

	return plan, report, nil
}

// planSync fetches every spec package that is not installed yet and plans the
// removal of every receipt the spec no longer declares. It writes nothing and
// asks nothing.
func (c *Client) planSync(ctx context.Context, doc *spec.Spec, adapters []host.Host, opts SyncOptions, switches Switches) (*SyncPlan, error) {
	receipts := receipt.NewStore(opts.Paths.ReceiptsDir)

	list, err := receipts.List()
	if err != nil {
		return nil, err
	}

	installed := map[string]bool{}

	for _, record := range list {
		installed[record.Package] = true
	}

	adapterByID := map[host.ID]host.Host{}

	for _, adapter := range adapters {
		adapterByID[adapter.ID()] = adapter
	}

	fetcher, err := source.NewFetcher(source.WithStore(c.Store()))
	if err != nil {
		return nil, err
	}

	//nolint:modernize // a composite literal cannot spell an embedded field's promoted names
	plan := &SyncPlan{Plan: Plan{Paths: opts.Paths, Adapters: adapters}, specDoc: doc, Switches: switches}

	desired := map[string]bool{}

	for _, entry := range doc.Packages {
		if !syncWants(opts, entry.ID) {
			continue
		}

		syncInstall(ctx, c, fetcher, plan, adapters, entry, installed, desired, opts, switches)
	}

	syncRemovals(plan, list, desired, adapterByID, opts)

	return plan, nil
}

// syncWants reports whether one spec entry takes part in this run.
func syncWants(opts SyncOptions, id string) bool {
	return opts.Only == "" || opts.Only == id
}

// syncInstall plans one spec entry's delivery: a disabled entry is left alone,
// an installed one has nothing to do, and anything else is fetched and planned
// for every adapter.
func syncInstall(
	ctx context.Context, c *Client, fetcher *source.Fetcher, plan *SyncPlan,
	adapters []host.Host, entry spec.Package, installed, desired map[string]bool,
	opts SyncOptions, switches Switches,
) {
	if entry.Disabled {
		plan.Notes = append(plan.Notes, entry.ID+": disabled in the spec")
		desired[entry.ID] = true

		return
	}

	desired[entry.ID] = true

	if installed[entry.ID] {
		return
	}

	targets := switches.Filter(adapters, ExceptFor(plan.spec(), entry.ID))
	if len(targets) == 0 {
		desired[entry.ID] = true

		return
	}

	fetched, ref, note := c.fetchSpecPackage(ctx, fetcher, plan.spec(), entry.ID)
	if fetched == nil {
		plan.Notes = append(plan.Notes, entry.ID+": "+note)

		return
	}

	defer func() { _ = fetched.Cleanup() }()

	version := firstNonEmpty(entry.Version, fetched.Package.Version)
	fetched.Package.Version = version

	pkg, err := c.buildHostPackage(fetched, targets[0].ID(), version, string(opts.Paths.Scope), opts.Paths.Project)
	if err != nil {
		plan.Notes = append(plan.Notes, entry.ID+": "+err.Error())

		return
	}

	plan.Install = append(plan.Install, PlannedPackage{Ref: ref, Package: pkg})
	plan.Packages = append(plan.Packages, PlannedPackage{Ref: ref, Package: pkg})

	for _, adapter := range targets {
		strategy, strategyNote := PickStrategy(pkg, adapter.ID(), ref.Kind)

		cell := Cell{
			Package: pkg.ID, Host: string(adapter.ID()), Scope: string(opts.Paths.Scope),
			Status: StatusPlanned, Version: version, Strategy: string(strategy), Kind: string(apply.ActionInstall),
		}

		if strategyNote != "" {
			cell.Notes = append(cell.Notes, strategyNote)
		}

		plan.Cells = append(plan.Cells, cell)
	}
}

// syncRemovals plans the removal of every receipt the spec no longer declares.
// A host whose adapter this build does not have is left installed, with the
// reason in the notes.
func syncRemovals(plan *SyncPlan, list []Receipt, desired map[string]bool, adapterByID map[host.ID]host.Host, opts SyncOptions) {
	for i := range list {
		record := list[i]

		if desired[record.Package] || opts.Only != "" {
			continue
		}

		hostID := host.ID(record.Host)

		if _, ok := adapterByID[hostID]; !ok {
			plan.Notes = append(plan.Notes,
				fmt.Sprintf("%s: adapter %s is not available; left installed", record.Package, record.Host))

			continue
		}

		removed := record
		plan.Remove = append(plan.Remove, removed)

		plan.Actions = append(plan.Actions, apply.Action{
			Kind: apply.ActionRemove, Host: hostID, Previous: &removed, Cause: string(receipt.CauseUser),
		})
		plan.Cells = append(plan.Cells, Cell{
			Package: record.Package, Host: record.Host, Scope: record.Scope,
			Status: StatusPlanned, Version: record.Version, Strategy: record.Strategy, Kind: string(apply.ActionRemove),
		})
	}
}

// spec returns the document a reconcile read.
func (s *SyncPlan) spec() *spec.Spec {
	if s.specDoc == nil {
		return spec.New()
	}

	return s.specDoc
}

// fetchSpecPackage resolves one spec id through the declared sources: the first
// local source whose payload carries the id wins.
func (c *Client) fetchSpecPackage(ctx context.Context, fetcher *source.Fetcher, doc *spec.Spec, id string) (*source.Fetched, source.Ref, string) {
	for _, src := range doc.Sources {
		if !isLocalSourceURL(src.URL) {
			continue
		}

		ref, err := source.Parse(src.URL)
		if err != nil {
			return nil, source.Ref{}, "source " + src.Name + ": " + err.Error()
		}

		fetched, err := fetcher.Fetch(ctx, ref)
		if err != nil {
			return nil, source.Ref{}, "source " + src.Name + ": " + err.Error()
		}

		if fetched.Package.ID == id || fetched.Package.Name == id {
			return fetched, ref, ""
		}

		_ = fetched.Cleanup()
	}

	return nil, source.Ref{}, "no declared source provides it"
}

// isLocalSourceURL reports whether one spec source is a local path (Ф1 supports
// local sources only; the owner/repo and marketplace forms arrive with the
// source resolver, T3.1).
func isLocalSourceURL(raw string) bool {
	return len(raw) > 1 && (raw[0] == '.' || raw[0] == '/' || raw[0] == '~')
}

// CheckFailedError reports that a check-only sync would have written, so the
// spec and the machine disagree.
type CheckFailedError struct {
	Planned  int
	LockPath string
}

// Error implements error.
func (e *CheckFailedError) Error() string {
	return fmt.Sprintf("%d change(s) would rewrite %s", e.Planned, e.LockPath)
}

// Receipt is the receipt shape the facade returns to a caller that needs the
// removed cells themselves rather than the actions that remove them.
type Receipt = receipt.Receipt
