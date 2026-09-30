package cli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"

	"github.com/odiumuniverse/verger/pkg/apply"
	"github.com/odiumuniverse/verger/pkg/consent"
	"github.com/odiumuniverse/verger/pkg/host"
	"github.com/odiumuniverse/verger/pkg/lock"
	"github.com/odiumuniverse/verger/pkg/manifest"
	"github.com/odiumuniverse/verger/pkg/pack"
	"github.com/odiumuniverse/verger/pkg/secret"
	"github.com/odiumuniverse/verger/pkg/source"
	"github.com/odiumuniverse/verger/pkg/spec"
	"github.com/odiumuniverse/verger/pkg/verger"
)

// open opens the facade once per run.
func (a *app) open(ctx context.Context) (*verger.Client, error) {
	if a.client != nil {
		return a.client, nil
	}

	opts := slices.Clone(a.opts.openOpts)

	if a.homeFlag != "" {
		opts = append(opts, verger.WithHome(a.homeFlag))
	}

	if a.logger.Log() != nil {
		opts = append(opts, verger.WithLogger(a.logger))
	}

	client, err := verger.Open(ctx, opts...)
	if err != nil {
		return nil, err
	}

	a.client = client

	return client, nil
}

// paths resolves the state paths of the requested scope through the facade
// (DESIGN §9.1): project scope lives in the repository (./verger.toml,
// ./verger.lock, ./.verger/state 0700 — OQ-T1.12.2), the user scope in the
// resolved home.
func (a *app) paths(client *verger.Client) (verger.Paths, error) {
	scope := verger.User
	if a.projectFlag {
		scope = verger.Project
	}

	return client.Paths(scope, "")
}

// hosts returns the registered adapters of one open client. The list is built
// by the facade itself (DESIGN §9.1), so the CLI and a second front end see the
// same adapters over the same store, trash, ownership and secrets.
func (a *app) hosts(client *verger.Client) []host.Host {
	return client.Hosts()
}

// loadLock delegates to the facade: the facade resolves lock documents.
func loadLock(path string) (*lock.Lock, error) {
	return verger.LoadLock(path)
}

// hostFilter renders this run's --hosts / --except flags as facade data.
func (a *app) hostFilter() verger.HostFilter {
	return verger.HostFilter{Only: a.hostsFlag, Except: a.exceptFlag}
}

// promptConfirmer asks [y/N] on the TTY.
type promptConfirmer struct {
	a *app
}

// Confirm implements apply.Confirmer.
func (c promptConfirmer) Confirm(_ context.Context, q apply.Question) (bool, error) {
	return c.a.ask(q.Message, false)
}

// requireConfirmation gates one mutating run (§1.3): -y accepts the defaults,
// a dry run is a preview, and a detached stdin without -y stops after the plan
// was printed with the typed ErrConfirmationRequired. Callers invoke it after
// printing the plan and before any write.
func (a *app) requireConfirmation() error {
	if a.yes || a.dryRun || a.tty(os.Stdin) {
		return nil
	}

	return fmt.Errorf("%w: not a terminal; pass -y to accept the defaults", ErrConfirmationRequired)
}

// ask reads one [Y/n] or [y/N] answer; a detached stdin returns
// ErrConfirmationRequired.
func (a *app) ask(message string, defaultYes bool) (bool, error) {
	if !a.interactive() {
		return false, ErrConfirmationRequired
	}

	suffix := "[y/N]"

	if defaultYes {
		suffix = "[Y/n]"
	}

	if _, err := fmt.Fprintf(a.out, "%s %s ", message, suffix); err != nil {
		return false, err
	}

	reader := bufio.NewReader(a.in)

	line, err := reader.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return false, err
	}

	// An empty read is not a "no". The terminal test is a character-device
	// check and /dev/null is a character device, so a run with stdin
	// redirected from it reaches the question, prints it, and gets nothing
	// back. Taking the default there turned a refusal into a silent success:
	// nothing written, exit 0. An unanswered question is a pending question,
	// and it leaves through the typed error so the exit code is 5. A bare
	// Enter - an empty line the user actually typed - is still a deliberate
	// default, and is answered with it.
	if errors.Is(err, io.EOF) && strings.TrimSpace(line) == "" {
		return false, fmt.Errorf("%w: nothing was read from stdin", ErrConfirmationRequired)
	}

	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return true, nil
	case "n", "no":
		return false, nil
	case "":
		return defaultYes, nil
	default:
		return defaultYes, nil
	}
}

// consentStore loads the hooks consent store.
func (a *app) consentStore(client *verger.Client) (*consent.Store, error) {
	return client.ConsentStore()
}

// trustStore loads the project trust store.
func (a *app) trustStore(client *verger.Client) (*consent.TrustStore, error) {
	return client.TrustStore()
}

// requireTrust refuses a project-scoped write when the project spec is absent or
// its hash is untrusted (D11). The verdict is the facade's; the CLI renders it
// with its own error type so the message and exit code stay the same.
func (a *app) requireTrust(client *verger.Client, paths verger.Paths) error {
	err := client.RequireTrust(paths)
	if err == nil {
		return nil
	}

	untrusted, ok := errors.AsType[*verger.UntrustedProjectError](err)
	if !ok {
		return err
	}

	return &TrustError{Path: untrusted.Path, Cause: untrusted.Cause}
}

// loadSpec reads one spec document; a missing file is an empty spec and
// present=false.
// loadSpec reads one spec document. delegates to the facade (DESIGN §9.1): the logic now lives in
// pkg/verger so a second front end gets the same answer.
func loadSpec(path string) (*spec.Spec, bool, error) {
	return verger.LoadSpec(path)
}

// saveSpec writes one spec document, creating the parent directory.
// saveSpec writes one spec document. delegates to the facade (DESIGN §9.1): the logic now lives in
// pkg/verger so a second front end gets the same answer.
func saveSpec(path string, doc *spec.Spec) error {
	return verger.SaveSpec(path, doc)
}

// sourceName delegates to the facade: the facade derives source names.
func sourceName(ref source.Ref) string {
	return verger.SourceName(ref)
}

// addSpecSourceAt records one fetched ref, storing a local path relative to
// the spec's own directory when it lives under it, so a vault cloned to
// another machine still resolves its sources.
func addSpecSourceAt(doc *spec.Spec, ref source.Ref, specDir string) bool {
	return verger.AddSpecSourceAt(doc, ref, specDir)
}

// removeSpecSource drops every source with the name.
// removeSpecSource drops every source with the name. delegates to the facade (DESIGN §9.1): the logic now lives in
// pkg/verger so a second front end gets the same answer.
func removeSpecSource(doc *spec.Spec, name string) int {
	return verger.RemoveSpecSource(doc, name)
}

// buildHostPackage delegates to the facade: a payload a front end fetches
// itself resolves into the same host.Package the library resolves.
func buildHostPackage(client *verger.Client, fetched *source.Fetched, id host.ID, version, scope, projectRoot string) (host.Package, error) {
	return verger.BuildHostPackage(client, fetched, id, version, scope, projectRoot)
}

// idString renders the canonical id of one parsed manifest package.
func idString(meta *manifest.Package) string {
	if meta.ID != "" {
		return meta.ID
	}

	return meta.Name
}

// packInput delegates to the facade: a caller that previews a synth strategy
// renders exactly what the library would.
func packInput(pkg host.Package, root string) (pack.Input, error) {
	return verger.PackInput(pkg, root)
}

// splitID splits one owner/name id.
func splitID(id string) (string, string) {
	if index := strings.LastIndexByte(id, '/'); index >= 0 {
		return id[:index], id[index+1:]
	}

	return "", id
}

// secretsStore returns the facade secrets store.
func (a *app) secretsStore(client *verger.Client) *secret.Store {
	store, ok := client.Secrets().(*secret.Store)
	if !ok {
		return nil
	}

	return store
}

// schemaRef names a document and its version. Every `--json` document carries
// one as its first field (W7-UX-SPEC §2.1), so a consumer can tell what it is
// reading instead of inferring it from the shape.
//
// Within a major the promise is additive: a field may be added and a new
// optional enum value may appear, but nothing is removed, renamed or
// retyped, and a consumer must ignore fields it does not know.
type schemaRef struct {
	Name    string `json:"name"`
	Version int    `json:"version"`
}

// The document names, one per `--json` surface. They are tool-scoped, because
// a status matrix and a lint report are different documents.
const (
	schemaStatus    = "verger.status"
	schemaReport    = "verger.report"
	schemaPlan      = "verger.plan"
	schemaWhy       = "verger.why"
	schemaDoctor    = "verger.doctor"
	schemaLint      = "verger.lint"
	schemaPack      = "verger.pack"
	schemaPropagate = "verger.propagate"
	schemaSource    = "verger.source"
	schemaRemove    = "verger.remove"
	schemaSecret    = "verger.secret"
	schemaSearch    = "verger.search"
)

// schemaOf builds the first field of one document.
func schemaOf(name string) schemaRef {
	return schemaRef{Name: name, Version: 1}
}

// printJSON writes one compact JSON document with a trailing newline.
func (a *app) printJSON(value any) error {
	data, err := marshalJSON(value)
	if err != nil {
		return err
	}

	_, err = fmt.Fprintln(a.out, string(data))

	return err
}

// cellDoc is the stable JSON shape of one applied cell.
//
// `Status` is the user-facing word (W7-UX-SPEC §1.1) and `Detail` keeps the
// exact internal code, because the two are not one-to-one: `missing` and
// `skew` are both `skipped` to a user, and a consumer that has to tell them
// apart must not have to parse a footnote to do it. `removed` is an outcome
// rather than a state, so it keeps its own detail instead of collapsing
// into `delivered`.
type cellDoc struct {
	Package  string   `json:"package"`
	Host     string   `json:"host"`
	Scope    string   `json:"scope"`
	Status   string   `json:"status"`
	Detail   string   `json:"detail,omitempty"`
	Version  string   `json:"version,omitempty"`
	Strategy string   `json:"strategy,omitempty"`
	Level    string   `json:"level,omitempty"` // host maturity (DESIGN §10.3): experimental|beta|stable
	Kind     string   `json:"kind,omitempty"`
	Notes    []string `json:"notes,omitempty"`
	// Backup is where a forced run kept the user's own copy before
	// overwriting it. It is reported rather than printed inline because it
	// is the one path a user needs after the fact and cannot guess.
	Backup string `json:"backup,omitempty"`
	// Restored says the cell came from the lock, not from work on this
	// machine, so its status is `restored` rather than `delivered`.
	Restored bool `json:"restored,omitempty"`
}

// cliCells converts facade cells into the CLI's own document cells, so the
// rendered table and the --json shape stay byte-identical.
func cliCells(cells []verger.Cell) []cellDoc {
	out := make([]cellDoc, 0, len(cells))

	for _, cell := range cells {
		word, detail := cellState(cell.Status, cell.Notes)
		out = append(out, cellDoc{
			Package: cell.Package, Host: cell.Host, Scope: cell.Scope,
			Status: word, Detail: detail,
			Version: cell.Version, Strategy: cell.Strategy,
			Level: cell.Level, Kind: cell.Kind, Notes: cell.Notes,
		})
	}

	return out
}

// reportDoc is the `verger plan` / `verger install` / `verger sync` /
// `verger remove` document. The envelope names it; the cells keep the
// internal vocabulary, because `--json` does not translate (W7-UX-SPEC §1.1
// gives the words to humans, §2.2 defers changing what JSON emits).
type reportDoc struct {
	Schema schemaRef `json:"schema"`
	Cells  []cellDoc `json:"cells"`
	Notes  []string  `json:"notes,omitempty"`
	Home   string    `json:"home,omitempty"`
}

// reportDocFrom converts one apply report.
func reportDocFrom(report apply.Report) reportDoc {
	doc := reportDoc{Schema: schemaOf(schemaReport), Notes: report.Notes}

	for _, cell := range report.Cells {
		word, detail := cellState(string(cell.Status), cell.Notes)
		if cell.Restored {
			word, detail = wordRestored, "restored from lock"
		}

		doc.Cells = append(doc.Cells, cellDoc{
			Package: cell.Package, Host: string(cell.Host), Scope: cell.Scope,
			Status: word, Detail: detail, Version: cell.Version,
			Strategy: string(cell.Strategy), Kind: string(cell.Kind),
			Notes: cell.Notes, Backup: cell.Backup, Restored: cell.Restored,
		})
	}

	if doc.Cells == nil {
		doc.Cells = []cellDoc{}
	}

	return doc
}

// printReport renders one apply report as JSON or a table, then returns an
// *ApplyFailedError when a cell failed or was left in skew.
func (a *app) printReport(report apply.Report, homePath string) error {
	if err := a.renderReport(report, homePath); err != nil {
		return err
	}

	return failedCells(report)
}

// failedCells turns a finished report into the run's verdict.
//
// A `skipped` cell is not a failure: the delivery ran and wrote nothing on
// purpose, because the package had nothing for that host or all of it was
// already there. It prints as `skipped` with its reason and the run exits 0.
//
// The one exception is a package that produced no file anywhere. If every cell
// for a package is `skipped`, the ref resolved to something verger had nothing
// to install, and reporting success would be a lie the user cannot see
// through — nothing is on disk and the exit says it worked. That is reported
// as a usage error (exit 2): the invocation named a package, and what it
// named was empty.
func failedCells(report apply.Report) error {
	var failed []string

	for _, cell := range report.Cells {
		if cell.Status == apply.StatusFailed || cell.Status == apply.StatusSkew {
			failed = append(failed, cell.Package+"@"+string(cell.Host))
		}
	}

	if len(failed) != 0 {
		return &ApplyFailedError{Cells: failed}
	}

	return emptyPackages(report)
}

// emptyPackages reports the packages whose every cell was skipped, which means
// the delivery wrote nothing for them on any host.
func emptyPackages(report apply.Report) error {
	total := map[string]int{}
	skipped := map[string]int{}

	for _, cell := range report.Cells {
		total[cell.Package]++
		if cell.Status == apply.StatusSkipped {
			skipped[cell.Package]++
		}
	}

	empty := make([]string, 0, len(total))

	for pkg, count := range total {
		if skipped[pkg] == count {
			empty = append(empty, pkg)
		}
	}

	if len(empty) == 0 {
		return nil
	}

	slices.Sort(empty)

	return &UsageError{Cause: fmt.Errorf(
		"produced no files for any host: %s; the package is empty, or has nothing for this machine",
		strings.Join(empty, ", "))}
}

// renderReport writes one apply report as JSON or a table.
func (a *app) renderReport(report apply.Report, homePath string) error {
	if a.jsonOut {
		doc := reportDocFrom(report)
		doc.Home = homePath

		return a.printJSON(doc)
	}

	if len(report.Cells) == 0 {
		_, err := fmt.Fprintln(a.out, "nothing to do")

		return err
	}

	notes := newFootnotes()

	_, _ = fmt.Fprintf(a.out, "%-24s %-8s %-8s %-12s %s\n", "PACKAGE", "HOST", "SCOPE", "STATUS", "DETAIL")

	for _, cell := range report.Cells {
		word, detail := cellState(string(cell.Status), cell.Notes)
		if cell.Restored {
			word, detail = wordRestored, "restored from lock"
		}

		_, _ = fmt.Fprintf(a.out, "%-24s %-8s %-8s %-12s %s%s\n",
			cell.Package, cell.Host, cell.Scope, word, strategyPhrase(string(cell.Strategy)), notes.mark(word, detail))

		for _, note := range cell.Notes {
			_, _ = fmt.Fprintf(a.out, "  %s\n", note)
		}

		// A forced run overwrote someone's file. The backup path is the one
		// thing they cannot reconstruct afterwards, so it goes on screen
		// where the cell is, not into a footnote.
		if cell.Backup != "" {
			_, _ = fmt.Fprintf(a.out, "  your previous version is kept at %s\n", cell.Backup)
		}
	}

	notes.write(a.out)

	for _, note := range report.Notes {
		_, _ = fmt.Fprintf(a.out, "note: %s\n", note)
	}

	return nil
}

// printPlan renders the per-host plan table before an apply (rule 2).
func (a *app) printPlan(cells []cellDoc) error {
	if a.jsonOut {
		return nil
	}

	if len(cells) == 0 {
		_, err := fmt.Fprintln(a.out, "plan: nothing to do")

		return err
	}

	_, _ = fmt.Fprintf(a.out, "%-24s %-8s %-8s %-24s %s\n", "PACKAGE", "HOST", "SCOPE", "WILL BE", "VERSION")

	for _, cell := range cells {
		_, _ = fmt.Fprintf(a.out, "%-24s %-8s %-8s %-24s %s\n",
			cell.Package, cell.Host, cell.Scope, strategyPhrase(cell.Strategy), cell.Version)
	}

	return nil
}

// homeRoot returns the verger home root for display.
func homeRoot(client *verger.Client) string {
	return client.Home().Root()
}

// marshalJSON renders one compact, HTML-unescaped document.
func marshalJSON(value any) ([]byte, error) {
	var buf bytes.Buffer

	encoder := json.NewEncoder(&buf)
	encoder.SetEscapeHTML(false)

	if err := encoder.Encode(value); err != nil {
		return nil, err
	}

	return bytes.TrimSuffix(buf.Bytes(), []byte{'\n'}), nil
}

// applyOptionsFacade renders the facade's own apply options for one run: the
// dry-run flag, the clock and the confirmer this CLI would use. Every write
// command goes through it, so `-y` and the TTY prompt mean the same thing to
// the library as they do to the CLI.
func (a *app) applyOptionsFacade() verger.ApplyOptions {
	opts := verger.ApplyOptions{DryRun: a.dryRun, Now: a.now, Force: a.force}

	if a.yes {
		// -y accepts defaults and never resolves a destructive conflict
		// (rule 14): conflicts are declined, not confirmed.
		opts.Confirm = verger.YesConfirmer()
	} else if a.interactive() {
		opts.Confirm = promptConfirmer{a: a}
	}

	return opts
}
