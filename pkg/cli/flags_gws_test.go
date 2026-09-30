package cli

import (
	"testing"

	. "github.com/smartystreets/goconvey/convey"
)

// forceFlagVerbs are the verbs that can overwrite a file a person edited.
// Every one of them must carry --force, or the reader has no way to authorise
// the overwrite the tool would otherwise refuse.
var forceFlagVerbs = []string{"install", "remove", "sync", "update", "adopt"}

// noForceFlagVerbs are the verbs that cannot overwrite anyone's copy: they
// write the spec, a lock, a keychain entry or a config switch. They must not
// carry --force, because a flag that is accepted and then ignored is worse
// than a missing flag — the reader concludes the overwrite was authorised
// when nothing was overwritten.
var noForceFlagVerbs = []string{"pin", "unpin", "enable", "disable", "approve", "pack"}

// TestForceFlagAppearsOnlyWhereItIsHonoured pins the fix: --force used to
// ride in on addWriteFlags and therefore reached every write verb, while only
// install, remove, adopt, sync and update actually passed it to the facade.
func TestForceFlagAppearsOnlyWhereItIsHonoured(t *testing.T) {
	Convey("Given the command tree", t, func() {
		Convey("Then every verb that can overwrite a user's file offers --force", func() {
			for _, name := range forceFlagVerbs {
				cmd, _, err := NewRootCmd(Options{}).Find([]string{name})
				So(err, ShouldBeNil)
				So(cmd.Flags().Lookup("force"), ShouldNotBeNil)
			}
		})

		Convey("Then no verb that cannot offers --force", func() {
			for _, name := range noForceFlagVerbs {
				cmd, _, err := NewRootCmd(Options{}).Find([]string{name})
				So(err, ShouldBeNil)
				So(cmd.Flags().Lookup("force"), ShouldBeNil)
			}
		})
	})
}

// TestHostFilterOnlyWhereItFilters guards the same lie in the other
// direction: `pin` writes the spec, whose entries have no per-host cells, so
// a --hosts flag on it could only ever be ignored.
func TestHostFilterOnlyWhereItFilters(t *testing.T) {
	Convey("Given the verbs that filter by host", t, func() {
		// status carries it too: it reports a package x host matrix, so narrowing
		// it is the question it already answers, and a requested host this
		// machine does not detect must be the same refusal install gives rather
		// than a silently empty column.
		Convey("Then the delivery verbs and status carry --hosts and --except", func() {
			for _, name := range []string{"install", "remove", "sync", "update", "adopt", "status"} {
				cmd, _, err := NewRootCmd(Options{}).Find([]string{name})
				So(err, ShouldBeNil)
				So(cmd.Flags().Lookup("hosts"), ShouldNotBeNil)
				So(cmd.Flags().Lookup("except"), ShouldNotBeNil)
			}
		})

		Convey("Then a spec-only verb carries neither", func() {
			cmd, _, err := NewRootCmd(Options{}).Find([]string{"pin"})
			So(err, ShouldBeNil)
			So(cmd.Flags().Lookup("hosts"), ShouldBeNil)
			So(cmd.Flags().Lookup("except"), ShouldBeNil)
		})
	})
}

// TestYesAppearsOnlyWhereSomethingAsks closes W6-NITS N015. -y means "do not
// ask me", so a verb that never asks must not offer it: a user who types
// `verger pin -y` concludes a confirmation was skipped, and none existed.
func TestYesAppearsOnlyWhereSomethingAsks(t *testing.T) {
	Convey("Given the verbs that gate a write on a confirmation", t, func() {
		// Each of these calls requireConfirmation or ask before writing.
		for _, name := range []string{"install", "remove", "sync", "update", "adopt"} {
			Convey("When "+name+" is inspected", func() {
				cmd, _, err := NewRootCmd(Options{}).Find([]string{name})
				So(err, ShouldBeNil)
				So(cmd.Flags().ShorthandLookup("y"), ShouldNotBeNil)
			})
		}
	})

	Convey("Given the verbs that write a record without asking", t, func() {
		for _, name := range []string{"pin", "unpin", "enable", "disable", "approve", "revoke", "pack"} {
			Convey("When "+name+" is inspected", func() {
				cmd, _, err := NewRootCmd(Options{}).Find([]string{name})
				So(err, ShouldBeNil)

				Convey("Then it offers --dry-run but not -y", func() {
					So(cmd.Flags().Lookup("dry-run"), ShouldNotBeNil)
					So(cmd.Flags().ShorthandLookup("y"), ShouldBeNil)
				})
			})
		}
	})
}

// TestEveryVerbInheritsJSON pins the flag a front end drives verger with: it
// is persistent on the root, so it must reach every subcommand.
func TestEveryVerbInheritsJSON(t *testing.T) {
	Convey("Given the verbs a caller scripts", t, func() {
		for _, name := range []string{"install", "remove", "sync", "status", "why", "update", "pin", "enable", "disable", "doctor"} {
			Convey("When "+name+" is inspected", func() {
				cmd, _, err := NewRootCmd(Options{}).Find([]string{name})
				So(err, ShouldBeNil)
				So(cmd.InheritedFlags().Lookup("json"), ShouldNotBeNil)
			})
		}
	})
}
