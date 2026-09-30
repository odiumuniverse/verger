package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/odiumuniverse/verger/pkg/verger"
)

// schemaImport is declared with the other document names in wiring.go.

// importCandidate is one package the plan would record.
type importCandidate struct {
	ID       string   `json:"id"`
	Version  string   `json:"version,omitempty"`
	Host     string   `json:"host"`
	Disabled bool     `json:"disabled,omitempty"`
	Notes    []string `json:"notes,omitempty"`
}

// importSkipped is one installed entry the plan will not record, and why.
type importSkipped struct {
	Host   string `json:"host,omitempty"`
	ID     string `json:"id,omitempty"`
	Reason string `json:"reason"`
}

// importDoc is what `verger import` prints.
//
// `wrote` is in the document because the interesting difference between two
// runs is not the plan - it is almost always the same - but whether anything
// was written. A script that runs import in a loop needs to tell the first run
// from the hundredth without re-reading the file, and `dry_run` is what tells
// those two apart from each other.
type importDoc struct {
	Schema schemaRef `json:"schema"`

	// Wrote is the number of `[[package]]` entries added to the spec: 0 for a
	// dry run, 0 for a second run over the same home.
	Wrote   int  `json:"wrote"`
	DryRun  bool `json:"dry_run,omitempty"`
	Added   int  `json:"added"`
	Already int  `json:"already_in_spec"`

	Candidates []importCandidate `json:"candidates"`
	Skipped    []importSkipped   `json:"skipped"`
	// Spec is the file that was written, so a consumer does not have to
	// reconstruct the scope to know which one it was.
	Spec string `json:"spec"`
}

// newImportCmd builds `verger import`.
func newImportCmd(a *app) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "import",
		Short: "Write what your agents already installed into the spec",
		Long: "Collect what the detected hosts installed natively and record it as\n" +
			"`[[package]]` entries in the spec.\n\n" +
			"It writes the spec and nothing else: no host file is touched and nothing is\n" +
			"delivered. Run `verger sync` afterwards to make the machine match the spec.\n" +
			"Running it twice changes nothing the second time.",
		Args: usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.runImport(cmd.Context())
		},
	}

	addWriteFlags(cmd, a)
	cmd.Flags().BoolVar(&a.projectFlag, "project", false, "use the project scope")
	cmd.Flags().StringSliceVar(&a.hostsFlag, "hosts", nil, "only these hosts (comma separated)")
	cmd.Flags().StringSliceVar(&a.exceptFlag, "except", nil, "exclude these hosts")

	return cmd
}

// runImport records what the hosts already have. It reads every selected host's
// own listing, shows the plan, asks once, and writes the spec - never a host
// file.
func (a *app) runImport(ctx context.Context) error {
	client, err := a.open(ctx)
	if err != nil {
		return err
	}

	paths, err := a.paths(client)
	if err != nil {
		return err
	}

	if err := a.requireTrust(client, paths); err != nil {
		return err
	}

	plan, err := client.ImportPlan(ctx, verger.ImportOptions{
		Paths: paths,
		Hosts: a.hostFilter(),
	})
	if err != nil {
		return err
	}

	// The plan is on screen before anything is written, and one question covers
	// the whole set: a hundred `[[package]]` lines is one decision, not a
	// hundred (§1.3).
	if err := a.printImportPlan(plan); err != nil {
		return err
	}

	proceed, err := a.confirmImport(plan, paths)
	if err != nil {
		return err
	}

	if !proceed {
		return nil
	}

	doc := importDoc{
		Schema:     schemaOf(schemaImport),
		DryRun:     a.dryRun,
		Added:      len(plan.Candidates),
		Already:    plan.AlreadyInSpec,
		Candidates: importCandidates(plan.Candidates),
		Skipped:    importSkips(plan.Skipped),
		Spec:       paths.SpecPath,
	}

	if a.dryRun {
		return a.previewImport(plan, doc)
	}

	added, err := client.RecordImports(plan)
	if err != nil {
		return err
	}

	doc.Wrote = added

	if a.jsonOut {
		return a.printJSON(doc)
	}

	return a.reportImport(plan, paths, added)
}

// confirmImport gates the one write of this verb and reports whether the run
// may continue: a detached stdin stops with the typed pending error, a terminal
// is asked once, and a "no" is a refusal rather than a nil error — a helper
// that answered "declined" with a nil error would be read as "go ahead" by the
// caller, which is the opposite of what the user just said.
func (a *app) confirmImport(plan verger.ImportPlan, paths verger.Paths) (bool, error) {
	if err := a.requireConfirmation(); err != nil {
		return false, err
	}

	// A terminal gets a question, not a silent write. `requireConfirmation`
	// above only stops a *detached* stdin, so on a terminal it returned nil and
	// nothing else here asked: `verger import` would rewrite the user's spec
	// while they were still reading the plan. The default is no — this is the
	// write a user most wants to interrupt — and -y stays the way to say yes
	// without a prompt.
	if !a.interactive() {
		return true, nil
	}

	ok, err := a.ask(
		fmt.Sprintf("record %d package(s) in %s?", len(plan.Candidates), paths.SpecPath), false)
	if err != nil {
		return false, err
	}

	if ok {
		return true, nil
	}

	_, err = fmt.Fprintln(a.out, "nothing was recorded")

	return false, err
}

// previewImport prints what the write would do and writes nothing.
func (a *app) previewImport(plan verger.ImportPlan, doc importDoc) error {
	diff, err := specDiff(plan)
	if err != nil {
		return err
	}

	// The diff is for the reader; the document is for the parser. Under
	// `--json` stdout carries exactly one JSON document, so the human diff goes
	// beside it rather than in front of it.
	_, _ = fmt.Fprint(a.humanOut(), diff)
	doc.Wrote = 0

	if a.jsonOut {
		return a.printJSON(doc)
	}

	return nil
}

// reportImport says what the write did. It never claims a change that did not
// happen: a run that recorded nothing says nothing about the spec being changed.
func (a *app) reportImport(plan verger.ImportPlan, paths verger.Paths, added int) error {
	if added > 0 {
		_, err := fmt.Fprintf(a.out, "recorded %d package(s) in %s\n", added, paths.SpecPath)
		if err != nil {
			return err
		}

		_, err = fmt.Fprintln(a.out, "the spec changed; run `verger sync` to make the machine match it")

		return err
	}

	// The plan already said "nothing to import" when it had nothing to propose,
	// and saying it again would print one fact twice. This line is for the other
	// case: a plan that proposed something and found it already recorded by the
	// time it was written.
	if len(plan.Candidates) > 0 {
		_, err := fmt.Fprintf(a.out, "nothing to add: the spec already declares all %d\n",
			plan.AlreadyInSpec)

		return err
	}

	return nil
}

// humanOut is the writer for output meant for a person: stdout normally, stderr
// when stdout is carrying a `--json` document. A parser reading stdout must get
// one document and nothing else, so the human half moves rather than taking the
// whole line.
func (a *app) humanOut() io.Writer {
	if a.jsonOut {
		return a.errW
	}

	return a.out
}

// printImportPlan renders the plan as text: what would be written, and what
// would not, with the reason on the same line.
func (a *app) printImportPlan(plan verger.ImportPlan) error {
	if a.jsonOut {
		return nil
	}

	if len(plan.Candidates) == 0 {
		_, _ = fmt.Fprintln(a.out, "nothing to import: every installed package is already in the spec")

		return a.printImportSkipped(plan.Skipped)
	}

	_, _ = fmt.Fprintf(a.out, "%-32s %-10s %-10s %s\n", "PACKAGE", "VERSION", "FROM", "NOTE")

	for _, candidate := range plan.Candidates {
		_, _ = fmt.Fprintf(a.out, "%-32s %-10s %-10s %s\n",
			candidate.ID, orNone(candidate.Version), candidate.Host,
			orNone(importNote(candidate)))
	}

	return a.printImportSkipped(plan.Skipped)
}

// importNote renders what the user has to know about one candidate before the
// line is written.
func importNote(candidate verger.ImportCandidate) string {
	note := strings.Join(candidate.Notes, "; ")
	if !candidate.Disabled {
		return note
	}

	return strings.TrimSpace("disabled; " + note)
}

// printImportSkipped names every entry the plan will not record. A host that
// could not answer is on screen, not in a log: an import that silently skipped
// a whole agent would write a spec that reproduces the machine minus that
// agent, and the user would find out at the next sync.
func (a *app) printImportSkipped(skipped []verger.ImportSkipped) error {
	for _, entry := range skipped {
		subject := entry.Host
		if entry.ID != "" {
			subject = entry.Host + ":" + entry.ID
		}

		_, _ = fmt.Fprintf(a.out, "skipped %s: %s\n", subject, entry.Reason)
	}

	return nil
}

// specDiff renders what recording the plan would change in the spec file.
//
// The bytes it diffs come from the real writer applied to a throwaway copy, not
// from a second guess at what the writer would emit: a preview that disagrees
// with the save is worse than no preview, because it is trusted.
func specDiff(plan verger.ImportPlan) (string, error) {
	before, err := os.ReadFile(plan.Paths.SpecPath) //nolint:gosec // G304: the path is the resolved scope
	if err != nil && !os.IsNotExist(err) {
		return "", err
	}

	after, err := projectedSpec(plan)
	if err != nil {
		return "", err
	}

	return renderSpecDiff(string(before), after), nil
}

// projectedSpec applies the plan to a throwaway copy of the spec and returns
// the bytes it would leave behind.
func projectedSpec(plan verger.ImportPlan) (string, error) {
	dir, err := os.MkdirTemp("", "verger-import-dry-run")
	if err != nil {
		return "", err
	}

	defer func() { _ = os.RemoveAll(dir) }()

	staged := filepath.Join(dir, filepath.Base(plan.Paths.SpecPath))

	current, err := os.ReadFile(plan.Paths.SpecPath) //nolint:gosec // G304: the path is the resolved scope
	if err != nil && !os.IsNotExist(err) {
		return "", err
	}

	// G703: `staged` is the base name of the spec inside a temp dir this run
	// created, so nothing in the path came from the document being previewed.
	if err := os.WriteFile(staged, current, 0o600); err != nil { //nolint:gosec // G703: see above
		return "", err
	}

	if _, err := verger.RecordImportsAt(staged, plan); err != nil {
		return "", err
	}

	// G703: the path is `filepath.Join` of a temp dir this function created and
	// the base name of the spec, so nothing in it came from the document.
	projected, err := os.ReadFile(staged) //nolint:gosec // G703: staged under a temp dir this run created
	if err != nil {
		return "", err
	}

	return string(projected), nil
}

// renderSpecDiff prints the lines that differ between the current spec and the
// one the run would leave behind.
//
// It is a common-prefix/common-suffix diff, and for this command that is exact
// rather than approximate: import only ever appends entries, so everything
// between the shared head and the shared tail is what the run would write. A
// general in-place edit would need a real diff, and approximating one here
// would print a change list the writer does not agree with.
func renderSpecDiff(before, after string) string {
	if before == after {
		return "spec unchanged\n"
	}

	old := strings.Split(before, "\n")
	fresh := strings.Split(after, "\n")

	head := sharedHead(old, fresh)
	tail := sharedTail(old, fresh, head)

	var out strings.Builder

	_, _ = fmt.Fprintf(&out, "--- %s\n", "spec")
	_, _ = fmt.Fprintf(&out, "+++ %s\n", "spec (as import would leave it)")

	for _, line := range old[head : len(old)-tail] {
		_, _ = fmt.Fprintf(&out, "-%s\n", line)
	}

	for _, line := range fresh[head : len(fresh)-tail] {
		_, _ = fmt.Fprintf(&out, "+%s\n", line)
	}

	return out.String()
}

// sharedHead counts the leading lines two documents agree on.
func sharedHead(old, fresh []string) int {
	count := 0

	for count < len(old) && count < len(fresh) && old[count] == fresh[count] {
		count++
	}

	return count
}

// sharedTail counts the trailing lines two documents agree on, below the shared
// head. Counting from the end is what keeps a repeated line ("") from being
// mistaken for the boundary.
func sharedTail(old, fresh []string, head int) int {
	count := 0

	for count < len(old)-head && count < len(fresh)-head &&
		old[len(old)-1-count] == fresh[len(fresh)-1-count] {
		count++
	}

	return count
}

// importCandidates converts the facade's candidates into the document's rows.
func importCandidates(candidates []verger.ImportCandidate) []importCandidate {
	rows := make([]importCandidate, 0, len(candidates))
	for _, candidate := range candidates {
		rows = append(rows, importCandidate{
			ID:       candidate.ID,
			Version:  candidate.Version,
			Host:     candidate.Host,
			Disabled: candidate.Disabled,
			Notes:    candidate.Notes,
		})
	}

	return rows
}

// importSkips converts the facade's skipped entries into the document's rows.
func importSkips(skipped []verger.ImportSkipped) []importSkipped {
	rows := make([]importSkipped, 0, len(skipped))
	for _, entry := range skipped {
		rows = append(rows, importSkipped{Host: entry.Host, ID: entry.ID, Reason: entry.Reason})
	}

	return rows
}
