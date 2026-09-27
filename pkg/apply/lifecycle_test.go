package apply

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	. "github.com/smartystreets/goconvey/convey"

	"github.com/odiumuniverse/verger/pkg/digest"
	"github.com/odiumuniverse/verger/pkg/host"
	"github.com/odiumuniverse/verger/pkg/receipt"
)

// rollbackResidueHost writes one file, registers one host resource and fails;
// undoing the host resource fails too (a `mcp remove` of a server whose add
// never landed).
type rollbackResidueHost struct {
	path string
}

// ID implements host.Host.
func (h *rollbackResidueHost) ID() host.ID { return fxClaude }

// Detect implements host.Host.
func (h *rollbackResidueHost) Detect(string) bool { return true }

// Oracle implements host.Host.
func (h *rollbackResidueHost) Oracle() host.Oracle { return noOracle{} }

// Uninstall implements host.Host.
func (h *rollbackResidueHost) Uninstall(context.Context, string, receipt.Receipt) (host.Result, error) {
	return host.Result{}, errors.New("No MCP server found with name: github")
}

// Deliver implements host.Host.
func (h *rollbackResidueHost) Deliver(_ context.Context, _ string, d host.Delivery) (host.Result, error) {
	data := []byte("# payload\n")
	result := host.Result{
		Strategy: d.Strategy,
		RMA: []receipt.Op{
			{Kind: receipt.OpWriteFile, Path: h.path, Digest: digest.Bytes(data), Mode: 0o600},
			{Kind: receipt.OpHostInstall, Command: []string{"claude", "mcp", "remove", "github"}},
		},
	}

	if d.DryRun {
		return result, nil
	}

	if err := os.MkdirAll(filepath.Dir(h.path), 0o700); err != nil { //nolint:gosec // G703: test-owned temp path
		return host.Result{}, err
	}

	if err := os.WriteFile(h.path, data, 0o600); err != nil { //nolint:gosec // G703: test-owned temp path
		return host.Result{}, err
	}

	return host.Result{}, &host.DeliveryError{
		Host: string(fxClaude), Package: d.Package.ID, Step: "install",
		Cause: errors.New("mcp add failed"),
	}
}

// NF-3 (T1.6/T1.7 verify): a rollback continues past a failing reverse op, so
// one stale host resource cannot leave every file of the failed delivery.
func TestRunRollbackContinuesPastFailedOp(t *testing.T) {
	Convey("Given a delivery that wrote a file and failed, whose host undo also fails", t, func() {
		w := newWorld(t)
		path := filepath.Join(w.root, "skills", "tool", "SKILL.md")
		w.deps.Hosts[fxClaude] = &rollbackResidueHost{path: path}

		report, err := w.run(t, context.Background(), Plan{Actions: []Action{installAction(fxClaude, ActionInstall, fxVersion, nil)}}, Options{})

		Convey("When the install is rolled back", func() {
			Convey("Then the file is removed and the failed host undo is reported", func() {
				So(err, ShouldBeNil)
				So(report.Cells[0].Status, ShouldEqual, StatusFailed)
				So(fileExists(path), ShouldBeFalse)
				So(slices.ContainsFunc(report.Cells[0].Notes, func(n string) bool {
					return strings.Contains(n, "rollback:") && strings.Contains(n, "No MCP server found")
				}), ShouldBeTrue)
			})
		})
	})
}

// storedReceipt reads the fixture cell's receipt or fails the test.
func (w *world) storedReceipt(t *testing.T) receipt.Receipt {
	t.Helper()

	r, ok, err := w.deps.Receipts.Get(fxPkg, string(fxClaude), fxUser)
	if err != nil || !ok {
		t.Fatalf("receipt: ok=%v err=%v", ok, err)
	}

	return r
}

// NF-1 (T1.6/T1.7 verify): re-delivering or updating a package must not record
// verger's own previous artifact as the pre-install state, or a later removal
// restores it instead of leaving no residue.
func TestRunRedeliveryKeepsPreInstallState(t *testing.T) {
	Convey("Given a package delivered twice to a path verger created", t, func() {
		w := newWorld(t)
		f := w.fake(t, fxClaude)
		path := filepath.Join(w.root, "skills", "tool", "SKILL.md")
		ctx := context.Background()

		f.write(fxPkg, fakeFile{Path: path, Data: "# v1\n"})

		_, err := w.run(t, ctx, Plan{Actions: []Action{installAction(fxClaude, ActionInstall, fxVersion, nil)}}, Options{})
		So(err, ShouldBeNil)

		Convey("When the second delivery is an update carrying the previous receipt and the package is removed", func() {
			prev := w.storedReceipt(t)

			f.write(fxPkg, fakeFile{Path: path, Data: "# v2\n"})

			report, err := w.run(t, ctx, Plan{Actions: []Action{installAction(fxClaude, ActionUpdate, fxNext, &prev)}}, Options{})
			So(err, ShouldBeNil)
			So(report.Cells[0].Status, ShouldEqual, StatusCurrent)

			updated := w.storedReceipt(t)

			Convey("Then the receipt keeps the pre-install state of the path", func() {
				So(updated.RMA, ShouldHaveLength, 1)
				So(updated.RMA[0].Existed, ShouldBeFalse)
				So(updated.RMA[0].Backup, ShouldBeEmpty)
			})

			Convey("Then the intermediate backup of verger's own v1 is purged", func() {
				entries, err := w.store.Trash().List()
				So(err, ShouldBeNil)
				So(entries, ShouldBeEmpty)
			})

			Convey("Then removal leaves no residue", func() {
				removed, err := w.run(t, ctx, Plan{Actions: []Action{removeAction(fxClaude, updated, string(receipt.CauseUser), string(fxClaude))}}, Options{})
				So(err, ShouldBeNil)
				So(removed.Cells[0].Status, ShouldEqual, StatusCurrent)
				So(fileExists(path), ShouldBeFalse)
			})
		})

		Convey("When the second delivery is a re-install without a previous receipt (the CLI shape) and the package is removed", func() {
			f.write(fxPkg, fakeFile{Path: path, Data: "# v1 again\n"})

			report, err := w.run(t, ctx, Plan{Actions: []Action{installAction(fxClaude, ActionInstall, fxVersion, nil)}}, Options{})
			So(err, ShouldBeNil)
			So(report.Cells[0].Status, ShouldEqual, StatusCurrent)

			stored := w.storedReceipt(t)

			Convey("Then the stored receipt still records a path verger created", func() {
				So(stored.RMA, ShouldHaveLength, 1)
				So(stored.RMA[0].Existed, ShouldBeFalse)
				So(stored.RMA[0].Backup, ShouldBeEmpty)
			})

			Convey("Then removal leaves no residue", func() {
				removed, err := w.run(t, ctx, Plan{Actions: []Action{removeAction(fxClaude, stored, string(receipt.CauseUser), string(fxClaude))}}, Options{})
				So(err, ShouldBeNil)
				So(removed.Cells[0].Status, ShouldEqual, StatusCurrent)
				So(fileExists(path), ShouldBeFalse)
			})
		})
	})

	Convey("Given a user file that existed before verger, installed over and then updated", t, func() {
		w := newWorld(t)
		f := w.fake(t, fxClaude)
		path := filepath.Join(w.root, "skills", "tool", "SKILL.md")
		ctx := context.Background()

		writeFixture(t, path, "# user\n")
		f.write(fxPkg, fakeFile{Path: path, Data: "# v1\n"})

		_, err := w.run(t, ctx, Plan{Actions: []Action{installAction(fxClaude, ActionInstall, fxVersion, nil)}}, Options{})
		So(err, ShouldBeNil)

		prev := w.storedReceipt(t)
		userBackup := prev.RMA[0].Backup

		f.write(fxPkg, fakeFile{Path: path, Data: "# v2\n"})

		_, err = w.run(t, ctx, Plan{Actions: []Action{installAction(fxClaude, ActionUpdate, fxNext, &prev)}}, Options{})
		So(err, ShouldBeNil)

		updated := w.storedReceipt(t)

		Convey("When the package is removed", func() {
			removed, err := w.run(t, ctx, Plan{Actions: []Action{removeAction(fxClaude, updated, string(receipt.CauseUser), string(fxClaude))}}, Options{})

			Convey("Then the update kept the user's backup and removal restores the user's file", func() {
				So(userBackup, ShouldNotBeEmpty)
				So(updated.RMA[0].Existed, ShouldBeTrue)
				So(updated.RMA[0].Backup, ShouldEqual, userBackup)
				So(err, ShouldBeNil)
				So(removed.Cells[0].Status, ShouldEqual, StatusCurrent)
				So(readFixture(t, path), ShouldEqual, "# user\n")
			})
		})
	})
}
