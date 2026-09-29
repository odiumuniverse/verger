package cli

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/odiumuniverse/verger/pkg/consent"
	"github.com/odiumuniverse/verger/pkg/source"
)

// newApproveCmd builds `verger approve`.
func newApproveCmd(a *app) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "approve <id> [ref]",
		Short: "Approve the current hooks content hash (D5)",
		Args:  usageArgs(cobra.RangeArgs(1, 2)),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.runApprove(cmd.Context(), args)
		},
	}

	// `approve` asks nothing, so there is no -y to skip: it records the hash
	// the user just named. --dry-run is the only preview it has.
	addDryRunFlag(cmd, a)

	return cmd
}

// newRevokeCmd builds `verger revoke`.
func newRevokeCmd(a *app) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "revoke <id>",
		Short: "Revoke the hooks approval of a package",
		Args:  usageArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.runRevoke(cmd.Context(), args[0])
		},
	}

	// As with `approve`, there is no prompt to skip.
	addDryRunFlag(cmd, a)

	return cmd
}

// runApprove records the hooks hash of the package the ref resolves to.
func (a *app) runApprove(ctx context.Context, args []string) error {
	client, err := a.open(ctx)
	if err != nil {
		return err
	}

	id := args[0]

	if len(args) < 2 {
		return &NotAvailableError{
			Feature: "approve by id alone",
			Hint:    "point at the package: `verger approve " + id + " <ref>`; resolving bare ids needs the source resolver (T3.1)",
		}
	}

	ref, err := parseFetchRef(args[1])
	if err != nil {
		return &UsageError{Cause: err}
	}

	fetched, err := client.Fetch(ctx, ref)
	if err != nil {
		return err
	}

	defer func() { _ = fetched.Cleanup() }()

	pkgID := idString(fetched.Package)

	if _, short := splitID(id); pkgID != id && short != pkgID {
		return &UsageError{Cause: fmt.Errorf("the ref resolves to %q, not %q", pkgID, id)}
	}

	if len(fetched.Package.Hooks) == 0 {
		return &UsageError{Cause: fmt.Errorf("%s carries no hooks", pkgID)}
	}

	store, err := a.consentStore(client)
	if err != nil {
		return err
	}

	hash := consent.HookHash(pkgID, fetched.Package.Version, fetched.Package.Hooks)

	if err := store.ApproveHooks(pkgID, fetched.Package.Version, hash); err != nil {
		return err
	}

	if a.dryRun {
		_, err = fmt.Fprintf(a.out, "approve: would approve hooks for %s %s\n", pkgID, fetched.Package.Version)

		return err
	}

	if err := client.Home().Ensure(); err != nil {
		return err
	}

	if err := store.Save(); err != nil {
		return err
	}

	_, err = fmt.Fprintf(a.out, "approved hooks for %s %s\n", pkgID, fetched.Package.Version)

	return err
}

// runRevoke drops the hooks approval of one package.
func (a *app) runRevoke(ctx context.Context, id string) error {
	client, err := a.open(ctx)
	if err != nil {
		return err
	}

	store, err := a.consentStore(client)
	if err != nil {
		return err
	}

	if err := store.RevokeHooks(id); err != nil {
		return err
	}

	if a.dryRun {
		_, err = fmt.Fprintf(a.out, "revoke: would revoke hooks for %s\n", id)

		return err
	}

	if err := client.Home().Ensure(); err != nil {
		return err
	}

	if err := store.Save(); err != nil {
		return err
	}

	_, err = fmt.Fprintf(a.out, "revoked hooks for %s\n", id)

	return err
}

// parseFetchRef parses one CLI ref, accepting a bare path as "./path".
func parseFetchRef(input string) (source.Ref, error) {
	if !hasRefScheme(input) {
		input = "./" + input
	}

	return source.Parse(input)
}

// hasRefScheme reports whether the ref already carries a scheme or path shape.
func hasRefScheme(input string) bool {
	for _, prefix := range []string{"./", "github:", "git+", "npm:", "mcp:", "file://", "http://", "https://"} {
		if len(input) >= len(prefix) && input[:len(prefix)] == prefix {
			return true
		}
	}

	return false
}
