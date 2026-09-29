package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/odiumuniverse/verger/pkg/secret"
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

	backendCmd := &cobra.Command{
		Use:   "backend [file|keyring|auto]",
		Short: "Show the storage backend, or switch to the named one",
		Long: "With no argument, prints the backend in use and whether a keychain\n" +
			"is available here.\n\n" +
			"file    keeps values in a document under the verger home\n" +
			"keyring keeps them in the platform keychain\n" +
			"auto    uses the keyring when one is available, and file otherwise\n\n" +
			"Switching carries every value across; no value is printed and none is\n" +
			"dropped. On a machine with no keychain — a headless Linux box, a\n" +
			"container, CI — use `verger secret backend file`.",
		Args: usageArgs(cobra.MaximumNArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			want := ""
			if len(args) == 1 {
				want = args[0]
			}

			return a.runSecretBackend(cmd.Context(), want)
		},
	}

	// A keychain read is not a question, and `secret set` reads its value
	// from stdin: -y would have nothing to skip.
	addDryRunFlag(setCmd, a)
	addDryRunFlag(rmCmd, a)
	addDryRunFlag(backendCmd, a)

	cmd.AddCommand(setCmd, rmCmd, listCmd, backendCmd)

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
// secretListDoc is the `verger secret list --json` document.
type secretListDoc struct {
	Schema schemaRef `json:"schema"`
	Names  []string  `json:"names"`
}

func (a *app) runSecretList(ctx context.Context) error {
	client, err := a.open(ctx)
	if err != nil {
		return err
	}

	names := a.secretsStore(client).Names()

	if a.jsonOut {
		// An empty list is `[]`, never `null`: a consumer iterating the
		// names should not have to null-check, and the rest of the CLI
		// already emits empty slices.
		if names == nil {
			names = []string{}
		}

		return a.printJSON(secretListDoc{Schema: schemaOf(schemaSecret), Names: names})
	}

	for _, name := range names {
		if _, err := fmt.Fprintln(a.out, name); err != nil {
			return err
		}
	}

	return nil
}

// secretBackendDoc is the `verger secret backend --json` document.
type secretBackendDoc struct {
	Schema      schemaRef `json:"schema"`
	Backend     string    `json:"backend"`
	Changed     bool      `json:"changed,omitempty"`
	Requested   string    `json:"requested,omitempty"`
	Keyring     bool      `json:"keyring_available"`
	Unavailable string    `json:"keyring_unavailable,omitempty"`
	Names       []string  `json:"names"`
}

// runSecretBackend prints the backend in use and whether a keychain is
// available, or switches to the named one.
//
// Two rules shape it. The availability question is answered by the probe, not
// by opening a keychain: on macOS an isolated HOME makes the keychain look
// missing and the OS answers with a modal "Reset To Defaults", so anything
// that asks the keychain a question can put a dialog on a stranger's screen.
// +// And a switch to `keyring` on a machine with no keychain returns pR's typed
// error unwrapped, so it classifies as exit 6 with the fix command already in
// the message.
func (a *app) runSecretBackend(ctx context.Context, want string) error {
	client, err := a.open(ctx)
	if err != nil {
		return err
	}

	store := a.secretsStore(client)
	current := store.Backend()
	available, reason := a.keyringProbe.Available()

	// The names travel with the answer: a switch re-uploads every value, and a
	// user who cannot see what will be carried across will not press the key.
	names := store.Names()
	if names == nil {
		names = []string{}
	}

	if want == "" {
		return a.printBackend(current, "", false, available, reason, names)
	}

	// Resolve before touching the store: `auto` is decided by the probe, and an
	// explicit `keyring` on a machine without one is refused here rather than
	// half-applied and written to later.
	req := secret.BackendRequest{
		Want:    want,
		Dir:     client.Home().StateDir(),
		Service: secret.DefaultService,
		Probe:   a.keyringProbe,
		Shell:   a.buildKeyring,
	}

	_, resolved, err := secret.SelectBackend(req)
	if err != nil {
		// Two different failures, kept apart on purpose. An unknown name is a
		// bad argument: the user typed something verger does not have, and
		// that is exit 2. A keyring that is not there is a fact about the
		// machine, and pR's typed error classifies as exit 6 with the fix
		// command already in its message — wrapping it would turn a missing
		// keychain into a usage error and hide both the class and the way out.
		if _, unknown := errors.AsType[*secret.UnknownBackendError](err); unknown {
			return &UsageError{Cause: err}
		}

		return err
	}

	// A dry run and a no-op switch both stop here: the answer is the same, and
	// neither should move a value.
	if a.dryRun {
		return a.printBackend(resolved, want, resolved != current, available, reason, names)
	}

	if resolved == current {
		return a.printBackend(resolved, want, false, available, reason, names)
	}

	if err := store.SetBackend(resolved); err != nil {
		return err
	}

	if err := client.Home().Ensure(); err != nil {
		return err
	}

	if err := store.Save(); err != nil {
		return err
	}

	return a.printBackend(resolved, want, true, available, reason, names)
}

// printBackend renders one backend answer as JSON or as two lines of prose.
func (a *app) printBackend(backend, requested string, changed, available bool, reason string, names []string) error {
	if a.jsonOut {
		return a.printJSON(secretBackendDoc{
			Schema: schemaOf(schemaSecret), Backend: backend, Changed: changed,
			Requested: requested, Keyring: available, Unavailable: reason, Names: names,
		})
	}

	switch {
	case requested == "":
		if _, err := fmt.Fprintf(a.out, "%s\n", backend); err != nil {
			return err
		}
	case !changed:
		if _, err := fmt.Fprintf(a.out, "secrets backend: already %s\n", backend); err != nil {
			return err
		}
	default:
		if _, err := fmt.Fprintf(a.out, "secrets backend: %s (%d carried over)\n", backend, len(names)); err != nil {
			return err
		}
	}

	if available {
		_, err := fmt.Fprintln(a.out, "a keychain is available on this machine")

		return err
	}

	_, err := fmt.Fprintf(a.out, "no keychain here (%s); values are kept in the file backend\n", reason)

	return err
}
