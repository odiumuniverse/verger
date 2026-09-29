package lock

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	. "github.com/smartystreets/goconvey/convey"
)

// errorsAs is stdlib errors.As under a short name, so the assertions read as
// one line each.
func errorsAs(err error, target any) bool { return errors.As(err, target) }

// TestMarshalRefusesANonPositiveSchema closes N043. marshalCompact refused a
// schema newer than this build but not one at or below zero, so a Lock built
// in code with Schema 0 marshalled and was written — producing a file that
// claims a schema the read path already refused. The tool could write a lock
// it could not read back.
func TestMarshalRefusesANonPositiveSchema(t *testing.T) {
	Convey("Given a lock with no schema at all", t, func() {
		l := &Lock{}

		Convey("When it is marshalled", func() {
			_, err := l.Marshal()

			Convey("Then it is refused as an invalid schema", func() {
				So(err, ShouldNotBeNil)

				invalid := &SchemaInvalidError{}
				So(errorsAs(err, &invalid), ShouldBeTrue)
				So(invalid.Found, ShouldEqual, 0)
			})
		})
	})

	Convey("Given a lock with a negative schema", t, func() {
		l := &Lock{Schema: -3}

		Convey("When it is marshalled", func() {
			_, err := l.Marshal()

			Convey("Then it is refused too", func() {
				So(err, ShouldNotBeNil)

				invalid := &SchemaInvalidError{}
				So(errorsAs(err, &invalid), ShouldBeTrue)
				So(invalid.Found, ShouldEqual, -3)
			})
		})
	})
}

// TestSaveWritesNothingForANonPositiveSchema is the part that protects the
// disk. A refusal that still created a file would be worse than none.
func TestSaveWritesNothingForANonPositiveSchema(t *testing.T) {
	Convey("Given a lock with no schema", t, func() {
		l := &Lock{}
		path := filepath.Join(t.TempDir(), "verger.lock")

		Convey("When it is saved", func() {
			err := l.Save(path)

			Convey("Then it is refused and no file appears", func() {
				So(err, ShouldNotBeNil)
				So(fileExists(path), ShouldBeFalse)
			})
		})
	})
}

// TestMarshalRefusesANewerSchema keeps the guard the tracker already recorded
// (N123) pinned alongside the new one, and pins that it now names the
// supported version, which the old one-liner left at zero.
func TestMarshalRefusesANewerSchema(t *testing.T) {
	Convey("Given a lock from a newer verger", t, func() {
		l := &Lock{Schema: Schema + 1}

		Convey("When it is marshalled", func() {
			_, err := l.Marshal()

			Convey("Then it is refused, naming the supported schema", func() {
				So(err, ShouldNotBeNil)

				newer := &SchemaNewerError{}
				So(errorsAs(err, &newer), ShouldBeTrue)
				So(newer.Supported, ShouldEqual, Schema)
				So(newer.Found, ShouldEqual, Schema+1)
			})
		})
	})
}

// TestMarshalStillWorksForAValidLock keeps the guard from refusing ordinary
// locks, which would break every write in the tool.
func TestMarshalStillWorksForAValidLock(t *testing.T) {
	Convey("Given a lock at the current schema", t, func() {
		l := &Lock{Schema: Schema, Cells: []Cell{}}

		Convey("Then it marshals", func() {
			data, err := l.Marshal()
			So(err, ShouldBeNil)
			So(string(data), ShouldNotBeEmpty)
		})
	})
}

// fileExists reports whether path is present, for the save-refusal assertion.
func fileExists(path string) bool {
	_, err := os.Stat(path)

	return err == nil
}
