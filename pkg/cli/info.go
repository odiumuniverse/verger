package cli

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"os"
	"slices"

	"github.com/spf13/cobra"

	"github.com/odiumuniverse/verger/pkg/spec"
	"github.com/odiumuniverse/verger/pkg/verger"
)

// orNone prints a field the matrix left empty, so the text form says "none"
// rather than ending a line on a stray space - a reader cannot tell an absent
// fact from a truncated one.
func orNone(value string) string {
	if value == "" {
		return "none"
	}

	return value
}

// schemaInfo names the `verger info` document.
const schemaInfo = "verger.info"

// infoHost is one host's row in the matrix `info` prints. It is a read-only
// projection of a status cell: what is on that host, at which version, by which
// strategy, and what the last delivery had to say about it. `info` adds no
// field of its own - if a fact is not in the cell, `info` does not invent it.
type infoHost struct {
	Host     string `json:"host"`
	Version  string `json:"version,omitempty"`
	Strategy string `json:"strategy,omitempty"`
	Status   string `json:"status,omitempty"`
	// Detail is the exact internal code behind Status, the pair every other
	// document in this CLI already carries.
	Detail string   `json:"detail,omitempty"`
	Kind   string   `json:"kind,omitempty"`
	Level  string   `json:"level,omitempty"`
	Notes  []string `json:"notes,omitempty"`
}

// infoDoc is what `verger info` prints for one package: the spec's own
// declaration (source, pin, disabled) and, per host, the delivered state.
//
// The two halves answer two different questions and are kept apart on
// purpose. `Declared` is what the user wrote in the spec - intent, before
// anything was delivered. `Hosts` is what the receipt says is on disk - fact,
// after. `npm info` answers both in one place because its reader expects that;
// verger's cells are per host, so a single version cannot stand for the row.
// A package pinned at 1.2.3 and delivered at 1.0.0 on one host and 1.2.3 on
// another is a skew, and printing one number would hide the very fact the user
// ran `info` to find.
type infoDoc struct {
	Schema schemaRef `json:"schema"`

	Package string `json:"package"`
	Scope   string `json:"scope"`

	// Source is the ref the package was declared with, and Channel the track it
	// follows when the ref does not pin one.
	Source  string `json:"source,omitempty"`
	Channel string `json:"channel,omitempty"`

	// Pinned is the spec's own `version` pin, distinct from the version a host
	// carries: the pin is the ask, the host version is the answer.
	Pinned   string `json:"pinned,omitempty"`
	Disabled bool   `json:"disabled,omitempty"`

	// AdoptedFrom names the host a package was taken over from, empty for a
	// package verger installed itself.
	AdoptedFrom string `json:"adopted_from,omitempty"`

	Hosts []infoHost `json:"hosts"`
}

// newInfoCmd builds `verger info`.
func newInfoCmd(a *app) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "info <id>",
		Short: "Show what one package is and where it is installed",
		Args:  usageArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.runInfo(cmd.Context(), args[0])
		},
	}

	cmd.Flags().BoolVar(&a.projectFlag, "project", false, "use the project scope")

	return cmd
}

// runInfo reports one package: the spec's declaration, and the state of every
// host it was delivered to. It reads only - no resolver, no fetch, no delivery,
// no lock write. That is the point of the verb and the reason it is safe to run
// against a home in the middle of anything.
func (a *app) runInfo(ctx context.Context, id string) error {
	client, err := a.open(ctx)
	if err != nil {
		return err
	}

	paths, err := a.paths(client)
	if err != nil {
		return err
	}

	doc, err := a.infoDoc(client, ctx, paths, id)
	if err != nil {
		return err
	}

	if a.jsonOut {
		return a.printJSON(doc)
	}

	return a.printInfo(doc)
}

// infoDoc gathers the document. The spec is the authority on whether the
// package exists at all: a package with no spec entry but cells in the matrix
// is a leftover, and reporting it as a normal package would tell the user their
// spec says something it does not.
func (a *app) infoDoc(client *verger.Client, ctx context.Context, paths verger.Paths, id string) (infoDoc, error) {
	doc := infoDoc{Schema: schemaOf(schemaInfo), Package: id, Scope: string(paths.Scope), Hosts: []infoHost{}}

	specDoc, err := spec.ParseFile(paths.SpecPath)
	if err != nil {
		// A home with no spec at all is the same answer as an id that is not
		// in it: there is no such package here. ParseFile wraps the read error,
		// so errors.Is still reaches os.ErrNotExist.
		if errors.Is(err, os.ErrNotExist) {
			return infoDoc{}, unknownPackage(id)
		}

		return infoDoc{}, err
	}

	entry, found := verger.SpecPackage(specDoc, id)
	if !found {
		return infoDoc{}, unknownPackage(id)
	}

	doc.Source = entry.ID
	doc.Channel = entry.Channel
	doc.Pinned = entry.Version
	doc.Disabled = entry.Disabled
	doc.AdoptedFrom = entry.AdoptedFrom

	matrix, err := client.Status(ctx, verger.StatusOptions{Paths: paths})
	if err != nil {
		return infoDoc{}, err
	}

	for _, cell := range matrix.Cells {
		if cell.Package != id {
			continue
		}

		// The matrix stores the internal code; `info` speaks the same
		// vocabulary as `status`, `report` and `why`, with the exact code
		// beside it. A fourth document inventing a fifth wording would be one
		// more thing for a user to learn.
		word, detail := cellState(cell.Status, cell.Notes)

		doc.Hosts = append(doc.Hosts, infoHost{
			Host:     cell.Host,
			Version:  cell.Version,
			Strategy: cell.Strategy,
			Status:   word,
			Detail:   detail,
			Kind:     cell.Kind,
			Level:    cell.Level,
			Notes:    cell.Notes,
		})
	}

	// The matrix is built by merge, so its order is not the user's. Sorted, the
	// document is stable across runs and a golden can pin it.
	slices.SortFunc(doc.Hosts, func(x, y infoHost) int { return cmp.Compare(x.Host, y.Host) })

	return doc, nil
}

// unknownPackage is what `info` says about an id the spec does not declare.
// It is a usage error - the invocation named a package and what it named is
// not there - so it exits 2, and it carries the way out, because "not in the
// spec" alone leaves the user guessing between a typo and a missing entry.
func unknownPackage(id string) error {
	return &verger.UsageError{
		Cause: fmt.Errorf("package %q is not in the spec: check the id, or add it with `verger install %s`", id, id),
	}
}

// printInfo renders the document as text.
func (a *app) printInfo(doc infoDoc) error {
	_, _ = fmt.Fprintf(a.out, "%s (%s)\n", doc.Package, doc.Scope)

	if doc.Source != "" {
		_, _ = fmt.Fprintf(a.out, "  source: %s", doc.Source)

		if doc.Channel != "" {
			_, _ = fmt.Fprintf(a.out, " channel %s", doc.Channel)
		}

		_, _ = fmt.Fprintln(a.out)
	}

	if doc.Pinned != "" {
		_, _ = fmt.Fprintf(a.out, "  pinned: %s\n", doc.Pinned)
	}

	if doc.Disabled {
		_, _ = fmt.Fprintln(a.out, "  disabled: yes")
	}

	if doc.AdoptedFrom != "" {
		_, _ = fmt.Fprintf(a.out, "  adopted from: %s\n", doc.AdoptedFrom)
	}

	if len(doc.Hosts) == 0 {
		_, _ = fmt.Fprintln(a.out, "  not installed on any host")

		return nil
	}

	for _, host := range doc.Hosts {
		_, _ = fmt.Fprintf(a.out, "  %s: %s", host.Host, orNone(host.Status))

		if host.Version != "" {
			_, _ = fmt.Fprintf(a.out, " (%s, %s)", host.Version, orNone(host.Strategy))
		}

		_, _ = fmt.Fprintln(a.out)

		for _, note := range host.Notes {
			_, _ = fmt.Fprintf(a.out, "    note: %s\n", note)
		}
	}

	return nil
}
