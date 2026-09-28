package cli

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/odiumuniverse/verger/pkg/apply"
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

// runSync reconciles the effective spec: missing packages are installed,
// dropped packages are removed. The plan and the execution both come from the
// facade (DESIGN §9.1); the CLI renders the plan, gates the confirmation and
// renders the report.
func (a *app) runSync(ctx context.Context, locked bool) error {
	client, err := a.open(ctx)
	if err != nil {
		return err
	}

	paths, err := a.paths(client)
	if err != nil {
		return err
	}

	// The trust gate is the facade's; the CLI renders it with its own error type
	// so the message and exit code stay the same.
	if err := a.requireTrust(client, paths); err != nil {
		return err
	}

	hooksMode, err := a.hooksMode(paths, spec.HooksMode(a.hooksFlag))
	if err != nil {
		return err
	}

	opts := verger.SyncOptions{
		Paths:  paths,
		Filter: a.hostFilter(),
		Only:   a.updateID,
		Check:  locked,
		Hooks:  verger.LibraryHooksMode(hooksMode),
		DryRun: a.dryRun,
		Confirm: func() verger.Confirmer {
			return a.applyOptionsFacade().Confirm
		}(),
	}

	// The plan is on screen before anything is written, and a check-only run
	// stops here (§1.3).
	plan, err := client.SyncPlan(ctx, opts)
	if err != nil {
		return a.translatePlanError(err)
	}

	if err := a.printPlan(cliCells(plan.Cells)); err != nil {
		return err
	}

	if locked && len(plan.Actions) > 0 {
		return &LockedError{Reason: fmt.Sprintf("%d change(s) would rewrite %s", len(plan.Actions), paths.LockPath)}
	}

	if len(plan.Actions) == 0 {
		return a.printReport(apply.Report{Notes: plan.Notes}, homeRoot(client))
	}

	if err := a.requireConfirmation(); err != nil {
		return err
	}

	_, report, err := client.Sync(ctx, opts)
	if err != nil {
		return err
	}

	return a.printReport(*report, homeRoot(client))
}
