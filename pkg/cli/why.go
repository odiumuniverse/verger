package cli

import (
	"context"
	"fmt"
	"slices"

	"github.com/spf13/cobra"

	"github.com/odiumuniverse/verger/pkg/lock"
	"github.com/odiumuniverse/verger/pkg/receipt"
	"github.com/odiumuniverse/verger/pkg/verger"
)

// whyDoc is the stable JSON document of `verger why`.
type whyDoc struct {
	Package  string   `json:"package"`
	Host     string   `json:"host"`
	Scope    string   `json:"scope"`
	Status   string   `json:"status"`
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

// runWhy explains one cell without writing.
func (a *app) runWhy(ctx context.Context, id, hostID string) error {
	client, err := a.open(ctx)
	if err != nil {
		return err
	}

	paths, err := a.paths(client)
	if err != nil {
		return err
	}

	doc := whyDoc{Package: id, Host: hostID, Scope: paths.name, Reasons: []string{}, Blockers: []string{}}

	receipts := receipt.NewStore(paths.receiptsDir)

	list, err := receipts.List()
	if err != nil {
		return err
	}

	record, hasReceipt := findCell(list, id, hostID, paths.name)

	lockDoc, err := loadLock(paths.lockPath)
	if err != nil {
		return err
	}

	lockCell, hasLock := lockDoc.Cell(id, hostID, paths.name)

	whyCell(&doc, record, hasReceipt, lockCell, hasLock)

	doc.Reasons = append(doc.Reasons, a.whySpecReasons(paths, id)...)

	if err := a.appendHooksReason(client, id, &doc); err != nil {
		return err
	}

	slices.Sort(doc.Reasons)

	return a.printWhy(doc)
}

// whyCell fills the receipt/lock state of one explanation.
func whyCell(doc *whyDoc, record receipt.Receipt, hasReceipt bool, lockCell lock.Cell, hasLock bool) {
	switch {
	case hasReceipt:
		doc.Status = statusCurrent
		doc.Version = record.Version
		doc.Strategy = record.Strategy
		doc.Reasons = append(doc.Reasons, fmt.Sprintf("strategy %s from the %s receipt", record.Strategy, record.Scope))

		if record.Version != "" {
			doc.Reasons = append(doc.Reasons, "version "+record.Version)
		}

		if hasLock && lockCell.Version != record.Version {
			doc.Status = statusSkew
			doc.Blockers = append(doc.Blockers, "the lock cell wants "+lockCell.Version)
		}
	case hasLock:
		doc.Status = statusMissing
		doc.Version = lockCell.Version
		doc.Strategy = string(lockCell.Strategy)
		doc.Blockers = append(doc.Blockers, "the lock has a cell but no receipt: run `verger install` or `verger remove`")
	default:
		doc.Status = statusMissing
		doc.Blockers = append(doc.Blockers, "no receipt and no lock cell for this package/host")
	}
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

// whySpecReasons reports the spec entry state of one package.
func (a *app) whySpecReasons(paths scopePaths, id string) []string {
	doc, ok, err := loadSpec(paths.specPath)
	if err != nil || !ok {
		return nil
	}

	for _, entry := range doc.Packages {
		if entry.ID != id {
			continue
		}

		reasons := []string{"spec: desired"}

		if entry.Version != "" {
			reasons = append(reasons, "pinned to "+entry.Version)
		}

		if entry.Disabled {
			reasons = append(reasons, "disabled in the spec")
		}

		if entry.AdoptedFrom != "" {
			reasons = append(reasons, "adopted from "+entry.AdoptedFrom)
		}

		return reasons
	}

	return []string{"spec: not declared"}
}

// appendHooksReason reports the hooks consent state of one package.
func (a *app) appendHooksReason(client *verger.Client, id string, doc *whyDoc) error {
	store, err := a.consentStore(client)
	if err != nil {
		return err
	}

	if record, ok := store.Hooks(id); ok {
		doc.Reasons = append(doc.Reasons, "hooks approved at "+record.Version)

		return nil
	}

	doc.Reasons = append(doc.Reasons, "hooks: no approval recorded")

	return nil
}
