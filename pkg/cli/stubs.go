package cli

import (
	"github.com/spf13/cobra"
)

// newMarketplaceCmd builds the marketplace stub: `marketplace add/rm/list`
// (DESIGN §7.5, native registration §4.8) is T3.3.
func newMarketplaceCmd(a *app) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "marketplace",
		Short: "Manage marketplaces (not available in Ф1)",
		Args:  usageArgs(cobra.NoArgs),
		RunE:  notAvailable(a, "marketplace management", "marketplace add/rm/list with native Claude/Codex registration arrives in T3.3"),
	}

	for _, name := range []string{"add", "rm", "list"} {
		cmd.AddCommand(&cobra.Command{
			Use:   name,
			Short: "Not available in Ф1",
			RunE:  notAvailable(a, "marketplace "+name, "marketplace add/rm/list with native Claude/Codex registration arrives in T3.3"),
		})
	}

	return cmd
}

// newWatchCmd builds the watcher stub.
func newWatchCmd(a *app) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "watch",
		Short: "Run the reconcile daemon (not available in Ф1)",
		Args:  usageArgs(cobra.NoArgs),
		RunE:  notAvailable(a, "the watcher daemon", "pkg/watch arrives in a later phase (D19)"),
	}

	return cmd
}

// newSelfUpdateCmd builds the self-update stub.
func newSelfUpdateCmd(a *app) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "self-update",
		Short: "Update the verger binary (install.sh installs only)",
		Args:  usageArgs(cobra.NoArgs),
		RunE:  notAvailable(a, "self-update", "the install.sh updater ships with the distribution task (D18)"),
	}

	return cmd
}

// notAvailable renders a *NotAvailableError command body.
func notAvailable(_ *app, feature, hint string) func(*cobra.Command, []string) error {
	return func(*cobra.Command, []string) error {
		return &NotAvailableError{Feature: feature, Hint: hint}
	}
}
