package cli

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/odiumuniverse/verger/pkg/verger"
)

// schemaSearch is declared with the other document names in wiring.go.

// searchDoc is what `verger search` prints.
//
// It adds one field to the facade's result and nothing else: the schema
// envelope every `--json` document opens with, so a consumer can tell what it
// is reading instead of inferring it from the shape. The rows stay the
// facade's own types, so the table a human reads and the document a script
// parses are rendered from one struct and cannot drift apart.
//
// Neither list is omitted and neither may be null. A consumer ranging over the
// matches must not have to tell "nothing matched" from "field absent", and a
// consumer ranging over the skipped sources must not have to tell "no source
// was unreadable" from the same thing.
type searchDoc struct {
	Schema  schemaRef              `json:"schema"`
	Query   string                 `json:"query"`
	Matches []verger.SearchMatch   `json:"matches"`
	Skipped []verger.SearchSkipped `json:"skipped"`
}

// newSearchCmd builds `verger search`.
func newSearchCmd(a *app) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "search <query>",
		Short: "Find packages by name, and say which of them are already installed",
		Long: "Find packages verger already knows about: the spec, the lock, the receipts\n" +
			"and the offers of local sources. It never reaches the network - a source\n" +
			"that cannot be read without one is listed under SKIPPED, so the answer is\n" +
			"never silently short of what you declared.",
		Args: usageArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.runSearch(cmd.Context(), args[0])
		},
	}

	cmd.Flags().BoolVar(&a.projectFlag, "project", false, "use the project scope")
	cmd.Flags().StringSliceVar(&a.hostsFlag, "hosts", nil, "only these hosts (comma separated)")
	cmd.Flags().StringSliceVar(&a.exceptFlag, "except", nil, "exclude these hosts")

	return cmd
}

// runSearch answers one query. It reads only - no resolver, no fetch, no
// delivery, no lock write. That is the point of the verb, and it is what makes
// it safe to run against a home in the middle of anything.
func (a *app) runSearch(ctx context.Context, query string) error {
	client, err := a.open(ctx)
	if err != nil {
		return err
	}

	paths, err := a.paths(client)
	if err != nil {
		return err
	}

	res, err := client.Search(ctx, verger.SearchOptions{
		Paths: paths,
		Query: query,
		Hosts: a.hostFilter(),
	})
	if err != nil {
		return err
	}

	doc := searchDocFrom(res)

	if a.jsonOut {
		return a.printJSON(doc)
	}

	return a.printSearch(doc)
}

// searchDocFrom wraps one result in the CLI document.
func searchDocFrom(res verger.SearchResult) searchDoc {
	doc := searchDoc{
		Schema:  schemaOf(schemaSearch),
		Query:   res.Query,
		Matches: res.Matches,
		Skipped: res.Skipped,
	}

	if doc.Matches == nil {
		doc.Matches = []verger.SearchMatch{}
	}

	if doc.Skipped == nil {
		doc.Skipped = []verger.SearchSkipped{}
	}

	return doc
}

// printSearch renders the document as text.
//
// The state columns are the whole reason the verb exists, so none of them is
// dropped when it is false: a blank under INSTALLED and a blank under SPEC are
// different facts, and only one of them means "you already have this". HOST
// answers the question INSTALLED raises next - yes, but where.
func (a *app) printSearch(doc searchDoc) error {
	if len(doc.Matches) == 0 {
		_, _ = fmt.Fprintf(a.out, "no matches for %q\n", doc.Query)

		return a.printSkipped(doc)
	}

	_, _ = fmt.Fprintf(a.out, "%-32s %-8s %-8s %-10s %-16s %s\n",
		"PACKAGE", "SPEC", "LOCK", "INSTALLED", "OFFERED BY", "HOST")

	for _, m := range doc.Matches {
		_, _ = fmt.Fprintf(a.out, "%-32s %-8s %-8s %-10s %-16s %s\n",
			m.ID, yesNo(m.InSpec), yesNo(m.InLock), yesNo(m.Installed),
			orNone(m.OfferedBy), orNone(m.Source))
	}

	return a.printSkipped(doc)
}

// printSkipped lists the declared sources this run could not read. It is on
// screen rather than in a log because the user is looking at a list: a package
// missing from a list they did not know was incomplete is the failure the row
// exists to prevent.
func (a *app) printSkipped(doc searchDoc) error {
	for _, s := range doc.Skipped {
		_, _ = fmt.Fprintf(a.out, "skipped %s: %s\n", s.Source, s.Reason)
	}

	return nil
}

// yesNo renders a state column. The words are the whole point: a table of
// blanks and dashes is a table the reader has to learn.
func yesNo(value bool) string {
	if value {
		return "yes"
	}

	return "no"
}
