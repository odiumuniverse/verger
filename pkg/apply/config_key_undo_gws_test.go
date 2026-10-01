package apply

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	. "github.com/smartystreets/goconvey/convey"

	"github.com/odiumuniverse/verger/pkg/lock"
	"github.com/odiumuniverse/verger/pkg/receipt"
)

// A removal has to take away the keys verger wrote into a host's settings.json —
// hooks, MCP records — and leave everything else in that document exactly as it
// found it.
//
// It could not, and the reason was an absence read as a fault. A config-key op
// records `Existed: true` with no Backup whenever the key already held exactly
// what the package wanted: there was no replaced value to put in the trash, so
// there is no backup id. The undo read that empty Backup as "nothing to restore"
// and refused with hands-off — so `verger remove` left the package's own hook
// wired into the host forever, and the only way to unhook it was to edit
// settings.json by hand.
//
// The claim that makes the removal safe is the VALUE: the key on disk still
// hashes to what the receipt recorded, so it is still the key verger owns. When
// the value has moved, that is the user's edit, and hands-off is the answer.

// hooksReceipt is a previous receipt that owns the `hooks` key of a settings
// document with no backup recorded — the shape a real hook delivery leaves
// behind when the key was already correct.
func hooksReceipt(t *testing.T, settings string, owned map[string]any) receipt.Receipt {
	t.Helper()

	sum := valueDigest(t, owned)

	return receipt.Receipt{
		Package: fxPkg, Host: string(fxClaude), Scope: fxUser,
		Strategy: string(lock.StrategyNative), Version: fxVersion,
		Artifacts: []receipt.Artifact{
			{Kind: "hook", Name: "hooks", Path: settings, Digest: sum},
		},
		RMA: []receipt.Op{{
			Kind: receipt.OpConfigKey, Path: settings, KeyPath: "hooks",
			Digest: sum, Existed: true,
		}},
	}
}

// TestARemovedPackageTakesItsHookKeyAndLeavesForeignOnes is the fix: the
// package's own key goes, the user's own keys stay.
func TestARemovedPackageTakesItsHookKeyAndLeavesForeignOnes(t *testing.T) {
	Convey("Given a settings document holding verger's hook key and two keys of the user's", t, func() {
		w := newWorld(t)
		w.fake(t, fxClaude)

		settings := filepath.Join(w.root, "settings.json")

		owned := map[string]any{"PreToolUse": []any{map[string]any{"acme": true}}}

		writeFixture(t, settings,
			`{"hooks":{"PreToolUse":[{"acme":true}]},"model":"opus","permissions":{"allow":["Bash"]}}`+"\n")

		prev := hooksReceipt(t, settings, owned)

		report, err := w.run(t, context.Background(), Plan{Actions: []Action{
			removeAction(fxClaude, prev, string(receipt.CauseUser), ""),
		}}, Options{})

		So(err, ShouldBeNil)

		Convey("Then the removal completes rather than refusing", func() {
			So(report.Cells[0].Status, ShouldEqual, StatusCurrent)
		})

		Convey("Then the package's own key is gone", func() {
			So(readFixture(t, settings), ShouldNotContainSubstring, "PreToolUse")
		})

		Convey("Then the user's own keys are untouched", func() {
			So(readFixture(t, settings), ShouldContainSubstring, `"model"`)
			So(readFixture(t, settings), ShouldContainSubstring, "Bash")
		})
	})
}

// TestARemovedPackageHandsOffWhenTheUserEditedItsKey is the other half: the same
// claim, when the value has moved since the receipt. That is the user's edit and
// hands-off is the only honest answer — but it must SAY so, naming the key, so a
// user who ran remove knows why their hook is still wired.
func TestARemovedPackageHandsOffWhenTheUserEditedItsKey(t *testing.T) {
	Convey("Given the key verger owned, edited by the user since the receipt", t, func() {
		w := newWorld(t)
		w.fake(t, fxClaude)

		settings := filepath.Join(w.root, "settings.json")

		owned := map[string]any{"PreToolUse": []any{map[string]any{"acme": true}}}

		writeFixture(t, settings, `{"hooks":{"PreToolUse":[{"acme":true},{"user":true}]}}`+"\n")

		prev := hooksReceipt(t, settings, owned)

		report, err := w.run(t, context.Background(), Plan{Actions: []Action{
			removeAction(fxClaude, prev, string(receipt.CauseUser), ""),
		}}, Options{})

		So(err, ShouldBeNil)

		Convey("Then the cell is hands-off", func() {
			So(report.Cells[0].Status, ShouldEqual, StatusHandsOff)
		})

		Convey("Then the document is left exactly as it was", func() {
			So(readFixture(t, settings), ShouldEqual, `{"hooks":{"PreToolUse":[{"acme":true},{"user":true}]}}`+"\n")
		})

		Convey("Then the note names the key and says it was left in place", func() {
			notes := strings.Join(report.Cells[0].Notes, " ")

			So(notes, ShouldContainSubstring, "hooks")
			So(notes, ShouldContainSubstring, "hands-off")
			So(notes, ShouldContainSubstring, "left in place")
		})
	})
}

// TestARolledBackDeliveryLeavesAKeyItNeverReplaced keeps the two modes apart. A
// rollback undoes what the failing delivery wrote; a key it never replaced has
// nothing to undo, and taking it away would destroy a value the user had before
// the run started.
func TestARolledBackDeliveryLeavesAKeyItNeverReplaced(t *testing.T) {
	Convey("Given a config key recorded without a backup and a failing install", t, func() {
		w := newWorld(t)

		settings := filepath.Join(w.root, "settings.json")

		owned := map[string]any{"PreToolUse": []any{map[string]any{"acme": true}}}

		writeFixture(t, settings, `{"hooks":{"PreToolUse":[{"acme":true}]}}`+"\n")

		f := w.fake(t, fxClaude)
		f.mu.Lock()
		f.deliverErr = errors.New("host refused")
		f.mu.Unlock()

		prev := hooksReceipt(t, settings, owned)

		_, err := w.run(t, context.Background(), Plan{Actions: []Action{
			installAction(fxClaude, ActionInstall, fxVersion, &prev),
		}}, Options{})

		So(err, ShouldBeNil)

		Convey("Then the key survives the rollback", func() {
			So(readFixture(t, settings), ShouldEqual, `{"hooks":{"PreToolUse":[{"acme":true}]}}`+"\n")
		})

		Convey("And the cell says the delivery failed", func() {
			So(f.delivers[fxPkg], ShouldEqual, 0)
		})
	})
}

// TestARollbackLeavesAKeyThePackageFoundRatherThanReplaced is the guard at
// config.go:221, and it is a different question from the test above it.
//
// That one fails the delivery, so nothing was ever replaced and the file being
// unchanged says nothing about the rollback branch: a run that never reached
// the undo proves the undo harmless. This one is the shape a real host has: it
// PUT the key into settings.json, then tripped over a later step and told the
// run what it had already written. The rollback now reaches undoConfigKey with
// a key whose digest matches, whose `Existed` is true and whose Backup is empty
// — the exact op a removal is allowed to unset.
//
// A removal may unset it: the value is the package's, and a removal that left
// it behind wires a deleted package into the host for good. A rollback may
// not: the key held that value before the run started, the failing delivery
// never replaced it, and unsetting it destroys something the user had. Same op,
// +// same digest, opposite answer — which is the whole reason the branch exists.
func TestARollbackLeavesAKeyThePackageFoundRatherThanReplaced(t *testing.T) {
	Convey("Given a config key recorded without a backup and a delivery that failed after writing it", t, func() {
		w := newWorld(t)

		settings := filepath.Join(w.root, "settings.json")

		owned := map[string]any{"PreToolUse": []any{map[string]any{"acme": true}}}

		writeFixture(t, settings, `{"hooks":{"PreToolUse":[{"acme":true}]}}`+"\n")

		f := w.fake(t, fxClaude)

		// The host planned fine, put the key into the document, then tripped
		// over a later step and said what it had written.
		f.mu.Lock()
		f.failAfterWrite = true
		f.rmaOnErr = []receipt.Op{{
			Kind: receipt.OpConfigKey, Path: settings, KeyPath: "hooks",
			Digest: valueDigest(t, owned), Existed: true,
		}}
		f.mu.Unlock()

		prev := hooksReceipt(t, settings, owned)

		report, err := w.run(t, context.Background(), Plan{Actions: []Action{
			installAction(fxClaude, ActionInstall, fxVersion, &prev),
		}}, Options{})

		So(err, ShouldBeNil)

		Convey("Then the key the user already had is still there", func() {
			So(readFixture(t, settings), ShouldEqual, `{"hooks":{"PreToolUse":[{"acme":true}]}}`+"\n")
		})

		Convey("And the rollback says it left the key alone, naming it", func() {
			notes := strings.Join(report.Cells[0].Notes, "\n")
			So(notes, ShouldContainSubstring, "hands-off")
			So(notes, ShouldContainSubstring, "hooks")
		})

		Convey("And the cell says the delivery failed", func() {
			So(report.Cells[0].Status, ShouldNotEqual, StatusCurrent)
		})
	})
}
