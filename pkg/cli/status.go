package cli

import (
	"cmp"
	"context"
	"fmt"
	"slices"

	"github.com/spf13/cobra"

	"github.com/odiumuniverse/verger/pkg/receipt"
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

	doc, err := a.statusCells(paths, outdatedOnly)
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

// statusCells merges receipts and lock cells into the stable matrix.
func (a *app) statusCells(paths scopePaths, outdatedOnly bool) (statusDoc, error) {
	receipts := receipt.NewStore(paths.receiptsDir)

	list, err := receipts.List()
	if err != nil {
		return statusDoc{}, err
	}

	lockDoc, err := loadLock(paths.lockPath)
	if err != nil {
		return statusDoc{}, err
	}

	doc := statusDoc{Home: paths.root, Cells: []cellDoc{}}

	for _, record := range list {
		cell := cellDoc{
			Package: record.Package, Host: record.Host, Scope: record.Scope,
			Status: string(statusCurrent), Version: record.Version, Strategy: record.Strategy,
			Level: hostMaturity(record.Host),
		}

		if lockCell, ok := lockDoc.Cell(record.Package, record.Host, record.Scope); ok && lockCell.Version != record.Version {
			cell.Status = string(statusSkew)
			cell.Notes = []string{"lock has " + lockCell.Version}
		}

		doc.Cells = append(doc.Cells, cell)
	}

	for _, lockCell := range lockDoc.Cells {
		if _, ok := findCell(list, lockCell.Package, lockCell.Host, lockCell.Scope); ok {
			continue
		}

		doc.Cells = append(doc.Cells, cellDoc{
			Package: lockCell.Package, Host: lockCell.Host, Scope: lockCell.Scope,
			Status: string(statusMissing), Version: lockCell.Version, Strategy: string(lockCell.Strategy),
			Level: hostMaturity(lockCell.Host),
			Notes: []string{"lock cell without a receipt"},
		})
	}

	slices.SortFunc(doc.Cells, func(left, right cellDoc) int {
		return cmp.Or(
			cmp.Compare(left.Package, right.Package),
			cmp.Compare(left.Host, right.Host),
			cmp.Compare(left.Scope, right.Scope),
		)
	})

	if outdatedOnly {
		doc.Cells = slices.DeleteFunc(doc.Cells, func(cell cellDoc) bool {
			return cell.Status != string(statusSkew) && cell.Status != string(statusMissing)
		})

		if doc.Cells == nil {
			doc.Cells = []cellDoc{}
		}
	}

	return doc, nil
}

// findCell locates one receipt by key.
func findCell(list []receipt.Receipt, pkg, hostID, scope string) (receipt.Receipt, bool) {
	for _, record := range list {
		if record.Package == pkg && record.Host == hostID && record.Scope == scope {
			return record, true
		}
	}

	return receipt.Receipt{}, false
}

// Cell status and severity values reused by the CLI documents.
const (
	statusCurrent = "current"
	statusMissing = "missing"
	statusSkew    = "skew"

	statePlanned = "planned"

	severityOK      = "ok"
	severityWarning = "warning"
	severityError   = "error"
)
