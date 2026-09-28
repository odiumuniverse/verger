package cli

import (
	"context"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/odiumuniverse/verger/pkg/verger"
)

// newUpdateCmd builds `verger update`: Ф1 re-applies the spec (no upstream
// update checks until the source resolver, T3.1).
func newUpdateCmd(a *app) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "update [id]",
		Short: "Re-apply spec packages (Ф1: no upstream update checks yet)",
		Args:  usageArgs(cobra.MaximumNArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 1 {
				a.updateID = args[0]
			}

			return a.runSync(cmd.Context(), false)
		},
	}

	addWriteFlags(cmd, a)
	cmd.Flags().BoolVar(&a.projectFlag, "project", false, "use the project scope")

	return cmd
}

// newOutdatedCmd builds `verger outdated`.
func newOutdatedCmd(a *app) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "outdated",
		Short: "List skew and missing cells",
		Args:  usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.runStatus(cmd.Context(), true)
		},
	}

	cmd.Flags().BoolVar(&a.projectFlag, "project", false, "use the project scope")

	return cmd
}

// newPinCmd builds `verger pin`.
func newPinCmd(a *app) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "pin <id>@<version>",
		Short: "Pin a package to a version in the spec",
		Args:  usageArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, version, ok := strings.Cut(args[0], "@")
			if !ok || id == "" || version == "" {
				return &UsageError{Cause: fmt.Errorf("pin needs <id>@<version>, got %q", args[0])}
			}

			return a.runPin(cmd.Context(), id, version)
		},
	}

	addWriteFlags(cmd, a)
	cmd.Flags().BoolVar(&a.projectFlag, "project", false, "use the project scope")

	return cmd
}

// newUnpinCmd builds `verger unpin`.
func newUnpinCmd(a *app) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "unpin <id>",
		Short: "Drop the version pin of a package",
		Args:  usageArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.runPin(cmd.Context(), args[0], "")
		},
	}

	addWriteFlags(cmd, a)
	cmd.Flags().BoolVar(&a.projectFlag, "project", false, "use the project scope")

	return cmd
}

// runPin stores or clears one spec version pin through the facade; the CLI
// only renders the answer.
func (a *app) runPin(ctx context.Context, id, version string) error {
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

	result, err := client.Pin(ctx, verger.PinOptions{Paths: paths, ID: id, Version: version, DryRun: a.dryRun})
	if err != nil {
		return err
	}

	if a.dryRun {
		_, err := fmt.Fprintf(a.out, "pin: would set %s to %q\n", id, version)

		return err
	}

	if version == "" {
		_, err = fmt.Fprintf(a.out, "unpinned %s\n", result.ID)

		return err
	}

	_, err = fmt.Fprintf(a.out, "pinned %s to %s\n", result.ID, result.Version)

	return err
}
