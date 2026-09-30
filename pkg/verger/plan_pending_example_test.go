package verger

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/odiumuniverse/verger/pkg/host"
)

// The right way to drive a delivery that might need consent: let Client.Plan
// build the plan, let the planning put the question, and read the answer off the
// plan — then run it and read the same answer off the result.
//
// This is in-package because the world needs a host adapter, and the only one
// available to a test is the fake. The API it shows is all exported: Client.Plan,
// Plan.PendingConsent, Client.Apply, Report.PendingConsent.
func ExampleClient_planAndApply_pendingConsent() {
	root, err := os.MkdirTemp("", "verger-example-pending")
	if err != nil {
		fmt.Println("setup failed")

		return
	}

	defer func() { _ = os.RemoveAll(root) }()

	user := filepath.Join(root, "user")

	for _, dir := range []string{
		user,
		filepath.Join(root, "data"),
		filepath.Join(root, "project"),
	} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			fmt.Println("setup failed")

			return
		}
	}

	for name, value := range map[string]string{
		"HOME":            user,
		"XDG_DATA_HOME":   filepath.Join(root, "data"),
		"XDG_CONFIG_HOME": filepath.Join(root, "config"),
		"VERGER_HOME":     "",
		"BEADLE_HOME":     "",
	} {
		if err := os.Setenv(name, value); err != nil {
			fmt.Println("setup failed")

			return
		}
	}

	// A local ref resolves against the working directory.
	previous, err := os.Getwd()
	if err != nil {
		fmt.Println("setup failed")

		return
	}

	if err := os.Chdir(root); err != nil {
		fmt.Println("setup failed")

		return
	}

	defer func() { _ = os.Chdir(previous) }()

	writeExamplePackage(root)

	fake := &fakeHost{id: host.Claude, artifacts: []fakeArtifact{{
		Path: filepath.Join(root, "host", "one.md"), Data: "# one\n",
	}}}

	client, err := Open(context.Background(), WithHosts(fake))
	if err != nil {
		fmt.Println("open failed")

		return
	}

	defer func() { _ = client.Close() }()

	paths, err := client.Paths(User, "")
	if err != nil {
		fmt.Println("paths failed")

		return
	}

	plan, err := client.Plan(context.Background(), PlanOptions{
		Paths: paths, Refs: []string{"./hooked"}, Hosts: []host.Host{fake},
	})
	if err != nil {
		fmt.Println("plan failed")

		return
	}

	// The question is put where the confirmer exists: in the planning that
	// Install and Sync run. YesConfirmer resolves with the default, which for
	// "install these hooks?" is not consent — so nobody was asked, and the plan
	// says so.
	if err := client.buildInstallActions(context.Background(), plan, ApplyOptions{Confirm: YesConfirmer()}); err != nil {
		fmt.Println("consent failed")

		return
	}

	fmt.Println("plan says pending:", plan.PendingConsent())

	report, err := client.Apply(context.Background(), plan, ApplyOptions{Confirm: YesConfirmer()})
	if err != nil {
		fmt.Println("apply failed")

		return
	}

	// The result repeats the plan rather than making up an answer of its own.
	fmt.Println("result says pending:", report.PendingConsent)

	// Output:
	// plan says pending: [local:hooked]
	// result says pending: [local:hooked]
}

// writeExamplePackage writes a package that ships a hook, so there is a consent
// question to answer at all.
func writeExamplePackage(root string) {
	pkg := filepath.Join(root, "hooked")
	_ = os.MkdirAll(filepath.Join(pkg, ".claude-plugin"), 0o700)
	_ = os.MkdirAll(filepath.Join(pkg, "skills", "one"), 0o700)
	_ = os.WriteFile(filepath.Join(pkg, ".claude-plugin", "plugin.json"),
		[]byte(`{"name":"hooked","version":"1.0.0","hooks":{"SessionStart":[{"hooks":[{"type":"command","command":"echo hi"}]}]}}`), 0o600)
	_ = os.WriteFile(filepath.Join(pkg, "skills", "one", "SKILL.md"),
		[]byte("---\nname: one\ndescription: an example package\n---\n\nbody\n"), 0o600)
}
