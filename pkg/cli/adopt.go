package cli

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/odiumuniverse/verger/pkg/source"
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

	addDeliverFlags(cmd, a)
	cmd.Flags().BoolVar(&a.projectFlag, "project", false, "use the project scope")

	return cmd
}

// runAdopt adopts packages a host already has: the source host's own oracle
// names them, the spec records where they came from, and every other detected
// host receives them without hooks (D4). Both the plan and the execution come
// from the facade (DESIGN §9.1).
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

	raw := make([]string, 0, len(refs))

	for _, ref := range refs {
		raw = append(raw, ref.Raw)
	}

	plan, err := client.Plan(ctx, verger.PlanOptions{Paths: paths, Refs: raw, Filter: a.hostFilter()})
	if err != nil {
		return a.translatePlanError(err)
	}

	return a.runAdoptPlan(ctx, client, plan)
}

// runAdoptPlan renders an adopted plan and executes it.
func (a *app) runAdoptPlan(ctx context.Context, client *verger.Client, plan *verger.Plan) error {
	if err := a.printPlan(cliCells(plan.Cells)); err != nil {
		return err
	}

	// The plan is on screen; without a TTY and without -y the run stops here,
	// before the spec record and every other write (§1.3).
	if err := a.requireConfirmation(); err != nil {
		return err
	}

	report, err := client.Install(ctx, plan, a.applyOptionsFacade())
	if err != nil {
		return err
	}

	return a.printReport(*report, homeRoot(client))
}
