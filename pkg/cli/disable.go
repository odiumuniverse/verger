package cli

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"
)

// newDisableCmd builds `verger disable <id>`.
func newDisableCmd(a *app) *cobra.Command {
	return newSetDisabledCmd(a, true)
}

// newEnableCmd builds `verger enable <id>`.
func newEnableCmd(a *app) *cobra.Command {
	return newSetDisabledCmd(a, false)
}

// newSetDisabledCmd builds the shared enable/disable command.
func newSetDisabledCmd(a *app, disabled bool) *cobra.Command {
	label := "disable"
	if !disabled {
		label = "enable"
	}

	cmd := &cobra.Command{
		Use:   label + " <id>",
		Short: "Turn one package's delivery on or off in the spec",
		Args:  usageArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.runSetDisabled(cmd.Context(), args[0], disabled)
		},
	}

	// The spec flag is written directly; there is no question to skip.
	addDryRunFlag(cmd, a)
	cmd.Flags().BoolVar(&a.projectFlag, "project", false, "use the project scope")

	return cmd
}

// runSetDisabled sets one package's disabled flag through the facade.
func (a *app) runSetDisabled(ctx context.Context, id string, disabled bool) error {
	client, err := a.open(ctx)
	if err != nil {
		return err
	}

	paths, err := a.paths(client)
	if err != nil {
		return err
	}

	if err := client.SetDisabled(ctx, paths, id, disabled, a.dryRun); err != nil {
		return err
	}

	if a.dryRun {
		fmt.Fprintf(a.out, "would %s %s\n", map[bool]string{true: "disable", false: "enable"}[disabled], id)

		return nil
	}

	fmt.Fprintf(a.out, "%s %s\n", map[bool]string{true: "disabled", false: "enabled"}[disabled], id)

	return nil
}
