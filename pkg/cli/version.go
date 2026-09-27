package cli

import (
	"fmt"

	"github.com/spf13/cobra"
)

// newVersionCmd prints the running verger version.
func newVersionCmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the verger version",
		Args:  usageArgs(cobra.NoArgs),
		RunE: func(_ *cobra.Command, _ []string) error {
			_, err := fmt.Fprintln(a.out, "verger "+a.opts.Version)

			return err
		},
	}
}
