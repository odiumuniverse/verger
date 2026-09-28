package cli

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/odiumuniverse/verger/pkg/verger"
)

// statusDoc is the stable JSON document of `verger status`.
type statusDoc struct {
	Home  string    `json:"home"`
	Cells []cellDoc `json:"cells"`
}

// newStatusCmd builds `verger status`.
func newStatusCmd(a *app) *cobra.Command {
	var outdatedOnly bool

	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show the package × host matrix",
		Args:  usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.runStatus(cmd.Context(), outdatedOnly)
		},
	}

	cmd.Flags().BoolVar(&outdatedOnly, "outdated-only", false, "only show skew and missing cells")
	cmd.Flags().BoolVar(&a.projectFlag, "project", false, "use the project scope")

	return cmd
}

// runStatus builds the matrix from receipts and the lock; it writes nothing.
func (a *app) runStatus(ctx context.Context, outdatedOnly bool) error {
	client, err := a.open(ctx)
	if err != nil {
		return err
	}

	paths, err := a.paths(client)
	if err != nil {
		return err
	}

	doc, err := a.statusCells(client, ctx, paths, outdatedOnly)
	if err != nil {
		return err
	}

	if a.jsonOut {
		return a.printJSON(doc)
	}

	if len(doc.Cells) == 0 {
		_, err := fmt.Fprintln(a.out, "no cells")

		return err
	}

	_, _ = fmt.Fprintf(a.out, "%-24s %-8s %-12s %-8s %-10s %-10s %s\n",
		"PACKAGE", "HOST", "LEVEL", "SCOPE", "STATUS", "STRATEGY", "VERSION")

	for _, cell := range doc.Cells {
		_, _ = fmt.Fprintf(a.out, "%-24s %-8s %-12s %-8s %-10s %-10s %s\n",
			cell.Package, cell.Host, cell.Level, cell.Scope, cell.Status, cell.Strategy, cell.Version)
	}

	return nil
}

// statusCells merges receipts and lock cells into the stable matrix through the
// facade (DESIGN §9.1); the CLI only converts the document into its own JSON
// shape and its own level column.
func (a *app) statusCells(client *verger.Client, ctx context.Context, paths verger.Paths, outdatedOnly bool) (statusDoc, error) {
	doc, err := client.Status(ctx, verger.StatusOptions{Paths: paths, OutdatedOnly: outdatedOnly})
	if err != nil {
		return statusDoc{}, err
	}

	return statusDoc{Home: doc.Home, Cells: cliCells(doc.Cells)}, nil
}

// Cell status and severity values reused by the CLI documents.
const (
	severityOK      = "ok"
	severityWarning = "warning"
	severityError   = "error"
)
