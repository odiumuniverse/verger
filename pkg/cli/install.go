package cli

import (
	"context"
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/odiumuniverse/verger/pkg/spec"
	"github.com/odiumuniverse/verger/pkg/verger"
)

// addWriteFlags registers -y/--dry-run on one write command. Every verb that
// can change a machine gets these two and nothing else.
func addWriteFlags(cmd *cobra.Command, a *app) {
	cmd.Flags().BoolVarP(&a.yes, "yes", "y", false, "accept defaults; never resolve destructive conflicts")
	cmd.Flags().BoolVar(&a.dryRun, "dry-run", false, "plan without writing")
}

// addDryRunFlag registers --dry-run alone, for the verbs that write a
// record but never ask anything. There is no confirmation in
// `approve`/`revoke`/`pin` to skip, so offering -y there would promise a
// prompt that does not exist.
func addDryRunFlag(cmd *cobra.Command, a *app) {
	cmd.Flags().BoolVar(&a.dryRun, "dry-run", false, "plan without writing")
}

// addForceFlag registers --force on the verbs that can actually overwrite a
// file a person edited: install, remove, adopt, sync and update. It is a
// separate helper on purpose. A flag that is accepted and then ignored is
// worse than a missing flag — the reader concludes the overwrite was
// authorised when nothing was overwritten.
func addForceFlag(cmd *cobra.Command, a *app) {
	// The help says what it costs: the old file is kept, and the report
	// names where. It is deliberately not part of -y, which must never
	// resolve a destructive conflict (rule 14).
	cmd.Flags().BoolVar(&a.force, "force", false, "overwrite files you edited; the previous version is kept and reported")
}

// addDeliverFlags is the set every verb that delivers packages gets: the
// write flags, the force flag, and the host filter.
func addDeliverFlags(cmd *cobra.Command, a *app) {
	addWriteFlags(cmd, a)
	addForceFlag(cmd, a)
	cmd.Flags().StringSliceVar(&a.hostsFlag, "hosts", nil, "only these hosts (comma separated)")
	cmd.Flags().StringSliceVar(&a.exceptFlag, "except", nil, "exclude these hosts")
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

	addDeliverFlags(cmd, a)

	cmd.Flags().StringVar(&a.hooksFlag, "hooks", "", "hooks decision: ask|yes|no")
	cmd.Flags().StringVar(&a.pinFlag, "pin", "", "pin the version")
	cmd.Flags().BoolVar(&a.projectFlag, "project", false, "use the project scope")

	return cmd
}

// runInstall fetches the refs, resolves targets, asks the hooks question and
// runInstall fetches the refs, resolves targets, asks the hooks question and
// applies the plan. The planning and the execution both come from the facade
// (DESIGN §9.1); the CLI parses flags, prints the plan, gates the confirmation
// and renders the report.
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

	plan, err := client.Plan(ctx, verger.PlanOptions{
		Paths:  paths,
		Refs:   args,
		Filter: a.hostFilter(),
		Pin:    a.pinFlag,
	})
	if err != nil {
		return a.translatePlanError(err)
	}

	// An agent ref (host:ref) is an adopt, not a fetch.
	if len(plan.Adopts) > 0 {
		return a.runAdoptPlan(ctx, client, plan)
	}

	if err := a.printPlan(cliCells(plan.Cells)); err != nil {
		return err
	}

	// The plan is on screen; without a TTY and without -y the run stops here,
	// before the hooks consent and every other write (§1.3).
	if err := a.requireConfirmation(); err != nil {
		return err
	}

	opts := a.applyOptionsFacade()

	flagMode, err := a.hooksMode(paths, spec.HooksMode(a.hooksFlag))
	if err != nil {
		return err
	}

	planHooks, err := verger.HooksModeFromSpec(paths, verger.LibraryHooksMode(flagMode))
	if err != nil {
		return err
	}

	opts.Hooks = planHooks

	report, err := client.Install(ctx, plan, opts)
	if err != nil {
		return err
	}

	return a.printReport(*report, homeRoot(client))
}

// translatePlanError renders a facade planning failure with the CLI's own error
// types, so the message and the exit code stay what they were.
func (a *app) translatePlanError(err error) error {
	usage, ok := errors.AsType[*verger.UsageError](err)
	if ok {
		return &UsageError{Cause: usage}
	}

	unavailable, ok := errors.AsType[*verger.HostUnavailableError](err)
	if ok {
		return unavailable
	}

	check, ok := errors.AsType[*verger.CheckFailedError](err)
	if ok {
		return &LockedError{Reason: check.Error()}
	}

	return err
}

// hooksMode resolves the effective hooks mode: the flag wins, then the spec
// default, then ask.
func (a *app) hooksMode(paths verger.Paths, flag spec.HooksMode) (spec.HooksMode, error) {
	switch flag {
	case "":
		break
	case spec.HooksAsk, spec.HooksYes, spec.HooksNo:
		return flag, nil
	default:
		return "", &UsageError{Cause: fmt.Errorf("--hooks: unknown mode %q", flag)}
	}

	doc, ok, err := loadSpec(paths.SpecPath)
	if err != nil {
		return "", err
	}

	if ok && doc.Defaults.Hooks != "" {
		return doc.Defaults.Hooks, nil
	}

	return spec.HooksAsk, nil
}
