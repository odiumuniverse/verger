package cli

import (
	"context"
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/odiumuniverse/verger/pkg/apply"
	"github.com/odiumuniverse/verger/pkg/host"
	"github.com/odiumuniverse/verger/pkg/source"
	"github.com/odiumuniverse/verger/pkg/spec"
	"github.com/odiumuniverse/verger/pkg/verger"
)

// addWriteFlags registers -y/--dry-run on one write command.
func addWriteFlags(cmd *cobra.Command, a *app) {
	cmd.Flags().BoolVarP(&a.yes, "yes", "y", false, "accept defaults; never resolve destructive conflicts")
	cmd.Flags().BoolVar(&a.dryRun, "dry-run", false, "plan without writing")
}

// newInstallCmd builds `verger install`.
func newInstallCmd(a *app) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "install <spec…>",
		Short: "Install packages everywhere",
		Args:  usageArgs(cobra.MinimumNArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.runInstall(cmd.Context(), args)
		},
	}

	addWriteFlags(cmd, a)

	cmd.Flags().StringSliceVar(&a.hostsFlag, "hosts", nil, "only these hosts (comma separated)")
	cmd.Flags().StringSliceVar(&a.exceptFlag, "except", nil, "exclude these hosts")
	cmd.Flags().StringVar(&a.hooksFlag, "hooks", "", "hooks decision: ask|yes|no")
	cmd.Flags().StringVar(&a.pinFlag, "pin", "", "pin the version")
	cmd.Flags().BoolVar(&a.projectFlag, "project", false, "use the project scope")

	return cmd
}

// installPkg is one fetched package of an install run.
type installPkg struct {
	ref   source.Ref
	pkg   host.Package
	allow bool
}

// runInstall fetches the refs, resolves targets, asks the hooks question and
// applies the plan.
//
//nolint:gocyclo,cyclop // the command body is the documented install flow (rules 2–4)
func (a *app) runInstall(ctx context.Context, args []string) error {
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

	refs, err := source.ParseAll(args)
	if err != nil {
		return &UsageError{Cause: err}
	}

	agentRefs, fetchRefs := splitRefs(refs)
	if len(agentRefs) > 0 {
		if len(fetchRefs) > 0 {
			return &UsageError{Cause: errors.New("install cannot mix agent refs (host:ref) with package refs")}
		}

		return a.adoptRefs(ctx, client, paths, agentRefs)
	}

	for _, ref := range fetchRefs {
		if ref.Kind == source.KindMCP {
			return &NotAvailableError{Feature: "mcp: refs", Hint: "mcp: refs resolve through the official MCP Registry in the source resolver (T3.1)"}
		}
	}

	adapters, err := a.targets(client, a.hosts(client))
	if err != nil {
		return err
	}

	if len(adapters) == 0 {
		return errors.New("no detected hosts; pass --hosts to target one explicitly")
	}

	hooksMode, err := a.hooksMode(paths, spec.HooksMode(a.hooksFlag))
	if err != nil {
		return err
	}

	packages, err := a.fetchInstallPackages(ctx, client, fetchRefs, paths, adapters)
	if err != nil {
		return err
	}

	if err := a.printPlan(a.plannedCells(packages, adapters)); err != nil {
		return err
	}

	// The plan is on screen; without a TTY and without -y the run stops here,
	// before the hooks consent and every other write (§1.3).
	if err := a.requireConfirmation(); err != nil {
		return err
	}

	// The hooks question comes after the plan and before any write (rule 4):
	// a detached stdin without -y stops here with ErrConfirmationRequired.
	for i := range packages {
		allow, err := a.hooksDecision(client, packages[i].pkg, hooksMode)
		if err != nil {
			return err
		}

		packages[i].allow = allow
	}

	actions, err := a.installActions(ctx, client, packages, adapters)
	if err != nil {
		return err
	}

	if !a.dryRun {
		if err := a.ensure(client, paths); err != nil {
			return err
		}

		if err := a.recordInstall(paths, packages); err != nil {
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

	return a.printReport(report, homeRoot(client))
}

// fetchInstallPackages fetches every ref into one host package.
func (a *app) fetchInstallPackages(ctx context.Context, client *verger.Client, refs []source.Ref, paths scopePaths, adapters []host.Host) ([]installPkg, error) {
	fetcher, err := source.NewFetcher(source.WithStore(client.Store()))
	if err != nil {
		return nil, err
	}

	var packages []installPkg

	for _, ref := range refs {
		fetched, err := fetcher.Fetch(ctx, ref)
		if err != nil {
			return nil, err
		}

		defer func() { _ = fetched.Cleanup() }()

		version := firstNonEmpty(a.pinFlag, fetched.Package.Version)
		fetched.Package.Version = version

		pkg, err := buildHostPackage(client, fetched, adapters[0].ID(), version, paths.name, paths.project)
		if err != nil {
			return nil, err
		}

		packages = append(packages, installPkg{ref: ref, pkg: pkg})
	}

	return packages, nil
}

// plannedCells renders the display plan of one install run without writes.
func (a *app) plannedCells(packages []installPkg, adapters []host.Host) []cellDoc {
	var planned []cellDoc

	for _, item := range packages {
		for _, adapter := range adapters {
			strategy, note := a.pickStrategy(item.pkg, adapter.ID(), item.ref.Kind)

			cell := cellDoc{
				Package: item.pkg.ID, Host: string(adapter.ID()), Scope: paths2Scope(item.pkg.Scope),
				Status: statePlanned, Version: item.pkg.Version, Strategy: string(strategy), Kind: string(apply.ActionInstall),
			}

			if note != "" {
				cell.Notes = append(cell.Notes, note)
			}

			planned = append(planned, cell)
		}
	}

	return planned
}

// installActions builds one install action per package and host.
func (a *app) installActions(ctx context.Context, client *verger.Client, packages []installPkg, adapters []host.Host) ([]apply.Action, error) {
	var actions []apply.Action

	for _, item := range packages {
		for _, adapter := range adapters {
			strategy, _ := a.pickStrategy(item.pkg, adapter.ID(), item.ref.Kind)

			pkg, _, err := a.prepareDelivery(ctx, client, item.pkg, strategy)
			if err != nil {
				return nil, err
			}

			delivery := host.Delivery{
				Package:    pkg,
				Strategy:   strategy,
				AllowHooks: item.allow,
				DryRun:     a.dryRun,
			}

			actions = append(actions, apply.Action{Kind: apply.ActionInstall, Host: adapter.ID(), Delivery: delivery})
		}
	}

	return actions, nil
}

// paths2Scope renders one scope value.
func paths2Scope(scope string) string {
	if scope == "" {
		return "user"
	}

	return scope
}

// hooksMode resolves the effective hooks mode: the flag wins, then the spec
// default, then ask.
func (a *app) hooksMode(paths scopePaths, flag spec.HooksMode) (spec.HooksMode, error) {
	switch flag {
	case "":
		break
	case spec.HooksAsk, spec.HooksYes, spec.HooksNo:
		return flag, nil
	default:
		return "", &UsageError{Cause: fmt.Errorf("--hooks: unknown mode %q", flag)}
	}

	doc, ok, err := loadSpec(paths.specPath)
	if err != nil {
		return "", err
	}

	if ok && doc.Defaults.Hooks != "" {
		return doc.Defaults.Hooks, nil
	}

	return spec.HooksAsk, nil
}

// splitRefs separates agent refs (host:ref) from fetchable refs.
func splitRefs(refs []source.Ref) ([]source.Ref, []source.Ref) {
	var agents, fetchable []source.Ref

	for _, ref := range refs {
		if ref.Kind == source.KindAgent {
			agents = append(agents, ref)

			continue
		}

		fetchable = append(fetchable, ref)
	}

	return agents, fetchable
}

// recordInstall writes the fetched sources and packages into the spec (D3).
func (a *app) recordInstall(paths scopePaths, packages []installPkg) error {
	doc, _, err := loadSpec(paths.specPath)
	if err != nil {
		return err
	}

	changed := false

	for _, item := range packages {
		changed = addSpecSource(doc, item.ref) || changed
		changed = addSpecPackage(doc, item.pkg.ID, a.pinFlag) || changed
	}

	if !changed {
		return nil
	}

	return saveSpec(paths.specPath, doc)
}

// removeSpecRecord deletes one package entry from the spec.
func removeSpecRecord(paths scopePaths, id string) error {
	doc, ok, err := loadSpec(paths.specPath)
	if err != nil {
		return err
	}

	if !ok || removeSpecPackage(doc, id) == 0 {
		return nil
	}

	return saveSpec(paths.specPath, doc)
}
