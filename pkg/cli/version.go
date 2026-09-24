package cli

import (
	"github.com/spf13/cobra"
)

// newVersionCmd prints the running verger version.
func newVersionCmd(version string) *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the verger version",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cmd.Println("verger " + version)

			return nil
		},
	}
}
