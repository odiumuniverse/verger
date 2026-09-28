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

// loadLock reads a lock document; a missing file is an empty lock.
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

// sourceName derives a stable source name from one ref.
// sourceName derives a stable source name from one ref. delegates to the facade (DESIGN §9.1): the logic now lives in
// pkg/verger so a second front end gets the same answer.
func sourceName(ref source.Ref) string {
	return verger.SourceName(ref)
}

// addSpecSource records one fetched ref as a spec source; an existing source
// with the same URL is kept as-is.
// addSpecSource records one fetched ref as a spec source. delegates to the facade (DESIGN §9.1): the logic now lives in
// pkg/verger so a second front end gets the same answer.
func addSpecSource(doc *spec.Spec, ref source.Ref) bool {
	return verger.AddSpecSource(doc, ref)
}

// removeSpecSource drops every source with the name.
// removeSpecSource drops every source with the name. delegates to the facade (DESIGN §9.1): the logic now lives in
// pkg/verger so a second front end gets the same answer.
func removeSpecSource(doc *spec.Spec, name string) int {
	return verger.RemoveSpecSource(doc, name)
}

// buildHostPackage converts one fetched payload into one host delivery
// package.
func buildHostPackage(client *verger.Client, fetched *source.Fetched, id host.ID, version, scope, projectRoot string) (host.Package, error) {
	meta := fetched.Package

	dataDir, err := client.Store().PackageDataPath(idString(meta), string(id))
	if err != nil {
		return host.Package{}, err
	}

	pkg := host.Package{
		ID:          idString(meta),
		Version:     firstNonEmpty(version, meta.Version),
		Format:      meta.Format,
		Root:        fetched.Root,
		Components:  meta.Components,
		MCP:         meta.MCP,
		Hooks:       meta.Hooks,
		Scope:       scope,
		ProjectRoot: projectRoot,
		DataDir:     dataDir,
	}

	return pkg, nil
}

// idString renders the canonical id of one parsed manifest package.
func idString(meta *manifest.Package) string {
	if meta.ID != "" {
		return meta.ID
	}

	return meta.Name
}

// firstNonEmpty returns the first non-empty value.
func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}

	return ""
}

// packInput builds the chimera input of one host package.
func packInput(pkg host.Package, root string) (pack.Input, error) {
	owner, name := splitID(pkg.ID)
	if owner == "" || name == "" {
		return pack.Input{}, errors.New("the package id needs owner/name")
	}

	return pack.Input{
		ID: pkg.ID, Name: name, Owner: owner, Version: pkg.Version,
		Description: "", License: "", Format: pkg.Format,
		Root: root, Components: pkg.Components, MCP: pkg.MCP, Hooks: pkg.Hooks,
	}, nil
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
	return client.Secrets()
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
type cellDoc struct {
	Package  string   `json:"package"`
	Host     string   `json:"host"`
	Scope    string   `json:"scope"`
	Status   string   `json:"status"`
	Version  string   `json:"version,omitempty"`
	Strategy string   `json:"strategy,omitempty"`
	Level    string   `json:"level,omitempty"` // host maturity (DESIGN §10.3): experimental|beta|stable
	Kind     string   `json:"kind,omitempty"`
	Notes    []string `json:"notes,omitempty"`
}

// cliCells converts facade cells into the CLI's own document cells, so the
// rendered table and the --json shape stay byte-identical.
func cliCells(cells []verger.Cell) []cellDoc {
	out := make([]cellDoc, 0, len(cells))

	for _, cell := range cells {
		out = append(out, cellDoc{
			Package: cell.Package, Host: cell.Host, Scope: cell.Scope,
			Status: cell.Status, Version: cell.Version, Strategy: cell.Strategy,
			Level: cell.Level, Kind: cell.Kind, Notes: cell.Notes,
		})
	}

	return out
}

// reportDoc is the stable JSON shape of one executed plan.
type reportDoc struct {
	Cells []cellDoc `json:"cells"`
	Notes []string  `json:"notes,omitempty"`
	Home  string    `json:"home,omitempty"`
}

// reportDocFrom converts one apply report.
func reportDocFrom(report apply.Report) reportDoc {
	doc := reportDoc{Notes: report.Notes}

	for _, cell := range report.Cells {
		doc.Cells = append(doc.Cells, cellDoc{
			Package: cell.Package, Host: string(cell.Host), Scope: cell.Scope,
			Status: string(cell.Status), Version: cell.Version,
			Strategy: string(cell.Strategy), Kind: string(cell.Kind),
			Notes: cell.Notes,
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

// failedCells collects the failed and skewed cells of one report.
func failedCells(report apply.Report) error {
	var failed []string

	for _, cell := range report.Cells {
		if cell.Status == apply.StatusFailed || cell.Status == apply.StatusSkew {
			failed = append(failed, cell.Package+"@"+string(cell.Host))
		}
	}

	if len(failed) == 0 {
		return nil
	}

	return &ApplyFailedError{Cells: failed}
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

	_, _ = fmt.Fprintf(a.out, "%-24s %-8s %-8s %-10s %-10s %s\n", "PACKAGE", "HOST", "SCOPE", "STATUS", "STRATEGY", "VERSION")

	for _, cell := range report.Cells {
		_, _ = fmt.Fprintf(a.out, "%-24s %-8s %-8s %-10s %-10s %s\n",
			cell.Package, cell.Host, cell.Scope, cell.Status, cell.Strategy, cell.Version)

		for _, note := range cell.Notes {
			_, _ = fmt.Fprintf(a.out, "  %s\n", note)
		}
	}

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

	_, _ = fmt.Fprintf(a.out, "%-24s %-8s %-8s %-10s %s\n", "PACKAGE", "HOST", "SCOPE", "STRATEGY", "VERSION")

	for _, cell := range cells {
		_, _ = fmt.Fprintf(a.out, "%-24s %-8s %-8s %-10s %s\n",
			cell.Package, cell.Host, cell.Scope, cell.Strategy, cell.Version)
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
	opts := verger.ApplyOptions{DryRun: a.dryRun, Now: a.now}

	if a.yes {
		// -y accepts defaults and never resolves a destructive conflict
		// (rule 14): conflicts are declined, not confirmed.
		opts.Confirm = verger.YesConfirmer()
	} else if a.interactive() {
		opts.Confirm = promptConfirmer{a: a}
	}

	return opts
}
