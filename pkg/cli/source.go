package cli

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/spf13/cobra"
)

// newSourceCmd builds `verger source`.
func newSourceCmd(a *app) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "source",
		Short: "Manage spec sources (order = priority)",
		Args:  usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}

	addCmd := &cobra.Command{
		Use:   "add <ref>",
		Short: "Add a named source to the spec",
		Args:  usageArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.runSourceAdd(cmd.Context(), args[0])
		},
	}

	rmCmd := &cobra.Command{
		Use:   "rm <name>",
		Short: "Remove a source by name",
		Args:  usageArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.runSourceRm(cmd.Context(), args[0])
		},
	}

	listCmd := &cobra.Command{
		Use:   "list",
		Short: "List the declared sources in priority order",
		Args:  usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.runSourceList(cmd.Context())
		},
	}

	for _, sub := range []*cobra.Command{addCmd, rmCmd} {
		addDryRunFlag(sub, a)
		sub.Flags().BoolVar(&a.projectFlag, "project", false, "use the project scope")
	}

	listCmd.Flags().BoolVar(&a.projectFlag, "project", false, "use the project scope")

	cmd.AddCommand(addCmd, rmCmd, listCmd)

	return cmd
}

// sourceDoc is the stable JSON shape of one source.
type sourceDoc struct {
	Schema schemaRef `json:"schema"`
	Name   string    `json:"name"`
	URL    string    `json:"url"`
}

// sourceListDoc is the `source list --json` document: the envelope names the
// document, the entries name the sources.
type sourceListDoc struct {
	Schema  schemaRef   `json:"schema"`
	Sources []sourceDoc `json:"sources"`
}

// runSourceAdd appends one source to the spec.
func (a *app) runSourceAdd(ctx context.Context, raw string) error {
	ref, err := parseFetchRef(raw)
	if err != nil {
		return &UsageError{Cause: err}
	}

	client, err := a.open(ctx)
	if err != nil {
		return err
	}

	paths, err := a.paths(client)
	if err != nil {
		return err
	}

	if err := a.requireTrust(client, paths); err != nil {
		return err
	}

	doc, _, err := loadSpec(paths.SpecPath)
	if err != nil {
		return err
	}

	addSpecSourceAt(doc, ref, filepath.Dir(paths.SpecPath))

	if a.dryRun {
		_, err := fmt.Fprintf(a.out, "source: would add %s\n", raw)

		return err
	}

	if err := client.Home().Ensure(); err != nil {
		return err
	}

	if err := saveSpec(paths.SpecPath, doc); err != nil {
		return err
	}

	_, err = fmt.Fprintf(a.out, "added source %s\n", sourceName(ref))

	return err
}

// runSourceRm removes one source by name.
func (a *app) runSourceRm(ctx context.Context, name string) error {
	client, err := a.open(ctx)
	if err != nil {
		return err
	}

	paths, err := a.paths(client)
	if err != nil {
		return err
	}

	if err := a.requireTrust(client, paths); err != nil {
		return err
	}

	doc, _, err := loadSpec(paths.SpecPath)
	if err != nil {
		return err
	}

	if removeSpecSource(doc, name) == 0 {
		return &UsageError{Cause: fmt.Errorf("source %q is not declared", name)}
	}

	if a.dryRun {
		_, err := fmt.Fprintf(a.out, "source: would remove %s\n", name)

		return err
	}

	if err := saveSpec(paths.SpecPath, doc); err != nil {
		return err
	}

	_, err = fmt.Fprintf(a.out, "removed source %s\n", name)

	return err
}

// runSourceList prints the declared sources in priority order.
func (a *app) runSourceList(ctx context.Context) error {
	client, err := a.open(ctx)
	if err != nil {
		return err
	}

	paths, err := a.paths(client)
	if err != nil {
		return err
	}

	doc, _, err := loadSpec(paths.SpecPath)
	if err != nil {
		return err
	}

	sources := make([]sourceDoc, 0, len(doc.Sources))

	for _, src := range doc.Sources {
		sources = append(sources, sourceDoc{Name: src.Name, URL: src.URL})
	}

	if a.jsonOut {
		return a.printJSON(sourceListDoc{Schema: schemaOf(schemaSource), Sources: sources})
	}

	for _, src := range sources {
		if _, err := fmt.Fprintf(a.out, "%s %s\n", src.Name, src.URL); err != nil {
			return err
		}
	}

	return nil
}
