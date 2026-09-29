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
	"path/filepath"
	"strings"
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

	if err := normalisePages(*out, when, *name, *version); err != nil {
		return err
	}

	return nil
}

// mandocDate is the .TH date form mandoc can parse. It is deliberately not a
// spelled-out month: mandoc rejects both "Sep 2026" and "September 2026" with
// "cannot parse date, using it verbatim". An ISO date is the one spelling that
// is unambiguous and accepted, and the release smoke step lints every page
// with mandoc, so a page mandoc cannot read the date off is a page the release
// would flag.
const mandocDate = "2006-01-02"

// manualName is the MANUAL field of the header: the man section a reader sees
// at the bottom of the page. go-md2man leaves it empty, which renders as a
// blank line there.
const manualName = "User Commands"

// normalisePages repairs the header and the whitespace on every generated
// page. The pages are produced by cobra, which renders its own header through
// go-md2man, and that path emits a malformed .TH: the date field is left as
// "Sep 2026" 2026" — a duplicated token that mandoc reports as "skipping
// excess arguments" — and the title keeps the lower-case command name, which
// mandoc reports as a STYLE warning. Neither is configurable from here, and
// the release smoke step lints every page, so gendocs writes the header it
// means to write.
//
// The tab before each flag description is repaired the same way: roff reads a
// leading tab as a filled-text break and mandoc warns about it. Two spaces
// break identically, so the rendered page is unchanged.
func normalisePages(dir string, when time.Time, name, version string) error {
	pages, err := filepath.Glob(filepath.Join(dir, "*.1"))
	if err != nil {
		return fmt.Errorf("list the generated pages: %w", err)
	}

	if len(pages) == 0 {
		return fmt.Errorf("no man pages were written to %s", dir)
	}

	// A well-formed mdoc header takes exactly five arguments after .TH, and
	// the whole line is rebuilt rather than patched: the upstream line has an
	// extra token, so correcting one field in place would leave the rest of the
	// damage in place. The title is upper-cased because that is what a whatis
	// parser matches on.
	header := strings.Join([]string{
		".TH",
		`"` + strings.ToUpper(name) + `"`,
		`"1"`,
		`"` + when.Format(mandocDate) + `"`,
		`"` + name + " " + version + `"`,
		`"` + manualName + `"`,
	}, " ")

	for _, page := range pages {
		data, err := os.ReadFile(page) //nolint:gosec // the glob is over gendocs' own output directory
		if err != nil {
			return fmt.Errorf("read %s: %w", page, err)
		}

		// Keep the mode the generator already chose. These pages end up in a
		// tarball and in a shared install prefix, so they have to stay as
		// readable as the generator made them; rewriting a file is not a
		// reason to change who may read it.
		info, err := os.Stat(page)
		if err != nil {
			return fmt.Errorf("stat %s: %w", page, err)
		}

		lines := strings.Split(string(data), "\n")

		for i, line := range lines {
			if strings.HasPrefix(line, ".TH ") {
				lines[i] = header

				continue
			}

			lines[i] = strings.TrimRight(strings.ReplaceAll(line, "\t", "  "), " ")
		}

		// G703 is a false positive: `page` comes from globbing gendocs' own
		// output directory, which is the -out flag the caller just created.
		//nolint:gosec // the path is inside the output dir gendocs just wrote
		if err := os.WriteFile(page, []byte(strings.Join(lines, "\n")), info.Mode().Perm()); err != nil {
			return fmt.Errorf("rewrite %s: %w", page, err)
		}
	}

	return nil
}
