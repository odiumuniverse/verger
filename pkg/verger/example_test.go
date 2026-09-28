package verger_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/odiumuniverse/verger/pkg/apply"
	"github.com/odiumuniverse/verger/pkg/host"
	"github.com/odiumuniverse/verger/pkg/verger"
)

// The examples below are the godoc contract of the facade (DESIGN §9.1): every
// one is a program a second front end could run. They compile but do not execute
// a real delivery — an Example without an "Output:" comment is compiled and not
// run, which is what keeps them free of a home and a network.

// exampleClient opens a client over a temp home and returns it with the cleanup
// the example body does not need to spell out.
func exampleClient() (*verger.Client, context.Context, error) {
	root, err := os.MkdirTemp("", "verger-example")
	if err != nil {
		return nil, nil, err
	}

	client, err := verger.Open(context.Background(), verger.WithHome(filepath.Join(root, "home")))
	if err != nil {
		return nil, nil, err
	}

	return client, context.Background(), nil
}

// Open a home and read the machine paths back. Everything else in the facade is
// reached from the returned Client.
func Example() {
	client, _, err := exampleClient()
	if err != nil {
		return
	}

	defer func() { _ = client.Close() }()

	machine := client.Machine()
	_ = machine.Home
	_ = machine.StoreRoot
	_ = machine.SpecPath
}

// Plan an install and read the cells back as data: the caller renders them in
// whatever surface it has, and nothing is written until Apply runs.
func ExampleClient_Plan() {
	client, ctx, err := exampleClient()
	if err != nil {
		return
	}

	defer func() { _ = client.Close() }()

	paths, err := client.Paths(verger.User, "")
	if err != nil {
		return
	}

	plan, err := client.Plan(ctx, verger.PlanOptions{Paths: paths, Refs: []string{"./my-plugin"}})
	if err != nil {
		if _, ok := errors.AsType[*verger.UsageError](err); ok {
			// A caller mistake: report it in your own usage surface.
			return
		}

		return
	}

	for _, cell := range plan.Cells {
		_ = cell.Package
		_ = cell.Host
		_ = cell.Strategy
	}
}

// Apply a plan with an injected Confirmer. `-y` is exactly
// verger.YesConfirmer: it accepts defaults and never resolves a destructive
// conflict.
func ExampleClient_Install() {
	client, ctx, err := exampleClient()
	if err != nil {
		return
	}

	defer func() { _ = client.Close() }()

	paths, err := client.Paths(verger.User, "")
	if err != nil {
		return
	}

	plan, err := client.Plan(ctx, verger.PlanOptions{Paths: paths, Refs: []string{"./my-plugin"}})
	if err != nil {
		return
	}

	report, err := client.Install(ctx, plan, verger.ApplyOptions{
		Confirm: verger.YesConfirmer(),
		Hooks:   verger.HooksAsk,
		Events:  nil,
	})
	if err != nil {
		return
	}

	for _, cell := range report.Cells {
		_ = cell.Status
	}
}

// Targets narrows the hosts an operation runs on; the same filter is what the
// CLI's --hosts and --except produce.
func ExampleClient_Targets() {
	client, _, err := exampleClient()
	if err != nil {
		return
	}

	defer func() { _ = client.Close() }()

	adapters, err := client.Targets(verger.HostFilter{Only: []string{"claude"}, Except: []string{"cursor"}})
	if err != nil {
		if _, ok := errors.AsType[*verger.HostUnavailableError](err); ok {
			// The verdict names why each requested host produced no adapter.
			return
		}

		return
	}

	for _, adapter := range adapters {
		_ = adapter.ID()
	}
}

// Status is the package × host matrix, the same data `status --json` prints.
func ExampleClient_Status() {
	client, ctx, err := exampleClient()
	if err != nil {
		return
	}

	defer func() { _ = client.Close() }()

	paths, err := client.Paths(verger.Project, "")
	if err != nil {
		return
	}

	status, err := client.Status(ctx, verger.StatusOptions{Paths: paths, OutdatedOnly: true})
	if err != nil {
		return
	}

	for _, cell := range status.Cells {
		fmt.Println(cell.Package, cell.Host, cell.Level, cell.Status)
	}
}

// Why explains one cell: what the receipt, the lock, the spec and the consent
// gate each say about it.
func ExampleClient_Why() {
	client, ctx, err := exampleClient()
	if err != nil {
		return
	}

	defer func() { _ = client.Close() }()

	paths, err := client.Paths(verger.User, "")
	if err != nil {
		return
	}

	why, err := client.Why(ctx, paths, "acme/caveman", "claude")
	if err != nil {
		return
	}

	for _, reason := range why.Reasons {
		fmt.Println("reason:", reason)
	}

	for _, blocker := range why.Blockers {
		fmt.Println("blocker:", blocker)
	}
}

// Owns is the ownership invariant a second front end filters its own pulls with
// (DESIGN §9.3): it answers from the receipts, never from the filesystem.
func ExampleClient_Owns() {
	client, _, err := exampleClient()
	if err != nil {
		return
	}

	defer func() { _ = client.Close() }()

	pkg, owned := client.Owns(filepath.Join(client.Home().Root(), "skills", "alpha"))
	if owned {
		fmt.Println("owned by", pkg)
	}
}

// Remove plans first and executes second, so a front end can show the plan and
// ask its own confirmer before anything is written.
func ExampleClient_Remove() {
	client, ctx, err := exampleClient()
	if err != nil {
		return
	}

	defer func() { _ = client.Close() }()

	paths, err := client.Paths(verger.User, "")
	if err != nil {
		return
	}

	plan, err := client.PlanRemove(ctx, "acme/caveman", verger.RemoveOptions{Paths: paths})
	if err != nil {
		return
	}

	if len(plan.Actions) == 0 {
		fmt.Println("nothing installed")
		return
	}

	report, err := client.Remove(ctx, plan, verger.ApplyOptions{Confirm: verger.YesConfirmer(), Hooks: verger.HooksSkip})
	if err != nil {
		return
	}

	_ = report
}

// Restore puts back what a removal trashed.
func ExampleClient_Restore() {
	client, ctx, err := exampleClient()
	if err != nil {
		return
	}

	defer func() { _ = client.Close() }()

	paths, err := client.Paths(verger.User, "")
	if err != nil {
		return
	}

	result, err := client.Restore(ctx, "acme/caveman", verger.RemoveOptions{Paths: paths})
	if err != nil {
		return
	}

	for _, entry := range result.Restored {
		fmt.Println("restored", entry.Original)
	}
}

// Absorb moves one home's state into another, merging the spec by id and the
// lock and receipts by cell. It is what a second front end calls when it takes
// verger over (DESIGN §3.3, §9.3).
func ExampleAbsorb() {
	from, _, err := exampleClient()
	if err != nil {
		return
	}

	defer func() { _ = from.Close() }()

	to, _, err := exampleClient()
	if err != nil {
		return
	}

	defer func() { _ = to.Close() }()

	report, err := verger.Absorb(context.Background(), from, to)
	if err != nil {
		return
	}

	for _, name := range report.Moved {
		fmt.Println("moved", name)
	}

	for _, name := range report.Skipped {
		fmt.Println("kept", name)
	}
}

// Eject is the reverse: it refuses a target that already holds state rather
// than merging two homes silently.
func ExampleEject() {
	from, ctx, err := exampleClient()
	if err != nil {
		return
	}

	defer func() { _ = from.Close() }()

	to, _, err := exampleClient()
	if err != nil {
		return
	}

	defer func() { _ = to.Close() }()

	if _, err := verger.Eject(ctx, from, to); err != nil {
		fmt.Println(strings.SplitN(err.Error(), ":", 2)[0])
	}
}

// Confirmer is the only way a library operation can ask anything: the CLI
// supplies a TTY prompt, a second front end its own.
type askingConfirmer struct{}

// Confirm implements verger.Confirmer: ask, then answer.
func (askingConfirmer) Confirm(_ context.Context, q apply.Question) (bool, error) {
	fmt.Println("question:", q.Message)

	return true, nil
}

func ExampleConfirmer() {
	var confirmer verger.Confirmer = askingConfirmer{}

	allowed, err := confirmer.Confirm(context.Background(), apply.Question{Message: "Install hooks?"})
	if err != nil {
		return
	}

	_ = allowed
}

// PickStrategy is the ladder a plan resolves per host; a front end that
// previews a plan needs the same answer the executor will use.
func ExamplePickStrategy() {
	strategy, _ := verger.PickStrategy(host.Package{ID: "acme/caveman", Marketplace: "caveman@acme"}, host.Claude, "local")

	fmt.Println(strategy)
}
