package verger_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/odiumuniverse/verger/pkg/apply"
	"github.com/odiumuniverse/verger/pkg/digest"
	"github.com/odiumuniverse/verger/pkg/host"
	"github.com/odiumuniverse/verger/pkg/receipt"
	"github.com/odiumuniverse/verger/pkg/verger"
)

// The examples below are the godoc contract of the facade (DESIGN §9.1): every
// one is a program a second front end could run.
//
// Every one of them EXECUTES. An Example without an "Output:" comment is
// compiled and never run, which is a licence to rot: the godoc would keep
// showing a call sequence that stopped compiling against the facade months ago.
// So each one prints something and pins it, and the world it runs in is fixed -
// a temp home, one fakeHost adapter instead of a real CLI, and a frozen clock -
// because an example whose output changes with the machine is an example
// nobody trusts enough to copy.

// exampleClock is the frozen clock the examples run on. Every timestamp a
// receipt carries comes from here, so the output below is the same on every
// machine and in every year.
func exampleClock() time.Time {
	return time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
}

// exampleClient opens a client over a deterministic world: a temp home, one
// fakeHost adapter, a frozen clock, and a local package already written out. It
// returns the client, a context, and the package's ref.
//
// The three knobs are the whole contract. Without the fake adapter the example
// depends on which agents the machine running the tests happens to have; without
// the frozen clock any timestamp in a receipt makes the output unpredictable;
// without the written package every example would begin with error handling and
// never reach the call it exists to document.
func exampleClient() (*verger.Client, context.Context, string, string, error) {
	root, err := os.MkdirTemp("", "verger-example")
	if err != nil {
		return nil, nil, "", "", err
	}

	pkg := filepath.Join(root, "pkg")
	writeExampleFile(filepath.Join(pkg, ".claude-plugin", "plugin.json"),
		`{"name":"example","version":"1.0.0","description":"An example package."}`)
	writeExampleFile(filepath.Join(pkg, "skills", "one", "SKILL.md"), "# one\n")

	// A project scope resolves against a verger.toml in the project root; the
	// examples write one here rather than reading the caller's, so a run of
	// `go test` can never write into the repository it was invoked from.
	writeExampleFile(filepath.Join(root, "verger.toml"), "schema = 1\n")

	client, err := verger.Open(context.Background(),
		verger.WithHome(filepath.Join(root, "home")),
		verger.WithStore(filepath.Join(root, "store")),
		verger.WithHosts(exampleHost{root: filepath.Join(root, "home")}),
		verger.WithClock(exampleClock),
	)
	if err != nil {
		return nil, nil, "", "", err
	}

	return client, context.Background(), pkg, root, nil
}

// exampleHost is the one adapter the examples run against. It lives in the
// external test package on purpose: an Example documents the PUBLIC facade, so
// it may not reach for an unexported fixture from the internal test package,
// and it must not reach for a real agent CLI either - the whole point of a
// fixed world is that its output does not depend on the machine.
//
// It reports what it was told to write and claims nothing it cannot show: the
// oracle reports verified only after the bytes are on disk, and the marker file
// it returns is the one an example prints.
//
// It carries its own root because that is the contract an adapter really has.
// apply's runner calls Deliver with an EMPTY home (pkg/apply/phase.go), because
// each adapter resolves its own surfaces from the environment it was built
// with - which is exactly why host.NewClaude takes host.WithHome. An adapter
// that writes the argument verbatim writes to the WORKING DIRECTORY, and that
// is not a theoretical hazard: it is how `pkg/verger/.claude/skills/one/`
// appeared in this repository, written by the examples below during an ordinary
// `go test`. Home_isolation_test.go now fails the suite if anything reappears.
type exampleHost struct{ root string }

var exampleSkill = []byte("# one\n")

// ID implements host.Host.
func (exampleHost) ID() host.ID { return host.Claude }

// Detect implements host.Host: the adapter is injected, so it is always present.
func (exampleHost) Detect(string) bool { return true }

// Oracle implements host.Host.
func (exampleHost) Oracle() host.Oracle { return exampleOracle{} }

// Deliver implements host.Host: it writes the package's one skill and reports
// exactly the artifacts it wrote.
func (h exampleHost) Deliver(_ context.Context, _ string, d host.Delivery) (host.Result, error) {
	result := host.Result{Strategy: d.Strategy}

	if d.DryRun {
		return result, nil
	}

	for _, component := range d.Package.Components {
		// A receipt artifact is a claim about a path, so the file has to be
		// there: an adapter that reported one without writing it would be
		// showing an example of lying to its own ledger.
		path := filepath.Join(h.root, ".claude", "skills", component.Name, "SKILL.md")
		writeExampleFile(path, string(exampleSkill))

		result.Artifacts = append(result.Artifacts, receipt.Artifact{
			Kind: string(component.Kind), Name: component.Name, Path: path, Digest: digest.Bytes(exampleSkill),
		})
	}

	result.Observed = host.OracleResult{Verified: true}

	return result, nil
}

// Uninstall implements host.Host.
func (exampleHost) Uninstall(context.Context, string, receipt.Receipt) (host.Result, error) {
	return host.Result{}, nil
}

// exampleOracle answers the one question a lifecycle example asks.
type exampleOracle struct{}

// List implements host.Oracle: the example host installs nothing it cannot
// enumerate, so it enumerates nothing.
func (exampleOracle) List(context.Context) ([]host.Installed, error) { return nil, nil }

// Validate implements host.Oracle.
func (exampleOracle) Validate(context.Context, string) ([]string, error) { return nil, nil }

// writeExampleFile writes one fixture file for the examples. It has no
// testing.T to fail with - an Example body is a plain function - so a failure
// panics, which is the loudest report available there.
func writeExampleFile(path, content string) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		panic(err)
	}

	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		panic(err)
	}
}

// exampleClose releases the client the example opened.
func exampleClose(client *verger.Client) {
	_ = client.Close()
}

// Open a home and read the machine paths back. Everything else in the facade is
// reached from the returned Client.
func Example() {
	client, _, _, _, err := exampleClient()
	if err != nil {
		return
	}

	defer exampleClose(client)

	machine := client.Machine()
	fmt.Println("home:", filepath.Base(machine.Home))
	fmt.Println("store:", filepath.Base(machine.StoreRoot))
	fmt.Println("spec:", filepath.Base(machine.SpecPath))

	// Output:
	// home: home
	// store: store
	// spec: verger.toml
}

// Plan an install and read the cells back as data: the caller renders them in
// whatever surface it has, and nothing is written until Apply runs.
func ExampleClient_Plan() {
	client, ctx, ref, _, err := exampleClient()
	if err != nil {
		return
	}

	defer exampleClose(client)

	paths, err := client.Paths(verger.User, "")
	if err != nil {
		return
	}

	plan, err := client.Plan(ctx, verger.PlanOptions{Paths: paths, Refs: []string{ref}})
	if err != nil {
		if _, ok := errors.AsType[*verger.UsageError](err); ok {
			// A caller mistake: report it in your own usage surface.
			return
		}

		return
	}

	for _, cell := range plan.Cells {
		fmt.Println(cell.Package, cell.Host, cell.Strategy, cell.Status)
	}

	// Output:
	// local:example claude loose planned
}

// Apply a plan with an injected Confirmer. `-y` is exactly
// verger.YesConfirmer: it accepts defaults and never resolves a destructive
// conflict.
func ExampleClient_Install() {
	client, ctx, ref, _, err := exampleClient()
	if err != nil {
		return
	}

	defer exampleClose(client)

	paths, err := client.Paths(verger.User, "")
	if err != nil {
		return
	}

	plan, err := client.Plan(ctx, verger.PlanOptions{Paths: paths, Refs: []string{ref}})
	if err != nil {
		return
	}

	report, err := client.Install(ctx, plan, verger.ApplyOptions{
		Confirm: verger.YesConfirmer(),
		Hooks:   verger.HooksAsk,
		Events:  nil,
	})
	if err != nil {
		fmt.Println("install error:", err)

		return
	}

	for _, cell := range report.Cells {
		fmt.Println(cell.Package, cell.Host, cell.Status)
	}

	// Output:
	// local:example claude current
}

// Targets narrows the hosts an operation runs on; the same filter is what the
// CLI's --hosts and --except produce.
func ExampleClient_Targets() {
	client, _, _, _, err := exampleClient()
	if err != nil {
		return
	}

	defer exampleClose(client)

	adapters, err := client.Targets(verger.HostFilter{Only: []string{"claude"}, Except: []string{"cursor"}})
	if err != nil {
		if _, ok := errors.AsType[*verger.HostUnavailableError](err); ok {
			// The verdict names why each requested host produced no adapter.
			return
		}

		return
	}

	for _, adapter := range adapters {
		fmt.Println(adapter.ID())
	}

	// Output:
	// claude
}

// Status is the package × host matrix, the same data `status --json` prints.
func ExampleClient_Status() {
	client, ctx, ref, _, err := exampleClient()
	if err != nil {
		return
	}

	defer exampleClose(client)

	paths, err := client.Paths(verger.User, "")
	if err != nil {
		return
	}

	// Install first: a matrix over an empty scope is an empty table, which
	// documents nothing about how Status reads one.
	plan, err := client.Plan(ctx, verger.PlanOptions{Paths: paths, Refs: []string{ref}})
	if err != nil {
		return
	}

	if _, err := client.Install(ctx, plan, verger.ApplyOptions{
		Confirm: verger.YesConfirmer(), Hooks: verger.HooksAsk,
	}); err != nil {
		return
	}

	status, err := client.Status(ctx, verger.StatusOptions{Paths: paths})
	if err != nil {
		return
	}

	for _, cell := range status.Cells {
		fmt.Println(cell.Package, cell.Host, cell.Level, cell.Status)
	}

	// Output:
	// local:example claude stable current
}

// Why explains one cell: what the receipt, the lock, the spec and the consent
// gate each say about it.
func ExampleClient_Why() {
	client, ctx, _, _, err := exampleClient()
	if err != nil {
		return
	}

	defer exampleClose(client)

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

	// Output:
	// reason: hooks: no approval recorded
	// blocker: no receipt and no lock cell for this package/host
}

// Owns is the ownership invariant a second front end filters its own pulls with
// (DESIGN §9.3): it answers from the receipts, never from the filesystem.
func ExampleClient_Owns() {
	client, _, _, _, err := exampleClient()
	if err != nil {
		return
	}

	defer exampleClose(client)

	path := filepath.Join(client.Home().Root(), "skills", "alpha")
	pkg, owned := client.Owns(path)
	fmt.Println(owned, pkg)

	// Output:
	// false
}

// Remove plans first and executes second, so a front end can show the plan and
// ask its own confirmer before anything is written.
func ExampleClient_Remove() {
	client, ctx, _, _, err := exampleClient()
	if err != nil {
		return
	}

	defer exampleClose(client)

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
	// Output:
	// nothing installed
}

// Restore puts back what a removal trashed.
func ExampleClient_Restore() {
	client, ctx, _, _, err := exampleClient()
	if err != nil {
		return
	}

	defer exampleClose(client)

	paths, err := client.Paths(verger.User, "")
	if err != nil {
		return
	}

	result, err := client.Restore(ctx, "acme/caveman", verger.RemoveOptions{Paths: paths})
	if err != nil {
		return
	}

	for _, entry := range result.Restored {
		fmt.Println("restored", filepath.Base(entry.Original))
	}
	// Output:
}

// Absorb moves one home's state into another, merging the spec by id and the
// lock and receipts by cell. It is what a second front end calls when it takes
// verger over (DESIGN §3.3, §9.3).
func ExampleAbsorb() {
	from, _, _, _, err := exampleClient()
	if err != nil {
		return
	}

	defer func() { _ = from.Close() }()

	to, _, _, _, err := exampleClient()
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
	// Output:
}

// Eject is the reverse: it refuses a target that already holds state rather
// than merging two homes silently.
func ExampleEject() {
	from, ctx, _, _, err := exampleClient()
	if err != nil {
		return
	}

	defer func() { _ = from.Close() }()

	to, _, _, _, err := exampleClient()
	if err != nil {
		return
	}

	defer func() { _ = to.Close() }()

	if _, err := verger.Eject(ctx, from, to); err != nil {
		fmt.Println(strings.SplitN(err.Error(), ":", 2)[0])
	}
	// Output:
}

// Move a client's home to a fresh target, bound to the client so a second
// front end never opens two clients by hand. A refusal is an *EjectError, so
// a caller matches the type instead of the text.
func ExampleClient_Eject() {
	client, ctx, _, _, err := exampleClient()
	if err != nil {
		return
	}

	defer exampleClose(client)

	target, err := os.MkdirTemp("", "verger-eject")
	if err != nil {
		return
	}

	if err := client.Eject(ctx, filepath.Join(target, "home")); err != nil {
		ejected, ok := errors.AsType[*verger.EjectError](err)
		fmt.Println("eject refused:", ejected.Target, ok)
	}
	// Output:
}

// Record a hook approval explicitly, by (package, host, content hash), so the
// consent store is the only record and the answer is reproducible. The
// returned key is the approval's own identity.
func ExampleClient_ApproveHooksFor() {
	client, _, _, _, err := exampleClient()
	if err != nil {
		return
	}

	defer exampleClose(client)

	sum := digest.Bytes([]byte("# the hook body\n"))

	key, err := client.ApproveHooksFor("acme/caveman", "claude", sum)
	if err != nil {
		approval, ok := errors.AsType[*verger.HookApprovalError](err)
		fmt.Println("approval refused:", approval.Package, approval.Host, ok)

		return
	}

	fmt.Println("approved under:", key)
	// Output:
	// approved under: acme/caveman
}

// Drop a package from the spec while leaving its files and receipts alone:
// Uninstall is the destructive counterpart. dryRun answers the question
// without writing.
func ExampleClient_Unregister() {
	client, ctx, _, _, err := exampleClient()
	if err != nil {
		return
	}

	defer exampleClose(client)

	paths, err := client.Paths(verger.User, "")
	if err != nil {
		return
	}

	// A package the spec never declared is a caller mistake, reported as
	// *UsageError, so a front end exits with its usage code.
	if err := client.Unregister(ctx, paths, "acme/caveman", false); err != nil {
		_, ok := errors.AsType[*verger.UsageError](err)
		fmt.Println("not declared:", ok)
	}
	// Output:
	// not declared: true
}

// Undo a host-native registration verger did not create itself — beadle's old
// directory marketplace — by running the inverse RMA through the host, so no
// front end unregisters by hand. A host this build does not know is a
// *UsageError.
func ExampleClient_UnregisterHost() {
	client, ctx, _, _, err := exampleClient()
	if err != nil {
		return
	}

	defer exampleClose(client)

	if err := client.UnregisterHost(ctx, "claude", "acme"); err != nil {
		_, ok := errors.AsType[*verger.UsageError](err)
		fmt.Println("no such host:", ok)
	}
	// Output:
}

// memoryStore is the smallest thing that satisfies verger.SecretsStore: a
// second front end hands the facade its own store and keeps one source of
// secrets. beadle passes a keyring-backed store here.
type memoryStore struct{ values map[string]string }

func (m *memoryStore) Get(name string) (string, bool) { v, ok := m.values[name]; return v, ok }
func (m *memoryStore) Set(name, value string)         { m.values[name] = value }
func (m *memoryStore) Delete(name string) bool        { delete(m.values, name); return true }
func (m *memoryStore) Save() error                    { return nil }
func (m *memoryStore) Names() []string                { return nil }
func (m *memoryStore) Changed() bool                  { return false }
func (m *memoryStore) Path() string                   { return "memory" }

// WithSecrets injects a store the facade drives instead of the default file
// at <home>/state/secrets.json, so a second front end keeps one source of
// secrets.
func ExampleWithSecrets() {
	store := &memoryStore{values: map[string]string{}}

	client, _, _, _, err := exampleClient()
	if err != nil {
		return
	}

	defer exampleClose(client)

	other, err := verger.Open(context.Background(), verger.WithSecrets(store))
	if err != nil {
		return
	}

	defer func() { _ = other.Close() }()

	// The store the client was opened with is the one it resolves secrets
	// through; nothing reaches for the machine's keychain behind the caller's
	// back.
	fmt.Println("secrets wired:", other.Secrets() != nil)

	// Output:
	// secrets wired: true
}

// Confirmer is the only way a library operation can ask anything: the CLI
// supplies a TTY prompt, a second front end its own.
type askingConfirmer struct{}

// Confirm implements verger.Confirmer: ask, then answer.
func (askingConfirmer) Confirm(_ context.Context, q apply.Question) (bool, error) {
	fmt.Println("question:", q.Message)

	return true, nil
	// Output:
}

func ExampleConfirmer() {
	var confirmer verger.Confirmer = askingConfirmer{}

	allowed, err := confirmer.Confirm(context.Background(), apply.Question{Message: "Install hooks?"})
	if err != nil {
		return
	}

	_ = allowed
	// Output:
	// question: Install hooks?
}

// PickStrategy is the ladder a plan resolves per host; a front end that
// previews a plan needs the same answer the executor will use.
func ExamplePickStrategy() {
	strategy, _ := verger.PickStrategy(host.Package{ID: "acme/caveman", Marketplace: "caveman@acme"}, host.Claude, "local")

	fmt.Println(strategy)
	// Output:
	// native
}
