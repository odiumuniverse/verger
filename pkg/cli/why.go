package cli

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"
)

// whyDoc is the stable JSON document of `verger why`.
type whyDoc struct {
	Schema  schemaRef `json:"schema"`
	Package string    `json:"package"`
	Host    string    `json:"host"`
	Scope   string    `json:"scope"`
	// Status is the user-facing word and Detail the exact internal code, the
	// same pair every other document carries: `why` exists to explain a
	// cell, and a cell explained in a vocabulary nothing else uses is not an
	// explanation a reader can match up with `status`.
	Status   string   `json:"status"`
	Detail   string   `json:"detail,omitempty"`
	Version  string   `json:"version,omitempty"`
	Strategy string   `json:"strategy,omitempty"`
	Reasons  []string `json:"reasons"`
	Blockers []string `json:"blockers"`
}

// newWhyCmd builds `verger why`.
func newWhyCmd(a *app) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "why <id> <host>",
		Short: "Explain one cell: strategy, version, blockers",
		Args:  usageArgs(cobra.ExactArgs(2)),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.runWhy(cmd.Context(), args[0], args[1])
		},
	}

	cmd.Flags().BoolVar(&a.projectFlag, "project", false, "use the project scope")

	return cmd
}

// runWhy builds the explanation for one package on one host through the facade
// (DESIGN §9.1); the CLI only renders what the facade reports.
func (a *app) runWhy(ctx context.Context, id, hostID string) error {
	client, err := a.open(ctx)
	if err != nil {
		return err
	}

	paths, err := a.paths(client)
	if err != nil {
		return err
	}

	explained, err := client.Why(ctx, paths, id, hostID)
	if err != nil {
		return err
	}

	word, detail := cellState(explained.Status, explained.Blockers)

	return a.printWhy(whyDoc{
		Schema:  schemaOf(schemaWhy),
		Package: explained.Package, Host: explained.Host, Scope: explained.Scope,
		Status: word, Detail: detail,
		Version: explained.Version, Strategy: explained.Strategy,
		Reasons: explained.Reasons, Blockers: explained.Blockers,
	})
}

// printWhy renders the explanation as JSON or text.
func (a *app) printWhy(doc whyDoc) error {
	if a.jsonOut {
		return a.printJSON(doc)
	}

	_, _ = fmt.Fprintf(a.out, "%s on %s: %s", doc.Package, doc.Host, doc.Status)

	if doc.Version != "" {
		_, _ = fmt.Fprintf(a.out, " (%s, %s)", doc.Version, doc.Strategy)
	}

	_, _ = fmt.Fprintln(a.out)

	for _, reason := range doc.Reasons {
		_, _ = fmt.Fprintf(a.out, "  reason: %s\n", reason)
	}

	for _, blocker := range doc.Blockers {
		_, _ = fmt.Fprintf(a.out, "  blocker: %s\n", blocker)
	}

	return nil
}
