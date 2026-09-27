package cli

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/odiumuniverse/verger/pkg/host"
	"github.com/odiumuniverse/verger/pkg/pack"
)

// newPackCmd builds `verger pack` (D16, for authors).
func newPackCmd(a *app) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "pack <path>",
		Short: "Render a package into a chimera directory in the store",
		Args:  usageArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.runPack(cmd.Context(), args[0])
		},
	}

	addWriteFlags(cmd, a)

	return cmd
}

// packResultDoc is the stable JSON shape of one pack run.
type packResultDoc struct {
	Dir     string `json:"dir"`
	Package string `json:"package"`
	Version string `json:"version"`
}

// runPack renders one local package into the store synth dir.
func (a *app) runPack(ctx context.Context, raw string) error {
	client, err := a.open(ctx)
	if err != nil {
		return err
	}

	ref, err := parseFetchRef(raw)
	if err != nil {
		return &UsageError{Cause: err}
	}

	fetched, err := fetchRef(ctx, client, ref)
	if err != nil {
		return err
	}

	defer func() { _ = fetched.Cleanup() }()

	pkg, err := buildHostPackage(client, fetched, host.Claude, fetched.Package.Version, "user", "")
	if err != nil {
		return err
	}

	input, err := packInput(pkg, fetched.Root)
	if err != nil {
		return err
	}

	if a.dryRun {
		_, err := fmt.Fprintf(a.out, "pack: would render %s %s\n", pkg.ID, pkg.Version)

		return err
	}

	result, err := pack.Write(ctx, client.Store(), input)
	if err != nil {
		return err
	}

	doc := packResultDoc{Dir: result.Dir, Package: pkg.ID, Version: pkg.Version}

	if a.jsonOut {
		return a.printJSON(doc)
	}

	_, err = fmt.Fprintln(a.out, doc.Dir)

	return err
}
