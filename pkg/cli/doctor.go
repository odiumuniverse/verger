package cli

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"

	"github.com/spf13/cobra"

	"github.com/odiumuniverse/verger/pkg/receipt"
	"github.com/odiumuniverse/verger/pkg/verger"
)

// doctorCheck is one doctor finding.
type doctorCheck struct {
	Severity string `json:"severity"`
	Check    string `json:"check"`
	Message  string `json:"message"`
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

	return cmd
}

// runDoctor collects checks, prints them and fails on any error severity.
func (a *app) runDoctor(ctx context.Context) error {
	client, err := a.open(ctx)
	if err != nil {
		return err
	}

	paths, err := a.paths(client)
	if err != nil {
		return err
	}

	checks := []doctorCheck{
		a.checkDir(client.Home().Root(), "home"),
		a.checkDir(client.Store().Root(), "store"),
		a.checkKeyring(client),
		a.checkReceipts(paths),
	}

	checks = append(checks, a.checkSpecs(client, paths)...)
	checks = append(checks, a.checkHosts(ctx, client)...)

	if a.jsonOut {
		if err := a.printJSON(checks); err != nil {
			return err
		}
	} else {
		for _, check := range checks {
			_, _ = fmt.Fprintf(a.out, "%-7s %-10s %s\n", check.Severity, check.Check, check.Message)
		}
	}

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

// checkDir reports one directory's existence and writability.
func (a *app) checkDir(path, name string) doctorCheck {
	info, err := os.Stat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return doctorCheck{Severity: severityWarning, Check: name, Message: path + " does not exist yet"}
	}

	if err != nil {
		return doctorCheck{Severity: severityError, Check: name, Message: err.Error()}
	}

	if !info.IsDir() {
		return doctorCheck{Severity: severityError, Check: name, Message: path + " is not a directory"}
	}

	probe, err := os.CreateTemp(path, ".doctor-*")
	if err != nil {
		return doctorCheck{Severity: severityError, Check: name, Message: path + " is not writable: " + err.Error()}
	}

	probeName := probe.Name()
	_ = probe.Close()
	_ = os.Remove(probeName)

	return doctorCheck{Severity: severityOK, Check: name, Message: path + " is writable"}
}

// checkKeyring probes the secrets store backend.
func (a *app) checkKeyring(client *verger.Client) doctorCheck {
	if err := client.Secrets().Probe(); err != nil {
		return doctorCheck{Severity: severityWarning, Check: "keyring", Message: err.Error()}
	}

	return doctorCheck{Severity: severityOK, Check: "keyring", Message: "keyring probe succeeded"}
}

// checkReceipts verifies the receipt store integrity.
func (a *app) checkReceipts(paths scopePaths) doctorCheck {
	if _, err := receipt.NewStore(paths.receiptsDir).List(); err != nil {
		return doctorCheck{Severity: severityError, Check: "receipts", Message: err.Error()}
	}

	return doctorCheck{Severity: severityOK, Check: "receipts", Message: "receipt store is readable"}
}

// checkSpecs parses the scope spec, the project spec and both locks.
func (a *app) checkSpecs(client *verger.Client, paths scopePaths) []doctorCheck {
	var checks []doctorCheck

	for _, path := range doctorFiles(client.Home().SpecPath(), paths.specPath) {
		doc, ok, err := loadSpec(path)

		switch {
		case err != nil:
			checks = append(checks, doctorCheck{Severity: severityError, Check: "spec", Message: err.Error()})
		case ok:
			checks = append(checks, doctorCheck{Severity: severityOK, Check: "spec", Message: fmt.Sprintf("%s (schema %d, %d package(s))", path, doc.Schema, len(doc.Packages))})
		}
	}

	for _, path := range doctorFiles(client.Home().LockPath(), paths.lockPath) {
		if _, err := loadLock(path); err != nil {
			checks = append(checks, doctorCheck{Severity: severityError, Check: "lock", Message: err.Error()})
		}
	}

	return checks
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
func (a *app) checkHosts(ctx context.Context, client *verger.Client) []doctorCheck {
	userHome, err := os.UserHomeDir()
	if err != nil {
		return []doctorCheck{{Severity: severityError, Check: "hosts", Message: err.Error()}}
	}

	var checks []doctorCheck

	for _, adapter := range a.hosts(client) {
		name := string(adapter.ID())

		if !adapter.Detect(userHome) {
			checks = append(checks, doctorCheck{Severity: severityOK, Check: "host:" + name, Message: "not detected"})

			continue
		}

		if _, err := adapter.Oracle().List(ctx); err != nil {
			checks = append(checks, doctorCheck{Severity: severityError, Check: "host:" + name, Message: err.Error()})

			continue
		}

		checks = append(checks, doctorCheck{Severity: severityOK, Check: "host:" + name, Message: "detected; oracle responds"})
	}

	return checks
}
