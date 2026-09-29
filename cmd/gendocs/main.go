// Command gendocs writes the man pages for verger from the real cobra command
// tree, so the pages and the binary cannot disagree: there is one source of
// truth for the flags, and this reads it rather than restating it.
//
// It writes roff only. Markdown is a second format for a second surface, and
// verger ships man pages; a generated .md nobody installs is a file that goes
// stale unnoticed.
//
// Usage:
//
//	gendocs [-out DIR] [-name NAME] [-short SHORT]
package main

import (
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra/doc"

	"github.com/odiumuniverse/verger/pkg/cli"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "gendocs:", err)
		os.Exit(1)
	}
}

func run() error {
	out := flag.String("out", "dist/man", "directory the man pages are written to")
	name := flag.String("name", "verger", "the command name the pages document")
	version := flag.String("version", "dev", "the version the pages document")
	date := flag.String("date", time.Now().Format("Jan 2006"), "release date for the page header")

	flag.Parse()

	// The real root command, built by the same constructor the binary uses, so
	// a flag that exists is documented and one that does not is not.
	root := cli.NewRootCmd(cli.Options{Version: *version})
	root.DisableAutoGenTag = true

	// 0750 rather than 0755: the generated man pages are read by the user who
	// built them, not by anyone else on the machine.
	if err := os.MkdirAll(*out, 0o750); err != nil {
		return fmt.Errorf("create %s: %w", *out, err)
	}

	// Title is the bare command name: the template appends the section, and
	// putting "(1)" in here renders as "verger(1)(1)".
	when, err := time.Parse("Jan 2006", *date)
	if err != nil {
		return fmt.Errorf("parse -date %q: %w", *date, err)
	}

	header := &doc.GenManHeader{
		Title:   *name,
		Section: "1",
		Source:  *name + " " + *version,
		Date:    &when,
	}

	// GenManTree, not GenManTreeCustom with a hand-rolled header: the header
	// is where a generator quietly stops matching the tool.
	if err := doc.GenManTree(root, header, *out); err != nil {
		return fmt.Errorf("write the man pages: %w", err)
	}

	return nil
}
