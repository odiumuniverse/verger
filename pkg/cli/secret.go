package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"
)

// secretNamePattern is the accepted secret name shape.
func validSecretName(name string) bool {
	if name == "" {
		return false
	}

	for i, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r == '_':
		case r >= '0' && r <= '9' && i > 0:
		default:
			return false
		}
	}

	return true
}

// newSecretCmd builds `verger secret`.
func newSecretCmd(a *app) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "secret",
		Short: "Manage keychain-backed secrets (values never printed)",
		Args:  usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}

	setCmd := &cobra.Command{
		Use:   "set <NAME>",
		Short: "Read a value from stdin and store it under NAME",
		Args:  usageArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.runSecretSet(cmd.Context(), args[0])
		},
	}

	rmCmd := &cobra.Command{
		Use:   "rm <NAME>",
		Short: "Delete the stored value of NAME",
		Args:  usageArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.runSecretRm(cmd.Context(), args[0])
		},
	}

	listCmd := &cobra.Command{
		Use:   "list",
		Short: "List stored secret names (never values)",
		Args:  usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.runSecretList(cmd.Context())
		},
	}

	addWriteFlags(setCmd, a)
	addWriteFlags(rmCmd, a)

	cmd.AddCommand(setCmd, rmCmd, listCmd)

	return cmd
}

// runSecretSet reads one line from stdin and stores it.
func (a *app) runSecretSet(ctx context.Context, name string) error {
	if !validSecretName(name) {
		return &UsageError{Cause: fmt.Errorf("invalid secret name %q", name)}
	}

	client, err := a.open(ctx)
	if err != nil {
		return err
	}

	reader := bufio.NewReader(a.in)

	line, err := reader.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return err
	}

	value := strings.TrimRight(line, "\r\n")
	if value == "" {
		return &UsageError{Cause: errors.New("empty secret value on stdin")}
	}

	store := a.secretsStore(client)
	store.Set(name, value)

	if a.dryRun {
		_, err := fmt.Fprintf(a.out, "secret %s: would be stored\n", name)

		return err
	}

	if err := client.Home().Ensure(); err != nil {
		return err
	}

	if err := store.Save(); err != nil {
		return err
	}

	_, err = fmt.Fprintf(a.out, "secret %s stored\n", name)

	return err
}

// runSecretRm deletes one secret.
func (a *app) runSecretRm(ctx context.Context, name string) error {
	if !validSecretName(name) {
		return &UsageError{Cause: fmt.Errorf("invalid secret name %q", name)}
	}

	client, err := a.open(ctx)
	if err != nil {
		return err
	}

	store := a.secretsStore(client)

	if !store.Delete(name) {
		_, err := fmt.Fprintf(a.out, "secret %s is not stored\n", name)

		return err
	}

	if a.dryRun {
		return nil
	}

	if err := store.Save(); err != nil {
		return err
	}

	_, err = fmt.Fprintf(a.out, "secret %s removed\n", name)

	return err
}

// runSecretList prints the stored names only.
func (a *app) runSecretList(ctx context.Context) error {
	client, err := a.open(ctx)
	if err != nil {
		return err
	}

	names := a.secretsStore(client).Names()

	if a.jsonOut {
		return a.printJSON(struct {
			Names []string `json:"names"`
		}{Names: names})
	}

	for _, name := range names {
		if _, err := fmt.Fprintln(a.out, name); err != nil {
			return err
		}
	}

	return nil
}
