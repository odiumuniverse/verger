package cli

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/odiumuniverse/verger/pkg/apply"
	"github.com/odiumuniverse/verger/pkg/host"
	"github.com/odiumuniverse/verger/pkg/receipt"
)

// newRemoveCmd builds `verger remove`.
func newRemoveCmd(a *app) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "remove <id>",
		Short: "Remove a package everywhere (tombstone + RMA)",
		Args:  usageArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.runRemove(cmd.Context(), args[0], receipt.CauseUser)
		},
	}

	addWriteFlags(cmd, a)
	cmd.Flags().BoolVar(&a.projectFlag, "project", false, "use the project scope")

	return cmd
}

// runRemove removes every receipt cell of one package and tombstones them.
//
//nolint:gocyclo,cyclop // the command body is the documented removal flow (rule 5)
func (a *app) runRemove(ctx context.Context, id string, cause receipt.Cause) error {
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

	receipts := receipt.NewStore(paths.receiptsDir)

	list, err := receipts.List()
	if err != nil {
		return err
	}

	adapters := a.hosts(client)

	adapterByID := map[host.ID]host.Host{}

	for _, adapter := range adapters {
		adapterByID[adapter.ID()] = adapter
	}

	var (
		actions []apply.Action
		cells   []cellDoc
		notes   []string
	)

	for i := range list {
		record := list[i]

		if !matchesID(record.Package, id) {
			continue
		}

		hostID := host.ID(record.Host)

		if _, ok := adapterByID[hostID]; !ok {
			notes = append(notes, fmt.Sprintf("%s: adapter %s is not available; left installed", record.Package, record.Host))

			continue
		}

		actions = append(actions, apply.Action{
			Kind: apply.ActionRemove, Host: hostID, Previous: &record, Cause: string(cause),
		})
		cells = append(cells, cellDoc{
			Package: record.Package, Host: record.Host, Scope: record.Scope,
			Status: statePlanned, Version: record.Version, Strategy: record.Strategy, Kind: string(apply.ActionRemove),
		})
	}

	if len(actions) == 0 {
		if _, err := fmt.Fprintf(a.out, "%s: no installed cell\n", id); err != nil {
			return err
		}

		return nil
	}

	if err := a.printPlan(cells); err != nil {
		return err
	}

	// The plan is on screen; without a TTY and without -y the run stops here,
	// before any write (§1.3).
	if err := a.requireConfirmation(); err != nil {
		return err
	}

	if !a.dryRun {
		if err := a.ensure(client, paths); err != nil {
			return err
		}

		if err := removeSpecRecord(paths, id); err != nil {
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

	report.Notes = append(report.Notes, notes...)

	return a.printReport(report, homeRoot(client))
}

// newRestoreCmd builds `verger restore`.
func newRestoreCmd(a *app) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "restore <id>",
		Short: "Restore a removed package from the trash (≤30 days)",
		Args:  usageArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, argv []string) error {
			return a.runRestore(cmd.Context(), argv[0])
		},
	}

	addWriteFlags(cmd, a)

	return cmd
}

// trashEntryDoc is the stable JSON shape of one restored trash entry.
type trashEntryDoc struct {
	Package  string `json:"package"`
	Host     string `json:"host"`
	Original string `json:"original"`
	TrashID  string `json:"trash_id"`
}

// runRestore restores every trash entry tagged with the package id.
func (a *app) runRestore(ctx context.Context, id string) error {
	client, err := a.open(ctx)
	if err != nil {
		return err
	}

	entries, err := client.Store().Trash().List()
	if err != nil {
		return err
	}

	var restored []trashEntryDoc

	for _, entry := range entries {
		if !matchesID(entry.Package, id) {
			continue
		}

		if a.dryRun {
			restored = append(restored, trashEntryDoc{Package: entry.Package, Host: entry.Host, Original: entry.Original, TrashID: entry.ID})

			continue
		}

		if _, err := client.Store().Trash().Restore(ctx, entry.ID); err != nil {
			return err
		}

		restored = append(restored, trashEntryDoc{Package: entry.Package, Host: entry.Host, Original: entry.Original, TrashID: entry.ID})
	}

	if a.jsonOut {
		return a.printJSON(struct {
			Restored []trashEntryDoc `json:"restored"`
		}{Restored: restored})
	}

	if len(restored) == 0 {
		_, err := fmt.Fprintf(a.out, "%s: nothing in the trash\n", id)

		return err
	}

	for _, entry := range restored {
		if _, err := fmt.Fprintf(a.out, "restored %s (%s)\n", entry.Original, entry.TrashID); err != nil {
			return err
		}
	}

	return nil
}
