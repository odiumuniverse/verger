package verger

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

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
	// Force has the meaning of ApplyOptions.Force: it overwrites files the
	// user has edited, keeping their copy under state/backups. Without it a
	// hands-off cell is left alone and the run says so.
	Force bool
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

	// fail is the first thing the spec asked for that this run could not
	// get. It is not a note: a spec naming an undeliverable package is a
	// mistake, and a run that says "nothing to do" and exits 0 about one
	// looks exactly like a run that had nothing to do.
	fail error
}

// SyncPlan plans a reconcile without writing or asking: it reads the spec,
// fetches what is missing and plans the removals of what the spec dropped. A
// caller renders Plan.Cells, confirms through its own Confirmer, and then calls
// Sync — or, once satisfied, runs the plan itself through Apply.
func (c *Client) SyncPlan(ctx context.Context, opts SyncOptions) (*SyncPlan, error) {
	// A lock or spec written by a newer verger is reported before anything
	// else, including "no spec at …": a build that cannot read the file has
	// no business planning against it or writing over it.
	if err := c.checkSchemaVersions(opts.Paths); err != nil {
		return nil, err
	}

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

	// A package the spec named and this run could not get is reported, not
	// planned around. Returning the plan here would print "nothing to do"
	// and exit 0, which is the answer a spec with a broken source deserves
	// least.
	if plan.fail != nil {
		return nil, plan.fail
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
		Force:    opts.Force,
	})
	if err != nil {
		return plan, nil, err
	}

	// A cell the executor refused is not a quiet success. Returning the
	// report and no error is what made `verger sync` answer "done" over a
	// file it had just declined to overwrite, which is the one answer a
	// script must never be able to read.
	if refused := refusedCells(report); len(refused) > 0 {
		return plan, report, &HandsOffError{Cells: refused}
	}

	return plan, report, nil
}

// refusedCells lists the cells the executor left alone: the user's own edits,
// and nothing else.
func refusedCells(report *apply.Report) []string {
	if report == nil {
		return nil
	}

	var refused []string

	for _, cell := range report.Cells {
		if cell.Status == apply.StatusHandsOff {
			refused = append(refused, cell.Package+"@"+string(cell.Host))
		}
	}

	return refused
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

	// receipted is "this machine has a record of doing it", which is not the
	// same question as "the files are here and match". A package with a
	// drifted file has a receipt and is not a restore; a package with none
	// arrived from the lock and is.
	receipted := map[string]bool{}

	for _, record := range list {
		receipted[record.Package] = true

		// A receipt whose files are not on this disk is not an install: it
		// is what a cloned machine inherits, and treating it as one is why
		// `verger sync` used to answer "nothing to do" about packages that
		// had never been delivered here.
		if !ReceiptFilesPresent(record) {
			continue
		}

		// A receipt whose files have moved is the user's own edit. Treating
		// the package as installed would make sync a silent no-op over that
		// work, so it is planned as an install and the executor's hands-off
		// guard is what refuses it — that is where the conflict and its
		// exit code come from.
		drift, driftErr := apply.ReceiptDrift(context.Background(), record, nil)
		if driftErr != nil || drift.Drifted() {
			continue
		}

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

		syncInstall(ctx, c, fetcher, plan, adapters, entry, installed, receipted, desired, opts, switches)
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
	adapters []host.Host, entry spec.Package, installed, receipted, desired map[string]bool,
	opts SyncOptions, switches Switches,
) {
	if entry.Disabled {
		plan.Notes = append(plan.Notes, entry.ID+": disabled in the spec")
		desired[entry.ID] = true

		return
	}

	// Resolve the package before anything is compared against the lock. A
	// spec may name it in a spelling the delivery will not keep — an
	// `owner/name` id from a marketplace source becomes `local:<name>` — and
	// comparing the spec's own string against the lock then reads one
	// declared package as two: one to remove, one to install, both over the
	// same files.
	fetched, ref, fetchErr := c.fetchSpecPackage(ctx, fetcher, plan.spec(), entry.ID, specDirOf(plan))
	if fetchErr != nil {
		if plan.fail == nil {
			plan.fail = fetchErr
		}

		return
	}

	if fetched != nil {
		defer func() { _ = fetched.Cleanup() }()

		if fetched.Package.ID != "" {
			entry.ID = fetched.Package.ID
		}
	}

	desired[entry.ID] = true

	if installed[entry.ID] {
		return
	}

	// No receipt for this package on this machine means what we are about to
	// write comes from the lock, not from anything done here. The executor
	// cannot tell the two apart on its own, so the plan says which it is.
	restored := !receipted[entry.ID]

	targets := switches.Filter(adapters, ExceptFor(plan.spec(), entry.ID))

	targets = propagateTargetsFor(plan.spec(), targets, &entry)
	if len(targets) == 0 {
		desired[entry.ID] = true

		return
	}

	if fetched == nil {
		return
	}

	version := firstNonEmpty(entry.Version, fetched.Package.Version)
	fetched.Package.Version = version

	pkg, err := c.buildHostPackage(fetched, targets[0].ID(), version, string(opts.Paths.Scope), opts.Paths.Project)
	if err != nil {
		plan.Notes = append(plan.Notes, entry.ID+": "+err.Error())

		return
	}

	plan.Install = append(plan.Install, PlannedPackage{Ref: ref, Package: pkg, Restored: restored})
	plan.Packages = append(plan.Packages, PlannedPackage{Ref: ref, Package: pkg, Restored: restored})

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

// propagateTargetsFor narrows one event's hosts by the effective [propagate]
// policy. ModeOrigin keeps the package at the first adapter, ModeAsk skips
// until something confirms, and the default — ModeAll — is every adapter.
// It is a function of its own because the three cases read as policy and not
// as part of planning a delivery.
func propagateTargetsFor(doc *spec.Spec, targets []host.Host, entry *spec.Package) []host.Host {
	filtered := make([]host.Host, 0, len(targets))

	for i, adapter := range targets {
		switch doc.EffectiveMode(spec.EventInstall, "", string(adapter.ID()), entry) {
		case spec.ModeOrigin:
			if i == 0 {
				filtered = append(filtered, adapter)
			}
		case spec.ModeAsk:
			// Skip unless confirmed; confirmation is a separate concern.
		default:
			filtered = append(filtered, adapter)
		}
	}

	return filtered
}

// spec returns the document a reconcile read.
func (s *SyncPlan) spec() *spec.Spec {
	if s.specDoc == nil {
		return spec.New()
	}

	return s.specDoc
}

// fetchSpecPackage resolves one spec id through the declared sources. A local
// source is a directory, which is either one package or a catalog of them;
// either way the id decides which is wanted.
//
// It returns an error rather than a note. A spec that names a package the
// machine cannot get is a mistake in the spec, and reporting it as a note
// ended the run with "nothing to do" and exit 0 — indistinguishable, from
// the outside, from a vault that has nothing to install.
func (c *Client) fetchSpecPackage(
	ctx context.Context, fetcher *source.Fetcher, doc *spec.Spec, id, specDir string,
) (*source.Fetched, source.Ref, error) {
	seen := 0

	for _, src := range doc.Sources {
		if !isLocalSourceURL(src.URL) {
			continue
		}

		// Against the spec's own directory, never the process working
		// directory: a relative source is a claim that the spec travels
		// with its packages, and resolving it from wherever the reader
		// happened to be turns a portable vault into "local path … does
		// not exist" for everyone but its author.
		ref, err := source.ParseSource(src.URL, specDir)
		if err != nil {
			return nil, source.Ref{}, &UsageError{Cause: fmt.Errorf("source %s: %w", src.Name, err)}
		}

		offers, err := fetcher.FetchLocalOffers(ctx, ref)
		if err != nil {
			return nil, source.Ref{}, &UsageError{Cause: fmt.Errorf("source %s: %w", src.Name, err)}
		}

		seen += len(offers)

		for _, fetched := range offers {
			if catalogMatch(fetched, src.Name, id) {
				return fetched, ref, nil
			}
		}
	}

	if seen == 0 && len(doc.Sources) > 0 {
		return nil, source.Ref{}, &UsageError{Cause: fmt.Errorf(
			"package %s: no declared source offers a package", id)}
	}

	return nil, source.Ref{}, &UsageError{Cause: fmt.Errorf(
		"package %s: no declared source provides it", id)}
}

// catalogMatch reports whether one offered package is the id a spec asked
// for. Four spellings reach the same package and all of them are written in
// the wild: the manifest's own id, its name, the directory it sits in, and
// the source-qualified form the spec's own id usually takes.
func catalogMatch(fetched *source.Fetched, sourceName, id string) bool {
	if fetched.Package.ID == id || fetched.Package.Name == id || fetched.Ref.ID == id {
		return true
	}

	owner, name, ok := strings.Cut(id, "/")
	if !ok {
		return false
	}

	return owner == sourceName && (name == fetched.Package.Name || name == fetched.Ref.ID)
}

// isLocalSourceURL reports whether one spec source is a local path (Ф1 supports
// local sources only; the owner/repo and marketplace forms arrive with the
// source resolver, T3.1). It delegates rather than listing prefixes: the
// parser owns the grammar, so a second table here is a second opinion that
// silently disagrees on the spellings it forgot.
func isLocalSourceURL(raw string) bool {
	return source.IsLocalURL(raw)
}

// specDirOf is the directory a plan's [[source]] paths are written relative
// to. An empty spec path means the machine has no spec file, and there is
// nothing for a relative source to be relative to.
func specDirOf(plan *SyncPlan) string {
	if plan == nil || plan.Paths.SpecPath == "" {
		return ""
	}

	return filepath.Dir(plan.Paths.SpecPath)
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

// HandsOffError reports cells the executor refused to write because the file
// on disk is not the one verger put there. It is a conflict, not a failure:
// the package is fine, the machine holds something the user changed, and the
// fix is theirs to choose — `--force` keeps their copy and overwrites.
type HandsOffError struct {
	Cells []string
}

// Error implements error.
func (e *HandsOffError) Error() string {
	return fmt.Sprintf("your edit left alone in %d cell(s): %s; rerun with --force to overwrite, keeping your copy",
		len(e.Cells), strings.Join(e.Cells, ", "))
}

// Receipt is the receipt shape the facade returns to a caller that needs the
// removed cells themselves rather than the actions that remove them.
type Receipt = receipt.Receipt
