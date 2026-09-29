package spec

import (
	"errors"
	"testing"

	. "github.com/smartystreets/goconvey/convey"
)

// errorsAs is a thin alias so the assertions below read as one line; it is
// stdlib errors.As, not a project helper.
func errorsAs(err error, target any) bool { return errors.As(err, target) }

// TestMarshalRefusesAnOutOfRangeSchema closes N042 and N044. The guard lived
// only in Save, so the two other callers were unguarded — and one of them is
// Digest, which is what a trust decision is taken against.
func TestMarshalRefusesAnOutOfRangeSchema(t *testing.T) {
	Convey("Given a hand-built spec with no schema", t, func() {
		s := &Spec{}

		Convey("When it is marshalled", func() {
			_, err := s.Marshal()

			Convey("Then it is refused as an invalid schema", func() {
				So(err, ShouldNotBeNil)

				invalid := &SchemaInvalidError{}
				So(errorsAs(err, &invalid), ShouldBeTrue)
				So(invalid.Found, ShouldEqual, 0)
			})
		})
	})
}

// TestMarshalRefusesANewerSchema is the other direction. A spec written by a
// newer verger must not be re-encoded by this one, however it was reached.
func TestMarshalRefusesANewerSchema(t *testing.T) {
	Convey("Given a spec claiming a newer schema", t, func() {
		s := &Spec{Schema: Schema + 1}

		Convey("When it is marshalled", func() {
			_, err := s.Marshal()

			Convey("Then it is refused as a newer schema", func() {
				So(err, ShouldNotBeNil)

				newer := &SchemaNewerError{}
				So(errorsAs(err, &newer), ShouldBeTrue)
				So(newer.Supported, ShouldEqual, Schema)
			})
		})
	})
}

// TestDigestIsEmptyForAnUnmarshalableSpec is the part that mattered. Before
// the guard reached Marshal, a hand-built spec with Schema 0 produced a real
// content hash, so a document that could never have been written by verger was
// still something a trust decision could be taken against.
func TestDigestIsEmptyForAnUnmarshalableSpec(t *testing.T) {
	// The three cases are separate Convey blocks rather than a loop inside
	// one: GoConvey's context manager does not survive a nested root call, and
	// a loop would have to be a loop of roots.
	Convey("Given a spec with no schema at all", t, func() {
		Convey("Then its digest is empty rather than a real hash", func() {
			So((&Spec{}).Digest(), ShouldBeEmpty)
		})
	})

	Convey("Given a spec with a negative schema", t, func() {
		Convey("Then its digest is empty rather than a real hash", func() {
			So((&Spec{Schema: -1}).Digest(), ShouldBeEmpty)
		})
	})

	Convey("Given a spec claiming a schema from a newer verger", t, func() {
		Convey("Then its digest is empty rather than a real hash", func() {
			So((&Spec{Schema: Schema + 1}).Digest(), ShouldBeEmpty)
		})
	})
}

// TestDigestStillWorksForAValidSpec keeps the guard from being a blanket
// refusal: the ordinary path must be untouched, or every trust record in the
// wild would stop matching.
func TestDigestStillWorksForAValidSpec(t *testing.T) {
	Convey("Given a spec at the current schema", t, func() {
		s := &Spec{Schema: Schema}

		Convey("Then it marshals and hashes", func() {
			data, err := s.Marshal()
			So(err, ShouldBeNil)
			So(string(data), ShouldNotBeEmpty)
			So(s.Digest(), ShouldNotBeEmpty)
		})
	})
}

// TestSaveStillNamesThePath keeps the two guards' division of labour honest:
// Save knows where it was writing, so its error says so, while Marshal — which
// has no path — must not invent one.
func TestSaveStillNamesThePath(t *testing.T) {
	Convey("Given a spec with no schema written to a real path", t, func() {
		s := &Spec{}
		path := t.TempDir() + "/verger.spec"

		Convey("When it is saved", func() {
			err := s.Save(path)

			Convey("Then the refusal names the path", func() {
				So(err, ShouldNotBeNil)

				invalid := &SchemaInvalidError{}
				So(errorsAs(err, &invalid), ShouldBeTrue)
				So(invalid.Path, ShouldEqual, path)
			})
		})
	})
}
