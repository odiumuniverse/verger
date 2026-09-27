package cli

import (
	"context"
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/odiumuniverse/verger/pkg/apply"
	"github.com/odiumuniverse/verger/pkg/host"
	"github.com/odiumuniverse/verger/pkg/receipt"
	"github.com/odiumuniverse/verger/pkg/source"
	"github.com/odiumuniverse/verger/pkg/spec"
	"github.com/odiumuniverse/verger/pkg/verger"
)

// newSyncCmd builds `verger sync`.
func newSyncCmd(a *app) *cobra.Command {
	var locked bool

	cmd := &cobra.Command{
		Use:   "sync",
		Short: "Make the machine match the spec",
		Args:  usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.runSync(cmd.Context(), locked)
		},
	}

	addWriteFlags(cmd, a)
	cmd.Flags().BoolVar(&locked, "locked", false, "fail when the result would change the lock")
	cmd.Flags().BoolVar(&a.projectFlag, "project", false, "use the project scope")

	return cmd
}

// syncCollection is the fetched, not-yet-delivered state of one sync.
type syncCollection struct {
	packages []installPkg
	removals []apply.Action
	planned  []cellDoc
	notes    []string
}

// runSync reconciles the effective spec: missing packages are installed,
// dropped packages are removed. Ф1 resolves packages through the spec sources
// (local paths); owner/repo and marketplace resolution arrives with the source
// resolver (T3.1).
//
//nolint:gocyclo,cyclop // the command body is the documented sync flow (rule 7)
func (a *app) runSync(ctx context.Context, locked bool) error {
	client, err := a.open(ctx)
	if err != nil {
		return err
	}

	paths, err := a.paths(client)
	if err != nil {
		return err
	}

	if err := a.requireTrust(client, paths); err != nil {
		return err
	}

	doc, ok, err := loadSpec(paths.specPath)
	if err != nil {
		return err
	}

	if !ok {
		return &UsageError{Cause: fmt.Errorf("no spec at %s", paths.specPath)}
	}

	adapters, err := a.targets(client, a.hosts(client))
	if err != nil {
		return err
	}

	if len(adapters) == 0 {
		return errors.New("no detected hosts; pass --hosts to target one explicitly")
	}

	collection, err := a.syncCollect(ctx, client, doc, adapters, paths)
	if err != nil {
		return err
	}

	if err := a.printPlan(collection.planned); err != nil {
		return err
	}

	plannedActions := len(collection.removals) + len(collection.packages)

	if locked && plannedActions > 0 {
		return &LockedError{Reason: fmt.Sprintf("%d change(s) would rewrite %s", plannedActions, paths.lockPath)}
	}

	// The plan is on screen; without a TTY and without -y a run that would
	// write stops here (§1.3).
	if plannedActions > 0 {
		if err := a.requireConfirmation(); err != nil {
			return err
		}
	}

	// The hooks question comes after the plan and before any write (rule 4).
	hooksMode, err := a.hooksMode(paths, spec.HooksMode(a.hooksFlag))
	if err != nil {
		return err
	}

	for i := range collection.packages {
		allow, err := a.hooksDecision(client, collection.packages[i].pkg, hooksMode)
		if err != nil {
			return err
		}

		collection.packages[i].allow = allow
	}

	actions := collection.removals

	if len(collection.packages) > 0 {
		installActions, err := a.installActions(ctx, client, collection.packages, adapters)
		if err != nil {
			return err
		}

		actions = append(actions, installActions...)
	}

	if len(actions) == 0 {
		return a.printReport(apply.Report{Notes: collection.notes}, homeRoot(client))
	}

	if !a.dryRun {
		if err := a.ensure(client, paths); err != nil {
			return err
		}
	}

	deps, err := a.applyDeps(client, adapters, paths)
	if err != nil {
		return err
	}

	report, err := apply.Run(ctx, deps, apply.Plan{Actions: actions}, a.applyOptions(client))
	if err != nil {
		return err
	}

	report.Notes = append(report.Notes, collection.notes...)

	return a.printReport(report, homeRoot(client))
}

// syncCollect fetches the desired packages and plans the removals without any
// write or confirmation.
//
//nolint:gocyclo,cyclop // the two scans (install/remove) mirror the tabular spec semantics
func (a *app) syncCollect(ctx context.Context, client *verger.Client, doc *spec.Spec, adapters []host.Host, paths scopePaths) (syncCollection, error) {
	var collection syncCollection

	receipts := receipt.NewStore(paths.receiptsDir)

	list, err := receipts.List()
	if err != nil {
		return collection, err
	}

	installed := map[string]bool{}

	for _, record := range list {
		installed[record.Package] = true
	}

	adapterByID := map[host.ID]host.Host{}

	for _, adapter := range adapters {
		adapterByID[adapter.ID()] = adapter
	}

	fetcher, err := source.NewFetcher(source.WithStore(client.Store()))
	if err != nil {
		return collection, err
	}

	desired := map[string]bool{}

	for _, entry := range doc.Packages {
		if a.updateID != "" && entry.ID != a.updateID {
			continue
		}

		if entry.Disabled {
			collection.notes = append(collection.notes, entry.ID+": disabled in the spec")
			desired[entry.ID] = true

			continue
		}

		desired[entry.ID] = true

		if installed[entry.ID] {
			continue
		}

		fetched, ref, note := fetchSpecPackage(ctx, fetcher, doc, entry.ID)
		if fetched == nil {
			collection.notes = append(collection.notes, entry.ID+": "+note)

			continue
		}

		defer func() { _ = fetched.Cleanup() }()

		version := firstNonEmpty(entry.Version, fetched.Package.Version)
		fetched.Package.Version = version

		pkg, err := buildHostPackage(client, fetched, adapters[0].ID(), version, paths.name, paths.project)
		if err != nil {
			return collection, err
		}

		entryVersion := version

		for _, adapter := range adapters {
			strategy, strategyNote := a.pickStrategy(pkg, adapter.ID(), ref.Kind)

			cell := cellDoc{
				Package: pkg.ID, Host: string(adapter.ID()), Scope: paths2Scope(pkg.Scope),
				Status: statePlanned, Version: entryVersion, Strategy: string(strategy), Kind: string(apply.ActionInstall),
			}

			if strategyNote != "" {
				cell.Notes = append(cell.Notes, strategyNote)
			}

			collection.planned = append(collection.planned, cell)
		}

		collection.packages = append(collection.packages, installPkg{ref: ref, pkg: pkg})
	}

	for _, record := range list {
		if desired[record.Package] || a.updateID != "" {
			continue
		}

		hostID := host.ID(record.Host)

		if _, ok := adapterByID[hostID]; !ok {
			collection.notes = append(collection.notes, fmt.Sprintf("%s: adapter %s is not available; left installed", record.Package, record.Host))

			continue
		}

		collection.removals = append(collection.removals, apply.Action{
			Kind: apply.ActionRemove, Host: hostID, Previous: &record, Cause: string(receipt.CauseUser),
		})
		collection.planned = append(collection.planned, cellDoc{
			Package: record.Package, Host: record.Host, Scope: record.Scope,
			Status: statePlanned, Version: record.Version, Strategy: record.Strategy, Kind: string(apply.ActionRemove),
		})
	}

	return collection, nil
}

// fetchSpecPackage resolves one spec id through the declared sources: the
// first local source whose payload carries the id wins. Ф1 supports local
// sources only; owner/repo and marketplace resolution arrives with the source
// resolver (T3.1). A nil result carries a note.
func fetchSpecPackage(ctx context.Context, fetcher *source.Fetcher, doc *spec.Spec, id string) (*source.Fetched, source.Ref, string) {
	for _, src := range doc.Sources {
		if !isLocalSourceURL(src.URL) {
			continue
		}

		ref, err := source.Parse(src.URL)
		if err != nil {
			continue
		}

		fetched, err := fetcher.Fetch(ctx, ref)
		if err != nil {
			continue
		}

		if idString(fetched.Package) != id {
			_ = fetched.Cleanup()

			continue
		}

		return fetched, ref, ""
	}

	return nil, source.Ref{}, "no local source resolves this id; install it once or add a local [[source]] (the source resolver is T3.1)"
}

// isLocalSourceURL reports whether a source URL is a local "./" path.
func isLocalSourceURL(raw string) bool {
	return len(raw) > 2 && raw[0] == '.' && raw[1] == '/'
}
