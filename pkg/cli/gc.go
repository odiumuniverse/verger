package cli

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"
)

// newGCCmd builds `verger gc`: purge trash entries older than the retention window.
func newGCCmd(a *app) *cobra.Command {
	var dryRun bool

	cmd := &cobra.Command{
		Use:   "gc",
		Short: "Purge trash entries older than the retention window",
		Args:  usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.runGC(cmd.Context(), dryRun)
		},
	}

	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "show what would be purged without deleting")

	return cmd
}

// runGC purges expired trash entries through the store.
func (a *app) runGC(ctx context.Context, dryRun bool) error {
	client, err := a.open(ctx)
	if err != nil {
		return err
	}

	trash := client.Store().Trash()

	if dryRun {
		fmt.Fprintf(a.out, "gc: dry run, nothing purged\n")

		return nil
	}

	purged, err := trash.Purge()
	if err != nil {
		return err
	}

	fmt.Fprintf(a.out, "gc: purged %d entr%s\n", purged, map[bool]string{true: "y", false: "ies"}[purged == 1])

	return nil
}
