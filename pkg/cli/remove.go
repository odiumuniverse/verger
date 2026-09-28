package cli

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/odiumuniverse/verger/pkg/receipt"
	"github.com/odiumuniverse/verger/pkg/verger"
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

// runRemove removes every receipt cell of one package and tombstones them. The
// plan and the execution both come from the facade (DESIGN §9.1); the CLI only
// renders the plan, gates the confirmation and renders the report.
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

	adapters := a.hosts(client)

	plan, err := client.PlanRemove(ctx, id, verger.RemoveOptions{Paths: paths, Hosts: adapters, Cause: cause})
	if err != nil {
		return err
	}

	if len(plan.Actions) == 0 {
		_, err := fmt.Fprintf(a.out, "%s: no installed cell\n", id)

		return err
	}

	if err := a.printPlan(cliCells(plan.Cells)); err != nil {
		return err
	}

	// The plan is on screen; without a TTY and without -y the run stops here,
	// before any write (§1.3).
	if err := a.requireConfirmation(); err != nil {
		return err
	}

	report, err := client.Remove(ctx, plan, a.applyOptionsFacade())
	if err != nil {
		return err
	}

	return a.printReport(*report, homeRoot(client))
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

// runRestore restores every trash entry tagged with the package id, through the
// facade (DESIGN §9.1); the CLI only renders the result.
func (a *app) runRestore(ctx context.Context, id string) error {
	client, err := a.open(ctx)
	if err != nil {
		return err
	}

	paths, err := a.paths(client)
	if err != nil {
		return err
	}

	result, err := client.Restore(ctx, id, verger.RemoveOptions{Paths: paths, DryRun: a.dryRun})
	if err != nil {
		return err
	}

	if a.jsonOut {
		return a.printJSON(struct {
			Restored []verger.RestoredEntry `json:"restored"`
		}{Restored: result.Restored})
	}

	if len(result.Restored) == 0 {
		_, err := fmt.Fprintf(a.out, "%s: nothing in the trash\n", id)

		return err
	}

	for _, entry := range result.Restored {
		if _, err := fmt.Fprintf(a.out, "restored %s (%s)\n", entry.Original, entry.TrashID); err != nil {
			return err
		}
	}

	return nil
}
