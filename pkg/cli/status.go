package cli

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/odiumuniverse/verger/pkg/verger"
)

// statusDoc is the stable JSON document of `verger status`.
type statusDoc struct {
	Schema schemaRef `json:"schema"`
	Home   string    `json:"home"`
	Cells  []cellDoc `json:"cells"`
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

	notes := newFootnotes()

	_, _ = fmt.Fprintf(a.out, "%-24s %-8s %-12s %-8s %-12s %s\n",
		"PACKAGE", "HOST", "LEVEL", "SCOPE", "STATUS", "DETAIL")

	for _, cell := range doc.Cells {
		st := humanState(cell.Status, cell.Detail)
		_, _ = fmt.Fprintf(a.out, "%-24s %-8s %-12s %-8s %-12s %s%s\n",
			cell.Package, cell.Host, cell.Level, cell.Scope, st.word, strategyPhrase(cell.Strategy), notes.mark(cell.Status, cell.Detail))
	}

	notes.write(a.out)

	// Per-host switches (U2): show runtime and other host-level toggles.
	if switches := a.hostSwitches(client, paths); len(switches) > 0 {
		fmt.Fprintln(a.out, "\nper-host switches")
		_, _ = fmt.Fprintf(a.out, "%-8s %-12s %s\n", "HOST", "SWITCH", "VALUE")

		for _, sw := range switches {
			_, _ = fmt.Fprintf(a.out, "%-8s %-12s %v\n", sw.Host, sw.Switch, sw.Value)
		}
	}

	return nil
}

// hostSwitch is one per-host feature toggle shown in `verger status`.
type hostSwitch struct {
	Host   string
	Switch string
	Value  bool
}

// hostSwitches reads the per-host switches from the spec.
func (a *app) hostSwitches(client *verger.Client, paths verger.Paths) []hostSwitch {
	doc, ok, err := loadSpec(paths.SpecPath)
	if err != nil || !ok {
		return nil
	}

	var switches []hostSwitch

	for id, host := range doc.Hosts {
		if host.Runtime != nil {
			switches = append(switches, hostSwitch{Host: id, Switch: "runtime", Value: *host.Runtime})
		}
	}

	return switches
}

// statusCells merges receipts and lock cells into the stable matrix through the
// facade (DESIGN §9.1); the CLI only converts the document into its own JSON
// shape and its own level column.
func (a *app) statusCells(client *verger.Client, ctx context.Context, paths verger.Paths, outdatedOnly bool) (statusDoc, error) {
	doc, err := client.Status(ctx, verger.StatusOptions{Paths: paths, OutdatedOnly: outdatedOnly})
	if err != nil {
		return statusDoc{}, err
	}

	return statusDoc{Schema: schemaOf(schemaStatus), Home: doc.Home, Cells: cliCells(doc.Cells)}, nil
}

// Cell status and severity values reused by the CLI documents.
const (
	// The severity vocabulary is `info | warning | error` (W7-UX-SPEC §3.2).
	// `ok` is gone: a check that passes is not a finding, it is the absence of
	// one. This matches what the sibling tool emits, so one parser reads both.
	severityInfo    = "info"
	severityWarning = "warning"
	severityError   = "error"
)
