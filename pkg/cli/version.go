package cli

import (
	"fmt"

	"github.com/spf13/cobra"
)

// printVersion writes the version line. `verger version`, `verger --version`
// and `verger -v` all come through here, so the three spellings cannot drift
// into three different answers.
func (a *app) printVersion() error {
	_, err := fmt.Fprintln(a.out, "verger "+a.opts.Version)

	return err
}

// newVersionCmd prints the running verger version.
func newVersionCmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the verger version",
		Args:  usageArgs(cobra.NoArgs),
		RunE: func(_ *cobra.Command, _ []string) error {
			return a.printVersion()
		},
	}
}
