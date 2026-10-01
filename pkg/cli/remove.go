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

	addDeliverFlags(cmd, a)
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

	// `RemoveOptions.Filter` is what `--hosts` / `--except` mean to the
	// facade, and leaving it empty is not "no filter": an empty filter is
	// every adapter, so a `remove --hosts claude` on a four-host machine
	// uninstalled the package from all four and the three surviving files
	// were gone before the user could see that they were going.
	plan, err := client.PlanRemove(ctx, id, verger.RemoveOptions{
		Paths:  paths,
		Hosts:  adapters,
		Filter: a.hostFilter(),
		Cause:  cause,
	})
	if err != nil {
		return err
	}

	if len(plan.Actions) == 0 {
		return a.renderNothingInstalled(id, homeRoot(client))
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

	// `restore` puts back every trash entry tagged with the id — the trash is
	// not organised by host, so a host filter would silently do nothing.
	// `-y` goes with the write flags because restore is gated on a
	// confirmation like any other write.
	addWriteFlags(cmd, a)

	return cmd
}

// runRestore restores every trash entry tagged with the package id, through the
// facade (DESIGN §9.1); the CLI only renders the result.
// restoreDoc is the `verger restore --json` document.
type restoreDoc struct {
	Schema   schemaRef              `json:"schema"`
	Restored []verger.RestoredEntry `json:"restored"`
}

func (a *app) runRestore(ctx context.Context, id string) error {
	client, err := a.open(ctx)
	if err != nil {
		return err
	}

	paths, err := a.paths(client)
	if err != nil {
		return err
	}

	// Restore is a mutating operation: it goes through the confirmer.
	if err := a.requireConfirmation(); err != nil {
		return err
	}

	// A terminal gets a question, not a silent write. `requireConfirmation`
	// above only stops a *detached* stdin, so on a terminal it returned nil
	// and nothing else here asked: `verger restore` put deleted files back
	// while the user was still reading. The default is no — a restore is the
	// write a user most wants to interrupt, and -y stays the way to say yes
	// without a prompt.
	if a.interactive() {
		ok, askErr := a.ask("Restore "+id+" from the trash?", false)
		if askErr != nil {
			return askErr
		}

		if !ok {
			_, err := fmt.Fprintf(a.out, "%s: nothing was written\n", id)

			return err
		}
	}

	result, err := client.Restore(ctx, id, verger.RemoveOptions{Paths: paths, DryRun: a.dryRun})
	if err != nil {
		return err
	}

	if a.jsonOut {
		return a.printJSON(restoreDoc{Schema: schemaOf(schemaRemove), Restored: result.Restored})
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

// statusNotInstalled is the report status of a removal that had nothing to
// remove. It is a named constant because a script keys off the string, and a
// literal in a format call is not something a reader can grep for.
const statusNotInstalled = "not-installed"

// renderNothingInstalled reports a removal of a package that is not installed
// on any host.
//
// The exit stays 0: `remove` is idempotent so a script can remove a package
// without first asking whether it is there, and turning "already gone" into a
// failure would break exactly that script. What was wrong was the reporting.
// "no installed cell" reads like a complaint about the arguments rather than a
// statement about the machine, and under --json this branch printed NOTHING AT
// ALL — no document, no cells, no way for a caller to tell "there was nothing
// to remove" from "the command produced no output". So the text now names the
// package and the reason, and the JSON carries a status a script can test.
func (a *app) renderNothingInstalled(id, homePath string) error {
	message := id + " is not installed on any host — nothing to remove"

	if a.jsonOut {
		return a.printJSON(reportDoc{
			Schema: schemaOf(schemaReport),
			Cells:  []cellDoc{},
			Notes:  []string{message},
			Home:   homePath,
			Status: statusNotInstalled,
		})
	}

	_, err := fmt.Fprintln(a.out, message)

	return err
}
