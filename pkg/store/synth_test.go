package store

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	. "github.com/smartystreets/goconvey/convey"
)

func TestSynthPathLayout(t *testing.T) {
	Convey("Given an open store", t, func() {
		st := newStore(t, time.Now)

		Convey("When a slashed package id resolves", func() {
			path, err := st.SynthPath("JuliusBrussee/caveman", "1.2.3")

			Convey("Then the synth dir nests the owner, the name and the version", func() {
				So(err, ShouldBeNil)
				So(path, ShouldEqual, filepath.Join(st.Root(), "synth", "JuliusBrussee", "caveman", "1.2.3"))
			})
		})

		Convey("When EnsureSynthPath runs", func() {
			path, err := st.EnsureSynthPath("JuliusBrussee/caveman", "1.2.3")

			Convey("Then the whole chain exists with mode 0700", func() {
				So(err, ShouldBeNil)
				So(path, ShouldEqual, filepath.Join(st.Root(), "synth", "JuliusBrussee", "caveman", "1.2.3"))

				for current := path; current != st.Root(); current = filepath.Dir(current) {
					assertMode(t, current, 0o700)
				}

				_, secondErr := st.EnsureSynthPath("JuliusBrussee/caveman", "1.2.3")
				So(secondErr, ShouldBeNil)
			})
		})

		Convey("When Ensure runs", func() {
			err := st.Ensure()

			Convey("Then the synth root exists", func() {
				So(err, ShouldBeNil)
				assertMode(t, filepath.Join(st.Root(), "synth"), 0o700)
			})
		})
	})
}

func TestSynthPathValidation(t *testing.T) {
	Convey("Given an open store", t, func() {
		st := newStore(t, time.Now)

		Convey("When path elements are hostile", func() {
			for _, pkg := range []string{"", "../x", "a//b", "/abs", "a/../b", "a\\b", "a\x00b"} {
				Convey("Then package id "+pkg+" is refused", func() {
					_, err := st.SynthPath(pkg, "1.0.0")
					_, ok := errors.AsType[*InvalidIDError](err)

					So(ok, ShouldBeTrue)
				})
			}

			for _, version := range []string{"", "..", "a/b", ".", "v 1"} {
				Convey("Then version "+version+" is refused", func() {
					_, err := st.SynthPath("JuliusBrussee/caveman", version)
					_, ok := errors.AsType[*InvalidIDError](err)

					So(ok, ShouldBeTrue)
				})
			}
		})

		Convey("When a refused path is ensured", func() {
			_, err := st.EnsureSynthPath("../escape", "1.0.0")

			Convey("Then nothing is created", func() {
				So(err, ShouldNotBeNil)

				_, statErr := os.Stat(st.Root())
				So(os.IsNotExist(statErr), ShouldBeTrue)
			})
		})
	})
}
