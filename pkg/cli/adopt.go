package cli

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/odiumuniverse/verger/pkg/apply"
	"github.com/odiumuniverse/verger/pkg/host"
	"github.com/odiumuniverse/verger/pkg/source"
	"github.com/odiumuniverse/verger/pkg/spec"
	"github.com/odiumuniverse/verger/pkg/verger"
)

// newAdoptCmd builds `verger adopt`.
func newAdoptCmd(a *app) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "adopt <host:ref>",
		Short: "Adopt a package installed by a host natively",
		Args:  usageArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			ref, err := source.Parse(args[0])
			if err != nil {
				return &UsageError{Cause: err}
			}

			if ref.Kind != source.KindAgent {
				return &UsageError{Cause: fmt.Errorf("adopt needs an agent ref like claude:caveman, got %q", args[0])}
			}

			return a.runAdopt(cmd.Context(), []source.Ref{ref})
		},
	}

	addWriteFlags(cmd, a)
	cmd.Flags().BoolVar(&a.projectFlag, "project", false, "use the project scope")

	return cmd
}

// runAdopt opens the facade and adopts the refs.
func (a *app) runAdopt(ctx context.Context, refs []source.Ref) error {
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

	return a.adoptRefs(ctx, client, paths, refs)
}

// adoptRefs is shared by `adopt` and `install host:ref`.
func (a *app) adoptRefs(ctx context.Context, client *verger.Client, paths scopePaths, refs []source.Ref) error {
	adapters := a.hosts(client)

	adapterByID := map[host.ID]host.Host{}

	for _, adapter := range adapters {
		adapterByID[adapter.ID()] = adapter
	}

	targets, err := a.targets(client, adapters)
	if err != nil {
		return err
	}

	doc, _, err := loadSpec(paths.specPath)
	if err != nil {
		return err
	}

	var (
		actions []apply.Action
		cells   []cellDoc
		notes   []string
	)

	for _, ref := range refs {
		refActions, refCells, refNotes, err := a.adoptOne(ctx, client, paths, doc, adapterByID, targets, ref)
		if err != nil {
			return err
		}

		actions = append(actions, refActions...)
		cells = append(cells, refCells...)
		notes = append(notes, refNotes...)
	}

	if err := a.printPlan(cells); err != nil {
		return err
	}

	// The plan is on screen; without a TTY and without -y the run stops here,
	// before the spec record and every other write (§1.3).
	if err := a.requireConfirmation(); err != nil {
		return err
	}

	if err := a.commitAdopted(client, paths, doc); err != nil {
		return err
	}

	if len(actions) == 0 {
		return a.printAdopted(notes)
	}

	deps, err := a.applyDeps(client, adapters, paths)
	if err != nil {
		return err
	}

	report, err := apply.Run(ctx, deps, apply.Plan{Actions: actions}, a.applyOptions(client))
	if err != nil {
		return err
	}

	report.Notes = append(report.Notes, notes...)

	return a.printReport(report, homeRoot(client))
}

// commitAdopted records the adopted spec on disk; a dry run keeps the plan in
// memory only.
func (a *app) commitAdopted(client *verger.Client, paths scopePaths, doc *spec.Spec) error {
	if a.dryRun {
		return nil
	}

	if err := a.ensure(client, paths); err != nil {
		return err
	}

	return saveSpec(paths.specPath, doc)
}

// adoptOne adopts one host-installed package: it records the spec entry and
// plans deliveries to the other detected hosts without hooks (D4).
func (a *app) adoptOne(
	ctx context.Context, client *verger.Client, paths scopePaths, doc *spec.Spec,
	adapterByID map[host.ID]host.Host, targets []host.Host, ref source.Ref,
) ([]apply.Action, []cellDoc, []string, error) {
	from, ok := adapterByID[host.ID(ref.Agent)]
	if !ok {
		return nil, nil, nil, &UsageError{Cause: fmt.Errorf("adopt: host %q is not available", ref.Agent)}
	}

	listed, err := from.Oracle().List(ctx)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("%s oracle: %w", ref.Agent, err)
	}

	entry, found := findInstalled(listed, ref.AgentRef)
	if !found {
		return nil, nil, nil, fmt.Errorf("%s: %s is not listed by the oracle", ref.Agent, ref.AgentRef)
	}

	id := firstNonEmpty(entry.Name, ref.AgentRef)

	if !hasSpecPackage(doc, id) {
		doc.Packages = append(doc.Packages, spec.Package{ID: id, AdoptedFrom: ref.Agent})
	}

	notes := []string{fmt.Sprintf("adopted %s from %s (%s)", id, ref.Agent, entry.Version)}

	var (
		actions []apply.Action
		cells   []cellDoc
	)

	for _, adapter := range targets {
		if adapter.ID() == from.ID() {
			continue
		}

		pkg := host.Package{
			ID: id, Version: entry.Version, Root: entry.Path,
			Marketplace: entry.Marketplace, Scope: paths.name, ProjectRoot: paths.project,
		}

		strategy, note := a.pickStrategy(pkg, adapter.ID(), source.KindLocal)
		if note != "" {
			notes = append(notes, note)
		}

		delivered, deliveryNotes, err := a.prepareDelivery(ctx, client, pkg, strategy)
		if err != nil {
			return nil, nil, nil, err
		}

		notes = append(notes, deliveryNotes...)

		actions = append(actions, apply.Action{
			Kind: apply.ActionInstall, Host: adapter.ID(),
			Delivery: host.Delivery{Package: delivered, Strategy: strategy, AllowHooks: false, DryRun: a.dryRun},
		})
		cells = append(cells, cellDoc{
			Package: id, Host: string(adapter.ID()), Scope: paths2Scope(paths.name),
			Status: statePlanned, Version: entry.Version, Strategy: string(strategy), Kind: string(apply.ActionInstall),
		})
	}

	return actions, cells, notes, nil
}

// printAdopted renders the spec-only adopt result.
func (a *app) printAdopted(notes []string) error {
	if a.jsonOut {
		doc := reportDoc{Notes: notes}
		doc.Cells = []cellDoc{}

		return a.printJSON(doc)
	}

	for _, note := range notes {
		if _, err := fmt.Fprintln(a.out, note); err != nil {
			return err
		}
	}

	return nil
}

// hasSpecPackage reports whether the spec already carries the id.
func hasSpecPackage(doc *spec.Spec, id string) bool {
	for _, pkg := range doc.Packages {
		if pkg.ID == id {
			return true
		}
	}

	return false
}

// findInstalled locates one oracle entry by name, id or name@owner shape.
func findInstalled(listed []host.Installed, name string) (host.Installed, bool) {
	_, short := splitID(name)

	for _, entry := range listed {
		if entry.Name == name || entry.Name == short {
			return entry, true
		}
	}

	return host.Installed{}, false
}
