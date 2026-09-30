package cli

import (
	"context"
	"errors"
	"fmt"
	"os"

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

	addDeliverFlags(cmd, a)
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
		Force:  a.force,
		// Only the update command offers the flag; a sync that was handed the
		// value would quietly downgrade from a command whose help never
		// mentions it.
		AllowDowngrade: a.allowDowngrade,
		Confirm: func() verger.Confirmer {
			return a.applyOptionsFacade().Confirm
		}(),
	}

	// The plan is on screen before anything is written, and a check-only run
	// stops here (§1.3).
	plan, err := client.SyncPlan(ctx, opts)
	if err != nil {
		// An absent spec is an empty desired state, not a mistake: there is
		// nothing to reconcile and the machine already matches it, so the run
		// succeeds with or without -y.
		if a.emptyState(err, paths) {
			return a.renderEmptySync()
		}

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

// renderEmptySync reports a scope with nothing to reconcile. An empty state
// is still a result: `--json` gets a document, so a consumer never has to
// parse prose to learn the run succeeded.
func (a *app) renderEmptySync() error {
	if a.jsonOut {
		return a.printJSON(reportDoc{Schema: schemaOf(schemaReport), Cells: []cellDoc{}})
	}

	fmt.Fprintln(a.out, "nothing to sync")

	return nil
}

// emptyState reports whether err means "this scope has no spec at all",
// which is an empty desired state rather than a mistake. A spec that exists
// but does not parse is a real error and returns false.
func (a *app) emptyState(err error, paths verger.Paths) bool {
	if _, ok := errors.AsType[*verger.UsageError](err); !ok {
		return false
	}

	return !specExists(paths.SpecPath)
}

// specExists reports whether the scope's spec document is present. It
// separates "no spec yet" (an empty desired state) from "a spec that does
// not parse" (a real error).
func specExists(path string) bool {
	_, err := os.Stat(path)

	return err == nil
}
