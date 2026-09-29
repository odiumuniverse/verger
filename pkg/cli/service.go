package cli

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/odiumuniverse/verger/pkg/service"
)

// systemRunner is the only Runner that shells out. Everything pkg/service
// does to the platform goes through one, so a test injects a recorder and
// never installs a unit.
type systemRunner struct{}

// Run implements service.Runner.
func (systemRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).CombinedOutput() //nolint:gosec // G204: the name and args are this package's own
}

// newServiceCmd builds `verger service`, the three verbs over pkg/service.
func newServiceCmd(a *app) *cobra.Command {
	var label string

	cmd := &cobra.Command{
		Use:   "service",
		Short: "Install, remove and inspect the background watch service",
		Args:  usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}

	cmd.PersistentFlags().StringVar(&label, "label", "", "unit label (default "+service.DefaultLabel+")")

	cmd.AddCommand(
		a.newServiceInstallCmd(label),
		a.newServiceUninstallCmd(label),
		a.newServiceStatusCmd(label),
	)

	return cmd
}

// newServiceInstallCmd builds `verger service install`.
func (a *app) newServiceInstallCmd(label string) *cobra.Command {
	return &cobra.Command{
		Use:   "install",
		Short: "Install the unit that runs `verger watch` in the background",
		Long: "Install a platform unit (launchd on macOS, systemd --user on Linux)\n" +
			"that runs `verger watch`, so the scope reconciles without a terminal open.",
		Args: usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.runServiceInstall(cmd.Context(), label)
		},
	}
}

// newServiceUninstallCmd builds `verger service uninstall`.
func (a *app) newServiceUninstallCmd(label string) *cobra.Command {
	return &cobra.Command{
		Use:   "uninstall",
		Short: "Remove the background watch unit",
		Args:  usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.runServiceUninstall(cmd.Context(), label)
		},
	}
}

// newServiceStatusCmd builds `verger service status`.
func (a *app) newServiceStatusCmd(label string) *cobra.Command {
	var asJSON bool

	cmd := &cobra.Command{
		Use:   "status",
		Short: "Report whether the background watch unit is installed and loaded",
		Args:  usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.runServiceStatus(cmd.Context(), label, asJSON)
		},
	}

	cmd.Flags().BoolVar(&asJSON, "json", false, "print one JSON document")

	return cmd
}

// runServiceInstall installs the watch unit.
//
// The spec is built from the running binary rather than from a path the user
// typed: the unit must name the executable that is actually here, or it is
// installed and broken at the same time.
func (a *app) runServiceInstall(ctx context.Context, label string) error {
	home, err := a.userHome()
	if err != nil {
		return err
	}

	if service.TemporaryHome(home) {
		// A unit pinned to a temporary home would outlive the tree that
		// built it, so the install refuses rather than writing one.
		return &service.ErrTemporaryHome{Home: home}
	}

	binary, err := os.Executable()
	if err != nil {
		return err
	}

	if resolved, resolveErr := filepath.EvalSymlinks(binary); resolveErr == nil {
		binary = resolved
	}

	spec := service.WatchSpec(binary, home, os.Getenv("VERGER_HOME"))
	spec.Label = label

	path, err := service.Install(ctx, spec, systemRunner{})
	if err != nil {
		return err
	}

	if a.jsonOut {
		fmt.Fprintf(a.out, "{\"installed\":%s,\"path\":%s}\n", "true", quoteJSON(path))

		return nil
	}

	fmt.Fprintln(a.out, "installed:", path)

	if service.IsNoUserSystemd(err) {
		fmt.Fprintln(a.errW, "note: systemd has no user bus here; the unit is written but will not start until one exists")
	}

	return nil
}

// runServiceUninstall removes the watch unit.
func (a *app) runServiceUninstall(ctx context.Context, label string) error {
	home, err := a.userHome()
	if err != nil {
		return err
	}

	spec := service.WatchSpec("", home, os.Getenv("VERGER_HOME"))
	spec.Label = label

	path, err := service.Uninstall(ctx, spec, systemRunner{})
	if err != nil {
		return err
	}

	if a.jsonOut {
		fmt.Fprintf(a.out, "{\"installed\":%s,\"path\":%s}\n", "false", quoteJSON(path))

		return nil
	}

	fmt.Fprintln(a.out, "removed:", path)

	return nil
}

// runServiceStatus reports the unit's state without changing anything.
func (a *app) runServiceStatus(ctx context.Context, label string, asJSON bool) error {
	home, err := a.userHome()
	if err != nil {
		return err
	}

	if label == "" {
		label = service.DefaultLabel
	}

	status, err := service.Check(ctx, home, label, systemRunner{})
	if err != nil {
		return err
	}

	if asJSON {
		fmt.Fprintf(a.out, "{\"state\":%s,\"path\":%s,\"installed\":%s,\"loaded\":%s}\n",
			quoteJSON(string(status.State)), quoteJSON(status.Path), boolJSON(status.Installed), boolJSON(status.Loaded))

		return nil
	}

	fmt.Fprintln(a.out, "state:  ", status.State)
	fmt.Fprintln(a.out, "path:   ", status.Path)
	fmt.Fprintln(a.out, "loaded: ", status.Loaded)

	if status.Binary != "" {
		fmt.Fprintln(a.out, "binary: ", status.Binary)
	}

	return nil
}

// userHome resolves the user's home the way the unit will see it, and
// refuses a home that does not exist rather than writing a unit that cannot
// resolve one.
func (a *app) userHome() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}

	if home == "" {
		return "", &service.ErrTemporaryHome{Home: home}
	}

	return home, nil
}

// boolJSON renders a bool as a JSON literal.
func boolJSON(v bool) string {
	if v {
		return "true"
	}

	return "false"
}
