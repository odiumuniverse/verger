package cli

import (
	"context"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/odiumuniverse/verger/pkg/hostpath"
	"github.com/odiumuniverse/verger/pkg/manifest"
)

// Shell completion (W7-UX-SPEC §4). Every list here is answered from what
// this machine already knows: the canonical host ids, the kinds the model
// defines, and the package ids the local spec declares. Nothing reaches the
// network, and completion on an empty home costs no more than a static list,
// because the local source is allowed to be empty.
//
// A flag that takes no value gets no completion: offering a list for a
// boolean would be decoration.

// noFileComp tells the shell not to fall back to filenames for a value
// drawn from these lists.
const noFileComp = cobra.ShellCompDirectiveNoFileComp

// componentKinds is every kind the normalized model defines. These are
// constants of the model, not machine state, so the list is the same
// everywhere.
func componentKinds() []string {
	kinds := []manifest.Kind{
		manifest.KindSkill, manifest.KindMCP, manifest.KindAgent,
		manifest.KindCommand, manifest.KindHook, manifest.KindRule,
		manifest.KindLSP, manifest.KindStyle, manifest.KindTheme,
		manifest.KindNativeJS, manifest.KindNativeTS, manifest.KindNativeOther,
	}

	out := make([]string, 0, len(kinds))
	for _, kind := range kinds {
		out = append(out, string(kind))
	}

	return out
}

// hostIDs is the canonical host list. It is a constant in pkg/hostpath, so
// completion never depends on which hosts happen to be installed: a user
// naming a host they have not installed yet still gets the name.
func hostIDs() []string { return hostpath.All() }

// knownPackages lists the package ids the local spec declares.
//
// It deliberately does not fetch a catalogue. A completion that reaches the
// network is slow on a bad connection and can hang inside a script, and the
// ids worth completing are the ones this machine can act on today.
func (a *app) knownPackages() []string {
	// The facade is opened with a background context: completion has no
	// command context to borrow, and a completion that cancelled itself
	// would leave the shell with no candidates at all.
	client, err := a.open(context.Background())
	if err != nil {
		return nil
	}

	doc, ok, err := loadSpec(client.Home().SpecPath())
	if err != nil || !ok {
		return nil
	}

	out := make([]string, 0, len(doc.Packages))
	seen := make(map[string]bool, len(doc.Packages))

	for _, pkg := range doc.Packages {
		if pkg.ID == "" || seen[pkg.ID] {
			continue
		}

		seen[pkg.ID] = true

		out = append(out, pkg.ID)
	}

	slices.Sort(out)

	return out
}

// completeWith returns a cobra completion over a fixed list.
func completeWith(values []string) func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
	return func(_ *cobra.Command, _ []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		return filterPrefix(values, toComplete), noFileComp
	}
}

// completeWithFunc returns a cobra completion over a list computed per call,
// so a package list reflects the spec as it is now.
func completeWithFunc(values func() []string) func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
	return func(_ *cobra.Command, _ []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		return filterPrefix(values(), toComplete), noFileComp
	}
}

// filterPrefix keeps the values that extend what the user has typed. With
// nothing typed every value is a candidate, and the shell shows them all.
func filterPrefix(values []string, toComplete string) []string {
	if toComplete == "" {
		return values
	}

	out := make([]string, 0, len(values))

	for _, value := range values {
		if strings.HasPrefix(value, toComplete) {
			out = append(out, value)
		}
	}

	return out
}

// registerFlagCompletion attaches a completion to a flag when that flag
// exists and takes a value.
func registerFlagCompletion(cmd *cobra.Command, flag string, values []string) {
	if cmd.Flags().Lookup(flag) == nil {
		return
	}

	_ = cmd.RegisterFlagCompletionFunc(flag, completeWith(values))
}

// registerCompletions wires every completion in the tree. It runs once, from
// the root, so no command can forget to ask for its own.
func (a *app) registerCompletions(root *cobra.Command) {
	for _, cmd := range root.Commands() {
		// --hosts and --except both name hosts, on every command that has
		// them.
		registerFlagCompletion(cmd, "hosts", hostIDs())
		registerFlagCompletion(cmd, "except", hostIDs())
	}

	// The commands whose argument is a package id.
	for _, name := range []string{
		"install", "remove", "restore", "pin", "unpin",
		"enable", "disable", "why", "update",
	} {
		if cmd := findSubcommand(root, name); cmd != nil {
			cmd.ValidArgsFunction = completeWithFunc(a.knownPackages)
		}
	}

	// The commands that filter by component kind.
	for _, name := range []string{"propagate"} {
		if cmd := findSubcommand(root, name); cmd != nil {
			for _, sub := range cmd.Commands() {
				registerFlagCompletion(sub, "kind", componentKinds())
			}
		}
	}

	// `verger why <id> <host>` names a host as well as a package.
	if cmd := findSubcommand(root, "why"); cmd != nil {
		completeSecondArg(cmd, hostIDs())
	}
}

// completeSecondArg completes the second positional argument of a command
// that takes two, leaving the first to the package list.
func completeSecondArg(cmd *cobra.Command, values []string) {
	packages := cmd.ValidArgsFunction
	cmd.ValidArgsFunction = func(c *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		if len(args) == 1 {
			return filterPrefix(values, toComplete), noFileComp
		}

		if packages == nil {
			return nil, noFileComp
		}

		return packages(c, args, toComplete)
	}
}

// findSubcommand returns the named subcommand, or nil. It is spelled out
// rather than `findCommand` so it cannot collide with another worker's test
// helper of that name.
func findSubcommand(root *cobra.Command, name string) *cobra.Command {
	for _, cmd := range root.Commands() {
		if cmd.Name() == name {
			return cmd
		}
	}

	return nil
}
