package verger

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	. "github.com/smartystreets/goconvey/convey"

	"github.com/odiumuniverse/verger/pkg/apply"
	"github.com/odiumuniverse/verger/pkg/host"
	"github.com/odiumuniverse/verger/pkg/receipt"
)

// A plugin a host REGISTERED is held in the receipt's RMA, not in its
// artifacts: the adapter ran the host CLI, and what the host now lists is the
// thing that has to come back. The e2e remote-archive leg is exactly this shape
// and it is where `remove` first went silent — the receipt carried two
// host-install ops and no artifacts at all, `held` was false, no action was
// built, and the command printed "no installed cell" and exited 0 with the
// plugin still enabled.
//
// So the contract these tests pin is one sentence: **removed means the host no
// longer lists it**, and every way of failing to reach that is an error.
//
// The tables run over EVERY host, not over the two the e2e happened to fail on.
// `PickStrategy` (pkg/verger) hands a host either the native strategy (a
// marketplace the host CLI resolves) or synth (any remote payload with an
// owner), and both end in host-install ops — so a check that held for claude
// would hold for all ten or for none of them. One table entry that quietly
// stopped registering is exactly the kind of gap this shape catches.

// nativeHost is a host whose delivery REGISTERS a plugin instead of writing a
// file: the receipt it produces has host-install ops and no artifacts, exactly
// what a synth/native install records.
type nativeHost struct {
	*fakeHost

	plugin      string
	marketplace string
	// stubborn makes Uninstall run and then leave the plugin listed — the host
	// CLI that answers without acting.
	stubborn bool
}

// ID implements host.Host.
func (h *nativeHost) ID() host.ID { return h.id }

// Detect implements host.Host.
//
// The embedded selector is not redundant here: `h.Detect` would resolve to
// THIS method and call it forever.
func (h *nativeHost) Detect(home string) bool { //nolint:staticcheck // QF1008: see above
	return h.fakeHost.Detect(home)
}

// Oracle implements host.Host.
func (h *nativeHost) Oracle() host.Oracle { return &fakeOracle{host: h.fakeHost} }

// Deliver implements host.Host: a registration, not a file write.
func (h *nativeHost) Deliver(_ context.Context, _ string, d host.Delivery) (host.Result, error) {
	if d.DryRun {
		return host.Result{Strategy: d.Strategy, RMA: h.rma()}, nil
	}

	h.mu.Lock()
	h.delivers++
	h.installed = append(h.installed, host.Installed{
		Name: h.plugin, Marketplace: h.marketplace, Enabled: true,
	})
	h.mu.Unlock()

	return host.Result{Strategy: d.Strategy, RMA: h.rma()}, nil
}

// Uninstall implements host.Host.
func (h *nativeHost) Uninstall(context.Context, string, receipt.Receipt) (host.Result, error) {
	h.mu.Lock()
	defer h.mu.Unlock()

	h.uninstall++

	if h.stubborn {
		return host.Result{}, nil
	}

	kept := h.installed[:0]

	for _, entry := range h.installed {
		if entry.Name != h.plugin || entry.Marketplace != h.marketplace {
			kept = append(kept, entry)
		}
	}

	h.installed = kept

	return host.Result{}, nil
}

// rma is the reverse the host CLI would run.
func (h *nativeHost) rma() []receipt.Op {
	id := h.plugin + "@" + h.marketplace

	return []receipt.Op{
		{Kind: receipt.OpHostInstall, Command: []string{"plugin", "marketplace", "rm", h.marketplace}},
		{Kind: receipt.OpHostInstall, Command: []string{"plugin", "uninstall", id}},
	}
}

// nativeWorld is a facade world whose single adapter registers plugins for one
// host id.
func nativeWorld(t *testing.T, id host.ID, stubborn bool) (Paths, *Client, *nativeHost) {
	t.Helper()

	world, _ := newFacadeWorld(t)

	native := &nativeHost{
		fakeHost:    &fakeHost{id: id},
		plugin:      "e2e-fixture",
		marketplace: "acme",
		stubborn:    stubborn,
	}

	client, err := Open(t.Context(), WithHosts(native))
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}

	t.Cleanup(func() { _ = client.Close() })

	_ = world

	paths, err := client.Paths(User, "")
	if err != nil {
		t.Fatalf("paths: %v", err)
	}

	return paths, client, native
}

// seedRegistration writes the receipt a synth/native install leaves and puts
// the plugin on the host's own list, which is the state the e2e remote-archive
// leg reaches.
//
// The receipt is seeded rather than produced by a delivery because the facade's
// own fixtures only reach the LOOSE strategy: a local payload is delivered
// loose by rule (DESIGN §2.1), and that writes artifacts, so it cannot produce
// the shape under test — a package held entirely by host-install ops. The shape
// is not invented here; it is the receipt the e2e leg prints.
func seedRegistration(t *testing.T, paths Paths, native *nativeHost, strategy string) {
	t.Helper()

	record := receipt.Receipt{
		Schema:   receipt.Schema,
		Package:  "acme/e2e-fixture",
		Host:     string(native.ID()),
		Scope:    "user",
		Strategy: strategy,
		Version:  "1.0.0",
		RMA:      native.rma(),
	}

	So(receipt.NewStore(paths.ReceiptsDir).Put(record), ShouldBeNil)

	native.mu.Lock()
	defer native.mu.Unlock()

	native.installed = append(native.installed, host.Installed{
		Name: native.plugin, Marketplace: native.marketplace, Enabled: true,
	})
}

// removeRegistration plans and runs one removal, returning the report.
func removeRegistration(t *testing.T, client *Client, paths Paths, native *nativeHost) *apply.Report {
	t.Helper()

	removal, err := client.PlanRemove(t.Context(), "acme/e2e-fixture", RemoveOptions{
		Paths: paths, Hosts: []host.Host{native},
	})
	So(err, ShouldBeNil)

	report, err := client.Remove(t.Context(), removal, ApplyOptions{})
	So(err, ShouldBeNil)

	return report
}

// The invariant, once per host: removed means the host's own list is clean.
func TestRemovingARegisteredPluginOnEveryHostClearsItsOracle(t *testing.T) {
	for _, id := range host.All() {
		t.Run(string(id), func(t *testing.T) {
			Convey("Given "+string(id)+" holding a registered plugin", t, func() {
				paths, client, native := nativeWorld(t, id, false)

				seedRegistration(t, paths, native, "synth")

				Convey("Then it is listed before anything is done", func() {
					listed, err := native.Oracle().List(t.Context())
					So(err, ShouldBeNil)
					So(listed, ShouldNotBeEmpty)
				})

				Convey("When the package is removed", func() {
					removal, err := client.PlanRemove(t.Context(), "acme/e2e-fixture", RemoveOptions{
						Paths: paths, Hosts: []host.Host{native},
					})
					So(err, ShouldBeNil)

					Convey("Then the removal is a real action, not an empty plan", func() {
						So(len(removal.Actions), ShouldEqual, 1)
						So(removal.Actions[0].Host, ShouldEqual, id)
					})

					removalReport := removeRegistration(t, client, paths, native)

					Convey("Then the host CLI was asked to uninstall it", func() {
						So(native.uninstall, ShouldBeGreaterThan, 0)
					})

					Convey("And removal is what the host's own list says it is", func() {
						So(removalReport.Cells[0].Status, ShouldNotEqual, "failed")

						after, listErr := native.Oracle().List(t.Context())
						So(listErr, ShouldBeNil)
						So(after, ShouldBeEmpty)
					})
				})
			})
		})
	}
}

// The other half of the same table: a host that answers without acting must not
// be reported as removed. This is the case the uniform oracle re-read in
// pkg/apply exists for — no adapter asks its own oracle, so removing that
// check has to fail here for EVERY host.
func TestARegistrationThatSurvivesUninstallFailsOnEveryHost(t *testing.T) {
	for _, id := range host.All() {
		t.Run(string(id), func(t *testing.T) {
			Convey("Given "+string(id)+" whose CLI answers and keeps the plugin", t, func() {
				paths, client, native := nativeWorld(t, id, true)

				seedRegistration(t, paths, native, "synth")

				removal, err := client.PlanRemove(t.Context(), "acme/e2e-fixture", RemoveOptions{
					Paths: paths, Hosts: []host.Host{native},
				})
				So(err, ShouldBeNil)
				So(len(removal.Actions), ShouldEqual, 1)

				report := removeRegistration(t, client, paths, native)

				Convey("Then the cell is failed, not removed", func() {
					// The facade reports a cell's fate in the report; the CLI
					// turns a failed cell into a non-zero exit (pkg/cli
					// failedCells), so this is the signal a user sees.
					So(len(report.Cells), ShouldEqual, 1)
					So(string(report.Cells[0].Status), ShouldEqual, "failed")
					So(strings.Join(report.Cells[0].Notes, " "), ShouldContainSubstring, "still lists")
				})

				Convey("And the receipt survives, so the plugin is still a thing to remove", func() {
					So(fileExists(filepath.Join(paths.ReceiptsDir, "acme", "e2e-fixture", string(id)+"-user.json")), ShouldBeTrue)
				})
			})
		})
	}
}

// fileExists reports whether path is there.
func fileExists(path string) bool {
	_, err := os.Stat(path)

	return err == nil
}
