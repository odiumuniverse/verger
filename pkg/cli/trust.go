package cli

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/odiumuniverse/verger/pkg/consent"
	"github.com/odiumuniverse/verger/pkg/spec"
	"github.com/odiumuniverse/verger/pkg/verger"
)

// newTrustCmd builds `verger trust`.
func newTrustCmd(a *app) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "trust [path]",
		Short: "Trust the project spec at its current content hash (D11)",
		Args:  usageArgs(cobra.MaximumNArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.runTrust(cmd.Context(), trustPath(args), true)
		},
	}

	addWriteFlags(cmd, a)

	return cmd
}

// newUntrustCmd builds `verger untrust`.
func newUntrustCmd(a *app) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "untrust [path]",
		Short: "Forget the trust record of the project spec",
		Args:  usageArgs(cobra.MaximumNArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.runTrust(cmd.Context(), trustPath(args), false)
		},
	}

	addWriteFlags(cmd, a)

	return cmd
}

// trustPath resolves the spec path argument: ./verger.toml by default.
func trustPath(args []string) string {
	if len(args) == 1 {
		return args[0]
	}

	return filepath.Join(".", "verger.toml")
}

// runTrust records or removes one project trust record.
func (a *app) runTrust(ctx context.Context, path string, trust bool) error {
	client, err := a.open(ctx)
	if err != nil {
		return err
	}

	abs, err := filepath.Abs(path)
	if err != nil {
		return fmt.Errorf("resolve %s: %w", path, err)
	}

	project := filepath.Dir(abs)

	store, err := a.trustStore(client)
	if err != nil {
		return err
	}

	if !trust {
		return a.untrust(client, store, project, abs)
	}

	return a.trustProject(client, store, project, abs)
}

// untrust removes one record.
func (a *app) untrust(client *verger.Client, store *consent.TrustStore, project, abs string) error {
	if a.dryRun {
		_, err := fmt.Fprintf(a.out, "untrust: would untrust %s\n", abs)

		return err
	}

	if err := store.Untrust(project); err != nil {
		return err
	}

	if err := client.Home().Ensure(); err != nil {
		return err
	}

	if err := store.Save(); err != nil {
		return err
	}

	_, err := fmt.Fprintf(a.out, "untrusted %s\n", abs)

	return err
}

// trustProject records one project at its current spec hash.
func (a *app) trustProject(client *verger.Client, store *consent.TrustStore, project, abs string) error {
	doc, err := spec.ParseFile(abs)
	if err != nil {
		return &TrustError{Path: abs, Cause: err}
	}

	if a.dryRun {
		_, err := fmt.Fprintf(a.out, "trust: would trust %s at its current content hash\n", abs)

		return err
	}

	if !a.yes {
		ok, err := a.ask(fmt.Sprintf("Trust %s at its current content hash?", abs), false)
		if err != nil {
			return err
		}

		if !ok {
			return ErrConfirmationRequired
		}
	}

	if err := client.Home().Ensure(); err != nil {
		return err
	}

	if err := store.Trust(project, consent.SpecHash(doc)); err != nil {
		return err
	}

	if err := store.Save(); err != nil {
		return err
	}

	_, err = fmt.Fprintf(a.out, "trusted %s\n", abs)

	return err
}
