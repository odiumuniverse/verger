package apply

import (
	"testing"

	. "github.com/smartystreets/goconvey/convey"

	"github.com/odiumuniverse/verger/pkg/host"
)

// One cell, one action. A plan that installs the same package to the same
// host twice runs the executor twice over the same paths: the first run
// writes, the second finds what the first wrote and — having no receipt of
// its own to compare against — calls it someone else's, or reports the cell
// as restored when nothing was ever installed here.
//
// The duplicate is easy to come by: a lock entry and a spec entry that name
// the same package in different spellings both resolve to one id.
func TestDedupeActionsKeepsOneActionPerCell(t *testing.T) {
	Convey("Given two actions for the same package and host", t, func() {
		actions := []Action{
			{Kind: ActionInstall, Host: host.Claude, Restored: true, Delivery: host.Delivery{
				Package: host.Package{ID: "local:caveman"},
			}},
			{Kind: ActionInstall, Host: host.Claude, Delivery: host.Delivery{
				Package: host.Package{ID: "local:caveman"},
			}},
		}

		Convey("Then only the first survives", func() {
			got := DedupeActions(actions)

			So(got, ShouldHaveLength, 1)
			So(got[0].Restored, ShouldBeTrue)
		})
	})

	Convey("Given the same package on two hosts", t, func() {
		actions := []Action{
			{Kind: ActionInstall, Host: host.Claude, Delivery: host.Delivery{
				Package: host.Package{ID: "local:caveman"},
			}},
			{Kind: ActionInstall, Host: host.Codex, Delivery: host.Delivery{
				Package: host.Package{ID: "local:caveman"},
			}},
		}

		Convey("Then both are kept, because they are different cells", func() {
			So(DedupeActions(actions), ShouldHaveLength, 2)
		})
	})

	Convey("Given two different packages on one host", t, func() {
		actions := []Action{
			{Kind: ActionInstall, Host: host.Claude, Delivery: host.Delivery{
				Package: host.Package{ID: "local:caveman"},
			}},
			{Kind: ActionInstall, Host: host.Claude, Delivery: host.Delivery{
				Package: host.Package{ID: "local:hammer"},
			}},
		}

		Convey("Then both are kept", func() {
			So(DedupeActions(actions), ShouldHaveLength, 2)
		})
	})

	Convey("Given a removal and an install of the same cell", t, func() {
		actions := []Action{
			{Kind: ActionRemove, Host: host.Claude, Delivery: host.Delivery{
				Package: host.Package{ID: "local:caveman"},
			}},
			{Kind: ActionInstall, Host: host.Claude, Delivery: host.Delivery{
				Package: host.Package{ID: "local:caveman"},
			}},
		}

		Convey("Then both are kept, because the order is the point", func() {
			// A remove followed by an install is how a package is replaced;
			// collapsing it would either leave the old files or drop the new
			// package on the floor.
			So(DedupeActions(actions), ShouldHaveLength, 2)
		})
	})
}
