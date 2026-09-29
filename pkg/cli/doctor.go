package cli

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/spf13/cobra"

	"github.com/odiumuniverse/verger/pkg/host"
	"github.com/odiumuniverse/verger/pkg/receipt"
	"github.com/odiumuniverse/verger/pkg/secret"
	"github.com/odiumuniverse/verger/pkg/verger"
)

// The argv words a fix is built from. They are constants because a fix is
// executed, not read: a typo here becomes a command that fails on a user's
// machine, and `goconst` is the cheapest guard against drift.
const (
	binVerger  = "verger"
	binLs      = "ls"
	flagFix    = "--fix"
	flagLocked = "--locked"
	flagLong   = "-ld"
	subDoctor  = "doctor"
	subList    = "list"
	subSecret  = "secret"
	subStatus  = "status"
)

// subjectKeyring names the secrets-backend finding. One constant because the
// four findings that share it have to stay one subject: `doctor --json` keys
// its output by it, and a user who greps for one spelling should find them
// all.
const subjectKeyring = "keyring"

// doctorCheck is one doctor finding (W7-UX-SPEC §3.1).
//
// `Subject` is `kind.name` and is what a script matches on; `Message` is prose
// and is not. `Fix` is a list of ready-to-run argv arrays, not a rendered
// string: a consumer can execute entry 0 directly, and one entry per step
// means a user can take step 0 and stop. `SafeToAutofix` is true only for a
// repair verger fully owns — creating a directory it owns, never touching
// anything a user wrote — so `--fix` can never consent on the user's behalf.
type doctorCheck struct {
	Severity      string     `json:"severity"`
	Subject       string     `json:"subject"`
	Message       string     `json:"message"`
	Fix           [][]string `json:"fix,omitempty"`
	SafeToAutofix bool       `json:"safe_to_autofix"`
}

// doctorDoc is the `verger doctor --json` document. It used to be a bare
// array, which cannot carry a schema field; wrapping it is a breaking change
// for a consumer that indexes into the array, and is required anyway so a
// consumer can tell a finding list from any other document
// (W7-UX-SPEC §2.1).
type doctorDoc struct {
	Schema schemaRef `json:"schema"`
	// Applied lists the subjects `--fix` repaired. It is additive: a consumer
	// that only reads `findings` is unaffected, and one that wants to know
	// what the run changed reads it here instead of scraping prose.
	Findings []doctorCheck `json:"findings"`
	Applied  []string      `json:"applied,omitempty"`
}

// newDoctorCmd builds `verger doctor`.
func newDoctorCmd(a *app) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Check the machine, stores, keyring and hosts",
		Args:  usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.runDoctor(cmd.Context())
		},
	}

	cmd.Flags().BoolVar(&a.projectFlag, "project", false, "also check the project scope")
	cmd.Flags().BoolVar(&a.fixFlag, "fix", false, "apply safe fixes (one confirmation)")
	// `--fix` asks before it repairs, so a script needs the same -y the other
	// write commands take. `--dry-run` is not offered: a plain doctor run
	// already writes nothing.
	cmd.Flags().BoolVarP(&a.yes, "yes", "y", false, "apply safe fixes without asking")

	return cmd
}

// runDoctor collects checks, prints them and fails on any error severity.
// With --fix, it applies findings with SafeToAutofix=true after one confirmation.
func (a *app) runDoctor(ctx context.Context) error {
	client, err := a.open(ctx)
	if err != nil {
		return err
	}

	paths, err := a.paths(client)
	if err != nil {
		return err
	}

	checks := a.collectChecks(ctx, client, paths)
	hostChecks := hostFindings(checks)

	// Apply the safe fixes, if any were asked for (W7-UX-SPEC §3.3).
	var applied []string

	if a.fixFlag {
		applied, err = a.applySafeFixes(ctx, client, checks)
		if err != nil {
			// The repairs that did land are reported before whatever stopped
			// the run, so a partial repair is never silent about what it did.
			a.reportApplied(applied)

			return err
		}

		// Re-collect, so the output describes the machine as it is now. The
		// host findings are carried over: nothing just repaired can change
		// whether a host answers, and asking again would double the slowest
		// part of the run for no new fact.
		checks = a.collectChecksExceptHosts(ctx, client, paths, hostChecks)
	}

	if a.jsonOut {
		doc := doctorDoc{Schema: schemaOf(schemaDoctor), Findings: checks}

		if len(applied) > 0 {
			doc.Applied = applied
		}

		if err := a.printJSON(doc); err != nil {
			return err
		}
	} else {
		a.reportApplied(applied)

		for _, check := range checks {
			_, _ = fmt.Fprintf(a.out, "%-7s %-12s %s\n", check.Severity, check.Subject, check.Message)

			for _, step := range check.Fix {
				_, _ = fmt.Fprintf(a.out, "          fix: %s\n", strings.Join(step, " "))
			}
		}
	}

	// The exit status is decided in both output modes: a machine with a
	// broken receipt is broken whether the caller asked for JSON or not.
	failures := 0

	for _, check := range checks {
		if check.Severity == severityError {
			failures++
		}
	}

	if failures > 0 {
		return fmt.Errorf("doctor: %d error(s)", failures)
	}

	return nil
}

// collectChecks runs every probe once. `--fix` calls it twice — before and
// after the repairs — so the machine is described as it is now, not as it was
// when the run started.
func (a *app) collectChecks(ctx context.Context, client *verger.Client, paths verger.Paths) []doctorCheck {
	return a.collectChecksExceptHosts(ctx, client, paths, nil)
}

// collectChecksExceptHosts runs every probe except the host sweep, and runs
// the host sweep only when reuse is nil. `--fix` re-reads the machine to
// describe it as it is now, and hands back the sweep it already took: the
// repairs create a home and a store, and neither says anything about whether
// a host's oracle answers.
func (a *app) collectChecksExceptHosts(
	ctx context.Context, client *verger.Client, paths verger.Paths, reuse []doctorCheck,
) []doctorCheck {
	checks := []doctorCheck{
		a.checkDir(client.Home().Root(), "home"),
		a.checkDir(client.Store().Root(), "store"),
		a.checkKeyring(client),
		a.checkReceipts(paths),
	}
	checks = append(checks, a.checkSpecs(client, paths)...)

	if reuse != nil {
		return append(checks, reuse...)
	}

	return append(checks, a.checkHosts(ctx, client)...)
}

// applyFix applies a safe fix for one finding.
func (a *app) applyFix(ctx context.Context, client *verger.Client, check doctorCheck) error {
	switch check.Subject {
	case "home":
		// The home is what this finding is about, and the store has its own
		// finding; creating the store here would repair the wrong directory
		// and leave the reported one missing.
		return client.Home().Ensure()
	case "store":
		return client.Store().Ensure()
	default:
		return nil
	}
}

// applySafeFixes applies the findings verger fully owns, and only those.
//
// The rules come from W7-UX-SPEC §3.3, and each exists to stop `--fix` from
// doing something the user would not have done: it asks once for the whole
// set rather than once per finding, it touches nothing whose repair is a
// consent (hooks, trust, a policy refusal) or a file the user edited, and it
// stops at the first failure so the machine is never left half-repaired
// without saying so.
//
// Findings are applied in subject order, so two runs repair the same things
// in the same sequence. It returns the subjects it repaired and a single
// error for whatever stopped it — the confirmation error when the user
// declined, the repair error when a fix itself failed.
func (a *app) applySafeFixes(ctx context.Context, client *verger.Client, checks []doctorCheck) ([]string, error) {
	fixable := make([]doctorCheck, 0, len(checks))

	for _, check := range checks {
		if check.SafeToAutofix {
			fixable = append(fixable, check)
		}
	}

	if len(fixable) == 0 {
		return nil, nil
	}

	slices.SortFunc(fixable, func(a, b doctorCheck) int {
		return cmp.Compare(a.Subject, b.Subject)
	})

	// One real question for the whole set, not one per finding and not the
	// no-TTY gate: on a terminal the user is asked, because
	// `requireConfirmation` deliberately lets a TTY run through unreviewed for
	// the other write commands, and `--fix` changes the machine (W7-UX-SPEC
	// §3.3).
	proceed := a.yes // -y is the script switch: proceed without asking

	if !proceed {
		var err error

		proceed, err = a.askForFixes(fixable)
		if err != nil {
			return nil, err
		}
	}

	if !proceed {
		// Declining is a decision, not a failure: nothing is changed, and the
		// findings are still reported as they stand.
		return nil, nil
	}

	applied := make([]string, 0, len(fixable))

	for i := range fixable {
		if err := a.applyFix(ctx, client, fixable[i]); err != nil {
			return applied, fmt.Errorf("fix %s: %w", fixable[i].Subject, err)
		}

		applied = append(applied, fixable[i].Subject)
	}

	return applied, nil
}

// askForFixes prints what `--fix` is about to change and asks once for the
// whole set (W7-UX-SPEC §3.3). It returns the answer, so a "no" is honoured
// rather than quietly treated as consent.
func (a *app) askForFixes(fixable []doctorCheck) (bool, error) {
	_, _ = fmt.Fprintf(a.out, "doctor: %d fix(es) to apply:\n", len(fixable))

	for _, check := range fixable {
		_, _ = fmt.Fprintf(a.out, "  %-12s %s\n", check.Subject, check.Message)
	}

	return a.ask("apply these?", false)
}

// reportApplied prints what `--fix` repaired, in text mode only: under
// `--json` the same list travels in the document, and prose on stdout would
// corrupt it.
func (a *app) reportApplied(applied []string) {
	if a.jsonOut {
		return
	}

	for _, subject := range applied {
		_, _ = fmt.Fprintf(a.out, "fixed: %s\n", subject)
	}
}

// checkDir reports one directory's existence and writability.
func (a *app) checkDir(path, name string) doctorCheck {
	info, err := os.Stat(path)
	if errors.Is(err, fs.ErrNotExist) {
		// verger owns this directory, so creating it changes nothing a user
		// wrote: that is exactly what `safe_to_autofix` is for.
		return doctorCheck{
			Severity:      severityWarning,
			Subject:       name,
			Message:       path + " does not exist yet",
			Fix:           [][]string{{binVerger, subDoctor, flagFix}},
			SafeToAutofix: true,
		}
	}

	if err != nil {
		return doctorCheck{Severity: severityError, Subject: name, Message: err.Error()}
	}

	if !info.IsDir() {
		return doctorCheck{
			Severity: severityError,
			Subject:  name,
			Message:  path + " is not a directory",
			Fix:      [][]string{{binLs, flagLong, path}},
		}
	}

	probe, err := os.CreateTemp(path, ".doctor-*")
	if err != nil {
		return doctorCheck{
			Severity: severityError,
			Subject:  name,
			Message:  path + " is not writable: " + err.Error(),
			Fix:      [][]string{{binLs, flagLong, path}},
		}
	}

	probeName := probe.Name()
	_ = probe.Close()
	_ = os.Remove(probeName)

	return doctorCheck{Severity: severityInfo, Subject: name, Message: path + " is writable"}
}

// checkKeyring reports the configured secrets backend and whether a keychain
// can be used here.
//
// The availability question is answered by the probe, never by opening the
// keychain. The store's own probe is a real Get/Delete round trip against the
// OS secret store, and on macOS an isolated HOME makes the keychain look
// missing — so the OS answers with a modal "Keychain Not Found / Reset To
// Defaults", offering to wipe the user's keychain because a diagnostic asked
// where to keep a string. The probe reads the filesystem and the environment
// instead, so the answer costs nothing and raises nothing.
func (a *app) checkKeyring(client *verger.Client) doctorCheck {
	store, ok := client.Secrets().(*secret.Store)
	if !ok {
		return doctorCheck{
			Severity: severityWarning,
			Subject:  subjectKeyring,
			Message:  "secrets store unavailable",
			Fix:      [][]string{{binVerger, subSecret, subList}},
		}
	}

	backend := store.Backend()
	available, reason := a.keyringProbe.Available()

	if backend != secret.BackendKeyring {
		// The file backend asks nothing of the keychain, so the check is
		// plain: what is in use, and nothing that sounds like a round trip.
		return doctorCheck{
			Severity: severityInfo,
			Subject:  subjectKeyring,
			Message:  "secrets backend: " + backend + " (no keychain involved)",
		}
	}

	// Configured for the keyring, so whether one is here is the question
	// worth answering. A keyring backend on a machine with no keychain is a
	// real problem — secrets cannot be stored where they were asked to go —
	// so it is a warning, and the way out is a backend switch. It is not a
	// `--fix` candidate: switching storage is a decision the user makes, not
	// a repair a diagnostic applies.
	if available {
		return doctorCheck{
			Severity: severityInfo,
			Subject:  subjectKeyring,
			Message:  "secrets backend: keyring; a keychain is available here",
		}
	}

	return doctorCheck{
		Severity: severityWarning,
		Subject:  subjectKeyring,
		Message: "secrets backend: keyring, but no keychain here (" + reason +
			"); secrets cannot be stored where they are configured to go",
	}
}

// checkReceipts verifies the receipt store integrity.
func (a *app) checkReceipts(paths verger.Paths) doctorCheck {
	if _, err := receipt.NewStore(paths.ReceiptsDir).List(); err != nil {
		return doctorCheck{Severity: severityError, Subject: "receipts", Message: err.Error()}
	}

	return doctorCheck{Severity: severityInfo, Subject: "receipts", Message: "receipt store is readable"}
}

// checkSpecs parses the scope spec, the project spec and both locks.
func (a *app) checkSpecs(client *verger.Client, paths verger.Paths) []doctorCheck {
	var checks []doctorCheck

	for _, path := range doctorFiles(client.Home().SpecPath(), paths.SpecPath) {
		doc, ok, err := loadSpec(path)

		switch {
		case err != nil:
			checks = append(checks, doctorCheck{
				Severity: severityError,
				Subject:  "spec",
				Message:  err.Error(),
				Fix:      [][]string{{binVerger, subStatus}},
			})
		case ok:
			checks = append(checks, doctorCheck{Severity: severityInfo, Subject: "spec", Message: fmt.Sprintf("%s (schema %d, %d package(s))", path, doc.Schema, len(doc.Packages))})
		}
	}

	checks = append(checks, checkPortableSources(paths)...)

	for _, path := range doctorFiles(client.Home().LockPath(), paths.LockPath) {
		if _, err := loadLock(path); err != nil {
			checks = append(checks, doctorCheck{
				Severity: severityError,
				Subject:  "lock",
				Message:  err.Error(),
				Fix:      [][]string{{binVerger, subStatus, flagLocked}},
			})
		}
	}

	return checks
}

// checkPortableSources reports sources that would stop resolving when the
// vault is cloned to another machine — an absolute path, or one outside the
// spec's own directory.
//
// It is a warning and never an autofix: rewriting a path somebody else
// wrote is the vault author's decision, not a repair verger may take on its
// own (W7-UX-SPEC §3.1 — a fix verger does not fully own is an
// instruction, not an action).
func checkPortableSources(paths verger.Paths) []doctorCheck {
	doc, ok, err := loadSpec(paths.SpecPath)
	if err != nil || !ok {
		return nil
	}

	urls := verger.NonPortableSources(doc, filepath.Dir(paths.SpecPath))
	if len(urls) == 0 {
		return nil
	}

	return []doctorCheck{{
		Severity: severityWarning,
		Subject:  "source.not-portable",
		Message: fmt.Sprintf("%d source(s) live outside this vault and will not resolve after a clone: %s",
			len(urls), strings.Join(urls, ", ")),
		Fix:           [][]string{{binVerger, subDoctor}},
		SafeToAutofix: false,
	}}
}

// doctorFiles lists the state files doctor should parse: the home-scope path,
// the project spec/lock in the working directory and the scope path, deduped.
func doctorFiles(homePath, scopePath string) []string {
	out := []string{homePath}

	add := func(path string) {
		if path != "" && !slices.Contains(out, path) && fileExistsIn(path) {
			out = append(out, path)
		}
	}

	if wd, err := os.Getwd(); err == nil {
		add(filepath.Join(wd, filepath.Base(homePath)))
	}

	add(scopePath)

	return out
}

// fileExistsIn reports whether path exists.
func fileExistsIn(path string) bool {
	_, err := os.Stat(path)

	return err == nil
}

// checkHosts probes every registered host adapter and its oracle.
//
// The sweep is parallel and shares one deadline. Sequentially the cost is the
// sum of the hosts' times, so the one host that is slow — a service-backed
// one whose background process is not running — sets the wall clock for the
// whole diagnostic. Overlapped it is the slowest single host, and the budget
// below caps even that.
func (a *app) checkHosts(ctx context.Context, client *verger.Client) []doctorCheck {
	adapters := a.hosts(client)

	userHome, err := os.UserHomeDir()
	if err != nil {
		return []doctorCheck{{Severity: severityError, Subject: "hosts", Message: err.Error()}}
	}

	ctx, cancel := context.WithTimeout(ctx, doctorOracleWait)
	defer cancel()

	found := make([]doctorCheck, len(adapters))

	var wg sync.WaitGroup

	for i, adapter := range adapters {
		wg.Go(func() {
			// Each probe writes its own slot and reads none, so the sweep
			// needs no lock and the output keeps the adapter order whatever
			// order the answers arrive in.
			found[i] = probeHost(ctx, adapter, userHome)
		})
	}

	wg.Wait()

	checks := make([]doctorCheck, 0, len(found))

	for _, check := range found {
		if check.Subject != "" {
			checks = append(checks, check)
		}
	}

	return checks
}

// doctorOracleWait bounds one host probe inside doctor. It is well under
// host.DefaultOracleWait on purpose: that bound exists so a write that needs
// the answer fails loudly instead of hanging, while a diagnostic already has
// a budget of its own and is the command people reach for when something is
// wrong and they are waiting to find out what.
const doctorOracleWait = 3 * time.Second

// probeHost answers one question about one host: is it there, and does it
// answer.
func probeHost(ctx context.Context, adapter host.Host, userHome string) doctorCheck {
	name := string(adapter.ID())

	if !adapter.Detect(userHome) {
		return doctorCheck{Severity: severityInfo, Subject: "host:" + name, Message: "not detected"}
	}

	if _, err := adapter.Oracle().List(ctx); err != nil {
		return doctorCheck{Severity: severityError, Subject: "host:" + name, Message: err.Error()}
	}

	return doctorCheck{
		Severity: severityInfo, Subject: "host:" + name, Message: "detected; oracle responds",
	}
}

// hostFindings picks the host section back out of a full check list, so a
// `--fix` re-read can reuse it rather than probing every host a second time.
func hostFindings(checks []doctorCheck) []doctorCheck {
	var hosts []doctorCheck

	for _, check := range checks {
		if strings.HasPrefix(check.Subject, "host:") {
			hosts = append(hosts, check)
		}
	}

	return hosts
}
