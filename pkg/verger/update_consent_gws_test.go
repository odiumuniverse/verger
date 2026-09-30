package verger

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/odiumuniverse/verger/pkg/spec"
	. "github.com/smartystreets/goconvey/convey"

	"github.com/odiumuniverse/verger/pkg/apply"
)

// Client.Update is the reconcile a second front end calls, and the CLI's
// `verger update` verb is not it: that verb calls the same runner as sync
// (pkg/cli/update.go -> a.runSync -> pkg/cli/sync.go client.Sync), so a CLI-level
// test of "update" is a test of sync wearing the other name. This file drives the
// method itself.
//
// The claim under test is the exit code, and it cannot be written as a number
// here: exitcode imports this package, so an in-package test that imported it
// back would be a cycle. What is asserted instead is the predicate the
// classifier keys on — errors.Is(err, apply.ErrConfirmationRequired) — which is
// the whole of isConsent's branch for this error. The number itself is pinned end
// to end by TestConsentMatrix in pkg/cli.
func TestUpdateUnansweredConsentIsNotASuccess(t *testing.T) {
	Convey("Given a spec that declares one hooked package", t, func() {
		world, client := newFacadeWorld(t)

		ref := world.withHooks(t)
		target := filepath.Join(world.root, "host", "one.md")
		world.fake.artifacts = []fakeArtifact{{Path: target, Data: "# one\n"}}

		paths, err := client.Paths(User, "")
		So(err, ShouldBeNil)

		doc, _, err := LoadSpec(paths.SpecPath)
		So(err, ShouldBeNil)
		So(AddSpecSourceAt(doc, mustRef(t, ref), filepath.Dir(paths.SpecPath)), ShouldBeTrue)
		So(AddSpecPackage(doc, spec.Package{ID: "local:hooked", Version: "1.0.0"}), ShouldBeTrue)
		So(SaveSpec(paths.SpecPath, doc), ShouldBeNil)

		Convey("When the confirmer resolves with the default, as -y does", func() {
			_, _, updateErr := client.Update(t.Context(), SyncOptions{
				Paths:   paths,
				Confirm: yesConfirmer{},
			})

			Convey("Then the run is a consent failure, which is exit 5", func() {
				So(updateErr, ShouldNotBeNil)
				So(errors.Is(updateErr, apply.ErrConfirmationRequired), ShouldBeTrue)
			})

			Convey("Then it names the package whose hooks were withheld", func() {
				So(updateErr.Error(), ShouldContainSubstring, "local:hooked")
			})

			Convey("Then the files are on disk even though the hooks are not", func() {
				// Skipping the hooks is the decision; skipping the package would
				// be a different defect wearing the same error.
				So(fileMissing(target), ShouldBeFalse)
			})
		})

		Convey("When the hooks question is answered instead", func() {
			_, _, updateErr := client.Update(t.Context(), SyncOptions{
				Paths: paths,
				Hooks: HooksYes,
			})

			Convey("Then it is a plain success", func() {
				So(updateErr, ShouldBeNil)
				So(fileMissing(target), ShouldBeFalse)
			})
		})

		Convey("When a confirmer actually asks and a human says yes", func() {
			_, _, updateErr := client.Update(t.Context(), SyncOptions{
				Paths:   paths,
				Confirm: answeringConfirmer{answer: true},
			})

			Convey("Then that is consent, so it is a plain success", func() {
				So(updateErr, ShouldBeNil)
			})
		})

		Convey("When a human is asked and says no", func() {
			_, _, updateErr := client.Update(t.Context(), SyncOptions{
				Paths:   paths,
				Confirm: answeringConfirmer{answer: false},
			})

			Convey("Then it is a plain success, because somebody did answer", func() {
				// Declining is an answer. A run that recorded a refusal as an
				// unanswered question would make it impossible to say no to a hook
				// without failing the command, and "no" is how a user switches a
				// feature off.
				So(updateErr, ShouldBeNil)
			})

			Convey("Then the files are on disk and only the hooks are missing", func() {
				So(fileMissing(target), ShouldBeFalse)
			})
		})

		Convey("When the error is rendered for a user", func() {
			_, _, updateErr := client.Update(t.Context(), SyncOptions{
				Paths:   paths,
				Confirm: yesConfirmer{},
			})
			So(updateErr, ShouldNotBeNil)

			Convey("Then it says the files are in and the hooks are not", func() {
				// A message that only said "hooks skipped" leaves the reader
				// unsure whether the run did anything at all.
				So(strings.ToLower(updateErr.Error()), ShouldContainSubstring, "skip")
			})
		})
	})
}

// answeringConfirmer is a human-shaped confirmer: it was asked, and it answered.
type answeringConfirmer struct {
	answer bool
}

// Confirm implements Confirmer.
func (c answeringConfirmer) Confirm(_ context.Context, _ apply.Question) (bool, error) {
	return c.answer, nil
}
