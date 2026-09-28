package receipt

import (
	"testing"

	. "github.com/smartystreets/goconvey/convey"
)

func TestJournalRollbackEvent(t *testing.T) {
	Convey("Given an empty journal", t, func() {
		j := newTestJournal(t)

		Convey("When a rollback event closes a failed intent", func() {
			err := j.Append(Event{
				Kind: EventRollback, Package: "owner/name", Host: "claude", Scope: ScopeUser,
				Version: "2.0.0", Cause: "rollback",
			})

			Convey("Then it is a valid kind and round-trips", func() {
				So(err, ShouldBeNil)

				events, readErr := j.Read()
				So(readErr, ShouldBeNil)
				So(events, ShouldHaveLength, 1)
				So(events[0].Kind, ShouldEqual, EventRollback)
				So(events[0].Package, ShouldEqual, "owner/name")
			})
		})
	})
}
