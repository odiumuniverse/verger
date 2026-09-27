package cli

import (
	"context"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/odiumuniverse/verger/pkg/manifest"
)

// newLintCmd builds `verger lint` (D16, for authors).
func newLintCmd(a *app) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "lint <path>",
		Short: "Validate a package payload for authors",
		Args:  usageArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.runLint(args[0])
		},
	}

	return cmd
}

// lintDoc is the stable JSON shape of one lint run.
type lintDoc struct {
	Root       string   `json:"root"`
	Package    string   `json:"package,omitempty"`
	Version    string   `json:"version,omitempty"`
	Formats    []string `json:"formats"`
	Components int      `json:"components"`
	MCP        int      `json:"mcp"`
	Hooks      int      `json:"hooks"`
	Warnings   []string `json:"warnings,omitempty"`
}

// runLint parses one local payload and reports its shape.
func (a *app) runLint(raw string) error {
	client, err := a.open(context.Background())
	if err != nil {
		return err
	}

	ref, err := parseFetchRef(raw)
	if err != nil {
		return &UsageError{Cause: err}
	}

	if ref.Path == "" {
		return &UsageError{Cause: fmt.Errorf("lint needs a local path, got %q", raw)}
	}

	info, err := os.Stat(ref.Path)
	if err != nil {
		return err
	}

	if !info.IsDir() {
		return &UsageError{Cause: fmt.Errorf("%s is not a directory", ref.Path)}
	}

	_ = client

	pkg, err := manifest.ParseAny(ref.Path)
	if err != nil {
		return err
	}

	doc := lintDoc{
		Root:       ref.Path,
		Package:    idString(pkg),
		Version:    pkg.Version,
		Components: len(pkg.Components),
		MCP:        len(pkg.MCP),
		Hooks:      len(pkg.Hooks),
		Warnings:   pkg.Warnings,
	}

	for _, format := range manifest.Detect(ref.Path) {
		doc.Formats = append(doc.Formats, string(format))
	}

	if len(doc.Formats) == 0 {
		return fmt.Errorf("%s: no package manifest detected", ref.Path)
	}

	if a.jsonOut {
		return a.printJSON(doc)
	}

	_, _ = fmt.Fprintf(a.out, "%s %s: formats=%d components=%d mcp=%d hooks=%d\n",
		doc.Package, doc.Version, len(doc.Formats), doc.Components, doc.MCP, doc.Hooks)

	for _, warning := range doc.Warnings {
		_, _ = fmt.Fprintf(a.out, "warning: %s\n", warning)
	}

	return nil
}
